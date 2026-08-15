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
)

func TestHelpAndUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"--help"}, &out, &errb); code != 0 {
		t.Fatalf("help exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "composectl") || !strings.Contains(out.String(), "validate") {
		t.Fatalf("help missing commands:\n%s", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
}

func TestHealthJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "tok", "--output", "json", "health"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"status": "ok"`) {
		t.Fatalf("output: %s", out.String())
	}
}

func TestTokenFileAndAPIErrorExit(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer file-token" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
	}))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token-file", tok, "health"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; err=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "authentication required") {
		t.Fatalf("stderr: %s", errb.String())
	}
}

func TestValidateMissingFileIsUsageOrError(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"validate"}, &out, &errb); code != 2 {
		t.Fatalf("missing file should be usage (2), got %d: %s", code, errb.String())
	}
}

func TestOrgListTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orgs" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"organizations":[{"id":"aaaa","slug":"dev","name":"Development"}]}`))
	}))
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t", "org", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "dev") || !strings.Contains(out.String(), "SLUG") {
		t.Fatalf("table:\n%s", out.String())
	}
}

func TestCommandFlagsAreNotSwallowedAsGlobals(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.URL.String()
		_, _ = w.Write([]byte(`{"applications":[]}`))
	}))
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t", "app", "list", "--org", "org-1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(saw, "/v1/orgs/org-1/apps") {
		t.Fatalf("request %s", saw)
	}
}

func TestUnknownFlagIsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"--explode"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "composectl") {
		t.Fatalf("version: %s", out.String())
	}
}
