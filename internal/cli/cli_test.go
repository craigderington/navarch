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
	if !strings.Contains(out.String(), "navarch") || !strings.Contains(out.String(), "validate") {
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
	// The org is given as an id, not the "org-1" placeholder this used to pass:
	// a non-id value is now a slug and would be resolved, turning a test about
	// argv parsing into a test about resolution. Nothing regressed by that —
	// a non-UUID sent raw was always rejected by the server's pathUUID.
	const orgID = "11111111-1111-1111-1111-111111111111"
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = r.URL.String()
		_, _ = w.Write([]byte(`{"applications":[]}`))
	}))
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t", "app", "list", "--org", orgID}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(saw, "/v1/orgs/"+orgID+"/apps") {
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
	if !strings.Contains(out.String(), "navarch") {
		t.Fatalf("version: %s", out.String())
	}
}

// The rename keeps COMPOSECTL_* working at lower precedence. Both halves need
// covering: that the legacy name is still honoured at all, and that it loses to
// the new one when both are set — a fallback that silently wins would make the
// rename look applied while the old value is what reaches the wire.
func TestEnvPrecedenceAcrossTheRename(t *testing.T) {
	// Isolate from the developer's real config file and environment.
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		envURL, envToken, envTokenFile, envAgentToken, envConfigPath,
		envURLLegacy, envTokenLegacy, envTokenFileLegacy, envAgentTokenLegacy, envConfigPathLegacy,
	} {
		t.Setenv(k, "")
	}

	tests := []struct {
		name      string
		set       map[string]string
		wantURL   string
		wantToken string
	}{
		{
			name:      "legacy names still work",
			set:       map[string]string{envURLLegacy: "http://legacy:1", envTokenLegacy: "legacy-tok"},
			wantURL:   "http://legacy:1",
			wantToken: "legacy-tok",
		},
		{
			name:      "new names win over legacy",
			set:       map[string]string{envURL: "http://new:2", envURLLegacy: "http://legacy:1", envToken: "new-tok", envTokenLegacy: "legacy-tok"},
			wantURL:   "http://new:2",
			wantToken: "new-tok",
		},
		{
			name:      "dedicated token outranks the shared stack token",
			set:       map[string]string{envAgentToken: "agent-tok", envTokenLegacy: "legacy-tok"},
			wantURL:   defaultURL,
			wantToken: "legacy-tok",
		},
		{
			name:      "shared stack token is used when no dedicated one is set",
			set:       map[string]string{envAgentTokenLegacy: "legacy-agent-tok"},
			wantURL:   defaultURL,
			wantToken: "legacy-agent-tok",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.set {
				t.Setenv(k, v)
			}
			cfg, err := resolveConfig(Config{})
			if err != nil {
				t.Fatalf("resolveConfig: %v", err)
			}
			if cfg.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", cfg.URL, tt.wantURL)
			}
			if cfg.Token != tt.wantToken {
				t.Errorf("Token = %q, want %q", cfg.Token, tt.wantToken)
			}
		})
	}
}
