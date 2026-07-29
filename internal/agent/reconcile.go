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
	EnsureImage(ctx context.Context, ref string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error)
	EnsureContainer(ctx context.Context, cs dockerd.ContainerSpec, secrets dockerd.SecretSource) (string, bool, error)
	AttachNetwork(ctx context.Context, containerID, network string, aliases ...string) error
	InspectHealth(ctx context.Context, containerID string) (dockerd.Health, error)
	StopRemove(ctx context.Context, containerID string) error
	ListManaged(ctx context.Context, env8 string) ([]dockerd.Managed, error)
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
}

func NewReconciler(drv DockerDriver) *Reconciler {
	return &Reconciler{drv: drv, debounce: 5 * time.Second}
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
// The second return value is the env8s whose RemoveEnv call failed. Reconcile
// itself is pure and carries no logger, so it cannot log the failure — the
// caller, which does, is responsible for surfacing it. A failure is not
// retried within this call; the tombstone stays in teardownEnvs for its full
// retention window, so the next tick tries again regardless of whether this
// one's failure got logged.
func (r *Reconciler) Reconcile(ctx context.Context, desired []store.DesiredInstance, secrets map[string]dockerd.SecretSource, teardownEnvs []string) ([]Report, []string) {
	reports := make([]Report, 0, len(desired))
	wanted := map[string]bool{} // container name → desired
	envs := map[string]bool{}

	for _, di := range desired {
		name := containerName(di)
		wanted[name] = true
		envs[di.Env8] = true
		reports = append(reports, r.ensure(ctx, di, name, secrets[di.Env8]))
	}

	// GC: any managed container in a touched env whose name is not wanted, and
	// which is swappable, is an orphan. Pinned containers are never GC'd here —
	// a live deployment still holds a desired row for them.
	for env8 := range envs {
		managed, err := r.drv.ListManaged(ctx, env8)
		if err != nil {
			continue
		}
		for _, m := range managed {
			if !wanted[m.Name] && m.Swappable {
				_ = r.drv.StopRemove(ctx, m.ID)
			}
		}
	}

	// Teardown runs last: an environment being destroyed may also appear in
	// desired (a tick straddling the reap), and destroying it after converging
	// costs one wasted create rather than leaving a half-removed env behind.
	var failedTeardowns []string
	for _, env8 := range teardownEnvs {
		if err := r.drv.RemoveEnv(ctx, env8); err != nil {
			failedTeardowns = append(failedTeardowns, env8)
			continue
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

	if err := r.drv.EnsureImage(ctx, di.Service.Image); err != nil {
		return fail(err)
	}
	if _, err := r.drv.EnsureNetwork(ctx, di.ProjectName, map[string]string{"cc.env": di.Env8}); err != nil {
		return fail(err)
	}

	cs := containerSpec(di, name)
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
	// An ingress service also joins the shared cc-ingress network so Traefik
	// (permanently on it) can reach this revision's ingress container by its
	// unique name. Blue and green never collide because the name carries the
	// revision + slot.
	if di.Service.Ingress != nil {
		if _, err := r.drv.EnsureNetwork(ctx, "cc-ingress", map[string]string{"cc.shared": "ingress"}); err != nil {
			return fail(err)
		}
		if err := r.drv.AttachNetwork(ctx, id, "cc-ingress", name); err != nil {
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
	rep.State, rep.HealthStatus = mapHealth(di.Service.Health != nil && len(di.Service.Health.Test) > 0, h)
	return rep
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
