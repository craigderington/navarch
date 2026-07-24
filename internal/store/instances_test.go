package store

import (
	"errors"
	"testing"

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
	return dep, node
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
	if err := st.ReportInstance(testCtx(t), desired[0].InstanceID, ObservedInstance{
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
	_ = st.CreateServiceInstances(testCtx(t), dep.ID, node.ID, []NewInstance{{ServiceName: "api", Swappable: true, ImageRef: "x"}})
	for _, s := range []DeploymentState{DeployScheduling, DeployStarting, DeployHealthy} {
		if err := st.UpdateDeploymentState(testCtx(t), dep.ID, s, ""); err != nil {
			t.Fatalf("advance %s: %v", s, err)
		}
	}
	if _, err := st.PromoteDeployment(testCtx(t), dep.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	routes, err := st.ListLiveRoutes(testCtx(t))
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
		}
	}
	if !found {
		t.Fatalf("live route not found in %+v", routes)
	}
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
			_ = st.ReportInstance(testCtx(t), d.InstanceID, ObservedInstance{State: InstanceRunning, ContainerID: "c", SetStarted: true})
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

func errorsIs(err, target error) bool { return errors.Is(err, target) }
