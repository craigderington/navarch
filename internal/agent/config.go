package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/craigderington/navarch/internal/transport"
)

// LoadConfig reads the node agent's configuration from the environment.
//
// It lives in package agent, NOT internal/config: if it lived there, the
// control plane (which imports internal/config) would drag the Docker SDK in
// transitively via agent → dockerd. Keeping it here means only the agent
// binary links the SDK.
func LoadConfig() (Config, error) {
	host, _ := os.Hostname()
	identity := envOr("COMPOSECTL_AGE_IDENTITY_FILE", "/identity/age.key")
	cfg := Config{
		ControlPlaneURL: envOr("COMPOSECTL_CONTROLPLANE_URL", "http://controlplane:8417"),
		Org:             envOr("COMPOSECTL_ORG", "dev"),
		Hostname:        envOr("COMPOSECTL_NODE_HOSTNAME", host),
		AdvertiseAddr:   envOr("COMPOSECTL_ADVERTISE_ADDR", "127.0.0.1"),
		AgentToken:      os.Getenv("COMPOSECTL_AGENT_TOKEN"),
		DockerHost:      os.Getenv("DOCKER_HOST"),
		CPUMillis:       intEnv("COMPOSECTL_NODE_CPU_MILLIS", runtime.NumCPU()*1000),
		MemoryBytes:     int64(intEnv("COMPOSECTL_NODE_MEMORY_MB", 8192)) << 20,
		PollInterval:    time.Duration(intEnv("COMPOSECTL_POLL_SECONDS", 2)) * time.Second,
		IdentityFile:    identity,
		NodeTokenFile:   envOr("COMPOSECTL_NODE_TOKEN_FILE", filepath.Join(filepath.Dir(identity), "node.token")),
		Labels:          parseLabels(os.Getenv("COMPOSECTL_NODE_LABELS")),
	}
	if cfg.ControlPlaneURL == "" {
		return Config{}, fmt.Errorf("COMPOSECTL_CONTROLPLANE_URL is required")
	}
	if cfg.AgentToken == "" {
		return Config{}, fmt.Errorf("COMPOSECTL_AGENT_TOKEN is required")
	}
	// The agent carries a node token and pulls age ciphertext over this URL, so
	// where it points is a security decision, not a connectivity detail.
	if err := transport.CheckBaseURL(cfg.ControlPlaneURL); err != nil {
		var insecure *transport.InsecureError
		if !errors.As(err, &insecure) {
			return Config{}, err
		}
		if !transport.Insecure(os.Getenv("COMPOSECTL_INSECURE")) {
			return Config{}, fmt.Errorf("%w\n\nSet COMPOSECTL_INSECURE=1 to proceed anyway", err)
		}
		cfg.InsecureTransport = true
	}
	addr, err := resolveAdvertiseAddr(cfg.AdvertiseAddr)
	if err != nil {
		return Config{}, err
	}
	cfg.AdvertiseAddr = addr
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// parseLabels reads COMPOSECTL_NODE_LABELS in the form "k=v,k2=v2".
//
// Malformed entries are dropped rather than rejected: a node label is
// advertisement, and the scheduler's job is to place against what a node claims
// it can do. A typo therefore costs that capability — the node is simply not
// chosen for work needing it — which is a failure the scheduler reports, rather
// than an agent that refuses to start and takes its capacity with it.
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveAdvertiseAddr turns the configured advertise address into an IP.
//
// The column is INET, and the value has to be an address the control plane can
// hand to the router as a connect target — but the most convenient thing to
// configure is often a name: a container's service name on a compose network, a
// node's DNS record. Resolving here keeps the schema honest without making the
// operator look up an address that something else already knows.
//
// A value that is already an IP literal is returned untouched, and a name that
// does not resolve is an error rather than a fallback: registering an address
// this node is not reachable at produces routes that point at nothing, which
// looks like a routing bug anywhere except here.
func resolveAdvertiseAddr(addr string) (string, error) {
	if addr == "" {
		return "", nil
	}
	if net.ParseIP(addr) != nil {
		return addr, nil
	}
	ips, err := net.LookupIP(addr)
	if err != nil {
		return "", fmt.Errorf("resolve advertise address %q: %w", addr, err)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	if len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", fmt.Errorf("advertise address %q resolved to nothing", addr)
}
