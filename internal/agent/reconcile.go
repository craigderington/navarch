package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// DockerDriver is the subset of *dockerd.Driver the reconciler needs. Naming it
// here (rather than importing the concrete type) lets tests substitute a fake
// and keeps this logic free of the Docker SDK.
type DockerDriver interface {
	EnsureImage(ctx context.Context, ref string) (string, error)
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
	EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec, secrets dockerd.SecretSource) (string, bool, error)
	AttachNetwork(ctx context.Context, containerID, network string, aliases ...string) error
	// AttachRouterToNetwork connects the platform ingress proxy to a
	// revision network so it can reach that revision's ingress container
	// without putting tenant containers on a shared mesh.
	AttachRouterToNetwork(ctx context.Context, network string) error
	InspectHealth(ctx context.Context, containerID string) (dockerd.Health, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context, env8 string) ([]dockerd.Managed, error)
	// PruneRevisionNetworks removes obsolete env-scoped revision networks after
	// disconnecting only containers managed for that same environment.
	PruneRevisionNetworks(ctx context.Context, env8 string, wanted map[string]bool) error
	// EnsureVolume creates a named volume with labels. Docker would create it
	// implicitly on first mount, but implicit volumes carry no labels, and
	// teardown must match volumes exactly — a volume is the one object here
	// whose deletion cannot be undone.
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	// RemoveEnv destroys everything belonging to an environment: containers
	// (including pinned), networks and named volumes. This is the ONLY path
	// that removes pinned containers or volumes, and it fires only on an
	// explicit tombstone.
	RemoveEnv(ctx context.Context, env8 string) error
}

type Report struct {
	InstanceID   uuid.UUID
	State        store.InstanceState
	ContainerID  string
	HealthStatus string
	LastError    string
	RestartCount int
	SetStarted   bool
}

type Reconciler struct {
	drv      DockerDriver
	debounce time.Duration
	// firstRunning is when a no-healthcheck container was first observed
	// running. It must stay up for debounce before we report running, so a
	// crash-loop cannot auto-promote on the first tick.
	firstRunning map[uuid.UUID]time.Time

	// tornDown records env8s whose RemoveEnv already returned nil. A tombstone
	// stays on offer for its full retention window (24h), so without this every
	// tick would re-run RemoveEnv — three Docker list calls — for every
	// environment reaped in the last day, multiplied by the poll rate and, in a
	// multi-node org, by the node count. All of it no-ops after the first.
	//
	// Deliberately in memory only: a restarted agent re-runs teardowns it has
	// already done, which is what we want. The tombstone is still being
	// offered, RemoveEnv is idempotent, and the restart may well be the thing
	// that fixed whatever left state behind. Persisting this set would trade a
	// harmless repeat for a way to permanently skip a teardown that never
	// actually completed — don't.
	//
	// No mutex: Run drives a single ticker loop in one goroutine and the
	// Reconciler it builds is not shared with anything else, so Reconcile is
	// never concurrent. Anything that changes that — a push endpoint waking a
	// second reconcile, per-env parallelism — has to guard this map, and the
	// reports/GC bookkeeping below with it.
	tornDown map[string]bool
	// knownEnvs retains environments seen on the previous tick so a failed
	// first rollout can still have its swappable containers and networks
	// collected after its desired rows disappear.
	knownEnvs map[string]bool
}

func NewReconciler(drv DockerDriver) *Reconciler {
	return &Reconciler{
		drv: drv, debounce: 5 * time.Second,
		tornDown: map[string]bool{}, knownEnvs: map[string]bool{},
		firstRunning: map[uuid.UUID]time.Time{},
	}
}

// Reconcile converges Docker to the desired instance set and returns a report
// per instance. It also garbage-collects containers this env manages that are
// no longer desired — that is how a superseded revision's swappable containers
// are torn down.
//
// secrets is keyed by env8; each environment's decrypted values only reach
// containers in that environment. A missing entry (env8 not in the map) is
// nil, which the driver treats as an empty source — every ${secret:KEY}
// reference then fails as missing rather than resolving to another env's value.
//
// teardownEnvs is the explicit, control-plane-issued list of env8s whose
// durable state this node must destroy. It is never inferred from desired
// being empty — an empty desired-state means "nothing to tell you", not
// "destroy everything", and a control-plane outage must not read as the latter.
//
// A teardown that has already succeeded on this Reconciler is skipped — see
// the tornDown field — because the same tombstone is offered on every tick for
// its whole retention window.
//
// The second return value is the env8s whose RemoveEnv call failed. Reconcile
// itself is pure and carries no logger, so it cannot log the failure — the
// caller, which does, is responsible for surfacing it. A failure is not
// retried within this call; the tombstone stays in teardownEnvs for its full
// retention window, so the next tick tries again regardless of whether this
// one's failure got logged.
// TeardownFailure is one environment whose RemoveEnv call did not succeed,
// carrying the reason so the caller can log something actionable.
type TeardownFailure struct {
	Env8 string
	Err  error
}

func (r *Reconciler) Reconcile(ctx context.Context, desired []store.DesiredInstance, secrets map[string]dockerd.SecretSource, teardownEnvs []string) ([]Report, []TeardownFailure) {
	reports := make([]Report, 0, len(desired))
	wanted := map[string]bool{} // container name → desired
	envs := map[string]bool{}
	wantedNetworks := map[string]bool{}
	liveIDs := map[uuid.UUID]bool{}

	for _, di := range orderDesired(desired) {
		name := containerName(di)
		wanted[name] = true
		envs[di.Env8] = true
		wantedNetworks[di.ProjectName] = true
		liveIDs[di.InstanceID] = true
		reports = append(reports, r.ensure(ctx, di, name, secrets[di.Env8]))
	}
	for id := range r.firstRunning {
		if !liveIDs[id] {
			delete(r.firstRunning, id)
		}
	}

	// GC: any managed container in a touched env whose name is not wanted, and
	// which is swappable, is an orphan. Pinned containers are never GC'd here —
	// a live deployment still holds a desired row for them.
	cleanupEnvs := make(map[string]bool, len(r.knownEnvs)+len(envs))
	for env8 := range r.knownEnvs {
		cleanupEnvs[env8] = true
	}
	for env8 := range envs {
		cleanupEnvs[env8] = true
	}
	nextKnown := make(map[string]bool, len(envs))
	for env8 := range envs {
		nextKnown[env8] = true
	}
	for env8 := range cleanupEnvs {
		cleanupFailed := false
		managed, err := r.drv.ListManaged(ctx, env8)
		if err != nil {
			cleanupFailed = true
		} else {
			for _, m := range managed {
				if !wanted[m.Name] && m.Swappable {
					if err := r.drv.StopRemove(ctx, m.ID); err != nil {
						cleanupFailed = true
					}
				}
			}
		}
		if err := r.drv.PruneRevisionNetworks(ctx, env8, wantedNetworks); err != nil {
			cleanupFailed = true
		}
		if cleanupFailed {
			nextKnown[env8] = true
		}
	}
	r.knownEnvs = nextKnown

	// Teardown runs last: an environment being destroyed may also appear in
	// desired (a tick straddling the reap), and destroying it after converging
	// costs one wasted create rather than leaving a half-removed env behind.
	var failedTeardowns []TeardownFailure
	for _, env8 := range teardownEnvs {
		if r.tornDown[env8] {
			continue
		}
		if err := r.drv.RemoveEnv(ctx, env8); err != nil {
			// Not recorded: a teardown that failed has not happened, so the
			// tombstone's next offer must still be acted on. The error travels
			// with the env8 — this used to report only which environment
			// failed, which turned a teardown that could never succeed into a
			// warning line repeating every two seconds with no way to tell why.
			failedTeardowns = append(failedTeardowns, TeardownFailure{Env8: env8, Err: err})
			continue
		}
		r.tornDown[env8] = true
	}
	// Forget env8s the control plane has stopped offering — their tombstones
	// were swept, and an env8 comes from a UUID so it never comes back. This
	// bounds the set by the live tombstone count instead of letting it grow
	// for the life of the process. Losing an entry early is harmless: it costs
	// one repeated idempotent RemoveEnv, the same as an agent restart.
	if len(r.tornDown) > len(teardownEnvs) {
		offered := make(map[string]bool, len(teardownEnvs))
		for _, env8 := range teardownEnvs {
			offered[env8] = true
		}
		for env8 := range r.tornDown {
			if !offered[env8] {
				delete(r.tornDown, env8)
			}
		}
	}
	return reports, failedTeardowns
}

func (r *Reconciler) ensure(ctx context.Context, di store.DesiredInstance, name string, secrets dockerd.SecretSource) Report {
	rep := Report{InstanceID: di.InstanceID}
	fail := func(err error) Report {
		rep.State = store.InstanceFailed
		rep.LastError = err.Error()
		return rep
	}

	digest, err := r.drv.EnsureImage(ctx, di.Service.Image)
	if err != nil {
		return fail(err)
	}
	if _, err := r.drv.EnsureNetwork(ctx, di.ProjectName, map[string]string{"cc.env": di.Env8}); err != nil {
		return fail(err)
	}

	cs := containerSpec(di, name)
	if digest != "" {
		cs.Image = digest
	}
	for _, m := range cs.Mounts {
		if err := r.drv.EnsureVolume(ctx, m.Volume, map[string]string{"cc.env": di.Env8}); err != nil {
			return fail(err)
		}
	}
	id, _, err := r.drv.EnsureContainer(ctx, cs, secrets)
	if err != nil {
		return fail(err)
	}
	// A pinned container is created once under its env network but must be
	// reachable from every revision's network; attach it to this revision's.
	if !di.Swappable {
		if err := r.drv.AttachNetwork(ctx, id, di.ProjectName, di.ServiceName); err != nil {
			return fail(err)
		}
	}
	// Traefik joins the revision network; tenant ingress stays off any
	// shared mesh so one fleet cannot talk to another.
	if di.Service.Ingress != nil {
		if err := r.drv.AttachRouterToNetwork(ctx, di.ProjectName); err != nil {
			return fail(err)
		}
	}

	rep.ContainerID = id
	rep.SetStarted = true
	h, err := r.drv.InspectHealth(ctx, id)
	if err != nil {
		return fail(err)
	}
	rep.RestartCount = h.RestartCount
	rep.State, rep.HealthStatus = r.observe(di, h)
	return rep
}

func (r *Reconciler) observe(di store.DesiredInstance, h dockerd.Health) (store.InstanceState, string) {
	hasHealthcheck := di.Service.Health != nil && len(di.Service.Health.Test) > 0
	if !h.Running {
		delete(r.firstRunning, di.InstanceID)
		return mapHealth(hasHealthcheck, h)
	}
	if hasHealthcheck {
		return mapHealth(true, h)
	}
	if r.firstRunning[di.InstanceID].IsZero() {
		r.firstRunning[di.InstanceID] = time.Now()
	}
	if time.Since(r.firstRunning[di.InstanceID]) < r.debounce {
		return store.InstanceStarting, "starting"
	}
	return store.InstanceRunning, "running"
}

// orderDesired starts dependencies before dependents within each
// environment/revision. Independent services keep their input order.
func orderDesired(desired []store.DesiredInstance) []store.DesiredInstance {
	if len(desired) < 2 {
		return desired
	}
	type key struct{ env, project string }
	groups := map[key][]store.DesiredInstance{}
	var order []key
	for _, di := range desired {
		k := key{di.Env8, di.ProjectName}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], di)
	}
	out := make([]store.DesiredInstance, 0, len(desired))
	for _, k := range order {
		out = append(out, topoGroup(groups[k])...)
	}
	return out
}

func topoGroup(in []store.DesiredInstance) []store.DesiredInstance {
	byName := make(map[string]store.DesiredInstance, len(in))
	for _, di := range in {
		byName[di.ServiceName] = di
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	out := make([]store.DesiredInstance, 0, len(in))
	var visit func(string)
	visit = func(name string) {
		if color[name] != white {
			return
		}
		color[name] = grey
		di := byName[name]
		for _, dep := range di.Service.Depends {
			if _, ok := byName[dep]; ok {
				visit(dep)
			}
		}
		color[name] = black
		out = append(out, di)
	}
	for _, di := range in {
		visit(di.ServiceName)
	}
	return out
}

// mapHealth turns a container's observed state into an instance state.
func mapHealth(hasHealthcheck bool, h dockerd.Health) (store.InstanceState, string) {
	if !h.Running {
		if h.ExitCode != 0 {
			return store.InstanceFailed, "exited"
		}
		return store.InstanceStopped, "stopped"
	}
	if hasHealthcheck {
		switch h.Status {
		case "healthy":
			return store.InstanceRunning, "healthy"
		case "unhealthy":
			return store.InstanceUnhealthy, "unhealthy"
		default: // "starting" or empty until the first probe resolves
			return store.InstanceStarting, "starting"
		}
	}
	// No healthcheck: running is the best signal we have. The controller's
	// start-timeout guards against a container that will crash after this tick.
	return store.InstanceRunning, "running"
}

func containerName(di store.DesiredInstance) string {
	if di.Swappable {
		return fmt.Sprintf("%s-%s", di.ProjectName, di.ServiceName)
	}
	return fmt.Sprintf("cc-%s-pinned-%s", di.Env8, di.ServiceName)
}

func containerSpec(di store.DesiredInstance, name string) dockerd.ContainerSpec {
	svc := di.Service
	mounts := make([]dockerd.VolumeMount, 0, len(svc.Mounts))
	for _, m := range svc.Mounts {
		if m.Kind != spec.MountVolume {
			continue // tmpfs handled later; Slice A: named volumes only
		}
		mounts = append(mounts, dockerd.VolumeMount{
			Volume:   fmt.Sprintf("cc-%s-%s", di.Env8, m.Source),
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return dockerd.ContainerSpec{
		Name: name, Image: svc.Image, Env: svc.Env, SecretEnv: svc.SecretEnv,
		Cmd: svc.Command, Entrypoint: svc.Entrypoint, WorkingDir: svc.WorkingDir,
		User: svc.User, Mounts: mounts, Health: svc.Health, Restart: svc.Restart,
		CPUMillis: svc.Limits.CPUMillis, MemoryBytes: svc.Limits.MemoryBytes,
		Network: di.ProjectName,
		Labels: map[string]string{
			"cc.env": di.Env8, "cc.deployment": di.DeploymentID.String(),
			"cc.service": di.ServiceName, "cc.swappable": boolStr(di.Swappable),
		},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
