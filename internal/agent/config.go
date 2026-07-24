package agent

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

// LoadConfig reads the node agent's configuration from the environment.
//
// It lives in package agent, NOT internal/config: if it lived there, the
// control plane (which imports internal/config) would drag the Docker SDK in
// transitively via agent → dockerd. Keeping it here means only the agent
// binary links the SDK.
func LoadConfig() (Config, error) {
	host, _ := os.Hostname()
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
		IdentityFile:    envOr("COMPOSECTL_AGE_IDENTITY_FILE", "/identity/age.key"),
	}
	if cfg.ControlPlaneURL == "" {
		return Config{}, fmt.Errorf("COMPOSECTL_CONTROLPLANE_URL is required")
	}
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
