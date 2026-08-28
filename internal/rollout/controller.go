package rollout

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

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
	orgID        *uuid.UUID
	// routeStrand is how long a node may go unheard from before its routes are
	// withdrawn. Zero means never, which is why it is not defaulted here: the
	// production constructor sets it from config, and the test constructors
	// choose deliberately rather than inheriting a number.
	routeStrand time.Duration
	// teardownGrace is how long a deployment must have been terminal before its
	// instance rows are deleted. See ReconcileOnce for why it is a duration and
	// not a tick count.
	teardownGrace time.Duration
	// firstTerminal records when each deployment was first seen terminal.
	// Unmutexed because the controller runs in one goroutine, the same
	// invariant the agent's Reconciler documents for its own maps.
	firstTerminal map[uuid.UUID]time.Time
	// now is injectable so the grace can be tested without sleeping.
	now func() time.Time
}

// DefaultTeardownGrace is how long a superseded revision keeps serving after
// the router has been repointed away from it.
//
// It must exceed the time for a router config change to actually take effect,
// which is not the time to write the file. Traefik's file provider throttles
// provider updates — `providers.providersThrottleDuration`, 2s by default — so
// for up to two seconds after Sync returns, Traefik is still sending traffic to
// the revision this is about to delete. Measured: with no grace at all, every
// promotion dropped ~1.2s of requests with 502s, while both containers were up
// and healthy the whole time. The containers were never the problem; the router
// was still pointing at the old one when its rows were deleted.
//
// Five seconds is deliberately more than double the throttle. The cost is that
// a superseded revision lives a few seconds longer; the benefit is that
// "zero-downtime" is a property rather than a hope.
const DefaultTeardownGrace = 5 * time.Second

// WithTeardownGrace overrides how long a superseded revision is kept after the
// router stops pointing at it. Zero tears down immediately, which is only
// correct when nothing external routes to it.
func WithTeardownGrace(d time.Duration) func(*Controller) {
	return func(c *Controller) { c.teardownGrace = d }
}

// WithRouteStrand sets how long a node may go unheard from before its routes
// are withdrawn. Applied by the control plane from COMPOSECTL_ROUTE_STRAND_SECONDS.
func WithRouteStrand(d time.Duration) func(*Controller) {
	return func(c *Controller) { c.routeStrand = d }
}

func newControllerForOrg(st *store.Store, log *slog.Logger, rtr RouterSync, orgID uuid.UUID) *Controller {
	return &Controller{st: st, log: log, rtr: rtr, startTimeout: 5 * time.Minute, orgID: &orgID}
}

func (c *Controller) listRollouts(ctx context.Context, states ...store.DeploymentState) ([]store.Deployment, error) {
	if c.orgID == nil {
		return c.st.ListRolloutsInState(ctx, states...)
	}
	return c.st.ListRolloutsInStateForOrg(ctx, *c.orgID, states...)
}

func NewController(st *store.Store, log *slog.Logger, rtr RouterSync, opts ...func(*Controller)) *Controller {
	c := &Controller{st: st, log: log, rtr: rtr, startTimeout: 5 * time.Minute,
		teardownGrace: DefaultTeardownGrace}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ReconcileOnce advances every active rollout by the aggregate of its
// instances, and tears down deployments that have left the live path. The
// deployment state machine (enforced in SQL) rejects any illegal nudge, so this
// only ever proposes the next legal step.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	active, err := c.listRollouts(ctx,
		store.DeployScheduling, store.DeployStarting, store.DeployHealthy)
	if err != nil {
		return err
	}
	for _, dep := range active {
		if err := c.advance(ctx, dep); err != nil {
			c.log.Warn("advance failed", "deployment", dep.ID, "err", err)
		}
	}

	// Repoint external traffic FIRST, before anything is torn down. Doing this
	// every tick (not only on promote) is also self-healing: a router restart
	// or a missed write reconverges within a tick.
	//
	// The order is load-bearing. Teardown used to run first, and the two steps
	// then raced: DeleteInstances made the agent GC the superseded revision's
	// containers on its next poll, while Traefik only stopped pointing at them
	// once it had reloaded the file this writes. Whichever lost, external
	// requests hit an address with nothing behind it — a measured ~1.2s window
	// of 502s on every promotion, found by deploying this platform's own
	// marketing site on it and asserting that not one request failed.
	if c.rtr != nil {
		if err := c.syncRouter(ctx); err != nil {
			c.log.Warn("router sync failed", "err", err)
			// Do not tear anything down on a tick where the router could not be
			// updated: the old revision is still the only thing serving.
			return nil
		}
	}

	// Teardown: a superseded or failed deployment's instance rows are deleted;
	// the agent then GCs their swappable containers. Pinned containers survive
	// because the now-live deployment still holds its own rows for them.
	//
	// Held for teardownGrace after a deployment first appears terminal. Writing
	// the router config is not the same as the router having applied it —
	// Traefik throttles provider updates and keeps serving the old target
	// meanwhile — and the agent's GC is a poll interval away on a clock nothing
	// here controls. Ordering alone narrows the race; only waiting closes it.
	terminal, err := c.listRollouts(ctx, store.DeploySuperseded, store.DeployFailed)
	if err != nil {
		return err
	}
	seen := make(map[uuid.UUID]time.Time, len(terminal))
	now := c.clock()
	for _, dep := range terminal {
		first, known := c.firstTerminal[dep.ID]
		if !known {
			first = now
		}
		if now.Sub(first) < c.teardownGrace {
			// Still serving, on purpose. Remember when it became terminal so the
			// grace is measured from that moment and not from this tick.
			seen[dep.ID] = first
			continue
		}
		if err := c.st.DeleteInstances(ctx, dep.ID); err != nil {
			c.log.Warn("teardown failed", "deployment", dep.ID, "err", err)
			seen[dep.ID] = first // retry next tick rather than forgetting it
		}
	}
	c.firstTerminal = seen
	return nil
}

func (c *Controller) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Controller) syncRouter(ctx context.Context) error {
	routes, err := c.st.ListLiveRoutes(ctx, c.routeStrand)
	if err != nil {
		return err
	}
	rr := make([]router.Route, 0, len(routes))
	for _, lr := range routes {
		// No address or no reported port means the agent has not brought the
		// ingress container up yet. Omit the route rather than inventing a
		// target: a wrong one would send this hostname's traffic somewhere,
		// while a missing one simply is not served until the next resync.
		if lr.NodeAddr == "" || lr.PublishedPort == 0 {
			continue
		}
		rr = append(rr, router.Route{
			Key:      lr.Env8,
			Hostname: lr.Hostname,
			Target:   lr.NodeAddr,
			Port:     lr.PublishedPort,
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
		// Read the reason BEFORE failing the deployment: the controller deletes
		// the instance rows on the way out, and the agent's description of what
		// went wrong lives only there. "an instance failed to start" on its own
		// is true of every possible cause and useful for none of them.
		return c.st.UpdateDeploymentState(ctx, dep.ID, store.DeployFailed, c.failureReason(ctx, dep.ID))
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

// failureReason turns the instance-level errors into something an operator can
// act on. It degrades rather than fails: if the detail cannot be read, the
// caller still gets the generic reason and the deployment is still failed —
// losing the explanation must not also lose the state transition.
func (c *Controller) failureReason(ctx context.Context, depID uuid.UUID) string {
	const generic = "an instance failed to start"
	failures, err := c.st.FailedInstances(ctx, depID)
	if err != nil || len(failures) == 0 {
		return generic
	}
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		switch {
		case f.LastError != "":
			parts = append(parts, fmt.Sprintf("%s: %s", f.ServiceName, f.LastError))
		default:
			// Unhealthy with no error is a real case — a container that runs and
			// fails its healthcheck reports no error at all — so name the service
			// and its state rather than inventing a cause.
			parts = append(parts, fmt.Sprintf("%s: %s", f.ServiceName, f.State))
		}
	}
	return generic + ": " + strings.Join(parts, "; ")
}
