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

// The agent holds a node token and pulls every hosted environment's age
// ciphertext over this URL, so a plaintext one that can leave the container
// network must stop it starting rather than be a line in a log nobody reads.
func TestLoadConfigRefusesPlaintextItCannotContain(t *testing.T) {
	base := map[string]string{
		"COMPOSECTL_AGENT_TOKEN":    "t",
		"COMPOSECTL_ADVERTISE_ADDR": "127.0.0.1",
	}
	load := func(t *testing.T, url, insecure string) (Config, error) {
		t.Helper()
		for k, v := range base {
			t.Setenv(k, v)
		}
		t.Setenv("COMPOSECTL_CONTROLPLANE_URL", url)
		t.Setenv("COMPOSECTL_INSECURE", insecure)
		return LoadConfig()
	}

	// The dev stack's own URL is a compose service name and must keep working
	// with no opt-in, or this guard lands as a fleet outage.
	if cfg, err := load(t, "http://controlplane:8417", ""); err != nil {
		t.Fatalf("a compose service name must be allowed: %v", err)
	} else if cfg.InsecureTransport {
		t.Fatal("a contained URL must not be flagged insecure")
	}
	if _, err := load(t, "https://navarch.example.com", ""); err != nil {
		t.Fatalf("https must be allowed: %v", err)
	}

	// A LAN address is the case this exists for.
	if _, err := load(t, "http://10.0.1.7:8417", ""); err == nil {
		t.Fatal("plaintext to a private address must refuse to start")
	}

	// The opt-in works and is recorded, so Run can warn about it every start.
	cfg, err := load(t, "http://10.0.1.7:8417", "1")
	if err != nil {
		t.Fatalf("COMPOSECTL_INSECURE=1 should allow it: %v", err)
	}
	if !cfg.InsecureTransport {
		t.Fatal("an opted-in insecure URL must be recorded so the agent warns")
	}

	// The opt-in is scoped to that one judgement: it does not make an unusable
	// URL usable.
	if _, err := load(t, "ftp://controlplane:8417", "1"); err == nil {
		t.Fatal("the insecure opt-in must not rescue an unsupported scheme")
	}
}
