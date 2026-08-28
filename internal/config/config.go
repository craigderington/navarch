// Package config loads control plane configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// DefaultListenAddr is the API bind address. Deliberately a high port —
// 3000/5000/8000/9000 are already crowded on a dev box.
const DefaultListenAddr = "0.0.0.0:8417"

// ListenAddr resolves the bind address without requiring the rest of the
// config. The -healthcheck probe runs in a container that has a listen
// address but no reason to hold a database URL, so it reads this alone.
func ListenAddr() string {
	return envOr("COMPOSECTL_LISTEN_ADDR", DefaultListenAddr)
}

type Config struct {
	DatabaseURL string
	ListenAddr  string
	// AgentToken is the shared bearer token. It no longer protects the whole
	// API: operator routes need an operator token and agent endpoints need
	// that node's own token, so this now opens exactly two machine-to-machine
	// paths — POST /v1/nodes/register and GET /metrics.
	//
	// It stays required because a node has no identity of its own until it has
	// registered, and a control plane that cannot enrol nodes is not a control
	// plane. See internal/api/identity.go for the full reasoning.
	AgentToken string
	// TickInterval paces the scheduler and rollout controller loops.
	TickInterval time.Duration
	// RouterDir is where Traefik dynamic config is written. Empty disables
	// routing (the controller runs without a router), which keeps the control
	// plane usable without Traefik in the stack.
	RouterDir string
	// PreviewDomain is the wildcard domain preview hostnames are generated under
	// (pr-142-hello.<domain>). The default resolves on a dev box without DNS
	// because Traefik routes on the Host header alone.
	PreviewDomain string
	// RouteStrandSeconds is how long a node may go unheard from before its
	// environments' routes are withdrawn. Deliberately separate from the 30s
	// staleness window that stops the scheduler placing new work: that decision
	// is cheap and reversible, while cutting live traffic is neither, and a
	// threshold chosen for one should not silently govern the other.
	//
	// The trade is real in both directions. With whole-stack placement there is
	// exactly one copy of an environment, so withdrawing the route of a node
	// that is unreachable-but-serving turns a working service into a 404. Keeping
	// it means every request to a genuinely dead node hangs until it times out —
	// and a timeout is the least diagnosable failure there is. Withdrawal wins
	// because a fast 404 can be explained and a hang cannot, but it waits four
	// times as long as the scheduler does before deciding.
	//
	// Zero means never withdraw, which is a legitimate operator preference: some
	// would rather have a hang than a 404. It is not the default because the
	// default should be the one that is easier to diagnose.
	RouteStrandSeconds int
	// BootstrapOperatorEmail creates the first operator at startup, so a fresh
	// install has somebody who can log in. Empty disables it, which is correct
	// once the install has operators of its own.
	//
	// From the environment rather than a seeded migration for the reason
	// POST /v1/orgs is self-serve: a UUID or token baked into a migration is
	// permanent and identical on every install.
	BootstrapOperatorEmail string
	// BootstrapOperatorToken pins that operator's token instead of generating
	// one. This is a dev-stack affordance — compose, the Makefile and the demo
	// scripts share a constant rather than scraping a generated value out of a
	// log line on every `make up`. Leave it empty anywhere real and the token
	// is minted from crypto/rand and logged once.
	BootstrapOperatorToken string
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:        os.Getenv("COMPOSECTL_DATABASE_URL"),
		ListenAddr:         ListenAddr(),
		AgentToken:         os.Getenv("COMPOSECTL_AGENT_TOKEN"),
		TickInterval:       time.Duration(intEnv("COMPOSECTL_TICK_SECONDS", 1)) * time.Second,
		RouterDir:          os.Getenv("COMPOSECTL_ROUTER_DIR"),
		PreviewDomain:      envOr("COMPOSECTL_PREVIEW_DOMAIN", "preview.localhost"),
		RouteStrandSeconds: intEnv("COMPOSECTL_ROUTE_STRAND_SECONDS", 120),

		BootstrapOperatorEmail: os.Getenv("COMPOSECTL_BOOTSTRAP_OPERATOR_EMAIL"),
		BootstrapOperatorToken: os.Getenv("COMPOSECTL_BOOTSTRAP_OPERATOR_TOKEN"),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("COMPOSECTL_DATABASE_URL is required")
	}
	if c.AgentToken == "" {
		return nil, fmt.Errorf("COMPOSECTL_AGENT_TOKEN is required")
	}
	return c, nil
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
