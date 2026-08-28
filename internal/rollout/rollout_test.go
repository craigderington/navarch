package rollout

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/router"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("COMPOSECTL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable"
	}
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.New(c, dsn)
	if err != nil {
		t.Skipf("postgres unreachable — run make up: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// fixture builds a pending deployment on a ready node and returns ids. Its
// cleanup deletes the org's children bottom-up before the org, because two FKs
// (service_instances→nodes, deployments→stack_versions) lack ON DELETE CASCADE.
func fixture(t *testing.T, st *store.Store) (deployID, nodeID, orgID uuid.UUID) {
	t.Helper()
	slug := "rollout-" + uuid.NewString()[:8]
	org, err := st.CreateOrganization(ctx(t), slug, "Rollout")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exec := func(sql string) { st.Pool().Exec(c, sql, org.ID) }
		exec(`UPDATE environments SET live_deployment_id=NULL WHERE stack_id IN (
			SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`)
		exec(`DELETE FROM service_instances WHERE node_id IN (SELECT id FROM nodes WHERE org_id=$1)`)
		exec(`DELETE FROM deployments WHERE environment_id IN (
			SELECT e.id FROM environments e JOIN stacks s ON e.stack_id=s.id
			JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`)
		exec(`DELETE FROM nodes WHERE org_id=$1`)
		exec(`DELETE FROM organizations WHERE id=$1`)
	})
	app, _ := st.CreateApplication(ctx(t), org.ID, slug, "app")
	stack, _ := st.CreateStack(ctx(t), app.ID, slug)
	sv, _ := st.CreateStackVersion(ctx(t), stack.ID, "raw", fixtureSpec(), "t")
	env, _ := st.CreateEnvironment(ctx(t), store.CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	dep, _ := st.CreateDeployment(ctx(t), store.CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	node, _ := st.RegisterNode(ctx(t), store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.0.0.1",
		CPUMillis: 8000, MemoryBytes: 16 << 30,
	})
	return dep.ID, node.ID, org.ID
}

func fixtureSpec() *spec.DeploymentSpec {
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
			"db": {Name: "db", Image: "postgres:16-alpine", Swappable: false,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
}

func TestSchedulerPlacesPendingDeployment(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)

	sc := newSchedulerForOrg(st, discardLog(), orgID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployScheduling {
		t.Fatalf("expected scheduling, got %s", dep.State)
	}
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	if len(desired) != 2 {
		t.Fatalf("expected 2 instances written, got %d", len(desired))
	}
}

func TestSchedulerRejectsNodeWithoutPeakCPU(t *testing.T) {
	st := testStore(t)
	depID, _, orgID := fixture(t, st)
	// Capacity is reserved from the node's advertised size, not heartbeat
	// alloc. A 100-millicpu node cannot fit a 750-millicpu peak rollout.
	_, err := st.RegisterNode(ctx(t), store.RegisterNodeParams{
		OrgID: orgID, Hostname: "tiny-" + uuid.NewString()[:8], AdvertiseAddr: "10.0.0.9",
		CPUMillis: 100, MemoryBytes: 16 << 30,
	})
	if err != nil {
		t.Fatalf("register tiny node: %v", err)
	}
	// Drain the original large node so only the tiny one is ready.
	nodes, err := st.ListNodes(ctx(t), orgID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, n := range nodes {
		if n.CPUMillis > 100 {
			if err := st.DrainNode(ctx(t), n.ID); err != nil {
				t.Fatalf("drain: %v", err)
			}
		}
	}
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("expected CPU-constrained deployment to fail, got %s", dep.State)
	}
}

func advance(t *testing.T, st *store.Store, id uuid.UUID, states ...store.DeploymentState) {
	t.Helper()
	for _, s := range states {
		if err := st.UpdateDeploymentState(ctx(t), id, s, ""); err != nil {
			t.Fatalf("advance to %s: %v", s, err)
		}
	}
}

func reportAll(t *testing.T, st *store.Store, nodeID uuid.UUID, state store.InstanceState, health string) {
	t.Helper()
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	for _, d := range desired {
		if err := st.ReportInstance(ctx(t), nodeID, d.InstanceID, store.ObservedInstance{
			State: state, ContainerID: "c-" + d.ServiceName, HealthStatus: health, SetStarted: true,
		}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
}

func TestControllerDrivesSchedulingToHealthy(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	c := newControllerForOrg(st, discardLog(), nil, orgID)

	// Instances still pending → controller cannot advance past scheduling.
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployScheduling {
		t.Fatalf("expected still scheduling, got %s", dep.State)
	}

	// All instances have containers now → starting.
	reportAll(t, st, nodeID, store.InstanceStarting, "starting")
	_ = c.ReconcileOnce(ctx(t))
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployStarting {
		t.Fatalf("expected starting, got %s", dep.State)
	}

	// All healthy → healthy.
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	_ = c.ReconcileOnce(ctx(t))
	if dep, _ := st.GetDeployment(ctx(t), depID); dep.State != store.DeployHealthy {
		t.Fatalf("expected healthy, got %s", dep.State)
	}
}

func TestControllerFailsOnInstanceFailure(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceFailed, "exited")

	c := newControllerForOrg(st, discardLog(), nil, orgID)
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("expected failed, got %s", dep.State)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("expected instances torn down, got %v", states)
	}
}

func TestControllerTearsDownSuperseded(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive, store.DeploySuperseded)

	c := newControllerForOrg(st, discardLog(), nil, orgID)
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("superseded deployment instances must be torn down, got %v", states)
	}
}

// The router is repointed before anything is torn down, and a tick whose router
// sync failed tears nothing down at all — on that tick the superseded revision
// is still the only thing serving.
func TestTeardownWaitsForTheRouter(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive, store.DeploySuperseded)

	failing := &failingRouter{}
	c := newControllerForOrg(st, discardLog(), failing, orgID)
	for i := 0; i < 2; i++ {
		if err := c.ReconcileOnce(ctx(t)); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) == 0 {
		t.Fatal("nothing may be torn down while the router cannot be updated")
	}

	// With a working router the same deployment tears down, so the guard above
	// is about the failure and not about teardown never happening.
	c2 := newControllerForOrg(st, discardLog(), &captureRouter{}, orgID)
	if err := c2.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("recovered reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("teardown should resume once the router syncs, got %v", states)
	}
}

type failingRouter struct{}

func (f *failingRouter) Sync([]router.Route) error { return errRouterDown }

var errRouterDown = errors.New("router unavailable")

func TestControllerAutoPromotes(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	c := newControllerForOrg(st, discardLog(), nil, orgID)
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	// One legal transition per tick: scheduling→starting→healthy→live.
	for i := 0; i < 4; i++ {
		if err := c.ReconcileOnce(ctx(t)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployLive {
		t.Fatalf("expected live after auto-promote, got %s", dep.State)
	}
}

// bestNode is pure, so the interesting cases need no database at all — which
// matters because a scheduler is exactly the component whose behaviour must be
// assertable without arranging a fleet.
func TestBestNodeScoring(t *testing.T) {
	// Ordered ids so the tie-break has a defined winner: a < b < c.
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	c := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	node := func(id uuid.UUID, cpu, allocCPU int, mem, allocMem int64) store.Node {
		return store.Node{ID: id, CPUMillis: cpu, AllocCPUMillis: allocCPU,
			MemoryBytes: mem, AllocMemoryBytes: allocMem}
	}
	const gb = 1 << 30

	tests := []struct {
		name  string
		nodes []store.Node
		homed map[uuid.UUID]int
		want  *uuid.UUID
	}{
		{
			name:  "no node with room is not a choice",
			nodes: []store.Node{node(a, 1000, 900, 8*gb, 8*gb-1)},
		},
		{
			name:  "capacity is a filter, not a preference",
			nodes: []store.Node{node(a, 100, 0, 8*gb, 0), node(b, 8000, 0, 8*gb, 0)},
			want:  &b, // a is roomier proportionally but cannot fit 750 millicpu
		},
		{
			name:  "spread wins over free capacity",
			nodes: []store.Node{node(a, 8000, 0, 16*gb, 0), node(b, 8000, 4000, 16*gb, 8*gb)},
			homed: map[uuid.UUID]int{a: 3, b: 1},
			want:  &b, // emptier by capacity is a, but b hosts fewer environments
		},
		{
			name:  "free capacity breaks an equal spread",
			nodes: []store.Node{node(a, 8000, 6000, 16*gb, 0), node(b, 8000, 0, 16*gb, 0)},
			homed: map[uuid.UUID]int{a: 2, b: 2},
			want:  &b,
		},
		{
			name:  "the constrained resource decides the ratio",
			nodes: []store.Node{node(a, 8000, 7900, 16*gb, 0), node(b, 8000, 4000, 16*gb, 8*gb)},
			want:  &b, // a has memory to spare but almost no cpu
		},
		{
			name:  "identical nodes fall back to the id, not to row order",
			nodes: []store.Node{node(c, 8000, 0, 16*gb, 0), node(b, 8000, 0, 16*gb, 0), node(a, 8000, 0, 16*gb, 0)},
			want:  &a,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bestNode(tt.nodes, tt.homed, 750, 768<<20)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("expected no node, got %s", got.ID)
			case tt.want == nil:
			case got == nil:
				t.Fatalf("expected %s, got none", *tt.want)
			case got.ID != *tt.want:
				t.Fatalf("expected %s, got %s", *tt.want, got.ID)
			}
		})
	}
}

// Reordering the input must not reorder the output. A scheduler that depends on
// the order Postgres returned rows reproduces its bugs only sometimes.
func TestBestNodeIsOrderIndependent(t *testing.T) {
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	const gb = 1 << 30
	na := store.Node{ID: a, CPUMillis: 8000, MemoryBytes: 16 * gb}
	nb := store.Node{ID: b, CPUMillis: 8000, MemoryBytes: 16 * gb}

	first := bestNode([]store.Node{na, nb}, nil, 750, 768<<20)
	second := bestNode([]store.Node{nb, na}, nil, 750, 768<<20)
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("scoring must not depend on input order: %v vs %v", first, second)
	}
}

// The scheduler must not treat "the home node is full" as a reason to look
// elsewhere. Relocating is the data-loss bug wearing a helpful face.
func TestSchedulerFailsRatherThanMovingAHomedEnvironment(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)

	sc := newSchedulerForOrg(st, discardLog(), orgID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("first ScheduleOnce: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployScheduling {
		t.Fatalf("first deployment should be scheduling, got %s", dep.State)
	}

	// Retire it so a second deployment for the same environment is allowed.
	if err := st.UpdateDeploymentState(ctx(t), depID, store.DeployFailed, "test"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// Fill the home node so only the roomy new node could host the rollout.
	if _, err := st.Pool().Exec(ctx(t),
		`UPDATE nodes SET alloc_cpu_millis = cpu_millis WHERE id=$1`, nodeID); err != nil {
		t.Fatalf("fill home node: %v", err)
	}
	if _, err := st.RegisterNode(ctx(t), store.RegisterNodeParams{
		OrgID: orgID, Hostname: "roomy-" + uuid.NewString()[:8], AdvertiseAddr: "10.0.0.7",
		CPUMillis: 16000, MemoryBytes: 64 << 30,
	}); err != nil {
		t.Fatalf("register roomy node: %v", err)
	}

	second := secondDeploymentForSameEnv(t, st, depID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("second ScheduleOnce: %v", err)
	}

	got, _ := st.GetDeployment(ctx(t), second)
	if got.State != store.DeployFailed {
		t.Fatalf("a homed environment whose node is full must fail, got %s", got.State)
	}
	if !strings.Contains(got.FailureReason, "home node") {
		t.Fatalf("failure reason should name the home node, got %q", got.FailureReason)
	}
}

// secondDeploymentForSameEnv creates another deployment for the environment of
// an existing one, so affinity tests do not need to rebuild the whole graph.
func secondDeploymentForSameEnv(t *testing.T, st *store.Store, existing uuid.UUID) uuid.UUID {
	t.Helper()
	prev, err := st.GetDeployment(ctx(t), existing)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	dep, err := st.CreateDeployment(ctx(t), store.CreateDeploymentParams{
		EnvironmentID: prev.EnvironmentID, StackVersionID: prev.StackVersionID,
		ResolvedSpec: prev.ResolvedSpec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	return dep.ID
}

// Slice B constrained an ingress stack to the node running the router, because
// the router could only reach a tenant by joining its revision network on a
// shared daemon. Slice C removed that: the router connects to a node address and
// a published port, so any node can host one. This pins the removal — a
// reintroduced filter would fail here rather than quietly stranding capacity.
func TestIngressStackMayBePlacedOnANodeWithoutARouter(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	setSpecIngress(t, st, depID)

	// The fixture's node advertises no labels at all.
	sc := newSchedulerForOrg(st, discardLog(), orgID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployScheduling {
		t.Fatalf("expected scheduling, got %s (%s)", dep.State, dep.FailureReason)
	}
	if desired, _ := st.DesiredStateForNode(ctx(t), nodeID); len(desired) == 0 {
		t.Fatal("the ingress stack should have been placed on the unlabelled node")
	}
}

// setSpecIngress marks the fixture deployment's resolved spec as having an
// ingress service. Done in SQL because the spec is stored resolved: rebuilding
// the whole catalog graph to change one field would test the fixture, not the
// scheduler.
func setSpecIngress(t *testing.T, st *store.Store, depID uuid.UUID) {
	t.Helper()
	if _, err := st.Pool().Exec(ctx(t), `
		UPDATE deployments
		SET resolved_spec = jsonb_set(resolved_spec, '{services,api,ingress}', '{"port":80}'::jsonb)
		WHERE id = $1
	`, depID); err != nil {
		t.Fatalf("set ingress on spec: %v", err)
	}
}

// The agent records why a container did not come up; nothing read it. The
// controller failed deployments with a bare "an instance failed to start" and
// DeleteInstances then removed the rows, destroying the only description of the
// cause microseconds after it was written. An intermittent failure was
// investigated twice and both times ended at "the agent logs are silent and the
// evidence is gone" — this is what makes the third time different.
func TestFailureReasonCarriesTheAgentsError(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	for i, d := range desired {
		obs := store.ObservedInstance{State: store.InstanceRunning, ContainerID: "c", HealthStatus: "healthy", SetStarted: true}
		if i == 0 {
			// What the agent actually writes when EnsureImage fails.
			obs = store.ObservedInstance{
				State:     store.InstanceFailed,
				LastError: `pull nginx:alpine: manifest unknown`,
			}
		}
		if err := st.ReportInstance(ctx(t), nodeID, d.InstanceID, obs); err != nil {
			t.Fatalf("report: %v", err)
		}
	}

	if err := newControllerForOrg(st, discardLog(), nil, orgID).ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("expected failed, got %s", dep.State)
	}
	if !strings.Contains(dep.FailureReason, "manifest unknown") {
		t.Fatalf("the agent's error must survive into the deployment: %q", dep.FailureReason)
	}
	if !strings.Contains(dep.FailureReason, desired[0].ServiceName) {
		t.Fatalf("the reason must name which service failed: %q", dep.FailureReason)
	}
}

// A container that runs and fails its healthcheck reports no error at all, so
// the reason must still say which service and what state rather than inventing
// a cause or falling back to the generic message.
func TestFailureReasonNamesAnUnhealthyServiceWithNoError(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	desired, _ := st.DesiredStateForNode(ctx(t), nodeID)
	for _, d := range desired {
		if err := st.ReportInstance(ctx(t), nodeID, d.InstanceID, store.ObservedInstance{
			State: store.InstanceUnhealthy, ContainerID: "c", HealthStatus: "unhealthy",
		}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	_ = newControllerForOrg(st, discardLog(), nil, orgID).ReconcileOnce(ctx(t))
	dep, _ := st.GetDeployment(ctx(t), depID)
	if !strings.Contains(dep.FailureReason, "unhealthy") {
		t.Fatalf("an unhealthy instance with no error must still be described: %q", dep.FailureReason)
	}
}

// captureRouter records the last Sync so a test can assert on the route set the
// controller computed, rather than on a file only Traefik can judge.
type captureRouter struct{ last []router.Route }

func (c *captureRouter) Sync(routes []router.Route) error { c.last = routes; return nil }

// syncRouter must omit a live route whose target is not fully known yet.
// ListLiveRoutes LEFT JOINs both the reporting instance and the home node on
// purpose, so a live deployment comes back with an empty address or a zero port
// instead of vanishing — the caller has to tell "not ready" from "not live".
// Inventing a target would send that hostname's traffic somewhere arbitrary,
// and serving nothing until the next resync is the honest alternative. Nothing
// pinned the branch, so removing it would have broken only the demos.
func TestSyncRouterOmitsARouteWithNoReportedTarget(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	setSpecIngress(t, st, depID)
	if err := newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx(t), sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	// ListLiveRoutes returns nothing without a hostname, so give it one. It has
	// to be unique to this run: ListLiveRoutes is a global scan with no
	// org-scoped variant, so a leftover route from a previous run under a fixed
	// name would be indistinguishable from this test's own.
	hostname := "omit-" + uuid.NewString()[:8] + ".example.com"
	exec(`UPDATE environments SET hostname=$2
	      WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, depID, hostname)

	rtr := &captureRouter{}
	// routeStrand stays zero, which disables reachability withdrawal entirely —
	// so the only thing that can drop a route here is the branch under test.
	c := newControllerForOrg(st, discardLog(), rtr, orgID)
	// syncRouter is deliberately NOT org-scoped — the router serves the whole
	// fleet — so the assertions must look only at this test's own route. A
	// count over everything passes on an empty database and fails the moment
	// the dev stack has run a demo, which is the shared-database hazard the
	// loop tests are org-scoped to avoid in the first place.
	sync := func(why string) []router.Route {
		t.Helper()
		if err := c.syncRouter(ctx(t)); err != nil {
			t.Fatalf("syncRouter (%s): %v", why, err)
		}
		mine := []router.Route{}
		for _, r := range rtr.last {
			if r.Hostname == hostname {
				mine = append(mine, r)
			}
		}
		return mine
	}

	// The agent has not reported a published port yet: address known, port not.
	if got := sync("no port"); len(got) != 0 {
		t.Fatalf("a live route with no published port must be omitted, got %+v", got)
	}

	// Once the port is reported the same route is served. Without this half the
	// assertion above would also pass against a syncRouter that omitted every
	// route unconditionally.
	exec(`UPDATE service_instances SET ingress_port=32768
	      WHERE deployment_id=$1 AND service_name='api'`, depID)
	if got := sync("port reported"); len(got) != 1 || got[0].Target != "10.0.0.1" || got[0].Port != 32768 {
		t.Fatalf("expected one route to 10.0.0.1:32768, got %+v", got)
	}

	// An environment whose home node row is gone reports no address at all
	// (home_node_id is ON DELETE SET NULL), which must drop the route the same
	// way a missing port does.
	exec(`UPDATE environments SET home_node_id=NULL
	      WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, depID)
	if got := sync("no node address"); len(got) != 0 {
		t.Fatalf("a live route with no node address must be omitted, got %+v", got)
	}
}

// A superseded revision keeps serving until the router has actually stopped
// pointing at it.
//
// Writing the router config is not the same as the router applying it: Traefik
// throttles provider updates (2s by default), so for a moment after Sync
// returns it is still sending traffic to the revision about to be deleted.
// Measured before this existed: ~1.2s of 502s on every promotion, with both
// containers up and healthy throughout — the containers were never the problem.
func TestSupersededRevisionOutlivesTheRouterUpdate(t *testing.T) {
	st := testStore(t)
	depID, nodeID, orgID := fixture(t, st)
	_ = newSchedulerForOrg(st, discardLog(), orgID).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive, store.DeploySuperseded)

	// A clock the test moves by hand, so the grace is asserted rather than
	// slept through.
	now := time.Now()
	c := newControllerForOrg(st, discardLog(), &captureRouter{}, orgID)
	c.teardownGrace = 5 * time.Second
	c.now = func() time.Time { return now }

	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) == 0 {
		t.Fatal("a just-superseded revision must keep serving")
	}

	// Still inside the grace.
	now = now.Add(4 * time.Second)
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) == 0 {
		t.Fatal("teardown happened before the grace elapsed")
	}

	// Past it. The grace is measured from when the deployment became terminal,
	// not from the tick that noticed, or a busy control plane would extend it
	// indefinitely.
	now = now.Add(2 * time.Second)
	if err := c.ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("teardown should have happened once the grace elapsed, got %v", states)
	}
}
