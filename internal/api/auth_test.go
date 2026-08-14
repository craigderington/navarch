package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerAuthentication(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/check", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &Server{mux: mux, bearerToken: "correct-token"}

	tests := []struct {
		name   string
		path   string
		auth   string
		status int
	}{
		{name: "missing", path: "/v1/check", status: http.StatusUnauthorized},
		{name: "wrong", path: "/v1/check", auth: "Bearer wrong-token", status: http.StatusUnauthorized},
		{name: "wrong scheme", path: "/v1/check", auth: "Basic correct-token", status: http.StatusUnauthorized},
		{name: "correct", path: "/v1/check", auth: "Bearer correct-token", status: http.StatusNoContent},
		{name: "health is public", path: "/healthz", status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
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

func TestValidBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs", nil)
	req.Header.Set("Authorization", "Bearer correct-token")
	if !validBearerToken(req, "correct-token") {
		t.Fatal("correct bearer token was rejected")
	}
}
