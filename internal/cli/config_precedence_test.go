package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeStoredConfig puts a config file where loadConfigFile will find it, which
// is what an operator who has ever run `navarch login` has.
func writeStoredConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	dir := filepath.Join(home, ".config", "navarch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tokenSeenBy runs a command against a server that records the credential it
// received, and returns it. Asserting on the wire rather than on the resolved
// Config, because what matters is which token the request actually carried.
func tokenSeenBy(t *testing.T, args ...string) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	full := append([]string{"--url", srv.URL}, args...)
	if code := Run(append(full, "health"), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	return got
}

// A credential named on the command line must beat one sitting in the config
// file, in both directions.
//
// --token already did, by accident: the combine step asked whether a token was
// set, and a flag token is. --token-file did NOT, because a `token:` in the
// config file is already non-empty by the time the flag is looked at, so the
// stored credential won and the file the operator pointed at was never read.
// Nothing reported it — the request simply went out as somebody else.
func TestACredentialOnTheCommandLineBeatsAStoredOne(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("--token-file over a stored token", func(t *testing.T) {
		writeStoredConfig(t, "token: stored-token\n")
		if got := tokenSeenBy(t, "--token-file", tokenPath); got != "Bearer from-the-file" {
			t.Fatalf("sent %q, want the file the operator named", got)
		}
	})

	t.Run("--token over a stored token_file", func(t *testing.T) {
		writeStoredConfig(t, "token_file: "+tokenPath+"\n")
		if got := tokenSeenBy(t, "--token", "flag-token"); got != "Bearer flag-token" {
			t.Fatalf("sent %q, want the flag", got)
		}
	})

	// A stale path in the config file must not be read at all once the command
	// line has named a credential — otherwise the failure is an unreadable-file
	// error about a setting the operator has just overridden.
	t.Run("a stored token_file that no longer exists is not read", func(t *testing.T) {
		writeStoredConfig(t, "token_file: /nonexistent/navarch-token\n")
		if got := tokenSeenBy(t, "--token", "flag-token"); got != "Bearer flag-token" {
			t.Fatalf("sent %q, want the flag", got)
		}
	})
}

// The environment sits between the file and the flags, and the same rule holds
// there: naming a token file in the environment must beat a token stored on
// disk, and a flag must beat both.
func TestTheEnvironmentBeatsTheFileAndLosesToTheFlags(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env-token")
	flagPath := filepath.Join(dir, "flag-token")
	if err := os.WriteFile(envPath, []byte("from-env-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagPath, []byte("from-flag-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("NAVARCH_TOKEN_FILE over a stored token", func(t *testing.T) {
		writeStoredConfig(t, "token: stored-token\n")
		t.Setenv(envTokenFile, envPath)
		if got := tokenSeenBy(t); got != "Bearer from-env-file" {
			t.Fatalf("sent %q, want the environment's file", got)
		}
	})

	t.Run("--token-file over NAVARCH_TOKEN", func(t *testing.T) {
		writeStoredConfig(t, "token: stored-token\n")
		t.Setenv(envToken, "env-token")
		if got := tokenSeenBy(t, "--token-file", flagPath); got != "Bearer from-flag-file" {
			t.Fatalf("sent %q, want the flag's file", got)
		}
	})

	// Within one tier the direct value still beats the indirection, which is
	// the pre-existing reading and the one nobody has to think about.
	t.Run("within a tier the direct token wins", func(t *testing.T) {
		writeStoredConfig(t, "")
		t.Setenv(envToken, "env-token")
		t.Setenv(envTokenFile, envPath)
		if got := tokenSeenBy(t); got != "Bearer env-token" {
			t.Fatalf("sent %q, want the direct value", got)
		}
	})
}
