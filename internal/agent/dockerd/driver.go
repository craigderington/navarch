// Package dockerd is the ONLY package that imports the Docker SDK. Everything
// above it speaks ContainerSpec and the Driver methods, so the container
// runtime could be swapped without touching the agent's reconcile logic.
package dockerd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"

	"github.com/craig/composectl/internal/spec"
)

const specFingerprintLabel = "cc.spec-fingerprint"

// SecretSource resolves ${secret:KEY} references at container start. Sprint 2
// uses a trivial dev implementation; Sprint 3 replaces it with the encrypted
// per-environment secret store. Plaintext never leaves the agent.
type SecretSource interface {
	Get(key string) (string, bool)
}

type Driver struct {
	cli *client.Client
}

func New(host string) (*Driver, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Driver{cli: cli}, nil
}

type VolumeMount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

type ContainerSpec struct {
	Name        string
	Image       string
	Env         map[string]string
	SecretEnv   map[string]string
	Cmd         []string
	Entrypoint  []string
	WorkingDir  string
	User        string
	Mounts      []VolumeMount
	Health      *spec.HealthCheck
	Labels      map[string]string
	Network     string
	Restart     string
	CPUMillis   int
	MemoryBytes int64
	// PublishPort is the container port to publish on the host, set only for an
	// ingress service. The host port is always 0 so Docker allocates it: there
	// is no allocator here to collide, and the assignment is read back from the
	// running container rather than predicted.
	//
	// This does not contradict the platform's rejection of `ports:` in tenant
	// compose files. That rule stops a *tenant* claiming a fixed host port, which
	// collides between revisions and between stacks. This is the platform
	// publishing an ephemeral port so its own router can reach the container from
	// another node — nothing a compose author can ask for or influence.
	PublishPort int
}

func (d *Driver) EnsureImage(ctx context.Context, ref string) (string, error) {
	// Pull only when absent — an image already present (common in dev) skips
	// the network round-trip. ImageInspectWithRaw is the form stable across
	// SDK versions (three returns: inspect, raw JSON, err).
	inspected, _, err := d.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		rc, pullErr := d.cli.ImagePull(ctx, ref, image.PullOptions{})
		if pullErr != nil {
			return "", fmt.Errorf("pull %s: %w", ref, pullErr)
		}
		defer rc.Close()
		_, _ = io.Copy(io.Discard, rc) // draining the stream is what blocks until done
		inspected, _, err = d.cli.ImageInspectWithRaw(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", ref, err)
		}
	}
	return imageDigest(inspected.ID, inspected.RepoDigests, ref), nil
}

func imageDigest(id string, repoDigests []string, ref string) string {
	for _, d := range repoDigests {
		if strings.Contains(d, "@sha256:") {
			return d
		}
	}
	if strings.HasPrefix(id, "sha256:") {
		return id
	}
	return ref
}

const (
	routerLabelKey   = "cc.role"
	routerLabelValue = "ingress-router"
)

// isIngressRouter reports whether a container is the platform's own router.
//
// The agent no longer puts it on tenant networks — routing is address-based, so
// the router reaches a tenant at its node's address and published port. But the
// label still has to be recognised, because endpoints created before that
// changed do not disappear when the code does: a daemon upgraded across this
// change still has a router attached to every revision network it ever served,
// and something has to let those converge. Treating the router as foreign is
// what made them unprunable in the first place.
func isIngressRouter(labels map[string]string) bool {
	return labels[routerLabelKey] == routerLabelValue
}

func (d *Driver) EnsureNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return "", err
	}
	for _, n := range nets {
		if n.Name == name {
			return n.ID, nil
		}
	}
	created, err := d.cli.NetworkCreate(ctx, name, network.CreateOptions{Labels: labels})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return created.ID, nil
}

// EnsureVolume creates a named volume with labels, idempotently — Docker
// returns the existing volume rather than erroring when the name already
// exists. Docker creates named volumes implicitly when a container mounts
// them, but implicit volumes carry no labels — and teardown matches on the
// label, because a volume is the one object in this system whose deletion
// cannot be undone and a name-substring filter is not an exact match.
func (d *Driver) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	return err
}

func (d *Driver) removeNetwork(ctx context.Context, name string) error {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == name {
			return d.cli.NetworkRemove(ctx, n.ID)
		}
	}
	return nil
}

// EnsureContainer creates and starts the container if absent, or adopts the
// existing one by name. Adoption is how a pinned service is shared: the second
// deployment to want it finds it already running and reports created=false.
//
// secrets is per-call, not a Driver field, because Sprint 3 resolves secrets
// per-environment — a single Driver reconciles instances across many envs.
func (d *Driver) EnsureContainer(ctx context.Context, cs ContainerSpec, secrets SecretSource) (string, bool, error) {
	env, err := d.resolveEnv(cs.Env, cs.SecretEnv, secrets)
	if err != nil {
		return "", false, err
	}
	fingerprint, err := containerFingerprint(cs, env)
	if err != nil {
		return "", false, err
	}
	if existing, err := d.findByName(ctx, cs.Name); err != nil {
		return "", false, err
	} else if existing != "" {
		inspected, err := d.cli.ContainerInspect(ctx, existing)
		if err != nil {
			return "", false, err
		}
		if inspected.Config.Labels[specFingerprintLabel] != fingerprint {
			return "", false, fmt.Errorf("container %s configuration changed; automatic stateful recreation is not supported", cs.Name)
		}
		return existing, false, nil
	}

	labels := make(map[string]string, len(cs.Labels)+1)
	for k, v := range cs.Labels {
		labels[k] = v
	}
	labels[specFingerprintLabel] = fingerprint
	envSlice := make([]string, 0, len(env))
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}

	cfg := &container.Config{
		Image:      cs.Image,
		Env:        envSlice,
		Cmd:        cs.Cmd,
		Entrypoint: cs.Entrypoint,
		WorkingDir: cs.WorkingDir,
		User:       cs.User,
		Labels:     labels,
	}
	if cs.Health != nil && len(cs.Health.Test) > 0 {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        cs.Health.Test,
			Interval:    durationSecs(cs.Health.IntervalSec),
			Timeout:     durationSecs(cs.Health.TimeoutSec),
			Retries:     cs.Health.Retries,
			StartPeriod: durationSecs(cs.Health.StartSec),
		}
	}

	var publish nat.PortMap
	binds := make([]string, 0, len(cs.Mounts))
	for _, m := range cs.Mounts {
		bind := m.Volume + ":" + m.Target
		if m.ReadOnly {
			bind += ":ro"
		}
		binds = append(binds, bind)
	}

	// Host port 0 lets Docker allocate. Both halves are needed: ExposedPorts on
	// the container config and PortBindings on the host config — binding without
	// exposing silently publishes nothing.
	if cs.PublishPort > 0 {
		port, err := nat.NewPort("tcp", strconv.Itoa(cs.PublishPort))
		if err != nil {
			return "", false, fmt.Errorf("ingress port %d: %w", cs.PublishPort, err)
		}
		cfg.ExposedPorts = nat.PortSet{port: struct{}{}}
		publish = nat.PortMap{port: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}}}
	}

	hostCfg := &container.HostConfig{
		Binds:         binds,
		PortBindings:  publish,
		RestartPolicy: restartPolicy(cs.Restart),
		Resources: container.Resources{
			NanoCPUs:   int64(cs.CPUMillis) * 1_000_000, // millicpu → nanocpu
			Memory:     cs.MemoryBytes,
			MemorySwap: cs.MemoryBytes, // no extra swap beyond the memory cap
			PidsLimit:  int64Ptr(256),
		},
		SecurityOpt: []string{"no-new-privileges:true"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID"},
	}

	var netCfg *network.NetworkingConfig
	if cs.Network != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{cs.Network: {}},
		}
	}

	created, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, cs.Name)
	if err != nil {
		return "", false, fmt.Errorf("create %s: %w", cs.Name, err)
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", false, fmt.Errorf("start %s: %w", cs.Name, err)
	}
	return created.ID, true, nil
}

func containerFingerprint(cs ContainerSpec, resolvedEnv map[string]string) (string, error) {
	// Network and management labels are intentionally excluded: a pinned
	// container joins each new revision network while its runtime configuration
	// remains the same. Secret plaintext participates only through this hash.
	v := struct {
		Image       string
		Env         map[string]string
		Cmd         []string
		Entrypoint  []string
		WorkingDir  string
		User        string
		Mounts      []VolumeMount
		Health      *spec.HealthCheck
		Restart     string
		CPUMillis   int
		MemoryBytes int64
	}{cs.Image, resolvedEnv, cs.Cmd, cs.Entrypoint, cs.WorkingDir, cs.User,
		cs.Mounts, cs.Health, cs.Restart, cs.CPUMillis, cs.MemoryBytes}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("fingerprint container %s: %w", cs.Name, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (d *Driver) AttachNetwork(ctx context.Context, containerID, netName string, aliases ...string) error {
	// Idempotent: ignore "already exists" so re-reconciling is safe.
	err := d.cli.NetworkConnect(ctx, netName, containerID, &network.EndpointSettings{Aliases: aliases})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	return nil
}

type Health struct {
	Running      bool
	Status       string // "healthy" | "unhealthy" | "starting" | "" (none)
	ExitCode     int
	RestartCount int
	// PublishedPort is the host port Docker assigned to this container's ingress
	// port, 0 when nothing is published. It rides along on this inspect rather
	// than taking a call of its own: reconcile already inspects every container
	// on every tick, and a second round-trip per instance per tick to read one
	// integer is a poor trade. The port is only knowable after the container
	// exists, which is why it is observed here and not predicted at create time.
	PublishedPort int
}

func (d *Driver) InspectHealth(ctx context.Context, containerID string) (Health, error) {
	c, err := d.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return Health{}, err
	}
	h := Health{
		Running:      c.State.Running,
		ExitCode:     c.State.ExitCode,
		RestartCount: c.RestartCount,
	}
	if c.State.Health != nil {
		h.Status = c.State.Health.Status
	}
	if c.NetworkSettings != nil {
		h.PublishedPort = firstPublishedPort(c.NetworkSettings.Ports)
	}
	return h, nil
}

// firstPublishedPort reads back what Docker allocated. Only one port is ever
// published per container, but Docker lists a binding per address family, so
// IPv4 is preferred and any binding accepted — the two carry the same host port
// and picking whichever came first would make the value depend on map order.
func firstPublishedPort(ports nat.PortMap) int {
	best := 0
	for _, bindings := range ports {
		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}
			n, err := strconv.Atoi(b.HostPort)
			if err != nil || n <= 0 {
				continue
			}
			if b.HostIP == "0.0.0.0" {
				return n
			}
			best = n
		}
	}
	return best
}

func (d *Driver) StopRemove(ctx context.Context, containerID string) error {
	_ = d.cli.ContainerStop(ctx, containerID, container.StopOptions{})
	return d.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

type Managed struct {
	ID        string
	Name      string
	Service   string
	Swappable bool
}

func (d *Driver) ListManaged(ctx context.Context, env8 string) ([]Managed, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "cc.env="+env8)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Managed, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Managed{
			ID: c.ID, Name: name, Service: c.Labels["cc.service"],
			Swappable: c.Labels["cc.swappable"] == "true",
		})
	}
	return out, nil
}

// PruneRevisionNetworks removes env-scoped networks that are no longer in the
// desired deployment set. Two kinds of container have to come off first, and
// they are not the same case:
//
//   - Pinned containers, which are attached to every revision network.
//   - The platform's own ingress router. It carries cc.role=ingress-router and
//     no cc.env, so a bare "is this container managed for this env" test reads
//     it as foreign.
//
// That second case used to make every superseded revision network unprunable
// for the life of the environment — one leaked network per revision, each
// holding a router endpoint, until Docker's address pool ran out and new
// rollouts could no longer create a network.
//
// The agent no longer attaches the router to anything, so it should stop
// arriving here. The exemption stays anyway, because removing it would strand
// exactly the networks that motivated it: an endpoint outlives the code that
// created it, and a daemon upgraded across that change still has a router on
// every revision network it once served. Keeping it is what lets those
// converge instead of leaking until someone removes them by hand.
//
// Disconnecting the router here is safe precisely because this network is not
// in `wanted`: no live revision uses it, and traffic reaches the live revision
// at its node's address rather than over this network.
//
// Anything else attached is genuinely unmanaged — a human, another tool — and
// still blocks removal rather than being yanked off a network it may be using.
// That restraint is the difference between this path and RemoveEnv, which runs
// only on an explicit tombstone and may disconnect anything.
//
// Failures are collected rather than returned on the first one: a single stuck
// network used to abort the pass and strand every other obsolete network for
// that environment behind it.
func (d *Driver) PruneRevisionNetworks(ctx context.Context, env8 string, wanted map[string]bool) error {
	f := filters.NewArgs(filters.Arg("label", "cc.env="+env8))
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range nets {
		if wanted[n.Name] {
			continue
		}
		if err := d.pruneNetwork(ctx, env8, n.Name, n.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *Driver) pruneNetwork(ctx context.Context, env8, name, id string) error {
	inspected, err := d.cli.NetworkInspect(ctx, id, network.InspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect network %s: %w", name, err)
	}
	for containerID := range inspected.Containers {
		ctr, err := d.cli.ContainerInspect(ctx, containerID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("inspect container %s on network %s: %w", containerID, name, err)
		}
		if ctr.Config.Labels["cc.env"] != env8 && !isIngressRouter(ctr.Config.Labels) {
			return fmt.Errorf("refusing to prune network %s: unmanaged container %s is attached", name, containerID)
		}
		if err := d.cli.NetworkDisconnect(ctx, id, containerID, true); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("disconnect container %s from network %s: %w", containerID, name, err)
		}
	}
	if err := d.cli.NetworkRemove(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	return nil
}

// RemoveEnv destroys everything belonging to one environment: containers
// (including pinned ones), the revision networks, and the named volumes. It is
// the only path that removes durable state, and the reconciler calls it only
// for an environment the control plane has explicitly tombstoned.
//
// Order matters: a network in use cannot be removed while a container is
// attached, and a volume in use cannot be removed at all.
//
// Idempotent by design, not by acknowledgement: a tombstone is re-offered on
// every tick for its whole retention window, so a second (or tenth) call
// against an already-gone environment must not error.
func (d *Driver) RemoveEnv(ctx context.Context, env8 string) error {
	f := filters.NewArgs(filters.Arg("label", "cc.env="+env8))

	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return err
	}
	for _, c := range containers {
		if err := d.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove container %s: %w", c.ID, err)
		}
	}

	// Failures are collected rather than returned early. The three resource
	// kinds are independent, and returning on the first one let a network that
	// would not delete strand the environment's volumes indefinitely: teardown
	// retried forever, never reaching the volume step, and the durable state a
	// tombstone exists to destroy survived.
	var problems []error

	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, n := range nets {
		// Disconnect whatever is still attached, including containers this
		// platform does not manage. Docker refuses to remove a network that
		// still has an active endpoint, and because networks are removed before
		// volumes, one stuck network used to fail teardown for every preview
		// that had an ingress — which was all of them — leaving the durable
		// state a tombstone exists to destroy alive indefinitely.
		//
		// The router was the original cause and no longer attaches itself, but
		// this stays unconditional on purpose. It is not "handle the router",
		// it is "a tombstone is an instruction to destroy": anything at all on
		// this network — a leftover router endpoint, a shell someone attached
		// to debug — must not be able to strand a volume.
		//
		// PruneRevisionNetworks deliberately does the opposite and refuses to
		// touch a network with an unmanaged container attached. That is right
		// for routine GC, which runs constantly and must never yank a live
		// container off a network. It is wrong here, for the reason above.
		inspected, err := d.cli.NetworkInspect(ctx, n.ID, network.InspectOptions{})
		if err != nil && !errdefs.IsNotFound(err) {
			problems = append(problems, fmt.Errorf("inspect network %s: %w", n.Name, err))
			continue
		}
		for containerID := range inspected.Containers {
			if err := d.cli.NetworkDisconnect(ctx, n.ID, containerID, true); err != nil && !errdefs.IsNotFound(err) {
				problems = append(problems, fmt.Errorf("disconnect %s from network %s: %w", containerID, n.Name, err))
			}
		}
		if err := d.cli.NetworkRemove(ctx, n.ID); err != nil && !errdefs.IsNotFound(err) {
			problems = append(problems, fmt.Errorf("remove network %s: %w", n.Name, err))
		}
	}

	vols, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, v := range vols.Volumes {
		if err := d.cli.VolumeRemove(ctx, v.Name, false); err != nil && !errdefs.IsNotFound(err) {
			problems = append(problems, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(problems...)
}

func (d *Driver) findByName(ctx context.Context, name string) (string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "^/"+name+"$")),
	})
	if err != nil {
		return "", err
	}
	for _, c := range list {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == name {
				return c.ID, nil
			}
		}
	}
	return "", nil
}

// resolveEnv merges plain env with secret templates expanded via the
// SecretSource, using exactly spec.SecretRefPattern so the agent expands the
// same syntax the parser recognized. A missing secret is a hard error — better
// than handing the container a half-built connection string.
//
// secrets may be nil (no source configured for this call) — every
// ${secret:KEY} reference is then treated as missing, same as a source that
// simply doesn't have the key.
func (d *Driver) resolveEnv(env, secretEnv map[string]string, secrets SecretSource) (map[string]string, error) {
	out := make(map[string]string, len(env)+len(secretEnv))
	for k, v := range env {
		out[k] = v
	}
	for k, tmpl := range secretEnv {
		var missing string
		expanded := spec.SecretRefPattern.ReplaceAllStringFunc(tmpl, func(m string) string {
			sub := spec.SecretRefPattern.FindStringSubmatch(m)
			var val string
			var ok bool
			if secrets != nil {
				val, ok = secrets.Get(sub[1])
			}
			if !ok {
				missing = sub[1]
				return m
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("secret %q referenced by %s is not available", missing, k)
		}
		out[k] = expanded
	}
	return out, nil
}

func durationSecs(sec int) time.Duration { return time.Duration(sec) * time.Second }

func int64Ptr(n int64) *int64 { return &n }

func restartPolicy(mode string) container.RestartPolicy {
	switch mode {
	case "", "no":
		return container.RestartPolicy{Name: container.RestartPolicyDisabled}
	case "always":
		return container.RestartPolicy{Name: container.RestartPolicyAlways}
	case "unless-stopped":
		return container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	default: // on-failure and friends
		return container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3}
	}
}
