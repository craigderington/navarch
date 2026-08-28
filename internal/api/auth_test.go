package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The shared token used to open every operator route. It now opens exactly two
// machine-to-machine paths, and this is the test that says so: a case asserting
// it still reaches an ordinary /v1 route is the regression, not a failure.
//
// The Server here has no store, so the operator-token branch cannot resolve
// anyone and answers no — which is what makes every "should be refused" case
// below meaningful without a database.
func TestSharedTokenOpensOnlyTheMachinePaths(t *testing.T) {
	mux := http.NewServeMux()
	for _, p := range []string{"GET /v1/check", "POST /v1/nodes/register", "GET /metrics"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &Server{mux: mux, bearerToken: "correct-token"}

	tests := []struct {
		name   string
		method string
		path   string
		auth   string
		status int
	}{
		{name: "missing", method: "GET", path: "/v1/check", status: http.StatusUnauthorized},
		{name: "wrong", method: "GET", path: "/v1/check", auth: "Bearer wrong-token", status: http.StatusUnauthorized},
		{name: "wrong scheme", method: "GET", path: "/v1/check", auth: "Basic correct-token", status: http.StatusUnauthorized},

		// The demotion itself. Before operator identity this was a 204.
		{name: "shared token no longer opens an operator route", method: "GET", path: "/v1/check",
			auth: "Bearer correct-token", status: http.StatusUnauthorized},

		// The two it still opens. A node has no identity of its own until it
		// has registered, and a metrics scraper is not a person.
		{name: "node registration", method: "POST", path: "/v1/nodes/register",
			auth: "Bearer correct-token", status: http.StatusNoContent},
		{name: "metrics", method: "GET", path: "/metrics",
			auth: "Bearer correct-token", status: http.StatusNoContent},

		// Still refused without the token, so the exemption is on the
		// credential and not on the path.
		{name: "registration still needs a token", method: "POST", path: "/v1/nodes/register",
			status: http.StatusUnauthorized},
		{name: "metrics still needs a token", method: "GET", path: "/metrics",
			status: http.StatusUnauthorized},

		{name: "health is public", method: "GET", path: "/healthz", status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("got %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			if tt.status == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 response is missing WWW-Authenticate")
			}
		})
	}
}

// isServicePath matches exactly, never by prefix — a prefix test would hand the
// shared token every route beginning /v1/nodes/, which is most of the agent
// surface it was just removed from.
func TestServicePathMatchesExactly(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/nodes/register", true},
		{"GET", "/metrics", true},
		{"GET", "/v1/nodes/register", false}, // wrong method
		{"POST", "/v1/nodes/register/x", false},
		{"POST", "/v1/nodes", false},
		{"POST", "/metrics", false},
		{"GET", "/metrics/extra", false},
		{"GET", "/v1/orgs", false},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if got := isServicePath(req); got != tc.want {
			t.Fatalf("isServicePath(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestValidBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs", nil)
	req.Header.Set("Authorization", "Bearer correct-token")
	if !validBearerToken(req, "correct-token") {
		t.Fatal("correct bearer token was rejected")
	}
}
