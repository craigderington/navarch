package rollout

import (
	"context"
	"log/slog"
	"time"

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
	st  *store.Store
	log *slog.Logger
}

func NewReaper(st *store.Store, log *slog.Logger) *Reaper {
	return &Reaper{st: st, log: log}
}

// ReapOnce deletes expired preview environments and drops teardown instructions
// no agent will act on any more.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	reaped, err := r.st.ExpireEnvironments(ctx)
	if err != nil {
		return err
	}
	for _, env8 := range reaped {
		r.log.Info("preview environment expired", "env", env8)
	}
	return r.st.SweepTombstones(ctx, TombstoneRetention)
}
