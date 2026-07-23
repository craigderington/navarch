package rollout

import (
	"context"
	"log/slog"
	"time"

	"github.com/craig/composectl/internal/store"
)

type Controller struct {
	st           *store.Store
	log          *slog.Logger
	startTimeout time.Duration
}

func NewController(st *store.Store, log *slog.Logger) *Controller {
	return &Controller{st: st, log: log, startTimeout: 5 * time.Minute}
}

// ReconcileOnce advances every active rollout by the aggregate of its
// instances, and tears down deployments that have left the live path. The
// deployment state machine (enforced in SQL) rejects any illegal nudge, so this
// only ever proposes the next legal step.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	active, err := c.st.ListRolloutsInState(ctx,
		store.DeployScheduling, store.DeployStarting, store.DeployHealthy)
	if err != nil {
		return err
	}
	for _, dep := range active {
		if err := c.advance(ctx, dep); err != nil {
			c.log.Warn("advance failed", "deployment", dep.ID, "err", err)
		}
	}

	// Teardown: a superseded or failed deployment's instance rows are deleted;
	// the agent then GCs their swappable containers. Pinned containers survive
	// because the now-live deployment still holds its own rows for them.
	terminal, err := c.st.ListRolloutsInState(ctx, store.DeploySuperseded, store.DeployFailed)
	if err != nil {
		return err
	}
	for _, dep := range terminal {
		if err := c.st.DeleteInstances(ctx, dep.ID); err != nil {
			c.log.Warn("teardown failed", "deployment", dep.ID, "err", err)
		}
	}
	return nil
}

func (c *Controller) advance(ctx context.Context, dep store.Deployment) error {
	states, err := c.st.InstanceStates(ctx, dep.ID)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		return nil // scheduler has not written instances yet
	}

	var pending, failed, healthy int
	for _, s := range states {
		switch s {
		case store.InstancePending:
			pending++
		case store.InstanceFailed, store.InstanceUnhealthy:
			failed++
		case store.InstanceRunning:
			healthy++
		}
	}

	switch {
	case failed > 0:
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, "an instance failed to start")
	case dep.State == store.DeployScheduling && pending == 0:
		// Every instance has a container (moved past pending) → starting.
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployStarting, "")
	case dep.State == store.DeployStarting && healthy == len(states):
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployHealthy, "")
	case dep.State == store.DeployStarting && time.Since(dep.UpdatedAt) > c.startTimeout:
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, "timed out waiting for health")
	}
	// Slice A stops at healthy. Slice B adds: healthy → rewrite Traefik →
	// PromoteDeployment → the terminal teardown above handles the old revision.
	return nil
}
