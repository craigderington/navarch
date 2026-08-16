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

	"github.com/craig/composectl/internal/agent/dockerd"
	"github.com/craig/composectl/internal/secrets"
	"github.com/craig/composectl/internal/store"
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
}

// Run registers this node and then reconciles on a ticker until ctx is done.
// The agent speaks only HTTP to the control plane — it never touches Postgres,
// preserving the store's exclusive ownership of pgx across binaries.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
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

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := c.reconcileTick(ctx, nodeID, rec, log); err != nil {
			log.Warn("reconcile tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
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
		"cpu_millis": cfg.CPUMillis, "memory_bytes": cfg.MemoryBytes, "agent_version": "sprint2-a",
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
	if len(reports) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/report",
			map[string]any{"instances": toReportDTO(reports)}, nil); err != nil {
			return err
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
