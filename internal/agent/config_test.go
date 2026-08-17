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

func TestParseLabels(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{name: "empty is nil, not an empty map", in: "", want: nil},
		{name: "single pair", in: "ingress=true", want: map[string]string{"ingress": "true"}},
		{name: "several pairs, spaces trimmed", in: " ingress=true , zone = eu ",
			want: map[string]string{"ingress": "true", "zone": "eu"}},
		{name: "an empty value is still a claim", in: "ingress=", want: map[string]string{"ingress": ""}},
		// A malformed entry costs that one capability rather than the agent's
		// whole registration: a node that fails to start takes its capacity
		// with it, while a missing label is a placement the scheduler explains.
		{name: "entries without = are dropped", in: "ingress", want: nil},
		{name: "a bad entry does not discard the good ones", in: "junk,zone=eu",
			want: map[string]string{"zone": "eu"}},
		{name: "empty keys are dropped", in: "=value", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLabels(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("parseLabels(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("parseLabels(%q)[%q] = %q, want %q", tt.in, k, got[k], v)
				}
			}
		})
	}
}

// The advertise address becomes the router's connect target, so a node that
// registers an address it is not reachable at produces routes pointing at
// nothing — a failure that looks like a routing bug everywhere except here.
func TestResolveAdvertiseAddr(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "an IPv4 literal is returned untouched", in: "10.201.0.4", want: "10.201.0.4"},
		{name: "an IPv6 literal is returned untouched", in: "::1", want: "::1"},
		// localhost is the one name guaranteed resolvable without a network.
		{name: "a name resolves to an address", in: "localhost", want: "127.0.0.1"},
		// Failing loudly matters more than falling back: the column is INET, and
		// a fallback would register an address that silently routes nowhere.
		{name: "an unresolvable name is an error", in: "no-such-host.invalid", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAdvertiseAddr(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveAdvertiseAddr(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAdvertiseAddr(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("resolveAdvertiseAddr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
