// Package version carries the build identity of every Navarch binary.
//
// One place, because three binaries need the same answer and an operator asking
// "what is running here" should not get a different shape from each. The values
// are stamped by the linker at build time — scripts/release.sh for the CLI,
// deploy/Dockerfile for the server images.
//
// Before this existed the agent reported a hardcoded "sprint2-a" at
// registration, which had been wrong since Sprint 2 and was visible in
// `navarch node get`. A version string nobody updates is worse than none: it
// looks like an answer.
package version

import "runtime"

// Defaults are deliberately not a version number. A binary somebody built from
// a checkout should say "dev", not claim to be a release nobody can reproduce.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

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
