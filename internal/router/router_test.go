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

// The wildcard's whole purpose is that N preview routes cost one certificate,
// so the assertion is on the count: both routes name the wildcard resolver and
// both ask for the same `*.preview...` domain. Traefik obtains that once.
//
// Note what is NOT asserted here, because Go cannot reach it: that Let's
// Encrypt issues the wildcard, or that Traefik serves it for a matching SNI.
// This checks the bytes. `make demo-wildcard` puts a real Traefik in front of a
// real ACME server for the rest.
func TestWildcardRoutesShareOneCertificate(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, WithCertResolver("le"), WithWildcard("preview.navar.ch", "lewild"))
	if err := r.Sync([]Route{
		{Key: "aaa11111", Hostname: "pr-1-main-aaa11111.preview.navar.ch", Target: "10.0.0.1", Port: 32768},
		{Key: "bbb22222", Hostname: "pr-2-main-bbb22222.preview.navar.ch", Target: "10.0.0.2", Port: 32769},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if strings.Count(body, "certResolver: lewild") != 2 {
		t.Fatalf("both previews must name the wildcard resolver:\n%s", body)
	}
	if strings.Count(body, `main: '*.preview.navar.ch'`) != 2 {
		t.Fatalf("both previews must ask for the same wildcard:\n%s", body)
	}
	if strings.Contains(body, "certResolver: le\n") {
		t.Fatalf("no preview route should fall through to the per-hostname resolver:\n%s", body)
	}
}

// Everything that is not a preview keeps its own certificate, and keeps it from
// the HTTP-01 resolver — which is the point of scoping the wildcard to a
// suffix. A tenant's hostname, and a customer's own domain, must never depend
// on a DNS credential the platform holds for its own preview names.
func TestWildcardLeavesOtherHostnamesOnHTTP01(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, WithCertResolver("le"), WithWildcard("preview.navar.ch", "lewild"))
	if err := r.Sync([]Route{
		{Key: "aaa11111", Hostname: "shop.acme.com", Target: "10.0.0.1", Port: 32768},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if !strings.Contains(body, "certResolver: le\n") {
		t.Fatalf("a tenant route must stay on the per-hostname resolver:\n%s", body)
	}
	if strings.Contains(body, "domains") {
		t.Fatalf("a tenant route must not name certificate domains:\n%s", body)
	}
}

// A DNS wildcard covers exactly one label. Claiming a route the certificate
// cannot cover would be worse than not claiming it: with `domains` set, Traefik
// asks for the wildcard and never for the hostname, so the route would serve a
// name the browser rejects — a failure that looks like a certificate bug rather
// than a routing one. The bare suffix is excluded for the same reason.
func TestWildcardCoversExactlyOneLabel(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, WithCertResolver("le"), WithWildcard("preview.navar.ch", "lewild"))
	for _, host := range []string{
		"a.b.preview.navar.ch", // two labels deep — outside the wildcard
		"preview.navar.ch",     // the suffix itself — a wildcard never covers it
		"notpreview.navar.ch",  // shares a tail, not a boundary
	} {
		if err := r.Sync([]Route{{Key: "abc12345", Hostname: host, Target: "10.0.0.1", Port: 32768}}); err != nil {
			t.Fatalf("Sync(%s): %v", host, err)
		}
		body := readConfig(t, dir)
		if strings.Contains(body, "lewild") || strings.Contains(body, "domains") {
			t.Fatalf("%s is not covered by *.preview.navar.ch and must keep its own certificate:\n%s", host, body)
		}
	}
}

// The wildcard needs both halves. A suffix with no resolver is a half-written
// config, and diverting routes onto a resolver name that is empty would emit
// `certResolver: ""` — which is not the plaintext case and not the TLS case,
// but a file Traefik takes and then cannot satisfy.
func TestWildcardWithoutAResolverChangesNothing(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, WithCertResolver("le"), WithWildcard("preview.navar.ch", ""))
	if err := r.Sync([]Route{
		{Key: "aaa11111", Hostname: "pr-1-main-aaa11111.preview.navar.ch", Target: "10.0.0.1", Port: 32768},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	body := readConfig(t, dir)
	if !strings.Contains(body, "certResolver: le\n") || strings.Contains(body, "domains") {
		t.Fatalf("an incomplete wildcard must leave the route exactly as it was:\n%s", body)
	}
}
