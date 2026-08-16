package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncWritesTraefikConfig(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	if err := r.Sync([]Route{
		{Key: "abc12345", Hostname: "prod.example.com", Target: "10.0.0.4", Port: 32770},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "composectl.yml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(b)
	for _, want := range []string{
		"Host(`prod.example.com`)",
		"http://10.0.0.4:32770",
		"entryPoints",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config missing %q:\n%s", want, out)
		}
	}
}

func TestSyncRejectsUnsafeHostname(t *testing.T) {
	dir := t.TempDir()
	err := New(dir).Sync([]Route{
		{Key: "abc12345", Hostname: "x.com`) || Host(`y.com", Target: "10.0.0.4", Port: 32770},
	})
	if err == nil {
		t.Fatal("expected unsafe hostname to be rejected")
	}
}

// An empty route set must produce a file with no http section. This test used
// to assert only that Sync returned nil and left a file behind, which is what
// let the real bug through: the file it wrote was `routers: {}`, which Traefik
// rejects outright ("routers cannot be a standalone element"). A rejected file
// leaves the previously accepted config in force, so the last environment's
// hostname kept routing after teardown — verified against traefik:v3.3, where
// a live route stayed at 200 across that write and only a section-less file
// dropped it to 404.
func TestSyncEmptyWritesNoHTTPSection(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir).Sync(nil); err != nil {
		t.Fatalf("empty sync: %v", err)
	}
	// ReadFile also carries the old assertion: a file must still exist when the
	// route set is empty, rather than the path being left absent.
	body, err := os.ReadFile(filepath.Join(dir, "composectl.yml"))
	if err != nil {
		t.Fatalf("expected a file even when empty: %v", err)
	}
	// Any of these means an empty element reached the file, and Traefik would
	// reject it — keeping stale routes alive rather than withdrawing them.
	for _, banned := range []string{"routers", "services", "http:"} {
		if strings.Contains(string(body), banned) {
			t.Fatalf("empty config must not contain %q, got:\n%s", banned, body)
		}
	}
}

// The target is interpolated into the generated config exactly as the hostname
// is, so it needs the same alphabet guard. A node address arrives from the
// database rather than from a request, but "this value came from somewhere
// trusted" is the assumption that stops being true first.
func TestSyncRejectsUnsafeTarget(t *testing.T) {
	dir := t.TempDir()
	err := New(dir).Sync([]Route{
		{Key: "abc12345", Hostname: "prod.example.com", Target: "10.0.0.4\nmalicious: true", Port: 80},
	})
	if err == nil {
		t.Fatal("expected an unsafe target to be rejected")
	}
}
