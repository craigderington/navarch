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

// The wildcard is off unless asked for. An install that quietly grew a
// DNS-01 resolver would be an install that quietly needed a DNS credential,
// and the whole reason HTTP-01 was chosen is that it needs none.
func TestWildcardResolverIsOffByDefault(t *testing.T) {
	t.Setenv("COMPOSECTL_DATABASE_URL", "postgres://example")
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "test-token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RouterWildcardResolver != "" {
		t.Fatalf("wildcard must be opt-in, got %q", cfg.RouterWildcardResolver)
	}
}

// And when it is asked for, the domain it covers comes from the preview domain
// rather than a setting of its own — the coupling is the safety property, so a
// test that let them drift apart would be testing the wrong thing.
func TestWildcardCoversThePreviewDomain(t *testing.T) {
	t.Setenv("COMPOSECTL_DATABASE_URL", "postgres://example")
	t.Setenv("COMPOSECTL_AGENT_TOKEN", "test-token")
	t.Setenv("COMPOSECTL_PREVIEW_DOMAIN", "preview.navar.ch")
	t.Setenv("COMPOSECTL_ROUTER_WILDCARD_RESOLVER", "lewild")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RouterWildcardResolver != "lewild" {
		t.Fatalf("got resolver %q", cfg.RouterWildcardResolver)
	}
	if cfg.PreviewDomain != "preview.navar.ch" {
		t.Fatalf("got preview domain %q", cfg.PreviewDomain)
	}
}
