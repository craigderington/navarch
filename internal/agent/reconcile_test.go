package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// fakeDriver records calls and returns canned health.
type fakeDriver struct {
	created         []string
	attached        []string
	removed         []string
	managed         []dockerd.Managed
	health          map[string]dockerd.Health
	lastSecretValue string
	volumes         []string
	removedEnvs     []string
	removeEnvErrs   map[string]error // env8 -> error RemoveEnv should return for it

	// volumesAtCreate captures len(volumes) at the moment EnsureContainer runs,
	// so tests can assert EnsureVolume happened first without adding a shared
	// call-order log just for one assertion.
	volumesAtCreate int
}

func (f *fakeDriver) EnsureImage(ctx context.Context, ref string) error { return nil }
func (f *fakeDriver) EnsureNetwork(ctx context.Context, name string, l map[string]string) (string, error) {
	return "net-" + name, nil
}
func (f *fakeDriver) EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec, secrets dockerd.SecretSource) (string, bool, error) {
	f.volumesAtCreate = len(f.volumes)
	f.created = append(f.created, cs.Name)
	if secrets != nil {
		f.lastSecretValue, _ = secrets.Get("name")
	}
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
func (f *fakeDriver) EnsureVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes = append(f.volumes, name)
	return nil
}
func (f *fakeDriver) RemoveEnv(ctx context.Context, env8 string) error {
	if err, ok := f.removeEnvErrs[env8]; ok {
		return err
	}
	f.removedEnvs = append(f.removedEnvs, env8)
	return nil
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
	reports, _ := r.Reconcile(context.Background(), []store.DesiredInstance{
		desired("api", true, dockerd.Health{}),
		desired("db", false, dockerd.Health{}),
	}, nil, nil)
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
	r.Reconcile(context.Background(), []store.DesiredInstance{desired("db", false, dockerd.Health{})}, nil, nil)
	if len(f.removed) != 1 || f.removed[0] != "id-old" {
		t.Fatalf("expected only the orphan swappable removed, got %v", f.removed)
	}
}

func TestHealthMappingHealthchecked(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: true, Status: "healthy"},
	}}
	r := NewReconciler(f)
	reports, _ := r.Reconcile(context.Background(), []store.DesiredInstance{
		func() store.DesiredInstance {
			d := desired("api", true, dockerd.Health{})
			d.Service.Health = &spec.HealthCheck{Test: []string{"CMD", "true"}, Retries: 3}
			return d
		}(),
	}, nil, nil)
	if reports[0].State != store.InstanceRunning {
		t.Fatalf("healthy container must map to running, got %s", reports[0].State)
	}
}

func TestHealthMappingExitedFails(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{
		"id-cc-env12345-r1-blue-api": {Running: false, ExitCode: 1},
	}}
	r := NewReconciler(f)
	reports, _ := r.Reconcile(context.Background(), []store.DesiredInstance{desired("api", true, dockerd.Health{})}, nil, nil)
	if reports[0].State != store.InstanceFailed {
		t.Fatalf("exited container must map to failed, got %s", reports[0].State)
	}
}

func TestReconcileAttachesIngressToSharedNetwork(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})
	d.Service.Ingress = &spec.Ingress{Port: 80}
	r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)
	var attached bool
	for _, a := range f.attached {
		if strings.HasSuffix(a, "->cc-ingress") {
			attached = true
		}
	}
	if !attached {
		t.Fatalf("ingress container must join cc-ingress, got attaches=%v", f.attached)
	}
}

func TestReconcilePassesPerEnvSecrets(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})
	d.Service.SecretEnv = map[string]string{"WHOAMI_NAME": "${secret:name}"}
	sources := map[string]dockerd.SecretSource{d.Env8: EnvSecrets{"name": "revealed"}}
	r.Reconcile(context.Background(), []store.DesiredInstance{d}, sources, nil)
	if f.lastSecretValue != "revealed" {
		t.Fatalf("expected the env's secret source to reach the driver, got %q", f.lastSecretValue)
	}
}

func TestReconcileTearsDownTombstonedEnv(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)

	_, failed := r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})

	if len(f.removedEnvs) != 1 || f.removedEnvs[0] != "deadbeef" {
		t.Fatalf("want RemoveEnv(deadbeef), got %v", f.removedEnvs)
	}
	if len(failed) != 0 {
		t.Fatalf("a successful teardown must not be reported as failed, got %v", failed)
	}
}

// Reconcile can't log a failed RemoveEnv itself — it carries no logger, so it
// reports the failing env8 back to the caller (agent.go's reconcileTick, which
// does have one) instead. A failure on one env must not stop the others in the
// same tick: RemoveEnv is still attempted for every entry in teardownEnvs.
func TestReconcileReportsFailedTeardown(t *testing.T) {
	f := &fakeDriver{removeEnvErrs: map[string]error{"deadbeef": errors.New("volume busy")}}
	r := NewReconciler(f)

	_, failed := r.Reconcile(context.Background(), nil, nil, []string{"deadbeef", "cafef00d"})

	if len(failed) != 1 || failed[0] != "deadbeef" {
		t.Fatalf("want deadbeef reported as failed, got %v", failed)
	}
	if len(f.removedEnvs) != 1 || f.removedEnvs[0] != "cafef00d" {
		t.Fatalf("cafef00d's teardown should still have run despite deadbeef's failure, got %v", f.removedEnvs)
	}
}

// The invariant this whole slice comes closest to breaking. An empty
// desired-state means "I have nothing to tell you", not "destroy everything":
// a control-plane outage, a failed migration, or an auth error can all produce
// one, and none of them should drop a production database.
//
// desired is deliberately NOT empty here — it holds one live instance for a
// service ("worker") other than the pinned "db" below. That keeps envs
// non-empty so the GC loop's body actually runs and inspects the pinned
// container. With desired truly empty, envs would be empty too, the GC loop
// would never execute, and this test would pass even if the `&& m.Swappable`
// guard were deleted from it — it would never see the pinned container to
// remove it wrongly.
func TestReconcileEmptyDesiredStateNeverRemovesAnEnv(t *testing.T) {
	f := &fakeDriver{
		managed: []dockerd.Managed{
			{ID: "id-pinned", Name: "cc-env12345-pinned-db", Service: "db", Swappable: false},
			{ID: "id-orphan", Name: "cc-env12345-r0-green-api", Service: "api", Swappable: true},
		},
	}
	r := NewReconciler(f)

	_, failed := r.Reconcile(context.Background(), []store.DesiredInstance{desired("worker", true, dockerd.Health{})}, nil, nil)

	if len(failed) != 0 {
		t.Fatalf("no teardownEnvs given, expected no reported failures, got %v", failed)
	}
	if len(f.removedEnvs) != 0 {
		t.Fatalf("empty teardownEnvs must never trigger RemoveEnv, got %v", f.removedEnvs)
	}
	for _, id := range f.removed {
		if id == "id-pinned" {
			t.Fatalf("pinned container must never be GC'd, got removed=%v", f.removed)
		}
	}
	var sawOrphanRemoved bool
	for _, id := range f.removed {
		if id == "id-orphan" {
			sawOrphanRemoved = true
		}
	}
	if !sawOrphanRemoved {
		t.Fatalf("orphaned swappable container should still be GC'd — otherwise the GC loop's body never ran and this test proves nothing, got removed=%v", f.removed)
	}
}

// EnsureVolume must run before EnsureContainer: Docker creates a named volume
// implicitly on first mount, but an implicit volume carries no labels, which
// would make exact-match teardown silently find nothing.
func TestReconcileCreatesVolumeBeforeContainer(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("db", false, dockerd.Health{})
	d.Service.Mounts = []spec.Mount{{Kind: spec.MountVolume, Source: "data", Target: "/var/lib/data"}}

	r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)

	if len(f.volumes) != 1 || f.volumes[0] != "cc-env12345-data" {
		t.Fatalf("want volume cc-env12345-data created, got %v", f.volumes)
	}
	if len(f.created) != 1 {
		t.Fatalf("expected the container to still be created, got %v", f.created)
	}
	if f.volumesAtCreate != 1 {
		t.Fatalf("EnsureVolume must be called before EnsureContainer, but only %d volume(s) existed when the container was created", f.volumesAtCreate)
	}
}
