package dockerd

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

// firstPublishedPort reads back what Docker allocated. Docker lists a binding
// per address family, so the IPv4 one must win deterministically — picking
// whichever came first would make the reported port depend on map iteration
// order, and the router would occasionally be handed the wrong one.
func TestFirstPublishedPort(t *testing.T) {
	p80 := nat.Port("80/tcp")
	tests := []struct {
		name  string
		ports nat.PortMap
		want  int
	}{
		{name: "nothing published", ports: nat.PortMap{}, want: 0},
		{name: "exposed but unbound", ports: nat.PortMap{p80: nil}, want: 0},
		{
			name:  "single ipv4 binding",
			ports: nat.PortMap{p80: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "32768"}}},
			want:  32768,
		},
		{
			// Both families carry the same host port, but only one ordering of
			// the slice puts IPv4 first — the result must not depend on it.
			name: "ipv6 listed before ipv4",
			ports: nat.PortMap{p80: []nat.PortBinding{
				{HostIP: "::", HostPort: "32769"},
				{HostIP: "0.0.0.0", HostPort: "32769"},
			}},
			want: 32769,
		},
		{
			name: "ipv6 only is still accepted",
			ports: nat.PortMap{p80: []nat.PortBinding{
				{HostIP: "::", HostPort: "32770"},
			}},
			want: 32770,
		},
		{
			name:  "an unparseable host port is not a port",
			ports: nat.PortMap{p80: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: ""}}},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstPublishedPort(tt.ports); got != tt.want {
				t.Fatalf("firstPublishedPort(%v) = %d, want %d", tt.ports, got, tt.want)
			}
		})
	}
}

// publishedPort reads the create-time bindings rather than the runtime ones,
// because adoption also picks up stopped containers — which report no active
// ports however they were created. Reading the wrong one would make every
// stopped ingress container look like it needed replacing.
func TestPublishedPortReadsHostConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *container.HostConfig
		want int
	}{
		{name: "nil host config", cfg: nil, want: 0},
		{name: "no bindings", cfg: &container.HostConfig{}, want: 0},
		{
			name: "bound",
			cfg: &container.HostConfig{PortBindings: nat.PortMap{
				nat.Port("80/tcp"): []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}},
			}},
			want: 80,
		},
		{
			// An entry with no bindings is an exposed-but-unpublished port; it
			// must not read as a publish or an adopted container would be
			// replaced on every tick.
			name: "entry with empty bindings",
			cfg: &container.HostConfig{PortBindings: nat.PortMap{
				nat.Port("5432/tcp"): {},
			}},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publishedPort(tt.cfg); got != tt.want {
				t.Fatalf("publishedPort = %d, want %d", got, tt.want)
			}
		})
	}
}
