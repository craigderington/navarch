package version

import (
	"runtime/debug"
	"testing"
)

// reset restores the linker-stamped defaults between cases, because
// applyBuildInfo deliberately only fills what is still at its default.
func reset(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := Version, Commit, Date
	Version, Commit, Date = v, c, d
	t.Cleanup(func() { Version, Commit, Date = ov, oc, od })
}

// The case the module rename was done for. `go install pkg@v1.2.0` passes no
// ldflags, so before this the binary reported "dev (unknown, unknown)" — a real
// release insisting it is not one, which is the exact failure this package
// exists to prevent, arriving through the one door nobody checked.
func TestVersionedInstallReportsItsVersion(t *testing.T) {
	reset(t, "dev", "unknown", "unknown")
	applyBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "v1.2.0"}})
	if Version != "1.2.0" {
		t.Fatalf("Version = %q, want 1.2.0", Version)
	}
}

// A build inside a repository has no module version but does have VCS stamps.
// Neither source is a substitute for the other, so each field takes whichever
// one has it.
func TestRepositoryBuildTakesTheVCSStamps(t *testing.T) {
	reset(t, "dev", "unknown", "unknown")
	applyBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "6df1e72ea883302e1a38b2a8be2d949fa26ac3fb"},
			{Key: "vcs.time", Value: "2026-09-02T15:41:00Z"},
		},
	})
	if Version != "dev" {
		t.Fatalf("(devel) must stay dev, got %q", Version)
	}
	if Commit != "6df1e72" {
		t.Fatalf("Commit = %q, want the short hash", Commit)
	}
	if Date != "2026-09-02T15:41:00Z" {
		t.Fatalf("Date = %q", Date)
	}
}

// The linker always wins. A release binary must never have its stamped identity
// overwritten by whatever the toolchain happened to record.
func TestLdflagsAreNeverOverwritten(t *testing.T) {
	reset(t, "1.2.0", "abc1234", "2026-09-02T00:00:00Z")
	applyBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "ffffffffffffffffffffffffffffffffffffffff"},
			{Key: "vcs.time", Value: "1999-01-01T00:00:00Z"},
		},
	})
	if Version != "1.2.0" || Commit != "abc1234" || Date != "2026-09-02T00:00:00Z" {
		t.Fatalf("stamped values were overwritten: %s", String())
	}
}

// A dirty tree is not the commit it names. Reporting the bare hash invites
// somebody to go and read it, and find it does not contain what they are
// running.
func TestDirtyTreeIsSaidOutLoud(t *testing.T) {
	reset(t, "dev", "unknown", "unknown")
	applyBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "6df1e72ea883302e1a38b2a8be2d949fa26ac3fb"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	if Commit != "6df1e72-dirty" {
		t.Fatalf("Commit = %q, want the hash marked dirty", Commit)
	}
}

// Nothing to read is not an error; it is the case where the defaults are
// already the honest answer.
func TestNoBuildInfoLeavesTheDefaults(t *testing.T) {
	reset(t, "dev", "unknown", "unknown")
	applyBuildInfo(&debug.BuildInfo{})
	if String() != "dev (unknown, unknown)" {
		t.Fatalf("got %q", String())
	}
}

// Go 1.24+ stamps a pseudo-version for a repository build rather than the
// "(devel)" older toolchains used. Accepting it would put "1.2.1-0.2026…" in
// `navarch node list` and /healthz, where at a glance it reads as a release
// number — which is the misreading this package's defaults were chosen to
// prevent. The commit and time from the same build info are still taken.
func TestPseudoVersionIsNotAReleaseVersion(t *testing.T) {
	for _, v := range []string{
		"v1.2.1-0.20260902200600-517bba904a51",     // base was a release tag
		"v0.0.0-20260902200600-517bba904a51",       // no base tag
		"v1.2.1-rc1.0.20260902200600-517bba904a51", // base had a prerelease
		"v1.2.1-0.20260902200600-517bba904a51+dirty",
		"(devel)",
		"",
	} {
		if isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"v1.2.0", "v1.0.0", "v2.0.0-rc1"} {
		if !isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = false, want true", v)
		}
	}
}

// The whole point, end to end: a dirty checkout says dev and still names the
// commit, so the answer is useful without being a claim.
func TestDirtyCheckoutSaysDevAndNamesTheCommit(t *testing.T) {
	reset(t, "dev", "unknown", "unknown")
	applyBuildInfo(&debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.1-0.20260902200600-517bba904a51+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "517bba904a51abcdef0123456789abcdef012345"},
			{Key: "vcs.modified", Value: "true"},
		},
	})
	if Version != "dev" {
		t.Fatalf("Version = %q, want dev", Version)
	}
	if Commit != "517bba9-dirty" {
		t.Fatalf("Commit = %q", Commit)
	}
}
