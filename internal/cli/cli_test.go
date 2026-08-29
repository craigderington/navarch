package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// flagMap consumes the argument after every flag it sees, which is correct for
// --tail 50 and wrong for --follow. A bare boolean at the end of the line — the
// natural place to put it — would otherwise fail with "requires a value", and
// one in the middle would silently eat the next argument.
func TestTakeBoolFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRest []string
		wantSet  bool
	}{
		{"absent", []string{"env", "--service", "api"}, []string{"env", "--service", "api"}, false},
		{"trailing", []string{"env", "--service", "api", "--follow"}, []string{"env", "--service", "api"}, true},
		{"in the middle, does not eat the next argument",
			[]string{"env", "--follow", "--service", "api"}, []string{"env", "--service", "api"}, true},
		{"short form", []string{"env", "-f"}, []string{"env"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, set := takeBoolFlag(tt.args, "follow", "f")
			if set != tt.wantSet {
				t.Fatalf("set = %v, want %v", set, tt.wantSet)
			}
			if strings.Join(rest, " ") != strings.Join(tt.wantRest, " ") {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
			// The surviving arguments must still parse, which is the whole point.
			if _, _, err := flagMap(rest); err != nil {
				t.Fatalf("remaining args do not parse: %v", err)
			}
		})
	}
}

// A bare "-" is a positional, not a flag.
//
// It parsed as a flag with an empty name, so the documented stdin form —
// `navarch secret set --env E KEY -`, which exists specifically to keep a
// secret out of shell history, `ps` and exec audit logs — failed with
// "flag -- requires a value". The only way through was to pass the value on
// argv, which is the handling S6 was closing.
func TestBareDashIsAPositionalNotAFlag(t *testing.T) {
	flags, pos, err := flagMap([]string{"--env", "dev/app/main/prod", "db_password", "-"})
	if err != nil {
		t.Fatalf("flagMap: %v", err)
	}
	if flags["env"] != "dev/app/main/prod" {
		t.Fatalf("--env was lost: %v", flags)
	}
	if len(pos) != 2 || pos[0] != "db_password" || pos[1] != "-" {
		t.Fatalf("expected [db_password -] as positionals, got %v", pos)
	}
	if _, ok := flags[""]; ok {
		t.Fatal(`"-" was recorded as a flag with an empty name`)
	}

	// A dash in any other position is still a positional, and real flags around
	// it keep working.
	flags, pos, err = flagMap([]string{"-", "--org", "dev"})
	if err != nil {
		t.Fatalf("flagMap (leading dash): %v", err)
	}
	if flags["org"] != "dev" || len(pos) != 1 || pos[0] != "-" {
		t.Fatalf("leading dash mishandled: flags=%v pos=%v", flags, pos)
	}

	// "--" still means "everything after this is positional", and "-x" is still
	// a flag — the fix is narrow.
	if _, pos, err = flagMap([]string{"--", "--not-a-flag"}); err != nil ||
		len(pos) != 1 || pos[0] != "--not-a-flag" {
		t.Fatalf(`"--" passthrough broke: pos=%v err=%v`, pos, err)
	}
	if flags, _, err = flagMap([]string{"--limit", "5"}); err != nil || flags["limit"] != "5" {
		t.Fatalf("ordinary flag parsing broke: %v %v", flags, err)
	}
}

// The operator token must not go on the wire in the clear to somewhere it can
// be read, and the refusal has to happen before the first request rather than
// after one has already carried it.
func TestGuardTransportRefusesPlaintextThatCanLeave(t *testing.T) {
	var errb bytes.Buffer

	// Contained URLs pass silently — the default and the dev stack among them.
	for _, u := range []string{"http://localhost:8417", "http://controlplane:8417", "https://navarch.example.com"} {
		errb.Reset()
		if err := guardTransport(u, &errb); err != nil {
			t.Fatalf("guardTransport(%q) = %v, want nil", u, err)
		}
		if errb.Len() != 0 {
			t.Fatalf("guardTransport(%q) warned unnecessarily: %s", u, errb.String())
		}
	}

	// A LAN address is refused, and the message names the way through.
	errb.Reset()
	err := guardTransport("http://10.0.1.7:8417", &errb)
	if err == nil {
		t.Fatal("plaintext to a private address must be refused")
	}
	if !strings.Contains(err.Error(), "NAVARCH_INSECURE") {
		t.Fatalf("the refusal must name the opt-in, got: %v", err)
	}

	// Opted in: allowed, and warned about every time rather than once.
	t.Setenv("NAVARCH_INSECURE", "1")
	for i := 0; i < 2; i++ {
		errb.Reset()
		if err := guardTransport("http://10.0.1.7:8417", &errb); err != nil {
			t.Fatalf("NAVARCH_INSECURE=1 should allow it: %v", err)
		}
		if !strings.Contains(errb.String(), "10.0.1.7") {
			t.Fatalf("call %d did not warn: %q", i, errb.String())
		}
	}

	// And it does not turn a broken URL into a working one.
	errb.Reset()
	if err := guardTransport("ftp://localhost:8417", &errb); err == nil {
		t.Fatal("the opt-in must not rescue an unsupported scheme")
	}
}

// login must verify before it writes. A config file holding a token that does
// not work is worse than none: every later command fails with 401 and nothing
// points back at the step that "succeeded".
func TestLoginVerifiesBeforeItSaves(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	t.Setenv("NAVARCH_CONFIG", cfgPath)

	var accept bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !accept || r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"operator":{"id":"1","email":"op@example.com","name":"Op"},`+
			`"organizations":[{"id":"o1","slug":"dev","name":"Dev"}]}`)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	// httptest binds 127.0.0.1, so the transport guard is satisfied without an
	// insecure opt-in — which is itself worth knowing: loopback stays usable.
	code := Run([]string{"login", "--url", srv.URL, "--token", "bad-token"}, &out, &errb)
	if code == 0 {
		t.Fatal("login accepted a token the control plane rejected")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("a rejected token must not be written to disk")
	}

	accept = true
	out.Reset()
	errb.Reset()
	if code := Run([]string{"login", "--url", srv.URL, "--token", "good-token"}, &out, &errb); code != 0 {
		t.Fatalf("login failed: %s %s", out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "op@example.com") {
		t.Fatalf("login should name who it logged in as, got %q", out.String())
	}

	// Stored, and stored privately — this is a bearer credential at rest.
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode is %o, want 600", perm)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "good-token") {
		t.Fatalf("token was not saved: %s", body)
	}

	// logout forgets it without touching the rest of the file.
	out.Reset()
	if code := Run([]string{"logout"}, &out, &errb); code != 0 {
		t.Fatalf("logout failed: %s", errb.String())
	}
	body, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(body), "good-token") {
		t.Fatalf("logout left the token behind: %s", body)
	}
	if !strings.Contains(string(body), srv.URL) {
		t.Fatalf("logout discarded the url too: %s", body)
	}
}

// Logging in must not quietly drop unrelated settings, or people stop trusting
// the file and go back to exporting variables.
func TestLoginPreservesOtherConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	t.Setenv("NAVARCH_CONFIG", cfgPath)
	if err := os.WriteFile(cfgPath, []byte("output: json\nurl: http://localhost:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveConfig("http://localhost:8417", "tok"); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "output: json") {
		t.Fatalf("saveConfig discarded an unrelated setting: %s", body)
	}
}
