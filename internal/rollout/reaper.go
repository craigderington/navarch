package rollout

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// TombstoneRetention is how long a teardown instruction is offered to agents.
// It doubles as how long a node may be offline and still clean up after itself:
// past it, that environment's pinned containers and named volumes leak and need
// manual removal.
const TombstoneRetention = 24 * time.Hour

// Reaper is the third control-plane loop. The scheduler places deployments and
// the controller drives them to live; the reaper is what ends them.
type Reaper struct {
	st    *store.Store
	log   *slog.Logger
	orgID *uuid.UUID
	// logs is the in-memory buffer holding delivered container output. The
	// reaper frees a request's buffer when it deletes its row, so memory cannot
	// outlive the instruction that justified holding it.
	logs LogBuffer
}

// LogBuffer is the part of the control plane's log buffer the reaper needs.
// Named here rather than imported concretely so this loop stays testable
// without one, and so nothing tempts a future caller into reading content here.
type LogBuffer interface {
	Drop(id uuid.UUID)
	Expire(maxAge time.Duration) int
}

// WithLogBuffer attaches the buffer whose entries the reaper frees alongside the
// rows they belong to.
func (r *Reaper) WithLogBuffer(b LogBuffer) *Reaper { r.logs = b; return r }

// LogBufferIdleTTL frees a buffer nobody has read or written for this long. A
// requester that walks away mid-tail leaves no row transition behind, so idleness
// is the only signal its memory is dead weight.
const LogBufferIdleTTL = 10 * time.Minute

func newReaperForOrg(st *store.Store, log *slog.Logger, orgID uuid.UUID) *Reaper {
	return &Reaper{st: st, log: log, orgID: &orgID}
}

func NewReaper(st *store.Store, log *slog.Logger) *Reaper {
	return &Reaper{st: st, log: log}
}

// ReapOnce deletes expired preview environments and drops teardown instructions
// no agent will act on any more.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	var reaped []string
	var err error
	if r.orgID == nil {
		reaped, err = r.st.ExpireEnvironments(ctx)
	} else {
		reaped, err = r.st.ExpireEnvironmentsForOrg(ctx, *r.orgID)
	}
	if err != nil {
		return err
	}
	for _, env8 := range reaped {
		r.log.Info("preview environment expired", "env", env8)
	}
	// Expired log instructions, and the memory their answers occupy. Both, and
	// in that order: deleting the rows while leaving the buffers would keep
	// container output — possibly containing secrets — alive in the control
	// plane with nothing left pointing at it.
	swept, err := r.st.SweepLogRequests(ctx)
	if err != nil {
		return err
	}
	if r.logs != nil {
		for _, id := range swept {
			r.logs.Drop(id)
		}
		r.logs.Expire(LogBufferIdleTTL)
	}
	if r.orgID == nil {
		return r.st.SweepTombstones(ctx, TombstoneRetention)
	}
	return r.st.SweepTombstonesForOrg(ctx, *r.orgID, TombstoneRetention)
}
