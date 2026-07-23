package rollout

import (
	"context"
	"io"
	"log/slog"
	"os"
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
	depID, nodeID, _ := fixture(t, st)

	sc := NewScheduler(st, discardLog())
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
		if err := st.ReportInstance(ctx(t), d.InstanceID, store.ObservedInstance{
			State: state, ContainerID: "c-" + d.ServiceName, HealthStatus: health, SetStarted: true,
		}); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
}

func TestControllerDrivesSchedulingToHealthy(t *testing.T) {
	st := testStore(t)
	depID, nodeID, _ := fixture(t, st)
	if err := NewScheduler(st, discardLog()).ScheduleOnce(ctx(t)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	c := NewController(st, discardLog())

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
	depID, nodeID, _ := fixture(t, st)
	_ = NewScheduler(st, discardLog()).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceFailed, "exited")

	if err := NewController(st, discardLog()).ReconcileOnce(ctx(t)); err != nil {
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
	depID, nodeID, _ := fixture(t, st)
	_ = NewScheduler(st, discardLog()).ScheduleOnce(ctx(t))
	reportAll(t, st, nodeID, store.InstanceRunning, "healthy")
	advance(t, st, depID, store.DeployStarting, store.DeployHealthy, store.DeployLive, store.DeploySuperseded)

	if err := NewController(st, discardLog()).ReconcileOnce(ctx(t)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if states, _ := st.InstanceStates(ctx(t), depID); len(states) != 0 {
		t.Fatalf("superseded deployment instances must be torn down, got %v", states)
	}
}
