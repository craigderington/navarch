// Package config loads control plane configuration from the environment.
package config

import (
	"fmt"
	"os"
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
	// AgentToken authenticates node agents. Sprint 1 uses a shared token;
	// replace with per-node mTLS or signed JWTs before multi-tenant use.
	AgentToken string
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL: os.Getenv("COMPOSECTL_DATABASE_URL"),
		ListenAddr:  ListenAddr(),
		AgentToken:  os.Getenv("COMPOSECTL_AGENT_TOKEN"),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("COMPOSECTL_DATABASE_URL is required")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
