package dockerd

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/google/uuid"
)

type staticSecrets map[string]string

func (s staticSecrets) Get(k string) (string, bool) { v, ok := s[k]; return v, ok }

func testDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New("")
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	return d
}

func TestEnsureContainerCreatesAndAdopts(t *testing.T) {
	d := testDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env8 := "testdrv1"
	name := "cc-" + env8 + "-r1-blue-web"
	netName := "cc-" + env8 + "-r1-blue"
	labels := map[string]string{"cc.env": env8, "cc.service": "web", "cc.swappable": "true"}

	if err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	netID, err := d.EnsureNetwork(ctx, netName, map[string]string{"cc.env": env8})
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if netID == "" {
		t.Fatal("expected a network id")
	}

	cs := ContainerSpec{
		Name: name, Image: "busybox:latest",
		Cmd:    []string{"sh", "-c", "sleep 30"},
		Labels: labels, Network: netName, MemoryBytes: 64 << 20,
	}
	id, created, err := d.EnsureContainer(ctx, cs, nil)
	if err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	if !created {
		t.Fatal("expected the container to be created")
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.StopRemove(c, id)
		_ = d.removeNetwork(c, netName)
	})

	// Second call adopts the existing container rather than creating a new one.
	id2, created2, err := d.EnsureContainer(ctx, cs, nil)
	if err != nil {
		t.Fatalf("EnsureContainer (adopt): %v", err)
	}
	if created2 || id2 != id {
		t.Fatalf("expected adoption of %s, got id=%s created=%v", id, id2, created2)
	}

	managed, err := d.ListManaged(ctx, env8)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(managed) != 1 || managed[0].Service != "web" {
		t.Fatalf("expected one managed 'web' container, got %+v", managed)
	}

	h, err := d.InspectHealth(ctx, id)
	if err != nil {
		t.Fatalf("InspectHealth: %v", err)
	}
	if !h.Running {
		t.Fatalf("expected the container to be running, got %+v", h)
	}
}

func TestSecretExpansion(t *testing.T) {
	d, err := New("")
	if err != nil {
		t.Skipf("docker client init: %v", err)
	}
	env, err := d.resolveEnv(
		map[string]string{"LOG_LEVEL": "info"},
		map[string]string{"URL": "postgres://app:${secret:db_password}@db/app"},
		staticSecrets{"db_password": "s3cr3t"},
	)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["URL"] != "postgres://app:s3cr3t@db/app" {
		t.Fatalf("secret not expanded mid-string: %q", env["URL"])
	}
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("plain env lost: %q", env["LOG_LEVEL"])
	}
}

func TestSecretExpansionMissingKeyErrors(t *testing.T) {
	d, _ := New("")
	if _, err := d.resolveEnv(nil, map[string]string{"URL": "${secret:absent}"}, staticSecrets{}); err == nil {
		t.Fatal("expected an error for a missing secret")
	}
}

// A nil SecretSource is what a caller passes when no per-environment source
// applies (e.g. today's minimal agent wiring, before Task 7 lands the real
// one). It must behave exactly like an empty source: no refs, no problem;
// any ref is reported missing rather than panicking.
func TestSecretExpansionNilSourceTreatsReferencesAsMissing(t *testing.T) {
	d, _ := New("")
	if _, err := d.resolveEnv(nil, map[string]string{"URL": "${secret:absent}"}, nil); err == nil {
		t.Fatal("expected a nil SecretSource to report the referenced secret as missing")
	}
	env, err := d.resolveEnv(map[string]string{"LOG_LEVEL": "info"}, nil, nil)
	if err != nil {
		t.Fatalf("resolveEnv with no secret refs and a nil source must not error: %v", err)
	}
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("plain env lost: %q", env["LOG_LEVEL"])
	}
}

// TestEnsureVolumeAndRemoveEnv exercises all three object kinds RemoveEnv
// tears down — container, network, volume — so that a wrong not-found symbol
// or a changed error shape for any one of them would fail this test, not
// just silently no-op it. A volume-only version doesn't exercise container
// or network removal at all, since an empty ContainerList/NetworkList never
// reaches the errdefs.IsNotFound branch either.
func TestEnsureVolumeAndRemoveEnv(t *testing.T) {
	d := testDriver(t) // existing helper: skips loudly without a daemon
	ctx := context.Background()
	env8 := "test" + uuid.NewString()[:4]
	labels := map[string]string{"cc.env": env8}
	vol := "cc-" + env8 + "-data"
	netName := "cc-" + env8 + "-net"
	ctrName := "cc-" + env8 + "-c1"

	// Registered before anything is created, so a mid-test Fatalf still
	// cleans up: RemoveEnv is idempotent, so a best-effort call here is safe
	// whether the happy path already removed everything or not.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = d.RemoveEnv(cctx, env8)
	})

	if err := d.EnsureVolume(ctx, vol, labels); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	// Idempotent: reconcile calls this on every tick.
	if err := d.EnsureVolume(ctx, vol, labels); err != nil {
		t.Fatalf("EnsureVolume (second call): %v", err)
	}

	if _, err := d.EnsureNetwork(ctx, netName, labels); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	// busybox:latest is already pulled by TestEnsureContainerCreatesAndAdopts
	// in this file, and EnsureImage only pulls when absent — this does not
	// add a network dependency to the test path.
	if err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	cs := ContainerSpec{
		Name: ctrName, Image: "busybox:latest",
		Cmd:    []string{"sh", "-c", "sleep 30"},
		Labels: labels, Network: netName, MemoryBytes: 64 << 20,
	}
	if _, _, err := d.EnsureContainer(ctx, cs, nil); err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}

	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}

	f := filters.NewArgs(filters.Arg("label", "cc.env="+env8))
	vols, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(vols.Volumes) != 0 {
		t.Errorf("RemoveEnv must delete the env's volumes, %d left", len(vols.Volumes))
	}
	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	if len(containers) != 0 {
		t.Errorf("RemoveEnv must delete the env's containers, %d left", len(containers))
	}
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		t.Fatalf("NetworkList: %v", err)
	}
	if len(nets) != 0 {
		t.Errorf("RemoveEnv must delete the env's networks, %d left", len(nets))
	}

	// Idempotent: a tombstone is re-offered every tick for its whole retention
	// window, so removing an already-gone environment must not error.
	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Errorf("RemoveEnv must be idempotent: %v", err)
	}
}
