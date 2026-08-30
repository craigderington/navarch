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

// With no resolver the generated config must be byte-identical to what it
// always was. Every existing install runs this way, and a stray `tls:` key
// would fail the whole file — Traefik refuses an element with no children, the
// same trap the empty-config bug fell into.
func TestNoCertResolverEmitsNoTLSKey(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	if err := r.Sync([]Route{{Key: "abc12345", Hostname: "app.example.com", Target: "10.0.0.1", Port: 32768}}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if strings.Contains(body, "tls") {
		t.Fatalf("plaintext config must not mention tls:\n%s", body)
	}
	if !strings.Contains(body, "- web") || strings.Contains(body, "websecure") {
		t.Fatalf("plaintext config must use only the web entrypoint:\n%s", body)
	}
}

// With one, every route is a TLS router naming the resolver.
//
// websecure only. This test first asserted "both entrypoints", on the belief
// that port 80 had to stay routable for the ACME challenge. Running it against
// traefik:v3.3 showed otherwise: a router with `tls` set matches TLS
// connections only, so listing `web` changed nothing, and the challenge is
// served by Traefik itself before any router or redirect sees the request.
// Sending plain HTTP somewhere useful is a static-config redirect, not a
// property of this file.
func TestCertResolverMakesEveryRouteTLSOnly(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, WithCertResolver("le"))
	if err := r.Sync([]Route{
		{Key: "abc12345", Hostname: "app.example.com", Target: "10.0.0.1", Port: 32768},
		{Key: "def67890", Hostname: "shop.acme.com", Target: "10.0.0.2", Port: 32769},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if strings.Count(body, "certResolver: le") != 2 {
		t.Fatalf("expected both routes to name the resolver:\n%s", body)
	}
	if strings.Count(body, "- websecure") != 2 {
		t.Fatalf("expected both routes on websecure:\n%s", body)
	}
	if strings.Contains(body, "- web\n") {
		t.Fatalf("a tls router must not also claim the plain entrypoint:\n%s", body)
	}
}

// An empty route set still writes the comment-only file, resolver or not. This
// is the case the empty-config bug was about: `http: {}` or `routers: {}` fails
// the entire file, Traefik keeps the last config it accepted, and a torn-down
// environment goes on being served.
func TestEmptyStaysCommentOnlyWithAResolver(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir, WithCertResolver("le")).Sync(nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if strings.Contains(body, "http:") || strings.Contains(body, "tls") {
		t.Fatalf("an empty config must carry no sections at all:\n%s", body)
	}
}

func readConfig(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "composectl.yml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(b)
}
