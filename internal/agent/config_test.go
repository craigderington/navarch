package agent

import "testing"

func TestLoadConfigRequiresAgentToken(t *testing.T) {
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected an error when COMPOSECTL_AGENT_TOKEN is empty")
	}
}

func TestLoadConfigAcceptsAgentToken(t *testing.T) {
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "test-token")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AgentToken != "test-token" {
		t.Fatalf("got token %q", cfg.AgentToken)
	}
}
