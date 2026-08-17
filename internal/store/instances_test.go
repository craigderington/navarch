package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
)

// deployFixture creates org→app→stack→version→env→deployment and returns the
// deployment plus a registered node, so instance tests have a real graph.
func deployFixture(t *testing.T, st *Store) (*Deployment, *Node) {
	t.Helper()
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv, err := st.CreateStackVersion(testCtx(t), stack.ID, "raw", twoServiceSpec(), "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := st.CreateDeployment(testCtx(t), CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	node, err := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("node"), AdvertiseAddr: "10.0.0.9",
		CPUMillis: 4000, MemoryBytes: 8 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	// RegisterNode does not set last_heartbeat — a real agent registers and then
	// heartbeats a moment later. Routing requires a node the control plane has
	// actually heard from (a NULL heartbeat is what MarkStaleNodesUnreachable
	// already treats as stale), so a fixture node that never heartbeats is not
	// routable and every route test would be asserting reachability by accident.
	if _, err := st.Pool().Exec(testCtx(t),
		`UPDATE nodes SET last_heartbeat=now() WHERE id=$1`, node.ID); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
	return dep, node
}

// placeInstances writes desired rows AND binds the environment to the node, the
// way PlaceDeployment does on every real path. CreateServiceInstances alone has
// no production caller: using it without the binding builds an environment with
// live instances and no home node, which the scheduler cannot produce and which
// makes routing tests assert against a state that does not occur.
func placeInstances(t *testing.T, st *Store, depID, nodeID uuid.UUID, insts []NewInstance) {
	t.Helper()
	if err := st.CreateServiceInstances(testCtx(t), depID, nodeID, insts); err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}
	if _, err := st.Pool().Exec(testCtx(t), `
		UPDATE environments SET home_node_id=$2
		WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)
	`, depID, nodeID); err != nil {
		t.Fatalf("bind home node: %v", err)
	}
}

func twoServiceSpec() *spec.DeploymentSpec {
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Ingress: &spec.Ingress{Port: 80},
				Limits:  spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
			"db": {Name: "db", Image: "postgres:16-alpine", Swappable: false,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
}

func TestCreateAndReadDesiredState(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)

	err := st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
		{ServiceName: "db", Swappable: false, ImageRef: "postgres:16-alpine"},
	})
	if err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}

	// A pending deployment's instances are not in desired state yet — the
	// deployment must be in an active rollout state to be reconciled.
	if got, _ := st.DesiredStateForNode(testCtx(t), node.ID); len(got) != 0 {
		t.Fatalf("pending deployment should not appear in desired state, got %d", len(got))
	}
	if err := st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, ""); err != nil {
		t.Fatalf("advance to scheduling: %v", err)
	}

	desired, err := st.DesiredStateForNode(testCtx(t), node.ID)
	if err != nil {
		t.Fatalf("DesiredStateForNode: %v", err)
	}
	if len(desired) != 2 {
		t.Fatalf("expected 2 desired instances, got %d", len(desired))
	}
	for _, d := range desired {
		if d.Env8 == "" || d.ProjectName == "" {
			t.Fatalf("desired instance missing project context: %+v", d)
		}
		if d.Service.Image == "" {
			t.Fatalf("desired instance %s missing resolved Service spec", d.ServiceName)
		}
	}
}

func TestReportInstanceAndAggregate(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	})
	_ = st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, "")

	desired, _ := st.DesiredStateForNode(testCtx(t), node.ID)
	if err := st.ReportInstance(testCtx(t), node.ID, desired[0].InstanceID, ObservedInstance{
		State: InstanceRunning, ContainerID: "deadbeef", HealthStatus: "healthy", SetStarted: true,
	}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}
	states, err := st.InstanceStates(testCtx(t), dep.ID)
	if err != nil {
		t.Fatalf("InstanceStates: %v", err)
	}
	if len(states) != 1 || states[0] != InstanceRunning {
		t.Fatalf("expected [running], got %v", states)
	}
}

func TestDeleteInstances(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	})
	if err := st.DeleteInstances(testCtx(t), dep.ID); err != nil {
		t.Fatalf("DeleteInstances: %v", err)
	}
	states, _ := st.InstanceStates(testCtx(t), dep.ID)
	if len(states) != 0 {
		t.Fatalf("expected no instances after delete, got %v", states)
	}
}

func TestRejectPinnedDrift(t *testing.T) {
	live := &spec.DeploymentSpec{Services: map[string]spec.Service{
		"db":  {Name: "db", Image: "postgres:16", Swappable: false},
		"web": {Name: "web", Image: "nginx:1", Swappable: true},
	}}
	unchanged := &spec.DeploymentSpec{Services: map[string]spec.Service{
		"db":  {Name: "db", Image: "postgres:16", Swappable: false},
		"web": {Name: "web", Image: "nginx:2", Swappable: true},
	}}
	if err := rejectPinnedDrift(live, unchanged); err != nil {
		t.Fatalf("swappable change rejected: %v", err)
	}

	for name, next := range map[string]*spec.DeploymentSpec{
		"changed": {Services: map[string]spec.Service{"db": {Name: "db", Image: "postgres:17", Swappable: false}}},
		"removed": {Services: map[string]spec.Service{"web": {Name: "web", Image: "nginx:2", Swappable: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rejectPinnedDrift(live, next); !errors.Is(err, ErrConflict) {
				t.Fatalf("got %v, want ErrConflict", err)
			}
		})
	}
}

func TestListPendingDeploymentsCarriesOrg(t *testing.T) {
	st := testStore(t)
	dep, _ := deployFixture(t, st)
	pend, err := st.ListPendingDeployments(testCtx(t))
	if err != nil {
		t.Fatalf("ListPendingDeployments: %v", err)
	}
	var found bool
	for _, p := range pend {
		if p.ID == dep.ID {
			found = true
			if p.OrgID == uuidNil() {
				t.Fatal("pending deployment must carry its org id")
			}
		}
	}
	if !found {
		t.Fatalf("created pending deployment %s not listed", dep.ID)
	}
}

func uuidNil() uuid.UUID { return uuid.UUID{} }

func TestListLiveRoutes(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_, _ = st.Pool().Exec(testCtx(t),
		`UPDATE environments SET hostname='prod.example.com' WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, dep.ID)
	placeInstances(t, st, dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		if err := st.UpdateDeploymentState(testCtx(t), dep.ID, s, ""); err != nil {
			t.Fatalf("advance %s: %v", s, err)
		}
	}
	if _, err := st.PromoteDeployment(testCtx(t), dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Report the published port the way the agent does. Without this the route
	// comes back with PublishedPort == 0, which the controller DISCARDS — this
	// test used to assert only the spec-side fields and so passed on a route
	// that would never have been served, and would have kept passing if
	// NodeAddr and PublishedPort were deleted from the struct outright.
	if err := st.ReportInstance(testCtx(t), node.ID, instanceIDFor(t, st, dep.ID, "api"),
		ObservedInstance{State: InstanceRunning, ContainerID: "c-api", IngressPort: 32768}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}

	routes, err := st.ListLiveRoutes(testCtx(t), 120*time.Second)
	if err != nil {
		t.Fatalf("ListLiveRoutes: %v", err)
	}
	var found bool
	for _, r := range routes {
		if r.Hostname == "prod.example.com" {
			found = true
			if r.IngressService != "api" || r.IngressPort != 80 {
				t.Fatalf("bad route: %+v", r)
			}
			if r.ProjectName == "" || r.Env8 == "" {
				t.Fatalf("route missing project context: %+v", r)
			}
			// What the router actually connects to. The container name is not
			// usable as a target once the tenant can be on another node.
			if r.PublishedPort != 32768 {
				t.Fatalf("route must carry the reported host port, got %+v", r)
			}
			if r.NodeAddr == "" {
				t.Fatalf("route must carry the node's registered address, got %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("live route not found in %+v", routes)
	}
}

// A live deployment whose agent has not reported a port yet must still come
// back, so the controller can tell "not ready to route" from "not live" and
// omit the route rather than inventing a target for it.
func TestListLiveRoutesReturnsRouteBeforeThePortIsReported(t *testing.T) {
	st := testStore(t)
	dep, node := deployFixture(t, st)
	_, _ = st.Pool().Exec(testCtx(t),
		`UPDATE environments SET hostname='unreported.example.com' WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, dep.ID)
	placeInstances(t, st, dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		if err := st.UpdateDeploymentState(testCtx(t), dep.ID, s, ""); err != nil {
			t.Fatalf("advance %s: %v", s, err)
		}
	}
	if _, err := st.PromoteDeployment(testCtx(t), dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	routes, err := st.ListLiveRoutes(testCtx(t), 120*time.Second)
	if err != nil {
		t.Fatalf("ListLiveRoutes: %v", err)
	}
	for _, r := range routes {
		if r.Hostname == "unreported.example.com" {
			if r.PublishedPort != 0 {
				t.Fatalf("no port has been reported, so none should be claimed: %+v", r)
			}
			return
		}
	}
	t.Fatal("a live deployment must appear even before its port is reported")
}

// instanceIDFor finds the instance row for one service of a deployment.
func instanceIDFor(t *testing.T, st *Store, depID uuid.UUID, service string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := st.Pool().QueryRow(testCtx(t),
		`SELECT id FROM service_instances WHERE deployment_id=$1 AND service_name=$2`,
		depID, service).Scan(&id); err != nil {
		t.Fatalf("find instance %s: %v", service, err)
	}
	return id
}

// driveToLive takes a deployment from pending to live: writes instances,
// reports them running, walks the state machine, and promotes.
func driveToLive(t *testing.T, st *Store, dep *Deployment, node *Node) {
	t.Helper()
	insts := make([]NewInstance, 0)
	for name, s := range dep.ResolvedSpec.Services {
		insts = append(insts, NewInstance{ServiceName: name, Swappable: s.Swappable, ImageRef: s.Image})
	}
	if err := st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, insts); err != nil {
		t.Fatalf("instances: %v", err)
	}
	if err := st.UpdateDeploymentState(testCtx(t), dep.ID, DeployScheduling, ""); err != nil {
		t.Fatalf("scheduling: %v", err)
	}
	desired, _ := st.DesiredStateForNode(testCtx(t), node.ID)
	for _, d := range desired {
		if d.DeploymentID == dep.ID {
			_ = st.ReportInstance(testCtx(t), node.ID, d.InstanceID, ObservedInstance{State: InstanceRunning, ContainerID: "c", SetStarted: true})
		}
	}
	for _, s := range []DeploymentState{DeployStarting, DeployHealthy} {
		if err := st.UpdateDeploymentState(testCtx(t), dep.ID, s, ""); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := st.PromoteDeployment(testCtx(t), dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
}

func TestRollbackDeploymentReusesEarlierVersion(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	node, _ := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("n"), AdvertiseAddr: "10.0.0.2", CPUMillis: 8000, MemoryBytes: 16 << 30})
	env, _ := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})

	v1, _ := st.CreateStackVersion(testCtx(t), stack.ID, "raw1", specWithImage(t, "nginx:1.25"), "t")
	d1, _ := st.CreateDeployment(testCtx(t), CreateDeploymentParams{EnvironmentID: env.ID, StackVersionID: v1.ID, ResolvedSpec: v1.Spec})
	driveToLive(t, st, d1, node)

	v2, _ := st.CreateStackVersion(testCtx(t), stack.ID, "raw2", specWithImage(t, "nginx:1.27"), "t")
	d2, _ := st.CreateDeployment(testCtx(t), CreateDeploymentParams{EnvironmentID: env.ID, StackVersionID: v2.ID, ResolvedSpec: v2.Spec})
	driveToLive(t, st, d2, node)

	// Roll back to revision 1: a new deployment reusing v1's stack version.
	d3, err := st.RollbackDeployment(testCtx(t), env.ID, 1)
	if err != nil {
		t.Fatalf("rollback to 1: %v", err)
	}
	if d3.StackVersionID != v1.ID {
		t.Fatalf("rollback should reuse v1 (%s), got %s", v1.ID, d3.StackVersionID)
	}
	if d3.Revision != 3 {
		t.Fatalf("expected revision 3, got %d", d3.Revision)
	}
	_ = d2

	// A rollback target that never existed is ErrNotFound.
	if _, err := st.RollbackDeployment(testCtx(t), env.ID, 99); !errorsIs(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown revision, got %v", err)
	}
}

func TestRollbackRejectsMissingSecrets(t *testing.T) {
	st := testStore(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	node, _ := st.RegisterNode(testCtx(t), RegisterNodeParams{
		OrgID: org.ID, Hostname: uniq("n"), AdvertiseAddr: "10.0.0.3", CPUMillis: 8000, MemoryBytes: 16 << 30})
	env, _ := st.CreateEnvironment(testCtx(t), CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})

	secretSpec := specWithImage(t, "nginx:1.25")
	svc := secretSpec.Services["app"]
	svc.SecretEnv = map[string]string{"PASSWORD": "${secret:db_password}"}
	secretSpec.Services["app"] = svc

	if err := st.SetSecret(testCtx(t), env.ID, "db_password", []byte("ct"), "age1x"); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	v1, _ := st.CreateStackVersion(testCtx(t), stack.ID, "raw1", secretSpec, "t")
	d1, _ := st.CreateDeployment(testCtx(t), CreateDeploymentParams{EnvironmentID: env.ID, StackVersionID: v1.ID, ResolvedSpec: secretSpec})
	driveToLive(t, st, d1, node)

	v2, _ := st.CreateStackVersion(testCtx(t), stack.ID, "raw2", specWithImage(t, "nginx:1.27"), "t")
	d2, _ := st.CreateDeployment(testCtx(t), CreateDeploymentParams{EnvironmentID: env.ID, StackVersionID: v2.ID, ResolvedSpec: v2.Spec})
	driveToLive(t, st, d2, node)

	if err := st.DeleteSecret(testCtx(t), env.ID, "db_password"); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	_, err := st.RollbackDeployment(testCtx(t), env.ID, 1)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid when rollback is missing secrets, got %v", err)
	}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// The data-loss regression. An environment's pinned container and named volumes
// live on the node its first deployment was placed on. Before home_node_id,
// nothing stopped revision 2 being placed elsewhere — where the agent would
// build a fresh pinned container over an empty volume, pass health checks, and
// be auto-promoted while the real data sat unreferenced on the original node.
// The rollout reported success. Placement must refuse instead.
func TestPlacementRefusesToMoveAHomedEnvironment(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv, err := st.CreateStackVersion(ctx, stack.ID, "raw", twoServiceSpec(), "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	nodeA, nodeB := newNode(t, st, org.ID), newNode(t, st, org.ID)
	insts := []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"}}

	d1, err := st.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if err := st.PlaceDeployment(ctx, d1.ID, nodeA.ID, insts, 250, 256<<20); err != nil {
		t.Fatalf("first placement: %v", err)
	}

	// First placement must have bound the environment.
	got, err := st.GetEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.HomeNodeID == nil || *got.HomeNodeID != nodeA.ID {
		t.Fatalf("first placement must home the environment to %s, got %v", nodeA.ID, got.HomeNodeID)
	}

	// Retire the first so the partial unique index permits a second active
	// deployment; the point under test is placement, not that constraint.
	// Failed, not superseded: the state machine only allows superseding a live
	// deployment, and this one never left scheduling.
	if err := st.UpdateDeploymentState(ctx, d1.ID, DeployFailed, "retired by test"); err != nil {
		t.Fatalf("retire first deployment: %v", err)
	}
	d2, err := st.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment (second): %v", err)
	}

	err = st.PlaceDeployment(ctx, d2.ID, nodeB.ID, insts, 250, 256<<20)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("placing a homed environment on another node must be ErrConflict, got %v", err)
	}

	// And the binding is unchanged — a refused placement must not half-apply.
	got, err = st.GetEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.HomeNodeID == nil || *got.HomeNodeID != nodeA.ID {
		t.Fatalf("home node must be unchanged after a refusal, got %v", got.HomeNodeID)
	}

	// The same deployment placed on the home node succeeds, proving the refusal
	// was about the node and not about the deployment being unplaceable.
	if err := st.PlaceDeployment(ctx, d2.ID, nodeA.ID, insts, 250, 256<<20); err != nil {
		t.Fatalf("placement on the home node: %v", err)
	}
}

func TestEnvironmentsHomedPerNode(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	sv, err := st.CreateStackVersion(ctx, stack.ID, "raw", twoServiceSpec(), "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	nodeA, nodeB := newNode(t, st, org.ID), newNode(t, st, org.ID)
	insts := []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"}}

	// Two environments on A, one on B.
	for i, node := range []*Node{nodeA, nodeA, nodeB} {
		env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{
			StackID: stack.ID, Slug: uniq("e"),
		})
		if err != nil {
			t.Fatalf("CreateEnvironment %d: %v", i, err)
		}
		dep, err := st.CreateDeployment(ctx, CreateDeploymentParams{
			EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
		})
		if err != nil {
			t.Fatalf("CreateDeployment %d: %v", i, err)
		}
		if err := st.PlaceDeployment(ctx, dep.ID, node.ID, insts, 250, 256<<20); err != nil {
			t.Fatalf("PlaceDeployment %d: %v", i, err)
		}
	}

	homed, err := st.EnvironmentsHomedPerNode(ctx, org.ID)
	if err != nil {
		t.Fatalf("EnvironmentsHomedPerNode: %v", err)
	}
	if homed[nodeA.ID] != 2 || homed[nodeB.ID] != 1 {
		t.Fatalf("want A=2 B=1, got %v", homed)
	}
}

// Routes must follow reachability, and — the half that matters more — come back.
// Withdrawal is derived, never written: nothing in ListLiveRoutes mutates, so a
// node that heartbeats again is routable on the next resync. A withdrawal that
// could not reverse would make a thirty-second blip permanent.
func TestListLiveRoutesWithdrawsAndRestoresWithNodeReachability(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	dep, node := deployFixture(t, st)
	_, _ = st.Pool().Exec(ctx,
		`UPDATE environments SET hostname='strand.example.com' WHERE id=(SELECT environment_id FROM deployments WHERE id=$1)`, dep.ID)
	placeInstances(t, st, dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		if err := st.UpdateDeploymentState(ctx, dep.ID, s, ""); err != nil {
			t.Fatalf("advance %s: %v", s, err)
		}
	}
	if _, err := st.PromoteDeployment(ctx, dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := st.ReportInstance(ctx, node.ID, instanceIDFor(t, st, dep.ID, "api"),
		ObservedInstance{State: InstanceRunning, ContainerID: "c-api", IngressPort: 32768}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}
	// A fresh heartbeat, so the node is genuinely reachable to begin with.
	if _, err := st.Pool().Exec(ctx, `UPDATE nodes SET state='ready', last_heartbeat=now() WHERE id=$1`, node.ID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	const strand = 120 * time.Second

	if !routeServed(t, st, "strand.example.com", strand) {
		t.Fatal("a live deployment on a reachable node must be routed")
	}

	// Unreachable state, heartbeat still fresh: proves the STATE clause alone.
	if _, err := st.Pool().Exec(ctx, `UPDATE nodes SET state='unreachable' WHERE id=$1`, node.ID); err != nil {
		t.Fatalf("mark unreachable: %v", err)
	}
	if routeServed(t, st, "strand.example.com", strand) {
		t.Fatal("an unreachable node's route must be withdrawn")
	}

	// Back to ready: the route returns without anything being rewritten.
	if _, err := st.Pool().Exec(ctx, `UPDATE nodes SET state='ready', last_heartbeat=now() WHERE id=$1`, node.ID); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if !routeServed(t, st, "strand.example.com", strand) {
		t.Fatal("a healed node's route must come back — withdrawal is derived, not written")
	}

	// Ready state, ancient heartbeat: proves the HEARTBEAT clause independently.
	// A test that only flipped state would still pass with this clause deleted.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE nodes SET state='ready', last_heartbeat=now() - interval '10 minutes' WHERE id=$1`, node.ID); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}
	if routeServed(t, st, "strand.example.com", strand) {
		t.Fatal("a node silent past the strand threshold must not be routed even while its state reads ready")
	}

	// Strand of zero means never withdraw — the "I would rather hang than 404"
	// position, available as a dial rather than only as an omission.
	if !routeServed(t, st, "strand.example.com", 0) {
		t.Fatal("a zero strand must disable withdrawal entirely")
	}
}

func routeServed(t *testing.T, st *Store, hostname string, strand time.Duration) bool {
	t.Helper()
	routes, err := st.ListLiveRoutes(testCtx(t), strand)
	if err != nil {
		t.Fatalf("ListLiveRoutes: %v", err)
	}
	for _, r := range routes {
		if r.Hostname == hostname {
			return true
		}
	}
	return false
}

// Re-homing works THROUGH the strict placement path, never around it: the
// release clears the binding and the ordinary PlaceDeployment then accepts a
// different node. That ordering is the whole design — relaxing PlaceDeployment
// instead would be the Slice A data-loss bug with a sympathetic motive.
func TestReleaseEnvironmentHomeThenPlaceElsewhere(t *testing.T) {
	st := testStore(t)
	ctx := testCtx(t)
	org := newOrg(t, st)
	app := newApp(t, st, org.ID)
	stack := newStack(t, st, app.ID)
	// Stateless by construction: one swappable service, no volumes.
	statelessSpec := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
	sv, err := st.CreateStackVersion(ctx, stack.ID, "raw", statelessSpec, "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	nodeA, nodeB := newNode(t, st, org.ID), newNode(t, st, org.ID)
	insts := []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"}}

	d1, _ := st.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t"})
	if err := st.PlaceDeployment(ctx, d1.ID, nodeA.ID, insts, 250, 256<<20); err != nil {
		t.Fatalf("first placement: %v", err)
	}
	// Make it live so the release judges durability from a real resolved spec.
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		_ = st.UpdateDeploymentState(ctx, d1.ID, s, "")
	}
	if _, err := st.PromoteDeployment(ctx, d1.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	if err := st.ReleaseEnvironmentHome(ctx, env.ID); err != nil {
		t.Fatalf("release a stateless environment: %v", err)
	}
	got, _ := st.GetEnvironment(ctx, env.ID)
	if got.HomeNodeID != nil {
		t.Fatalf("release must clear the binding, got %v", got.HomeNodeID)
	}

	// The unmodified strict path now accepts a different node.
	_ = st.UpdateDeploymentState(ctx, d1.ID, DeploySuperseded, "")
	d2, _ := st.CreateDeployment(ctx, CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t"})
	if err := st.PlaceDeployment(ctx, d2.ID, nodeB.ID, insts, 250, 256<<20); err != nil {
		t.Fatalf("placement on a new node after release: %v", err)
	}
	got, _ = st.GetEnvironment(ctx, env.ID)
	if got.HomeNodeID == nil || *got.HomeNodeID != nodeB.ID {
		t.Fatalf("re-homed environment should be bound to %s, got %v", nodeB.ID, got.HomeNodeID)
	}
}

// The two ways an environment can be stuck. The read-only case is the one a
// naive check gets wrong: read-only describes the container's access to the
// bytes, not where the bytes live, so the volume is still on that node and the
// environment still cannot move.
func TestReleaseEnvironmentHomeRefusesDurableState(t *testing.T) {
	limits := spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}
	cases := map[string]*spec.DeploymentSpec{
		"a pinned service": {
			SpecVersion: spec.SpecVersion,
			Services: map[string]spec.Service{
				"api": {Name: "api", Image: "nginx:alpine", Swappable: true, Limits: limits},
				"db":  {Name: "db", Image: "postgres:16-alpine", Swappable: false, Limits: limits},
			},
		},
		"a READ-ONLY volume on a swappable service": {
			SpecVersion: spec.SpecVersion,
			Volumes:     map[string]spec.Volume{"assets": {}},
			Services: map[string]spec.Service{
				"api": {Name: "api", Image: "nginx:alpine", Swappable: true, Limits: limits,
					Mounts: []spec.Mount{{Kind: spec.MountVolume, Source: "assets", Target: "/assets", ReadOnly: true}}},
			},
		},
	}
	for name, ds := range cases {
		t.Run(name, func(t *testing.T) {
			st := testStore(t)
			ctx := testCtx(t)
			org := newOrg(t, st)
			app := newApp(t, st, org.ID)
			stack := newStack(t, st, app.ID)
			sv, err := st.CreateStackVersion(ctx, stack.ID, "raw", ds, "t")
			if err != nil {
				t.Fatalf("CreateStackVersion: %v", err)
			}
			env, err := st.CreateEnvironment(ctx, CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
			if err != nil {
				t.Fatalf("CreateEnvironment: %v", err)
			}
			node := newNode(t, st, org.ID)
			dep, _ := st.CreateDeployment(ctx, CreateDeploymentParams{
				EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t"})
			if err := st.PlaceDeployment(ctx, dep.ID, node.ID,
				[]NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"}}, 250, 256<<20); err != nil {
				t.Fatalf("place: %v", err)
			}
			for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
				_ = st.UpdateDeploymentState(ctx, dep.ID, s, "")
			}
			if _, err := st.PromoteDeployment(ctx, dep.ID); err != nil {
				t.Fatalf("promote: %v", err)
			}

			err = st.ReleaseEnvironmentHome(ctx, env.ID)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("releasing durable state must be ErrConflict, got %v", err)
			}
			// The binding must be untouched — a refused release that half-applied
			// would be worse than one that never ran.
			got, _ := st.GetEnvironment(ctx, env.ID)
			if got.HomeNodeID == nil || *got.HomeNodeID != node.ID {
				t.Fatalf("a refused release must leave the binding intact, got %v", got.HomeNodeID)
			}
		})
	}
}
