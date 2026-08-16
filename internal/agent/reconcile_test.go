package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	prunedNetworks  []string
	pruneErrs       map[string]error

	// removeEnvCalls records every RemoveEnv invocation including the failing
	// ones, which removedEnvs (successes only) cannot distinguish from a call
	// that never happened.
	removeEnvCalls []string

	// volumesAtCreate captures len(volumes) at the moment EnsureContainer runs,
	// so tests can assert EnsureVolume happened first without adding a shared
	// call-order log just for one assertion.
	volumesAtCreate int
}

func (f *fakeDriver) EnsureImage(ctx context.Context, ref string) (string, error) {
	return ref, nil
}
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
func (f *fakeDriver) PruneRevisionNetworks(ctx context.Context, env8 string, wanted map[string]bool) error {
	f.prunedNetworks = append(f.prunedNetworks, env8)
	return f.pruneErrs[env8]
}
func (f *fakeDriver) EnsureVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes = append(f.volumes, name)
	return nil
}
func (f *fakeDriver) RemoveEnv(ctx context.Context, env8 string) error {
	f.removeEnvCalls = append(f.removeEnvCalls, env8)
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
	if len(f.prunedNetworks) != 1 || f.prunedNetworks[0] != "env12345" {
		t.Fatalf("expected obsolete revision networks pruned, got %v", f.prunedNetworks)
	}
}

func TestReconcileCleansPreviouslyKnownEnvAfterDesiredDisappears(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)
	r.Reconcile(context.Background(), []store.DesiredInstance{desired("api", true, dockerd.Health{})}, nil, nil)
	f.prunedNetworks = nil
	f.managed = []dockerd.Managed{{ID: "old", Name: "cc-env12345-r1-blue-api", Swappable: true}}

	r.Reconcile(context.Background(), nil, nil, nil)
	if len(f.removed) != 1 || f.removed[0] != "old" {
		t.Fatalf("expected vanished failed rollout container removed, got %v", f.removed)
	}
	if len(f.prunedNetworks) != 1 || f.prunedNetworks[0] != "env12345" {
		t.Fatalf("expected vanished failed rollout network pruned, got %v", f.prunedNetworks)
	}
}

func TestReconcileRetriesNetworkPruneFailure(t *testing.T) {
	f := &fakeDriver{pruneErrs: map[string]error{"env12345": errors.New("network busy")}}
	r := NewReconciler(f)
	r.Reconcile(context.Background(), []store.DesiredInstance{desired("api", true, dockerd.Health{})}, nil, nil)
	f.prunedNetworks = nil
	r.Reconcile(context.Background(), nil, nil, nil)
	delete(f.pruneErrs, "env12345")
	r.Reconcile(context.Background(), nil, nil, nil)
	if len(f.prunedNetworks) != 2 {
		t.Fatalf("expected failed prune retried next tick, calls=%v", f.prunedNetworks)
	}
}

func TestReconcileStartsDependenciesFirst(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)
	api := desired("api", true, dockerd.Health{})
	api.Service.Depends = []string{"db"}
	db := desired("db", false, dockerd.Health{})
	r.Reconcile(context.Background(), []store.DesiredInstance{api, db}, nil, nil)
	if len(f.created) != 2 {
		t.Fatalf("expected 2 containers, got %v", f.created)
	}
	if f.created[0] != "cc-env12345-pinned-db" {
		t.Fatalf("dependency must start first, created=%v", f.created)
	}
}

func TestNoHealthcheckDebouncesBeforeRunning(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)
	r.debounce = time.Hour
	d := desired("api", true, dockerd.Health{})
	reports, _ := r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)
	if reports[0].State != store.InstanceStarting {
		t.Fatalf("fresh no-healthcheck container must stay starting, got %s", reports[0].State)
	}
	r.firstRunning[d.InstanceID] = time.Now().Add(-2 * time.Hour)
	reports, _ = r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)
	if reports[0].State != store.InstanceRunning {
		t.Fatalf("after debounce, running container must map to running, got %s", reports[0].State)
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

// An ingress service joins no network beyond the revision network it is created
// on, and nothing is attached to that network on its behalf either. Traffic
// reaches it at its node's address and its published port, so neither the
// tenant nor the router crosses into the other's networking.
//
// This assertion has now outlived two mechanisms. It first named the shared
// `cc-ingress` network the Sprint 2 plan called for, then asserted the router
// was attached here instead. Both are gone; what survives is the property that
// mattered underneath them — no shared network in either direction — which is
// why it is stated as the absence of *any* extra attachment rather than as the
// absence of a particular one.
func TestIngressServiceJoinsNoNetworkBeyondItsRevision(t *testing.T) {
	f := &fakeDriver{health: map[string]dockerd.Health{}}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})
	d.Service.Ingress = &spec.Ingress{Port: 80}
	r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)
	if len(f.attached) != 0 {
		t.Fatalf("a swappable ingress must join no network beyond its revision's, got attaches=%v", f.attached)
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

// A tombstone stays on offer for its whole 24h retention window, so without a
// skip set every tick re-runs RemoveEnv for every environment reaped in the
// last day — three Docker list calls each, at the default 2s poll, forever
// no-ops after the first.
func TestReconcileSkipsAlreadyTornDownEnv(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)

	for i := 0; i < 3; i++ {
		r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})
	}

	if len(f.removeEnvCalls) != 1 || f.removeEnvCalls[0] != "deadbeef" {
		t.Fatalf("a tombstone already acted on must not be re-run every tick, got %v", f.removeEnvCalls)
	}
}

// The skip set records successes only. A teardown that failed has not
// happened, so the tombstone's next offer must still be acted on — that
// retry is the only thing standing between a transient failure and leaked
// pinned containers and volumes.
func TestReconcileRetriesFailedTeardownOnTheNextTick(t *testing.T) {
	f := &fakeDriver{removeEnvErrs: map[string]error{"deadbeef": errors.New("volume busy")}}
	r := NewReconciler(f)

	for i := 0; i < 3; i++ {
		_, failed := r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})
		if len(failed) != 1 || failed[0].Env8 != "deadbeef" {
			t.Fatalf("tick %d: want deadbeef reported failed, got %v", i, failed)
		}
		// The reason must travel with the env8; reporting only which
		// environment failed leaves an unfixable teardown undiagnosable.
		if failed[0].Err == nil || !strings.Contains(failed[0].Err.Error(), "volume busy") {
			t.Fatalf("tick %d: want the driver's error surfaced, got %v", i, failed[0].Err)
		}
	}

	if len(f.removeEnvCalls) != 3 {
		t.Fatalf("a failed teardown must be retried on every subsequent tick, got %v", f.removeEnvCalls)
	}
}

// A cleanup failure has to reach the caller with its reason attached, the same
// as a teardown does. This one collapsed into a bare boolean: the pass retried
// every tick and reported nothing, so a prune that could never succeed — the
// router's endpoint on a superseded revision network — looked exactly like an
// environment with nothing to clean.
func TestReconcileSurfacesCleanupFailureWithReason(t *testing.T) {
	f := &fakeDriver{
		health:    map[string]dockerd.Health{},
		pruneErrs: map[string]error{"env12345": errors.New("refusing to prune network")},
	}
	r := NewReconciler(f)
	d := desired("api", true, dockerd.Health{})

	_, failures := r.Reconcile(context.Background(), []store.DesiredInstance{d}, nil, nil)
	if len(failures) != 1 {
		t.Fatalf("want one failure reported, got %v", failures)
	}
	if failures[0].Env8 != "env12345" {
		t.Errorf("Env8 = %q", failures[0].Env8)
	}
	if failures[0].Op != opNetworkCleanup {
		t.Errorf("Op = %q, want %q", failures[0].Op, opNetworkCleanup)
	}
	if failures[0].Err == nil || !strings.Contains(failures[0].Err.Error(), "refusing to prune") {
		t.Errorf("the driver's reason must survive, got %v", failures[0].Err)
	}
}

// The skip set is bounded by what the control plane is currently offering, so
// an env8 it has stopped offering (its tombstone swept past retention) is
// forgotten rather than remembered for the life of the process. Re-offering it
// costs one idempotent RemoveEnv, the same as a restart would.
func TestReconcileForgetsEnvsNoLongerOffered(t *testing.T) {
	f := &fakeDriver{}
	r := NewReconciler(f)

	r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})
	r.Reconcile(context.Background(), nil, nil, []string{"cafef00d"})
	r.Reconcile(context.Background(), nil, nil, []string{"deadbeef"})

	want := []string{"deadbeef", "cafef00d", "deadbeef"}
	if len(f.removeEnvCalls) != len(want) {
		t.Fatalf("want %v, got %v", want, f.removeEnvCalls)
	}
	for i := range want {
		if f.removeEnvCalls[i] != want[i] {
			t.Fatalf("want %v, got %v", want, f.removeEnvCalls)
		}
	}
}

// The skip set is per-Reconciler and in memory, so a restarted agent re-runs
// teardowns it already completed. That is deliberate — RemoveEnv is
// idempotent and the tombstone is still being offered — and this test exists
// so a future reader who "fixes" it by persisting the set has to argue with a
// red bar first.
func TestReconcileSkipSetDoesNotSurviveAgentRestart(t *testing.T) {
	f := &fakeDriver{}

	NewReconciler(f).Reconcile(context.Background(), nil, nil, []string{"deadbeef"})
	NewReconciler(f).Reconcile(context.Background(), nil, nil, []string{"deadbeef"})

	if len(f.removeEnvCalls) != 2 {
		t.Fatalf("a fresh Reconciler must re-run the teardown, got %v", f.removeEnvCalls)
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

	if len(failed) != 1 || failed[0].Env8 != "deadbeef" {
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
