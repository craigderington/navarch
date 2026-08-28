package api

import (
	"slices"
	"testing"

	"github.com/craigderington/navarch/internal/spec"
)

func baseSpec() *spec.DeploymentSpec {
	return &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {
				Name: "api", Image: "nginx:alpine", Swappable: true,
				Env: map[string]string{"LOG_LEVEL": "info", "EXPLICIT": "from-compose"},
				SecretEnv: map[string]string{
					"DATABASE_URL": "postgres://app:${secret:db_password}@db/app",
				},
				Limits:     spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20},
				Command:    []string{"serve"},
				Entrypoint: []string{"/app"},
				Depends:    []string{"db"},
			},
		},
		Volumes:  map[string]spec.Volume{},
		Networks: []string{"default"},
	}
}

// Env config supplies defaults; a value set explicitly in the compose file is
// the author's deliberate choice for that service and wins. Reversed
// precedence would let an environment-wide LOG_LEVEL silently clobber a
// worker deliberately set to debug — a real bug, already fixed once.
func TestApplyEnvConfigComposeValuesBeatEnvConfig(t *testing.T) {
	resolved := applyEnvConfig(baseSpec(), map[string]string{
		"LOG_LEVEL": "debug", "NEW_KEY": "from-config",
	})
	svc := resolved.Services["api"]
	if svc.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("compose-file value must win, got LOG_LEVEL=%q", svc.Env["LOG_LEVEL"])
	}
	if svc.Env["NEW_KEY"] != "from-config" {
		t.Fatalf("env config supplies defaults, got NEW_KEY=%q", svc.Env["NEW_KEY"])
	}
	if svc.Env["EXPLICIT"] != "from-compose" {
		t.Fatalf("pre-existing compose value must survive, got %q", svc.Env["EXPLICIT"])
	}
}

// A secret template always wins over env config: env config must never
// replace a value the stack routes through the secret store. Otherwise an
// env-config DATABASE_URL would hand the container a plaintext URL with no
// secret in it — failing only at runtime, from the container.
func TestApplyEnvConfigSecretTemplatesBeatEverything(t *testing.T) {
	resolved := applyEnvConfig(baseSpec(), map[string]string{
		"DATABASE_URL": "postgres://app:plaintext@db/app",
	})
	svc := resolved.Services["api"]
	if _, ok := svc.Env["DATABASE_URL"]; ok {
		t.Fatal("a key with a secret template must not appear in plain Env")
	}
	if svc.SecretEnv["DATABASE_URL"] == "" {
		t.Fatal("the secret template must survive the overlay")
	}
}

// The overlay is additive by design: env config can set environment
// variables and nothing else. If a structural field could change, the spec
// digest would no longer identify what actually runs.
func TestApplyEnvConfigIsAdditiveOnly(t *testing.T) {
	resolved := applyEnvConfig(baseSpec(), map[string]string{"IMAGE": "evil:latest"})
	svc := resolved.Services["api"]
	if svc.Image != "nginx:alpine" {
		t.Fatalf("env config must not be able to change the image, got %q", svc.Image)
	}
	if svc.Limits.MemoryBytes != 256<<20 {
		t.Fatalf("env config must not be able to change limits, got %d", svc.Limits.MemoryBytes)
	}
}

// The resolved spec must be a deep copy: the stack version's stored spec is
// immutable by contract, and an overlay that mutated it would corrupt every
// later deployment of that version — visible only as one environment's env
// config leaking into another's.
func TestApplyEnvConfigDoesNotMutateTheBase(t *testing.T) {
	base := baseSpec()
	_ = applyEnvConfig(base, map[string]string{"LOG_LEVEL": "debug", "POISON": "yes"})

	svc := base.Services["api"]
	if _, ok := svc.Env["POISON"]; ok {
		t.Fatal("overlay leaked a new key into the stored base spec")
	}
	if svc.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("overlay clobbered the base spec: LOG_LEVEL=%q", svc.Env["LOG_LEVEL"])
	}

	// Slice fields must be copies, not shared backing arrays: appending to
	// the resolved copy must never be visible through the base.
	resolved := applyEnvConfig(base, nil)
	rcmd := resolved.Services["api"].Command
	rcmd = append(rcmd, "poisoned")
	if slices.Equal(base.Services["api"].Command, rcmd) {
		t.Fatal("resolved spec shares a backing array with the base")
	}
}

// A nil config is the ordinary case for a deployment against an environment
// that never set one — the result must be a faithful copy, not a panic or a
// spec with empty env where the compose file had values.
func TestApplyEnvConfigNilConfigCopiesFaithfully(t *testing.T) {
	resolved := applyEnvConfig(baseSpec(), nil)
	svc := resolved.Services["api"]
	if svc.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("base env must survive a nil config, got %q", svc.Env["LOG_LEVEL"])
	}
	if _, ok := svc.Env["DATABASE_URL"]; ok {
		t.Fatal("secret template key must still be routed to SecretEnv")
	}
	if len(resolved.Networks) != 1 || resolved.Networks[0] != "default" {
		t.Fatalf("networks must be copied, got %v", resolved.Networks)
	}
}
