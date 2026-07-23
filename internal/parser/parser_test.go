package parser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/craig/composectl/internal/spec"
)

// mustParse parses yaml and fails the test if it is rejected.
func mustParse(t *testing.T, yaml string) *spec.DeploymentSpec {
	t.Helper()
	s, err := Parse(context.Background(), []byte(yaml), "test")
	if err != nil {
		t.Fatalf("expected a valid spec, got error: %v", err)
	}
	return s
}

// parseErrs parses yaml expecting collected ValidationErrors.
func parseErrs(t *testing.T, yaml string) ValidationErrors {
	t.Helper()
	_, err := Parse(context.Background(), []byte(yaml), "test")
	if err == nil {
		t.Fatal("expected validation errors, got a valid spec")
	}
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}
	return verrs
}

// hasErr reports whether verrs contains an error on field whose message
// contains substr.
func hasErr(verrs ValidationErrors, field, substr string) bool {
	for _, ve := range verrs {
		if ve.Field == field && strings.Contains(ve.Message, substr) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------
// Bug 1: mounts that compose-go resolves to an anonymous volume
// --------------------------------------------------------------------

// A single-character volume name is read by compose-go as a Windows drive
// letter, producing an anonymous volume whose target is the whole string.
// Left unchecked the service is silently pinned on a volume the author
// never declared, and the declared volume is reported as unmounted.
func TestSingleCharacterVolumeNameIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  s:
    image: nginx
    volumes: ["d:/mnt"]
volumes:
  d: {}
`)
	if !hasErr(verrs, "services.s.volumes", "single-character") {
		t.Fatalf("expected a single-character volume diagnostic, got: %v", verrs)
	}
}

func TestAnonymousVolumeIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  s:
    image: nginx
    volumes: ["/data"]
`)
	if !hasErr(verrs, "services.s.volumes", "anonymous") {
		t.Fatalf("expected an anonymous-volume rejection, got: %v", verrs)
	}
}

// The mis-parse must not also surface as a bogus "declared but not
// mounted" error pointing at the wrong line.
func TestSingleCharacterVolumeDoesNotReportVolumeUnmounted(t *testing.T) {
	verrs := parseErrs(t, `
services:
  s:
    image: nginx
    volumes: ["d:/mnt"]
volumes:
  d: {}
`)
	if hasErr(verrs, "volumes.d", "not mounted") {
		t.Fatalf("mis-parsed mount should not be reported as an unmounted volume: %v", verrs)
	}
}

func TestMultiCharacterVolumeNamePinsService(t *testing.T) {
	got := mustParse(t, `
services:
  s:
    image: nginx
    volumes: ["data:/mnt"]
volumes:
  data: {}
`)
	if pinned := got.PinnedServices(); len(pinned) != 1 || pinned[0] != "s" {
		t.Fatalf("expected s to be pinned, got pinned=%v", pinned)
	}
}

// --------------------------------------------------------------------
// Bug 2: compose-go's fail-fast checks pre-empting collected reporting
// --------------------------------------------------------------------

func TestDependencyCycleIsCollectedAsValidationError(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    depends_on: [b]
  b:
    image: nginx
    depends_on: [a]
`)
	if !hasErr(verrs, "depends_on", "cycle") {
		t.Fatalf("expected a structured cycle error, got: %v", verrs)
	}
}

func TestUnknownDependsOnIsCollectedAsValidationError(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    depends_on: [ghost]
`)
	if !hasErr(verrs, "services.a.depends_on", "ghost") {
		t.Fatalf("expected a structured unknown-dependency error, got: %v", verrs)
	}
}

// The point of the fix: a bad depends_on must not mask the other
// violations sitting in the same file.
func TestPlatformViolationsCollectedAlongsideBadDependsOn(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    privileged: true
    container_name: fixed
    ports: ["8080:80"]
    depends_on: [ghost]
`)
	for _, want := range []struct{ field, substr string }{
		{"services.a.depends_on", "ghost"},
		{"services.a.privileged", "privileged"},
		{"services.a.container_name", "container_name"},
		{"services.a.ports", "8080"},
	} {
		if !hasErr(verrs, want.field, want.substr) {
			t.Errorf("missing %s (%q) in collected errors: %v", want.field, want.substr, verrs)
		}
	}
}

// Deferring compose-go's consistency check must not discard it. This
// healthcheck form is rejected by compose-go and by nothing in our parser.
func TestDeferredConsistencyCheckStillRejectsBadHealthcheck(t *testing.T) {
	_, err := Parse(context.Background(), []byte(`
services:
  a:
    image: nginx
    healthcheck:
      test: ["curl", "-f", "http://localhost/"]
`), "test")
	if err == nil {
		t.Fatal("expected compose-go's healthcheck validation to reject this")
	}
	if !strings.Contains(err.Error(), "CMD") {
		t.Fatalf("expected a healthcheck.test diagnostic, got: %v", err)
	}
}

// Likewise a mount referencing a volume that was never declared.
func TestDeferredConsistencyCheckStillRejectsUndeclaredVolume(t *testing.T) {
	_, err := Parse(context.Background(), []byte(`
services:
  a:
    image: nginx
    volumes: ["nope:/data"]
`), "test")
	if err == nil {
		t.Fatal("expected a reference to an undeclared volume to be rejected")
	}
}

// Legacy `scale:` must be rejected on its own terms. compose-go only folds
// Scale into Deploy.Replicas during the consistency check, and only when a
// deploy block already exists — so relying on that fold missed the bare
// case entirely and broke the rest once the check was deferred.
func TestLegacyScaleIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    scale: 3
`)
	if !hasErr(verrs, "services.a.scale", "platform") {
		t.Fatalf("expected legacy scale to be rejected, got: %v", verrs)
	}
}

func TestLegacyScaleWithDeployBlockIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    scale: 3
    deploy:
      resources:
        limits:
          memory: 128M
`)
	if !hasErr(verrs, "services.a.scale", "platform") {
		t.Fatalf("expected legacy scale to be rejected, got: %v", verrs)
	}
}

func TestScaleOfOneIsAccepted(t *testing.T) {
	mustParse(t, `
services:
  a:
    image: nginx
    scale: 1
`)
}

// --------------------------------------------------------------------
// Regression guards for behavior the fixes touch
// --------------------------------------------------------------------

func TestReadOnlyVolumeStaysSwappable(t *testing.T) {
	got := mustParse(t, `
services:
  app:
    image: nginx
    volumes: ["conf:/etc/nginx/conf.d:ro"]
  owner:
    image: busybox
    volumes: ["data:/data"]
volumes:
  conf: {}
  data: {}
`)
	swappable := got.SwappableServices()
	if len(swappable) != 1 || swappable[0] != "app" {
		t.Fatalf("read-only mount must stay swappable, got swappable=%v", swappable)
	}
}

func TestDeclaredButUnmountedVolumeStillRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
volumes:
  orphaned: {}
`)
	if !hasErr(verrs, "volumes.orphaned", "not mounted") {
		t.Fatalf("expected an unmounted-volume error, got: %v", verrs)
	}
}

func TestSecretEmbeddedMidStringKeepsTemplate(t *testing.T) {
	s, err := Parse(context.Background(), []byte(`
services:
  a:
    image: nginx
    environment:
      DATABASE_URL: postgres://app:${secret:db_password}@db:5432/app
      LOG_LEVEL: info
`), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	svc := s.Services["a"]
	const want = "postgres://app:${secret:db_password}@db:5432/app"
	if got := svc.SecretEnv["DATABASE_URL"]; got != want {
		t.Fatalf("secret template not preserved:\n got %q\nwant %q", got, want)
	}
	if _, leaked := svc.Env["DATABASE_URL"]; leaked {
		t.Fatal("secret-bearing value must not appear in Env")
	}
	if svc.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("non-secret env lost: %v", svc.Env)
	}
}

func TestResourceDefaultsApplied(t *testing.T) {
	s, err := Parse(context.Background(), []byte(`
services:
  a:
    image: nginx
`), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lim := s.Services["a"].Limits
	if lim.CPUMillis != DefaultCPUMillis || lim.MemoryBytes != DefaultMemoryBytes {
		t.Fatalf("expected defaults %d/%d, got %d/%d",
			DefaultCPUMillis, DefaultMemoryBytes, lim.CPUMillis, lim.MemoryBytes)
	}
}
