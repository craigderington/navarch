package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
		ID uuid.UUID `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/nodes/register", map[string]any{
		"org": cfg.Org, "hostname": cfg.Hostname, "advertise_addr": cfg.AdvertiseAddr,
		"cpu_millis": cfg.CPUMillis, "memory_bytes": cfg.MemoryBytes, "agent_version": "sprint2-a",
		"age_recipient": c.id.Recipient(),
	}, &out)
	return out.ID, err
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

	reports := rec.Reconcile(ctx, desired.Instances, sources, desired.TeardownEnvs)
	if len(reports) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/report",
			map[string]any{"instances": toReportDTO(reports)}, nil); err != nil {
			return err
		}
	}
	return c.do(ctx, http.MethodPost, "/v1/nodes/"+nodeID.String()+"/heartbeat",
		map[string]any{"alloc_cpu_millis": 0, "alloc_memory_bytes": 0}, nil)
}

func toReportDTO(reports []Report) []map[string]any {
	out := make([]map[string]any, len(reports))
	for i, r := range reports {
		out[i] = map[string]any{
			"instance_id": r.InstanceID, "state": string(r.State),
			"container_id": r.ContainerID, "health_status": r.HealthStatus,
			"last_error": r.LastError, "restart_count": r.RestartCount, "set_started": r.SetStarted,
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
