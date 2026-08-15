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
		{Key: "abc12345", Hostname: "prod.example.com", ServiceContainer: "cc-abc12345-r1-blue-api", Port: 80},
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
		"http://cc-abc12345-r1-blue-api:80",
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
		{Key: "abc12345", Hostname: "x.com`) || Host(`y.com", ServiceContainer: "cc-abc12345-r1-blue-api", Port: 80},
	})
	if err == nil {
		t.Fatal("expected unsafe hostname to be rejected")
	}
}

func TestSyncEmptyIsValid(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir).Sync(nil); err != nil {
		t.Fatalf("empty sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "composectl.yml")); err != nil {
		t.Fatalf("expected a file even when empty: %v", err)
	}
}
