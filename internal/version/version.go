// Package version carries the build identity of every Navarch binary.
//
// One place, because three binaries need the same answer and an operator asking
// "what is running here" should not get a different shape from each. The values
// are stamped by the linker at build time — scripts/release.sh for the CLI,
// deploy/Dockerfile for the server images — and fall back to Go's own build
// info when they were not.
//
// Before this existed the agent reported a hardcoded "sprint2-a" at
// registration, which had been wrong since Sprint 2 and was visible in
// `navarch node get`. A version string nobody updates is worse than none: it
// looks like an answer.
package version

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
)

// Defaults are deliberately not a version number. A binary somebody built from
// a checkout should say "dev", not claim to be a release nobody can reproduce.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok {
		applyBuildInfo(bi)
	}
}

// applyBuildInfo fills in whatever the linker did not.
//
// `go install github.com/craigderington/navarch/cmd/navarch@v1.2.0` is a
// documented install path — it is the reason the module was renamed at 1.0 —
// and it passes no ldflags, so a binary installed that way reported "dev
// (unknown, unknown)". That is precisely the failure this package was written
// to end, arriving through the one door nobody checked: not a stale version
// string, but a real release insisting it is not one.
//
// Go already knows. It stamps the module version for a versioned install, and
// vcs.revision/vcs.time for a build inside a repository. Neither is a
// substitute for the linker values — a versioned install has no VCS stamp and a
// repository build has no module version — so each field is filled from
// whichever source has it, and an ldflag always wins.
//
// Separate from init so it can be tested against a synthetic BuildInfo; the
// real one describes the test binary and would assert nothing.
func applyBuildInfo(bi *debug.BuildInfo) {
	if Version == "dev" && isReleaseVersion(bi.Main.Version) {
		Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			// Seven characters, matching what release.sh stamps and what git
			// prints, so the two sources are comparable at a glance.
			if Commit == "unknown" && len(s.Value) >= 7 {
				Commit = s.Value[:7]
			}
		case "vcs.time":
			if Date == "unknown" && s.Value != "" {
				Date = s.Value
			}
		case "vcs.modified":
			// A dirty tree is not the commit it claims to be. Say so rather
			// than report a hash somebody could go and read, and find it does
			// not contain what they are running.
			if s.Value == "true" && Commit != "unknown" {
				Commit += "-dirty"
			}
		}
	}
}

// pseudoVersion matches the tail Go appends when a build is not at a tag.
//
// The separator before the timestamp is a dot as often as a dash, because the
// three forms differ by what the base version was:
//
//	v0.0.0-20260902200600-517bba904a51        no base tag
//	v1.2.1-0.20260902200600-517bba904a51      base was a release tag
//	v1.2.1-rc1.0.20260902200600-517bba904a51  base had a prerelease
//
// Matching only the dash form let the middle one through, which is the one an
// ordinary `go build` past the latest tag actually produces.
var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}$`)

// isReleaseVersion reports whether a module version names a tag somebody could
// go and check out.
//
// Not merely "is it non-empty". Go 1.24 and later stamp a *pseudo-version* for
// a build inside a repository rather than the "(devel)" that older toolchains
// used, so a plain `go build` of a dirty tree yields something like
// v1.2.1-0.20260902200600-517bba904a51+dirty. Accepting that would put
// "1.2.1-0.2026…" in `navarch node list` and /healthz, where at a glance it
// reads as a release number — the precise misreading this package exists to
// prevent, and the reason its defaults were chosen to say "dev" instead.
//
// The commit and time from that same build info are still used; it is only the
// claim to be a version that is refused.
func isReleaseVersion(v string) bool {
	switch {
	case v == "", v == "(devel)":
		return false
	case strings.Contains(v, "+"):
		// Build metadata, which in practice means +dirty.
		return false
	case pseudoVersion.MatchString(v):
		return false
	}
	return strings.HasPrefix(v, "v")
}

// String is the one-line form: "1.0.0 (abc1234, 2026-08-29)".
func String() string { return Version + " (" + Commit + ", " + Date + ")" }

// Info is the structured form, for /healthz and `navarch version -o json`.
func Info() map[string]string {
	return map[string]string{
		"version": Version,
		"commit":  Commit,
		"built":   Date,
		"go":      runtime.Version(),
	}
}
