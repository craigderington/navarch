package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// commandsInRootHelp reads the user-facing command list out of rootHelp.
//
// The list comes from the help text rather than a slice in this file, because
// the help text is what a person actually sees. A command added there without a
// synopsis, or one that answers --help by doing something, fails below rather
// than shipping.
func commandsInRootHelp(t *testing.T) []string {
	t.Helper()
	_, after, ok := strings.Cut(rootHelp, "Commands:\n")
	if !ok {
		t.Fatal("rootHelp has no Commands: block — the tests below enumerate it")
	}
	block, _, _ := strings.Cut(after, "\n\n")
	var out []string
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		// "  promote ID           Manually promote ..." → "promote"
		if name := strings.Fields(line); len(name) > 0 {
			out = append(out, name[0])
		}
	}
	if len(out) < 10 {
		t.Fatalf("parsed only %d commands out of rootHelp; the format changed", len(out))
	}
	return out
}

// --help must never do anything. Not contact the server, not write a file, not
// mint a credential.
//
// It did all three. splitGlobals consumed the flag and dropped it, and Run
// honoured it only when there were no other arguments, so `navarch <cmd> ...
// --help` ran the command with the flag silently discarded. `navarch health
// --help` performed a health check; `navarch token create --help` MINTED AN
// OPERATOR TOKEN and printed its plaintext to stdout, which is a live
// credential handed to whoever was reading the terminal or the scrollback.
//
// The assertion is that the server is never contacted, because that is what
// distinguishes "printed help" from "did the thing and also printed something".
// A test that only checked the output would have passed against the bug for
// every command whose output happens to look like usage.
func TestHelpNeverPerformsAnAction(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Errorf("--help reached the control plane: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	for _, cmd := range commandsInRootHelp(t) {
		// Both the bare command and one with a subcommand and flags after it:
		// the bug needed a non-empty argument list to show itself, so `navarch
		// <cmd> --help` alone would have caught only some of it.
		for _, args := range [][]string{
			{cmd, "--help"},
			{cmd, "create", "--help"},
			{cmd, "create", "some-name", "--help", "--role", "member"},
			{"--help", cmd, "create"},
		} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				var out, errb bytes.Buffer
				full := append([]string{"--url", srv.URL, "--token", "tok"}, args...)
				if code := Run(full, &out, &errb); code != 0 {
					t.Fatalf("exit %d; --help must succeed: %s", code, errb.String())
				}
				if out.Len() == 0 {
					t.Fatal("--help printed nothing")
				}
				if !strings.Contains(out.String(), "navarch") {
					t.Fatalf("--help printed something that is not help:\n%s", out.String())
				}
			})
		}
	}
	if requests != 0 {
		t.Fatalf("--help made %d request(s)", requests)
	}
}

// Every command in the help text needs a synopsis, or `navarch <cmd> --help`
// silently falls back to the root help and says nothing about the command the
// person asked about.
func TestEveryCommandHasASynopsis(t *testing.T) {
	for _, cmd := range commandsInRootHelp(t) {
		if _, ok := commandUsage[cmd]; !ok {
			t.Errorf("%q is in rootHelp but has no commandUsage entry", cmd)
		}
	}
	// And the reverse for the aliases, which are not in the help text: every
	// synopsis must name a real command, so a rename cannot leave a stale entry
	// answering --help for something that no longer exists.
	var out, errb bytes.Buffer
	for cmd := range commandUsage {
		out.Reset()
		errb.Reset()
		if code := Run([]string{cmd, "--help"}, &out, &errb); code != 0 {
			t.Errorf("commandUsage has %q, but `navarch %s --help` exits %d", cmd, cmd, code)
		}
	}
}

// The synopsis a command prints when it is given nothing usable must be the one
// --help prints. They were separate string literals, twice over per command.
func TestUsageAndHelpAgree(t *testing.T) {
	for _, cmd := range []string{"org", "token", "invite", "access", "node", "secret", "member"} {
		var helpOut, helpErr bytes.Buffer
		if code := Run([]string{cmd, "--help"}, &helpOut, &helpErr); code != 0 {
			t.Fatalf("%s --help exit %d", cmd, code)
		}
		var bareOut, bareErr bytes.Buffer
		if code := Run([]string{cmd}, &bareOut, &bareErr); code != 2 {
			t.Fatalf("bare %s exit %d, want a usage error", cmd, code)
		}
		if strings.TrimSpace(helpOut.String()) != strings.TrimSpace(bareErr.String()) {
			t.Errorf("%s: --help says %q but the usage error says %q",
				cmd, strings.TrimSpace(helpOut.String()), strings.TrimSpace(bareErr.String()))
		}
	}
}
