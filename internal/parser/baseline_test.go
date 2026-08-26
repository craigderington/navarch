package parser

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/craig/composectl/internal/spec"
)

// The known-good baseline for examples/webapp. CLAUDE.md records this
// fingerprint and the reasoning behind it: a parser change that moves any of
// these numbers changed classification, and must either be justified against
// that reasoning or reverted. Without a test, the only thing that notices a
// drift is a human re-reading a demo's output — which is to say, nothing
// notices.
const (
	webappDigest          = "98d75411a605267292ea77e565309d6c7f60766b8c75c3ecc1e0404903b6e138"
	webappPeakMemoryBytes = 2281701376 // (512+256 MiB)*2 + 512 + 128 MiB
)

func parseWebapp(t *testing.T) *spec.DeploymentSpec {
	t.Helper()
	raw, err := os.ReadFile("../../examples/webapp/compose.yaml")
	if err != nil {
		t.Fatalf("read examples/webapp/compose.yaml: %v", err)
	}
	s, err := Parse(context.Background(), raw, "webapp-baseline")
	if err != nil {
		t.Fatalf("expected the example stack to parse, got: %v", err)
	}
	return s
}

func TestWebappBaselineClassification(t *testing.T) {
	s := parseWebapp(t)

	if got, err := s.Digest(); err != nil || got != webappDigest {
		t.Errorf("digest = %q (%v), want %q", got, err, webappDigest)
	}
	if got, want := s.SwappableServices(), []string{"api", "worker"}; !slices.Equal(got, want) {
		t.Errorf("swappable = %v, want %v", got, want)
	}
	if got, want := s.PinnedServices(), []string{"cache", "db"}; !slices.Equal(got, want) {
		t.Errorf("pinned = %v, want %v", got, want)
	}
	if ingress, ok := s.IngressService(); !ok || ingress != "api" {
		t.Errorf("ingress = %q (%v), want api", ingress, ok)
	}
	if got := s.PeakMemoryBytes(); got != webappPeakMemoryBytes {
		t.Errorf("peak memory = %d, want %d", got, webappPeakMemoryBytes)
	}
}

// Identical input must produce an identical digest: CreateStackVersion
// dedupes on it, so an unstable digest manufactures version churn and
// spurious redeploys. The parser sorts slice fields at parse time precisely
// to keep this true — a new unsorted slice field in DeploymentSpec shows up
// here first.
func TestWebappDigestIsStableAcrossParses(t *testing.T) {
	first, err := parseWebapp(t).Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := parseWebapp(t).Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first != second {
		t.Fatalf("digest varies run-to-run on an unchanged file: %q vs %q", first, second)
	}
}
