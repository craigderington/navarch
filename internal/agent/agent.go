package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/craigderington/navarch/internal/agent/dockerd"
	"github.com/craigderington/navarch/internal/router"
	"github.com/craigderington/navarch/internal/secrets"
	"github.com/craigderington/navarch/internal/store"
	"github.com/craigderington/navarch/internal/version"
)

type Config struct {
	ControlPlaneURL string
	Org             string
	Hostname        string
	AdvertiseAddr   string
	AgentToken      string
	DockerHost      string
	CPUMillis       int
	MemoryBytes     int64
	PollInterval    time.Duration
	IdentityFile    string
	NodeTokenFile   string
	// Labels advertise what this node can do, and the scheduler places against
	// them. `ingress=true` marks a node running the platform's router: until the
	// mesh lands, a stack with an ingress service is only servable there.
	Labels map[string]string
	// RouterDir, when set, makes this agent write router config for a router
	// running beside it — the bring-your-own-infrastructure shape, where the
	// customer owns ingress and the control plane never connects to them.
	RouterDir string
	// RouterCertResolver names an ACME resolver in the *customer's own* router
	// static config, and makes every route this agent writes an HTTPS router
	// using it. Without it a BYO node serves its tenant hostnames over plain
	// HTTP while the platform's own router has served HTTPS since tenant TLS
	// landed — the customer-side path is the one place that asymmetry can hide,
	// because nothing the control plane can see reports it.
	//
	// It is read from the agent's environment rather than delivered by the
	// control plane, and that is the boundary rather than an omission: a
	// resolver name is Traefik's vocabulary, and GET /v1/nodes/{id}/routes
	// deliberately answers in the platform's (hostname, target, port) so a
	// different router stays a change in one package. The name also has to
	// match a `--certificatesresolvers.<name>.acme...` flag the customer wrote,
	// which the control plane has never seen. Both live in their compose file,
	// where one person can see them together.
	RouterCertResolver string
	// InsecureTransport records that the control-plane URL is plaintext to
	// somewhere it could be read, and that COMPOSECTL_INSECURE allowed it
	// anyway. Kept as a field rather than re-derived so Run warns about exactly
	// what LoadConfig decided.
	InsecureTransport bool
}

// Run registers this node and then reconciles on a ticker until ctx is done.
// The agent speaks only HTTP to the control plane — it never touches Postgres,
// preserving the store's exclusive ownership of pgx across binaries.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.InsecureTransport {
		// Every start, at warn level. A node token is a long-lived credential
		// and this agent pulls age ciphertext for every environment it hosts;
		// anyone on the path gets both for as long as the token is valid.
		log.Warn("control plane URL is plaintext HTTP and not loopback or a container network — "+
			"the node token and every secret's ciphertext cross it unencrypted",
			"url", cfg.ControlPlaneURL, "override", "COMPOSECTL_INSECURE")
	}
	drv, err := dockerd.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	rec := NewReconciler(drv)

	// The identity persists across restarts (LoadOrGenerateIdentity writes it to
	// disk on first run) — a fresh one on every restart couldn't decrypt secrets
	// already sealed to the old recipient.
	id, err := secrets.LoadOrGenerateIdentity(cfg.IdentityFile)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}

	c := &cpClient{base: cfg.ControlPlaneURL, token: cfg.AgentToken, http: &http.Client{Timeout: 30 * time.Second}, id: id}

	nodeID, err := c.register(ctx, cfg)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	log.Info("agent registered", "node", nodeID, "org", cfg.Org)

	// Router mode: with a directory configured, this agent also writes the
	// config for a router running beside it. That is what makes
	// bring-your-own-infrastructure possible — the control plane's own router
	// cannot reach a node behind NAT, but a router on the same machine can, and
	// the agent is already polling with a credential.
	rtr := newRouter(cfg)
	if rtr != nil {
		log.Info("router mode enabled", "dir", cfg.RouterDir, "certResolver", cfg.RouterCertResolver)
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := c.reconcileTick(ctx, nodeID, rec, log); err != nil {
			log.Warn("reconcile tick failed", "err", err)
		}
		if rtr != nil {
			// Failing to write routes must not stop reconciliation: containers
			// coming up matters more than the router catching up, and the next
			// tick retries a second later.
			if err := c.syncRoutes(ctx, nodeID, rtr); err != nil {
				log.Warn("route sync failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// newRouter builds the router for router mode, or nil when it is off.
//
// Split out of Run so the wiring is reachable from a test: Run needs a Docker
// daemon and a registered node, so the one line that decides whether a BYO
// customer's tenants get HTTPS would otherwise be exercised by nothing.
func newRouter(cfg Config) *router.Router {
	if cfg.RouterDir == "" {
		return nil
	}
	var opts []router.Option
	if cfg.RouterCertResolver != "" {
		opts = append(opts, router.WithCertResolver(cfg.RouterCertResolver))
	}
	return router.New(cfg.RouterDir, opts...)
}

type cpClient struct {
	base  string
	token string
	http  *http.Client
	id    secrets.Identity
}

func (c *cpClient) register(ctx context.Context, cfg Config) (uuid.UUID, error) {
	var out struct {
		ID    uuid.UUID `json:"id"`
		Token string    `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/nodes/register", map[string]any{
		"org": cfg.Org, "hostname": cfg.Hostname, "advertise_addr": cfg.AdvertiseAddr,
		"cpu_millis": cfg.CPUMillis, "memory_bytes": cfg.MemoryBytes, "agent_version": version.Version,
		"age_recipient": c.id.Recipient(), "labels": cfg.Labels,
	}, &out)
	if err != nil {
		return uuid.Nil, err
	}
	if out.Token != "" {
		if err := persistNodeToken(cfg.NodeTokenFile, out.Token); err != nil {
			return uuid.Nil, fmt.Errorf("persist node token: %w", err)
		}
		c.token = out.Token
	} else if saved := loadNodeToken(cfg.NodeTokenFile); saved != "" {
		c.token = saved
	}
	return out.ID, nil
}

func (c *cpClient) reconcileTick(ctx context.Context, nodeID uuid.UUID, rec *Reconciler, log *slog.Logger) error {
	var desired struct {
		Instances    []store.DesiredInstance            `json:"instances"`
		Secrets      map[string][]store.EncryptedSecret `json:"secrets"`
		TeardownEnvs []string                           `json:"teardown_envs"`
		LogRequests  []store.PendingLogRequest          `json:"log_requests"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+nodeID.String()+"/desired-state", nil, &desired); err != nil {
		return err
	}

	// Decrypt happens here, agent-side, per environment — plaintext never
	// crosses back to the control plane. A single bad ciphertext (e.g. sealed
	// to a stale recipient after identity rotation) must not stall every other
	// env's containers, so a decrypt failure is logged and that key is skipped
	// rather than aborting the tick.
	sources := map[string]dockerd.SecretSource{}
	for env8, list := range desired.Secrets {
		m := EnvSecrets{}
		for _, es := range list {
			v, err := c.id.Decrypt(es.Ciphertext)
			if err != nil {
				log.Warn("secret decrypt failed", "env", env8, "key", es.Key, "err", err)
				continue
			}
			m[es.Key] = v
		}
		sources[env8] = m
	}

	reports, failures := rec.Reconcile(ctx, desired.Instances, sources, desired.TeardownEnvs)
	// Mirrors the secret-decrypt handling above: one environment's failed
	// teardown or cleanup (permissions, a volume still in use by something
	// unmanaged) must not stall the tick or hide the problem — it is logged and
	// left for the next tick, which reattempts both unconditionally.
	for _, f := range failures {
		log.Warn("env cleanup failed", "env", f.Env8, "op", f.Op, "err", f.Err)
	}
	for _, rep := range reports {
		if rep.Recreated {
			log.Warn("container replaced to correct its published port",
				"instance", rep.InstanceID, "container", rep.ContainerID)
		}
	}
	if len(reports) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/report",
			map[string]any{"instances": toReportDTO(reports)}, nil); err != nil {
			return err
		}
	}

	// Logs are delivered after the instance report, not before: reconciling is
	// what the node is for, and a slow or enormous log read must never delay the
	// state the controller needs to advance a rollout.
	if deliveries := rec.CollectLogs(ctx, desired.LogRequests); len(deliveries) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/logs",
			map[string]any{"deliveries": toLogDTO(deliveries)}, nil); err != nil {
			// Deliberately not fatal to the tick. The request stays pending and
			// the next tick retries it, whereas returning here would skip the
			// heartbeat below and eventually mark a perfectly healthy node
			// unreachable because somebody was tailing a log.
			log.Warn("log delivery failed", "err", err)
		}
	}
	allocCPU, allocMemory := desiredAllocations(desired.Instances)
	return c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/heartbeat",
		map[string]any{"alloc_cpu_millis": allocCPU, "alloc_memory_bytes": allocMemory}, nil)
}

// desiredAllocations reports the resources reserved by unique containers in
// desired state. Pinned services have one stable name shared by two revisions
// during blue/green, so they must be counted once rather than once per row.
func desiredAllocations(desired []store.DesiredInstance) (cpuMillis int, memoryBytes int64) {
	seen := make(map[string]bool, len(desired))
	for _, di := range desired {
		name := containerName(di)
		if seen[name] {
			continue
		}
		seen[name] = true
		cpuMillis += di.Service.Limits.CPUMillis
		memoryBytes += di.Service.Limits.MemoryBytes
	}
	return cpuMillis, memoryBytes
}

func persistNodeToken(path, token string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func loadNodeToken(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func toReportDTO(reports []Report) []map[string]any {
	out := make([]map[string]any, len(reports))
	for i, r := range reports {
		out[i] = map[string]any{
			"instance_id": r.InstanceID, "state": string(r.State),
			"container_id": r.ContainerID, "health_status": r.HealthStatus,
			"last_error": r.LastError, "restart_count": r.RestartCount, "set_started": r.SetStarted,
			"ingress_port": r.IngressPort,
		}
	}
	return out
}

func toLogDTO(ds []LogDelivery) []map[string]any {
	out := make([]map[string]any, len(ds))
	for i, d := range ds {
		out[i] = map[string]any{
			"request_id": d.RequestID, "data": d.Data, "error": d.Err,
		}
	}
	return out
}

func (c *cpClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// syncRoutes fetches this node's organization's routes and writes the router
// config for a router running beside this agent.
//
// The control plane answers in its own vocabulary — hostname, target, port —
// and internal/router turns that into Traefik's shape. Keeping the translation
// here rather than in the API is what lets a different router be a change in
// one package instead of a change to a published contract.
func (c *cpClient) syncRoutes(ctx context.Context, nodeID uuid.UUID, rtr *router.Router) error {
	var out struct {
		Routes []struct {
			Key      string `json:"key"`
			Hostname string `json:"hostname"`
			Target   string `json:"target"`
			Port     int    `json:"port"`
		} `json:"routes"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/nodes/"+nodeID.String()+"/routes", nil, &out); err != nil {
		return err
	}
	rr := make([]router.Route, 0, len(out.Routes))
	for _, r := range out.Routes {
		rr = append(rr, router.Route{
			Key: r.Key, Hostname: r.Hostname, Target: r.Target, Port: r.Port,
		})
	}
	return rtr.Sync(rr)
}
