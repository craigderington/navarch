package rollout

import (
	"context"
	"log/slog"
	"time"

	"github.com/craig/composectl/internal/router"
	"github.com/craig/composectl/internal/store"
)

// RouterSync repoints external traffic to the current set of live routes. It is
// an interface so the controller stays testable without a filesystem, and so
// the concrete *router.Router (which knows Traefik) is the only Traefik-aware
// piece. Nil is allowed — routing is opt-in via COMPOSECTL_ROUTER_DIR.
type RouterSync interface {
	Sync(routes []router.Route) error
}

type Controller struct {
	st           *store.Store
	log          *slog.Logger
	rtr          RouterSync
	startTimeout time.Duration
}

func NewController(st *store.Store, log *slog.Logger, rtr RouterSync) *Controller {
	return &Controller{st: st, log: log, rtr: rtr, startTimeout: 5 * time.Minute}
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

	// Repoint external traffic to the current live routes. Doing this every
	// tick (not only on promote) is self-healing: a router restart or a missed
	// write reconverges within a tick.
	if c.rtr != nil {
		if err := c.syncRouter(ctx); err != nil {
			c.log.Warn("router sync failed", "err", err)
		}
	}
	return nil
}

func (c *Controller) syncRouter(ctx context.Context) error {
	routes, err := c.st.ListLiveRoutes(ctx)
	if err != nil {
		return err
	}
	rr := make([]router.Route, 0, len(routes))
	for _, lr := range routes {
		rr = append(rr, router.Route{
			Key:              lr.Env8,
			Hostname:         lr.Hostname,
			ServiceContainer: lr.ProjectName + "-" + lr.IngressService,
			Port:             lr.IngressPort,
		})
	}
	return c.rtr.Sync(rr)
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
	case dep.State == store.DeployHealthy:
		// Auto-promote: flip to live atomically (supersedes the old revision).
		// The router sync at the end of the tick repoints Traefik; the terminal
		// teardown deletes the superseded revision's rows so the agent GCs its
		// swappable containers.
		if _, err := c.st.PromoteDeployment(ctx, dep.ID); err != nil {
			return err
		}
		c.log.Info("auto-promoted", "deployment", dep.ID, "revision", dep.Revision)
		return nil
	}
	return nil
}
