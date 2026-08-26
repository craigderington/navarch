package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// secretSetServer records the last secret value the CLI put on the wire and
// answers 200, so tests assert exactly what the control plane would receive.
// The env reference is a raw UUID: the resolver returns ids without a single
// request (its documented contract), so the test needs only the one route.
func secretSetServer(t *testing.T) (*struct {
	Key, Value string
}, *httptest.Server) {
	t.Helper()
	got := &struct{ Key, Value string }{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Key, Value string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		got.Key, got.Value = body.Key, body.Value
		_ = json.NewEncoder(w).Encode(map[string]string{"key": body.Key})
	}))
	t.Cleanup(srv.Close)
	return got, srv
}

// The @file form exists so the secret value never lands in shell history or
// ps output — the audit's S6. The value on the wire must be the file's
// content with the trailing newline stripped, because that newline is echo's
// artifact, not part of the secret.
func TestSecretSetAtFile(t *testing.T) {
	got, srv := secretSetServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "pw")
	if err := os.WriteFile(file, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"secret", "set", "--env", uuid.NewString(), "db_password", "@" + file}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got.Key != "db_password" || got.Value != "hunter2" {
		t.Fatalf("wire value = %q/%q, want db_password/hunter2 (newline stripped)", got.Key, got.Value)
	}
	if strings.Contains(errb.String(), "warning") {
		t.Fatalf("the @file form must not warn:\n%s", errb.String())
	}
}

// The legacy positional form keeps working — scripts depend on it — but it
// is the one place a secret value is handled casually, so it warns.
func TestSecretSetPositionalWarns(t *testing.T) {
	got, srv := secretSetServer(t)
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"secret", "set", "--env", uuid.NewString(), "db_password", "hunter2"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got.Value != "hunter2" {
		t.Fatalf("wire value = %q, want hunter2", got.Value)
	}
	if !strings.Contains(errb.String(), "warning") {
		t.Fatalf("positional form must warn about exposure:\n%s", errb.String())
	}
}

// An empty secret is almost always a botched substitution ($(cat empty)),
// and it fails far away — at container start, with a blank password. Fail
// it here instead.
func TestSecretSetEmptyIsRejected(t *testing.T) {
	_, srv := secretSetServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "empty")
	if err := os.WriteFile(file, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"secret", "set", "--env", uuid.NewString(), "db_password", "@" + file}, &out, &errb)
	if code != 1 {
		t.Fatalf("empty secret must be a runtime error, got exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "empty") {
		t.Fatalf("expected an empty-secret error, got: %s", errb.String())
	}
}
