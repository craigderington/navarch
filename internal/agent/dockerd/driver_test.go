package dockerd

import (
	"context"
	"strings"
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
	// A prior test process may have been interrupted before Cleanup ran. Start
	// from an empty, test-owned label namespace so reruns adopt no stale object.
	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Fatalf("remove stale test environment: %v", err)
	}

	if _, err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
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

	changed := cs
	changed.Cmd = []string{"sh", "-c", "sleep 60"}
	if _, _, err := d.EnsureContainer(ctx, changed, nil); err == nil {
		t.Fatal("expected changed runtime configuration to reject adoption")
	}

	obsoleteNetwork := "cc-" + env8 + "-r0-green"
	if _, err := d.EnsureNetwork(ctx, obsoleteNetwork, map[string]string{"cc.env": env8}); err != nil {
		t.Fatalf("create obsolete network: %v", err)
	}
	if err := d.AttachNetwork(ctx, id, obsoleteNetwork); err != nil {
		t.Fatalf("attach obsolete network: %v", err)
	}
	if err := d.PruneRevisionNetworks(ctx, env8, map[string]bool{netName: true}); err != nil {
		t.Fatalf("PruneRevisionNetworks: %v", err)
	}
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", obsoleteNetwork)),
	})
	if err != nil {
		t.Fatalf("list obsolete network: %v", err)
	}
	for _, n := range nets {
		if n.Name == obsoleteNetwork {
			t.Fatalf("obsolete network %s was not removed", obsoleteNetwork)
		}
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

func TestContainerFingerprintIncludesResolvedSecrets(t *testing.T) {
	cs := ContainerSpec{Image: "app:1", SecretEnv: map[string]string{"PASSWORD": "${secret:password}"}}
	a, err := containerFingerprint(cs, map[string]string{"PASSWORD": "old"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := containerFingerprint(cs, map[string]string{"PASSWORD": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("secret rotation must change the runtime fingerprint")
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
	if _, err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
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

// The shared router joins a revision network to serve that environment's
// ingress and carries no cc.env label, so RemoveEnv's container sweep never
// removes it. Docker refuses to delete a network with an active endpoint, so
// the network removal failed, and because volumes are removed *after*
// networks, the environment's durable state survived the teardown entirely.
// Every preview has an ingress, so every preview leaked its volume.
func TestRemoveEnvDisconnectsUnmanagedContainersFromEnvNetworks(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	env8 := "test" + uuid.NewString()[:4]
	labels := map[string]string{"cc.env": env8}
	vol := "cc-" + env8 + "-data"
	netName := "cc-" + env8 + "-r1-blue"
	routerName := "router-" + env8 // deliberately unlabelled: not ours to delete

	var routerID string
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if routerID != "" {
			_ = d.StopRemove(cctx, routerID)
		}
		_ = d.RemoveEnv(cctx, env8)
	})

	if err := d.EnsureVolume(ctx, vol, labels); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if _, err := d.EnsureNetwork(ctx, netName, labels); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if _, err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	// Stands in for Traefik: attached to the revision network, no cc.env label.
	id, _, err := d.EnsureContainer(ctx, ContainerSpec{
		Name: routerName, Image: "busybox:latest",
		Cmd:         []string{"sh", "-c", "sleep 60"},
		Labels:      map[string]string{"cc.role": "ingress-router"},
		Network:     netName,
		MemoryBytes: 64 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("EnsureContainer (router): %v", err)
	}
	routerID = id

	if err := d.RemoveEnv(ctx, env8); err != nil {
		t.Fatalf("RemoveEnv must not fail with an unmanaged container attached: %v", err)
	}

	f := filters.NewArgs(filters.Arg("label", "cc.env="+env8))
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		t.Fatalf("NetworkList: %v", err)
	}
	if len(nets) != 0 {
		t.Errorf("the env's networks must be removed, %d left", len(nets))
	}
	// The point of the fix: the volume step is reached at all.
	vols, err := d.cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(vols.Volumes) != 0 {
		t.Errorf("the env's volumes must be removed, %d left", len(vols.Volumes))
	}

	// Disconnected, not destroyed. The router serves other environments; a
	// tombstone authorises destroying its own environment, nothing else.
	if _, err := d.cli.ContainerInspect(ctx, routerID); err != nil {
		t.Errorf("the unmanaged router must survive teardown: %v", err)
	}
}

// A superseded revision's network keeps a router endpoint: the agent attached
// the router while that revision was live and nothing detaches it when the
// revision is torn down. The router carries no cc.env label, so the
// unmanaged-container guard used to refuse the prune outright and the network
// survived for the life of the environment — one per revision, each holding an
// address block, until the pool ran out and rollouts could no longer create a
// network. Verified non-vacuous: without the isIngressRouter exemption this
// fails with "refusing to prune network ...: unmanaged container ... attached".
func TestPruneRevisionNetworksDisconnectsTheIngressRouter(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	env8 := "test" + uuid.NewString()[:4]
	labels := map[string]string{"cc.env": env8}
	oldNet := "cc-" + env8 + "-r1-blue"   // superseded — must be pruned
	liveNet := "cc-" + env8 + "-r2-green" // still desired — must survive

	var routerID string
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if routerID != "" {
			_ = d.StopRemove(cctx, routerID)
		}
		_ = d.RemoveEnv(cctx, env8)
	})

	for _, n := range []string{oldNet, liveNet} {
		if _, err := d.EnsureNetwork(ctx, n, labels); err != nil {
			t.Fatalf("EnsureNetwork %s: %v", n, err)
		}
	}
	if _, err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	id, _, err := d.EnsureContainer(ctx, ContainerSpec{
		Name: "router-" + env8, Image: "busybox:latest",
		Cmd:         []string{"sh", "-c", "sleep 60"},
		Labels:      map[string]string{"cc.role": "ingress-router"},
		Network:     oldNet,
		MemoryBytes: 64 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("EnsureContainer (router): %v", err)
	}
	routerID = id
	// The router is on both, exactly as it is after a flip.
	if err := d.AttachNetwork(ctx, routerID, liveNet); err != nil {
		t.Fatalf("AttachNetwork: %v", err)
	}

	if err := d.PruneRevisionNetworks(ctx, env8, map[string]bool{liveNet: true}); err != nil {
		t.Fatalf("prune must not be blocked by the router's own endpoint: %v", err)
	}

	nets, err := d.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "cc.env="+env8)),
	})
	if err != nil {
		t.Fatalf("NetworkList: %v", err)
	}
	if len(nets) != 1 || nets[0].Name != liveNet {
		var got []string
		for _, n := range nets {
			got = append(got, n.Name)
		}
		t.Fatalf("only the wanted network may survive, got %v", got)
	}

	// Disconnected from the obsolete network, still serving the live one.
	ctr, err := d.cli.ContainerInspect(ctx, routerID)
	if err != nil {
		t.Fatalf("the router must survive a prune: %v", err)
	}
	if _, stillOn := ctr.NetworkSettings.Networks[oldNet]; stillOn {
		t.Errorf("router should have been disconnected from %s", oldNet)
	}
	if _, onLive := ctr.NetworkSettings.Networks[liveNet]; !onLive {
		t.Errorf("router must remain attached to the live network %s", liveNet)
	}
}

// The exemption is for the platform's own router only. A container that is
// neither managed for this env nor the router still blocks the prune, because
// routine GC runs constantly and must never yank something off a network it
// may be using — that restraint is what separates this path from RemoveEnv.
func TestPruneRevisionNetworksStillRefusesForeignContainers(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	env8 := "test" + uuid.NewString()[:4]
	netName := "cc-" + env8 + "-r1-blue"

	var foreignID string
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if foreignID != "" {
			_ = d.StopRemove(cctx, foreignID)
		}
		_ = d.RemoveEnv(cctx, env8)
	})

	if _, err := d.EnsureNetwork(ctx, netName, map[string]string{"cc.env": env8}); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if _, err := d.EnsureImage(ctx, "busybox:latest"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	id, _, err := d.EnsureContainer(ctx, ContainerSpec{
		Name: "foreign-" + env8, Image: "busybox:latest",
		Cmd:         []string{"sh", "-c", "sleep 60"},
		Labels:      map[string]string{"someone": "else"},
		Network:     netName,
		MemoryBytes: 64 << 20,
	}, nil)
	if err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	foreignID = id

	err = d.PruneRevisionNetworks(ctx, env8, map[string]bool{})
	if err == nil {
		t.Fatal("prune must refuse while a foreign container is attached")
	}
	if !strings.Contains(err.Error(), "refusing to prune") {
		t.Fatalf("unexpected error: %v", err)
	}
}
