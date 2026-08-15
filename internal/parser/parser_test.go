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
    x-composectl:
      rollout: pin
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
    x-composectl:
      rollout: swap
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
    x-composectl:
      rollout: swap
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
    x-composectl:
      rollout: swap
  owner:
    image: busybox
    volumes: ["data:/data"]
    x-composectl:
      rollout: pin
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
    x-composectl:
      rollout: swap
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
    x-composectl:
      rollout: swap
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

// --------------------------------------------------------------------
// Isolation contract: reject loudly, and never read the control-plane disk
// --------------------------------------------------------------------

func TestIncludeIsRejectedWithoutReadingTheFile(t *testing.T) {
	const marker = "/composectl-must-not-read-this-include.yaml"
	verrs := parseErrs(t, `
include:
  - `+marker+`
services:
  a:
    image: nginx
`)
	if !hasErr(verrs, "include", "not supported") {
		t.Fatalf("expected include to be rejected, got: %v", verrs)
	}
	for _, ve := range verrs {
		if strings.Contains(ve.Message, marker) && strings.Contains(strings.ToLower(ve.Message), "no such file") {
			t.Fatalf("loader read the include path: %v", verrs)
		}
	}
}

func TestExtendsFileIsRejectedWithoutReadingTheFile(t *testing.T) {
	const marker = "/composectl-must-not-read-this-extends.yaml"
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    extends:
      file: `+marker+`
      service: base
`)
	if !hasErr(verrs, "services.a.extends", "not supported") {
		t.Fatalf("expected extends to be rejected, got: %v", verrs)
	}
	for _, ve := range verrs {
		if strings.Contains(ve.Message, "no such file") {
			t.Fatalf("loader read the extends file: %v", verrs)
		}
	}
}

func TestEnvFileIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    env_file: /etc/passwd
`)
	if !hasErr(verrs, "services.a.env_file", "not supported") {
		t.Fatalf("expected env_file to be rejected, got: %v", verrs)
	}
}

func TestLabelFileIsRejectedWithoutReadingTheFile(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    label_file: /etc/passwd
`)
	if !hasErr(verrs, "services.a.label_file", "not supported") {
		t.Fatalf("expected label_file to be rejected, got: %v", verrs)
	}
}

func TestPidHostIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    pid: host
`)
	if !hasErr(verrs, "services.a.pid", "not permitted") {
		t.Fatalf("expected pid: host to be rejected, got: %v", verrs)
	}
}

func TestDevicesAreRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    devices:
      - /dev/sda:/dev/sda
`)
	if !hasErr(verrs, "services.a.devices", "not permitted") {
		t.Fatalf("expected devices to be rejected, got: %v", verrs)
	}
}

func TestTmpfsMountIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    volumes:
      - type: tmpfs
        target: /tmp
`)
	if !hasErr(verrs, "services.a.volumes", "tmpfs") {
		t.Fatalf("expected tmpfs mount to be rejected, got: %v", verrs)
	}
}

func TestCapDropIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    cap_drop: [ALL]
`)
	if !hasErr(verrs, "services.a.cap_drop", "not permitted") {
		t.Fatalf("expected cap_drop to be rejected, got: %v", verrs)
	}
}

func TestExtraHostsAreRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    extra_hosts:
      - "db:1.2.3.4"
`)
	if !hasErr(verrs, "services.a.extra_hosts", "not permitted") {
		t.Fatalf("expected extra_hosts to be rejected, got: %v", verrs)
	}
}

func TestProfilesAreRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    profiles: [debug]
`)
	if !hasErr(verrs, "services.a.profiles", "not supported") {
		t.Fatalf("expected profiles to be rejected, got: %v", verrs)
	}
}

func TestVolumeDriverOptsAreRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    volumes: ["data:/data"]
volumes:
  data:
    driver: local
    driver_opts:
      type: none
      o: bind
      device: /etc
`)
	if !hasErr(verrs, "volumes.data", "driver_opts") {
		t.Fatalf("expected volume driver_opts to be rejected, got: %v", verrs)
	}
}

func TestDependsOnHealthyIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  db:
    image: postgres
  api:
    image: nginx
    depends_on:
      db:
        condition: service_healthy
`)
	if !hasErr(verrs, "services.api.depends_on", "condition") {
		t.Fatalf("expected service_healthy depends_on to be rejected, got: %v", verrs)
	}
}

func TestEmptyServicesIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services: {}
`)
	if !hasErr(verrs, "services", "empty") {
		t.Fatalf("expected empty services to be rejected, got: %v", verrs)
	}
}

// --------------------------------------------------------------------
// Rollout mode: cardinality is declared, never inferred
// --------------------------------------------------------------------

func TestRolloutModeMustBeDeclared(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
`)
	if !hasErr(verrs, "services.a.x-composectl.rollout", "must be declared") {
		t.Fatalf("expected a missing-rollout error, got: %v", verrs)
	}
}

// Missing declarations are collected like every other violation, not
// reported one service at a time across repeated deploy attempts.
func TestEveryUndeclaredServiceIsReportedInOnePass(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
  b:
    image: busybox
  c:
    image: alpine
`)
	for _, name := range []string{"a", "b", "c"} {
		if !hasErr(verrs, "services."+name+".x-composectl.rollout", "must be declared") {
			t.Fatalf("expected %s to be reported, got: %v", name, verrs)
		}
	}
}

func TestUnknownRolloutModeIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    x-composectl:
      rollout: singleton
`)
	if !hasErr(verrs, "services.a.x-composectl.rollout", `unknown rollout mode "singleton"`) {
		t.Fatalf("expected an unknown-mode error, got: %v", verrs)
	}
}

func TestNonStringRolloutModeIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: nginx
    x-composectl:
      rollout: true
`)
	if !hasErr(verrs, "services.a.x-composectl.rollout", "must be a string") {
		t.Fatalf("expected a type error, got: %v", verrs)
	}
}

// The volume rule survives as a constraint. An author may declare their
// cardinality but may not declare two processes onto one filesystem.
func TestSwapWithWritableVolumeIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  a:
    image: postgres:16
    volumes: ["data:/var/lib/postgresql/data"]
    x-composectl:
      rollout: swap
volumes:
  data: {}
`)
	if !hasErr(verrs, "services.a.x-composectl.rollout", "one filesystem") {
		t.Fatalf("expected swap+volume to be refused, got: %v", verrs)
	}
}

// Read-only mounts were always exempt from the old inference; they must stay
// exempt from the new constraint, or a shared config volume would force its
// consumer out of blue/green.
func TestSwapWithReadOnlyVolumeIsAccepted(t *testing.T) {
	got := mustParse(t, `
services:
  a:
    image: nginx
    volumes: ["conf:/etc/nginx/conf.d:ro"]
    x-composectl:
      rollout: swap
  owner:
    image: busybox
    volumes: ["conf2:/data"]
    x-composectl:
      rollout: pin
volumes:
  conf: {}
  conf2: {}
`)
	if swappable := got.SwappableServices(); len(swappable) != 1 || swappable[0] != "a" {
		t.Fatalf("read-only mount must not block swap, got swappable=%v", swappable)
	}
}

// The point of the whole change: a service with no storage at all can now be
// declared pinned. Under the old rule this was unexpressible — a scheduler,
// cron runner or broker mounted nothing, so it was duplicated during every
// rollout no matter what its author intended.
func TestVolumelessServiceCanBeDeclaredPinned(t *testing.T) {
	got := mustParse(t, `
services:
  api:
    image: nginx
    x-composectl:
      rollout: swap
      ingress:
        port: 80
  beat:
    image: acme/scheduler
    command: ["celery", "beat"]
    x-composectl:
      rollout: pin
`)
	pinned := got.PinnedServices()
	if len(pinned) != 1 || pinned[0] != "beat" {
		t.Fatalf("expected beat to be pinned with no volume, got pinned=%v", pinned)
	}
	// Cardinality drives capacity: a pinned service is counted once.
	want := int64(DefaultMemoryBytes * 3) // api twice, beat once
	if got := got.PeakMemoryBytes(); got != want {
		t.Fatalf("peak memory %d, want %d", got, want)
	}
}

func TestPinnedIngressIsRejected(t *testing.T) {
	verrs := parseErrs(t, `
services:
  api:
    image: nginx
    x-composectl:
      rollout: pin
      ingress:
        port: 80
`)
	if !hasErr(verrs, "services.api.x-composectl.rollout", "cannot participate in blue/green") {
		t.Fatalf("expected a pinned-ingress rejection, got: %v", verrs)
	}
}

// One mistake, one error. The rule used to live in validateGraph as well,
// and checking it in both places reported a pinned ingress twice.
func TestPinnedIngressIsReportedOnce(t *testing.T) {
	verrs := parseErrs(t, `
services:
  api:
    image: nginx
    volumes: ["data:/data"]
    x-composectl:
      rollout: pin
      ingress:
        port: 80
volumes:
  data: {}
`)
	var n int
	for _, ve := range verrs {
		if strings.Contains(ve.Message, "cannot participate in blue/green") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one pinned-ingress error, got %d: %v", n, verrs)
	}
}
