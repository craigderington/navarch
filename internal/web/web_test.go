package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A fake control plane, so these tests exercise the console rather than the
// platform. What matters here is what the console does with an answer, not
// whether Postgres is up.
func fakeAPI(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/whoami":
			io.WriteString(w, `{"operator":{"id":"op1","email":"ada@example.com","name":"Ada"},
				"organizations":[{"id":"org1","slug":"acme","name":"Acme"}]}`)
		case strings.HasPrefix(r.URL.Path, "/v1/nodes"):
			io.WriteString(w, `{"nodes":[{"id":"n1","org_id":"org1","hostname":"node-a",
				"advertise_addr":"10.0.0.1","state":"ready","cpu_millis":4000,"memory_bytes":8589934592,
				"alloc_cpu_millis":1000,"alloc_memory_bytes":1073741824,"agent_version":"1.0.0"}]}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
}

func login(t *testing.T, s *Server, token string) string {
	t.Helper()
	form := url.Values{"token": {token}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	return ""
}

// The console holds the token; the browser gets a cookie that is useless
// anywhere else. This is the reason the console is a server rather than a
// bundle of JavaScript, so it is the thing most worth pinning.
func TestTheBrowserNeverReceivesTheToken(t *testing.T) {
	api := fakeAPI(t, "secret-operator-token")
	defer api.Close()
	s, err := New(api.URL, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	form := url.Values{"token": {"secret-operator-token"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected a redirect after login, got %d", res.StatusCode)
	}
	raw, _ := io.ReadAll(res.Body)
	if strings.Contains(string(raw)+rec.Header().Get("Set-Cookie"), "secret-operator-token") {
		t.Fatal("the operator token reached the browser")
	}
	var sess *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie was set")
	}
	if !sess.HttpOnly {
		t.Fatal("the session cookie must be HttpOnly — a script must not be able to read it")
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Fatal("the session cookie must be SameSite=Lax")
	}

	// And the page it lands on carries no token either.
	page := httptest.NewRequest(http.MethodGet, "/", nil)
	page.AddCookie(sess)
	prec := httptest.NewRecorder()
	s.ServeHTTP(prec, page)
	if strings.Contains(prec.Body.String(), "secret-operator-token") {
		t.Fatal("a rendered page contained the operator token")
	}
}

// Verify before you store, exactly as `navarch login` does. A session holding a
// token the control plane rejects makes every later page fail with nothing
// pointing back at the step that said it worked.
func TestLoginVerifiesBeforeCreatingASession(t *testing.T) {
	api := fakeAPI(t, "good")
	defer api.Close()
	s, err := New(api.URL, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if id := login(t, s, "bad"); id != "" {
		t.Fatal("a rejected token created a session")
	}
	if id := login(t, s, "good"); id == "" {
		t.Fatal("a valid token did not create a session")
	}
}

func TestPagesRequireASession(t *testing.T) {
	api := fakeAPI(t, "good")
	defer api.Close()
	s, _ := New(api.URL, discard())
	for _, path := range []string{"/", "/orgs/org1", "/orgs/org1/events", "/envs/e1", "/deployments/d1"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s without a session returned %d, want a redirect to /login", path, rec.Code)
		}
	}
}

// Every page defines `content`; Go templates share one namespace, so parsing
// them into a single set makes the last file parsed win and every page render
// the same body. It fails *quietly* — layout renders, tables are empty — which
// is why this asserts on content rather than on status.
func TestEachPageRendersItsOwnBody(t *testing.T) {
	api := fakeAPI(t, "good")
	defer api.Close()
	s, _ := New(api.URL, discard())
	id := login(t, s, "good")
	if id == "" {
		t.Fatal("login failed")
	}
	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	fleet := get("/")
	if !strings.Contains(fleet, "node-a") {
		t.Fatalf("the fleet page did not render its node:\n%s", truncate(fleet))
	}
	if !strings.Contains(fleet, "ada@example.com") {
		t.Fatal("the layout did not render who is signed in")
	}
	// The heading is the discriminator. A node hostname is not: the
	// environments page legitimately shows one in its home-node column.
	envs := get("/orgs/org1")
	if !strings.Contains(envs, "<h1>Environments</h1>") || strings.Contains(envs, "<h1>Fleet</h1>") {
		t.Fatal("the environments page rendered the fleet page's body — the template sets collided")
	}
}

// The console renders operator-supplied strings — hostnames, failure reasons,
// event messages. html/template escapes by construction, and this is the test
// that says so out loud rather than trusting it.
func TestRenderedValuesAreEscaped(t *testing.T) {
	nasty := `</td><script>alert(1)</script>`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/whoami" {
			io.WriteString(w, `{"operator":{"id":"op1","email":"ada@example.com"},
				"organizations":[{"id":"org1","slug":"acme"}]}`)
			return
		}
		body, _ := json.Marshal(map[string]any{"nodes": []map[string]any{
			{"id": "n1", "org_id": "org1", "hostname": nasty, "state": "ready"},
		}})
		w.Write(body)
	}))
	defer api.Close()
	s, _ := New(api.URL, discard())
	id := login(t, s, "anything")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatal("a hostname from the API was rendered unescaped")
	}
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
