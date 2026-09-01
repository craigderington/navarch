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

// A mutating request must have come from a page this console served.
//
// SameSite=Lax already blocks a cross-site POST in any current browser, so this
// is the second lock — and the one that does not depend on the browser being
// current, or on nobody later relaxing the cookie to None for an embed.
func TestActionsRequireTheCSRFToken(t *testing.T) {
	var called int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/whoami" {
			io.WriteString(w, `{"operator":{"id":"op1","email":"ada@example.com"},
				"organizations":[{"id":"org1","slug":"acme"}]}`)
			return
		}
		called++
		io.WriteString(w, `{"id":"d1","revision":3,"state":"pending"}`)
	}))
	defer api.Close()
	s, _ := New(api.URL, discard())
	id := login(t, s, "good")
	if id == "" {
		t.Fatal("login failed")
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/envs/e1/deploy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(""); rec.Code != http.StatusForbidden {
		t.Fatalf("a POST with no CSRF token returned %d, want 403", rec.Code)
	}
	if rec := post("csrf=wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("a POST with the wrong CSRF token returned %d, want 403", rec.Code)
	}
	if called != 0 {
		t.Fatal("a rejected request still reached the control plane")
	}

	// With the real token it goes through, so the guard is about the token and
	// not about actions never working.
	sess, _ := s.sessions.get(id)
	rec := post("csrf=" + sess.csrf)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid POST returned %d, want a redirect", rec.Code)
	}
	if called == 0 {
		t.Fatal("a valid POST never reached the control plane")
	}
}

// The `back` parameter must not be able to send somebody off this console —
// that is how a convenience turns into a phishing hop.
func TestBackRefusesAnOpenRedirect(t *testing.T) {
	for _, bad := range []string{"https://evil.example", "//evil.example", "javascript:alert(1)", ""} {
		if got := backTo(bad); got != "/" {
			t.Errorf("backTo(%q) = %q, want /", bad, got)
		}
	}
	for _, ok := range []string{"/", "/envs/e1", "/orgs/o1/events"} {
		if got := backTo(ok); got != ok {
			t.Errorf("backTo(%q) = %q, want it unchanged", ok, got)
		}
	}
}

// A failed action reports what the control plane said, next to the thing the
// operator was acting on — not on a dead-end error page mid-task.
func TestAFailedActionFlashesTheReason(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/whoami" {
			io.WriteString(w, `{"operator":{"id":"op1","email":"ada@example.com"},
				"organizations":[{"id":"org1","slug":"acme"}]}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"an active deployment already exists"}`)
	}))
	defer api.Close()
	s, _ := New(api.URL, discard())
	id := login(t, s, "good")
	sess, _ := s.sessions.get(id)

	req := httptest.NewRequest(http.MethodPost, "/envs/e1/deploy",
		strings.NewReader("csrf="+sess.csrf+"&back=/envs/e1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect after a failed action, got %d", rec.Code)
	}
	msg := s.sessions.takeFlash(id)
	if !strings.HasPrefix(msg, "✗") || !strings.Contains(msg, "active deployment") {
		t.Fatalf("the flash did not carry the control plane's reason: %q", msg)
	}
}

// Every template that uses the layout has to be registered in `pages`, or it is
// a runtime "no such template" and a blank page. A list is exactly the kind of
// thing that goes stale, so this walks the embedded files instead of trusting
// it — confirm.html was missing when actions first landed.
func TestEveryLayoutTemplateIsRegistered(t *testing.T) {
	parsed, err := parsePages()
	if err != nil {
		t.Fatalf("parsePages: %v", err)
	}
	files, err := templates.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, f := range files {
		name := f.Name()
		if name == "layout.html" {
			continue
		}
		if _, ok := parsed[name]; !ok {
			t.Errorf("%s exists but is not in `pages`, so rendering it is a blank page", name)
		}
	}
}

// A GET must not spend a single-use credential. Link previewers, mail scanners
// and browser prefetch all issue GETs nobody asked for, and the invitee would
// find their invitation already used before they ever saw the page.
func TestOpeningAnInviteLinkDoesNotRedeemIt(t *testing.T) {
	var redeems int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/invites/redeem" {
			redeems++
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"operator":{"id":"1","email":"ada@example.com"},"token":"nav_minted"}`)
	}))
	defer api.Close()

	srv, err := New(api.URL, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/invite?token=nav_the-invite", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /invite = %d", rec.Code)
	}
	if redeems != 0 {
		t.Fatalf("a GET redeemed the invitation %d time(s)", redeems)
	}
	// And the page must carry the invitation forward into the form, or the
	// button has nothing to submit.
	if !strings.Contains(rec.Body.String(), "nav_the-invite") {
		t.Fatalf("the acceptance form must carry the token:\n%s", rec.Body.String())
	}
}

// Accepting signs the person in with a session cookie, and the operator token
// the API minted must never reach the browser — the same property the console
// guarantees for login.
func TestAcceptingAnInviteSetsASessionAndNotAToken(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"operator":{"id":"1","email":"ada@example.com"},"token":"nav_minted-secret"}`)
	}))
	defer api.Close()

	srv, err := New(api.URL, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/invite", strings.NewReader("token=nav_the-invite"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("accept should redirect to the dashboard, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("accepting must establish a session")
	}
	if strings.Contains(rec.Body.String(), "nav_minted-secret") ||
		strings.Contains(cookie, "nav_minted-secret") {
		t.Fatal("the operator token must never reach the browser")
	}
}

// A dead invitation lands back on the page with an explanation rather than an
// error page, and the explanation must not narrow down which failure it was —
// the API deliberately does not say, and the console must not guess.
func TestARejectedInviteExplainsWithoutGuessing(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"not found"}`)
	}))
	defer api.Close()

	srv, err := New(api.URL, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/invite", strings.NewReader("token=nav_dead"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want a redirect back to the page", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/invite?error=") {
		t.Fatalf("should return to the invite page: %q", loc)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a rejected invitation must not establish a session")
	}
}
