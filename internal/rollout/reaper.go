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
}

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
	if r.orgID == nil {
		return r.st.SweepTombstones(ctx, TombstoneRetention)
	}
	return r.st.SweepTombstonesForOrg(ctx, *r.orgID, TombstoneRetention)
}
