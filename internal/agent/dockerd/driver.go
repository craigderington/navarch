// Package dockerd is the ONLY package that imports the Docker SDK. Everything
// above it speaks ContainerSpec and the Driver methods, so the container
// runtime could be swapped without touching the agent's reconcile logic.
package dockerd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/craig/composectl/internal/spec"
)

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
}

func (d *Driver) EnsureImage(ctx context.Context, ref string) error {
	// Pull only when absent — an image already present (common in dev) skips
	// the network round-trip. ImageInspectWithRaw is the form stable across
	// SDK versions (three returns: inspect, raw JSON, err).
	if _, _, err := d.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		return nil
	}
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc) // draining the stream is what blocks until done
	return nil
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
	if existing, err := d.findByName(ctx, cs.Name); err != nil {
		return "", false, err
	} else if existing != "" {
		return existing, false, nil
	}

	env, err := d.resolveEnv(cs.Env, cs.SecretEnv, secrets)
	if err != nil {
		return "", false, err
	}
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
		Labels:     cs.Labels,
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

	binds := make([]string, 0, len(cs.Mounts))
	for _, m := range cs.Mounts {
		bind := m.Volume + ":" + m.Target
		if m.ReadOnly {
			bind += ":ro"
		}
		binds = append(binds, bind)
	}

	hostCfg := &container.HostConfig{
		Binds:         binds,
		RestartPolicy: restartPolicy(cs.Restart),
		Resources: container.Resources{
			NanoCPUs: int64(cs.CPUMillis) * 1_000_000, // millicpu → nanocpu
			Memory:   cs.MemoryBytes,
		},
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
	return h, nil
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
