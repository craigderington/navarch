package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// fakeDriver records calls and returns canned health.
type fakeDriver struct {
	created  []string
	attached []string
	removed  []string
	managed  []dockerd.Managed
	health   map[string]dockerd.Health
}

func (f *fakeDriver) EnsureImage(ctx context.Context, ref string) error { return nil }
func (f *fakeDriver) EnsureNetwork(ctx context.Context, name string, l map[string]string) (string, error) {
	return "net-" + name, nil
}
func (f *fakeDriver) EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec) (string, bool, error) {
	f.created = append(f.created, cs.Name)
	return "id-" + cs.Name, true, nil
}
func (f *fakeDriver) AttachNetwork(ctx context.Context, id, net string, a ...string) error {
	f.attached = append(f.attached, id+"->"+net)
	return nil
}
func (f *fakeDriver) InspectHealth(ctx context.Context, id string) (dockerd.Health, error) {
	if h, ok := f.health[id]; ok {
		return h, nil
	}
	return dockerd.Health{Running: true}, nil
}
func (f *fakeDriver) StopRemove(ctx context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeDriver) ListManaged(ctx context.Context, env8 string) ([]dockerd.Managed, error) {
	return f.managed, nil
}

func desired(service string, swappable bool, health dockerd.Health) store.DesiredInstance {
	return store.DesiredInstance{
		InstanceID: uuid.New(), DeploymentID: uuid.New(), Env8: "env12345",
		ProjectName: "cc-env12345-r1-blue", Slot: "blue", Revision: 1,
		ServiceName: service, Swappable: swappable, State: store.InstancePending,
		Service: spec.Service{Name: service, Image: "img:" + service, Swappable: swappable,
			Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
	}
}

func TestReconcileCreatesSwappableAndPinned(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{
		desired("api", true, dockerd.Health{}),
		desired("db", false, dockerd.Health{}),
	})
	if len(f.created) != 2 {
		t.Fatalf("expected 2 containers created, got %v", f.created)
	}
	// pinned db uses the env-scoped stable name; swappable api uses the project name.
	var sawPinned, sawSwappable bool
	for _, name := range f.created {
		if name == "cc-env12345-pinned-db" {
			sawPinned = true
		}
		if name == "cc-env12345-r1-blue-api" {
			sawSwappable = true
		}
	}
	if !sawPinned || !sawSwappable {
		t.Fatalf("naming wrong: created=%v", f.created)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(reports))
	}
}

func TestReconcileGCsOrphanSwappable(t *testing.T) {
	f := &fakeDriver{
		managed: []dockerd.Managed{
			{ID: "id-old", Name: "cc-env12345-r0-green-api", Service: "api", Swappable: true},
			{ID: "id-keep", Name: "cc-env12345-pinned-db", Service: "db", Swappable: false},
		},
	}
	r := NewReconciler(f)
	// Desired set no longer includes the r0-green api, but still includes db.
	r.Reconcile(context.Background(), []store.DesiredInstance{desired("db", false, dockerd.Health{})})
	if len(f.removed) != 1 || f.removed[0] != "id-old" {
		t.Fatalf("expected only the orphan swappable removed, got %v", f.removed)
	}
}

func TestHealthMappingHealthchecked(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: true, Status: "healthy"},
	}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{
		func() store.DesiredInstance {
			d := desired("api", true, dockerd.Health{})
			d.Service.Health = &spec.HealthCheck{Test: []string{"CMD", "true"}, Retries: 3}
			return d
		}(),
	})
	if reports[0].State != store.InstanceRunning {
		t.Fatalf("healthy container must map to running, got %s", reports[0].State)
	}
}

func TestHealthMappingExitedFails(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: false, ExitCode: 1},
	}}
	r := NewReconciler(f)
	reports := r.Reconcile(context.Background(), []store.DesiredInstance{desired("api", true, dockerd.Health{})})
	if reports[0].State != store.InstanceFailed {
		t.Fatalf("exited container must map to failed, got %s", reports[0].State)
	}
}
