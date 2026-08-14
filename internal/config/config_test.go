package config

import "testing"

func TestLoadRequiresAgentToken(t *testing.T) {
	t.Setenv("COMPOSECTL_DATABASE_URL", "postgres://example")
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when COMPOSECTL_AGENT_TOKEN is empty")
	}
}

func TestLoadAcceptsAgentToken(t *testing.T) {
	t.Setenv("COMPOSECTL_DATABASE_URL", "postgres://example")
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "test-token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentToken != "test-token" {
		t.Fatalf("got token %q", cfg.AgentToken)
	}
}
