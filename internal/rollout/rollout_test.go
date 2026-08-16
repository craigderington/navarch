package rollout

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

	if err := newControllerForOrg(st, discardLog(), nil, orgID).ReconcileOnce(ctx(t)); err != nil {
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

	if err := newControllerForOrg(st, discardLog(), nil, orgID).ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("superseded deployment instances must be torn down, got %v", states)
	}
}

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

// A stack with an ingress service is only servable from a node running the
// router, because the router reaches a tenant by joining its revision network
// and can only do that on its own daemon. Placing it elsewhere would produce a
// deployment that goes live while its hostname resolves to nothing — a success
// report for something nobody can reach.
func TestSchedulerRefusesIngressStackWithNoIngressNode(t *testing.T) {
	st := testStore(t)
	depID, _, orgID := fixture(t, st)
	setSpecIngress(t, st, depID)

	// The fixture's node advertises no labels, so nothing in the fleet can serve
	// ingress.
	sc := newSchedulerForOrg(st, discardLog(), orgID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployFailed {
		t.Fatalf("expected failed, got %s", dep.State)
	}
	if !strings.Contains(dep.FailureReason, "ingress") {
		t.Fatalf("the reason should say what is missing, got %q", dep.FailureReason)
	}
}

// And the constraint outranks spread: an ingress stack goes to the labelled
// node even when an emptier one is available, because the emptier one cannot
// serve it at all.
func TestSchedulerSendsIngressStackToTheLabelledNode(t *testing.T) {
	st := testStore(t)
	depID, plainNodeID, orgID := fixture(t, st)
	setSpecIngress(t, st, depID)

	ingressNode, err := st.RegisterNode(ctx(t), store.RegisterNodeParams{
		OrgID: orgID, Hostname: "router-" + uuid.NewString()[:8], AdvertiseAddr: "10.0.0.8",
		CPUMillis: 8000, MemoryBytes: 16 << 30,
		Labels: map[string]string{"ingress": "true"},
	})
	if err != nil {
		t.Fatalf("register ingress node: %v", err)
	}

	sc := newSchedulerForOrg(st, discardLog(), orgID)
	if err := sc.ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	}

	dep, _ := st.GetDeployment(ctx(t), depID)
	if dep.State != store.DeployScheduling {
		t.Fatalf("expected scheduling, got %s (%s)", dep.State, dep.FailureReason)
	}
	desired, _ := st.DesiredStateForNode(ctx(t), ingressNode.ID)
	if len(desired) == 0 {
		t.Fatal("the ingress stack must be placed on the node advertising ingress=true")
	}
	if plain, _ := st.DesiredStateForNode(ctx(t), plainNodeID); len(plain) != 0 {
		t.Fatalf("nothing should have been placed on the node without a router, got %d", len(plain))
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
