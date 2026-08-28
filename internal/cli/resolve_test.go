package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/craigderington/navarch/internal/client"
)

// catalogServer answers the list endpoints resolution walks, and records every
// path it was asked for so a test can assert on the requests themselves — the
// point of resolution is which calls happen, not only what comes back.
func catalogServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	const (
		orgID   = "11111111-1111-1111-1111-111111111111"
		appID   = "22222222-2222-2222-2222-222222222222"
		stackID = "33333333-3333-3333-3333-333333333333"
		envID   = "44444444-4444-4444-4444-444444444444"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/v1/orgs":
			write(map[string]any{"organizations": []map[string]any{
				{"id": orgID, "slug": "dev"},
				{"id": "99999999-9999-9999-9999-999999999999", "slug": "other"},
			}})
		case r.URL.Path == "/v1/orgs/"+orgID+"/apps":
			write(map[string]any{"applications": []map[string]any{{"id": appID, "slug": "preview", "org_id": orgID}}})
		case r.URL.Path == "/v1/apps/"+appID+"/stacks":
			write(map[string]any{"stacks": []map[string]any{{"id": stackID, "slug": "main", "app_id": appID}}})
		case r.URL.Path == "/v1/stacks/"+stackID+"/envs":
			write(map[string]any{"environments": []map[string]any{{"id": envID, "slug": "staging"}}})
		case r.URL.Path == "/v1/orgs/"+orgID+"/nodes" || r.URL.Path == "/v1/nodes":
			write(map[string]any{"nodes": []map[string]any{
				{"id": "55555555-5555-5555-5555-555555555555", "hostname": "node-a"},
				{"id": "66666666-6666-6666-6666-666666666666", "hostname": "twin"},
				{"id": "77777777-7777-7777-7777-777777777777", "hostname": "twin"},
			}})
		case strings.HasPrefix(r.URL.Path, "/v1/envs/"):
			write(map[string]any{"id": envID, "slug": "staging"})
		case strings.HasPrefix(r.URL.Path, "/v1/stacks/"):
			write(map[string]any{"id": stackID, "slug": "main", "app_id": appID})
		default:
			w.WriteHeader(http.StatusNotFound)
			write(map[string]string{"error": "unexpected path " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newEnvFor(t *testing.T, url string) (env, *bytes.Buffer) {
	t.Helper()
	c, err := client.New(url, "tok")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	var out bytes.Buffer
	return env{cfg: Config{Output: "json"}, c: c, out: &out, err: &out}, &out
}

func TestResolveWalksTheSlugPath(t *testing.T) {
	var seen []string
	srv := catalogServer(t, &seen)
	e, _ := newEnvFor(t, srv.URL)

	id, err := e.resolveEnv(t.Context(), "dev/preview/main/staging")
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if id != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("resolved to %q", id)
	}
	// One request per level, in hierarchy order — no redundant refetching.
	want := 4
	if len(seen) != want {
		t.Fatalf("expected %d lookups, got %d: %v", want, len(seen), seen)
	}
}

// The property that keeps existing scripts fast and unchanged: a reference that
// is already an id must not trigger a single resolution request.
func TestResolveIDCostsNoRequests(t *testing.T) {
	var seen []string
	srv := catalogServer(t, &seen)
	e, _ := newEnvFor(t, srv.URL)

	const id = "44444444-4444-4444-4444-444444444444"
	got, err := e.resolveEnv(t.Context(), id)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if got != id {
		t.Fatalf("id was rewritten to %q", got)
	}
	if len(seen) != 0 {
		t.Fatalf("an id must resolve without any request, got %v", seen)
	}
}

// Mixed segments matter in practice: a script that already captured one id
// should still be able to name the rest.
func TestResolveAcceptsMixedIDAndSlugSegments(t *testing.T) {
	var seen []string
	srv := catalogServer(t, &seen)
	e, _ := newEnvFor(t, srv.URL)

	id, err := e.resolveStack(t.Context(), "11111111-1111-1111-1111-111111111111/preview/main")
	if err != nil {
		t.Fatalf("resolveStack: %v", err)
	}
	if id != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("resolved to %q", id)
	}
	// The org segment was already an id, so only apps and stacks were listed.
	if len(seen) != 2 {
		t.Fatalf("expected 2 lookups, got %v", seen)
	}
}

func TestResolveErrors(t *testing.T) {
	var seen []string
	srv := catalogServer(t, &seen)
	e, _ := newEnvFor(t, srv.URL)

	tests := []struct {
		name      string
		run       func() (string, error)
		wantUsage bool
		contains  string
	}{
		{
			name:      "wrong depth is a usage error naming the shape",
			run:       func() (string, error) { return e.resolveEnv(t.Context(), "main/staging") },
			wantUsage: true,
			contains:  "ORG/APP/STACK/ENV",
		},
		{
			name:      "empty segment is rejected",
			run:       func() (string, error) { return e.resolveEnv(t.Context(), "dev//main/staging") },
			wantUsage: true,
			contains:  "empty path segment",
		},
		{
			name:     "unknown org names what was not found",
			run:      func() (string, error) { return e.resolveOrg(t.Context(), "nope") },
			contains: `no organization with slug "nope"`,
		},
		{
			name:     "unknown leaf names its parent path",
			run:      func() (string, error) { return e.resolveEnv(t.Context(), "dev/preview/main/nope") },
			contains: `no environment "nope" in stack "dev/preview/main"`,
		},
		{
			name:     "ambiguous hostname refuses to guess",
			run:      func() (string, error) { return e.resolveNode(t.Context(), "dev/twin") },
			contains: "matches 2 nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.run()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("error %q does not contain %q", err, tt.contains)
			}
			if isUsage(err) != tt.wantUsage {
				t.Fatalf("isUsage = %v, want %v (err: %v)", isUsage(err), tt.wantUsage, err)
			}
		})
	}
}

// A UUID is valid slug syntax, so the id/slug decision has to be made by
// parsing rather than by shape.
func TestIsIDRequiresCanonicalForm(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"44444444-4444-4444-4444-444444444444", true},
		{"dev", false},
		{"dev/preview", false},
		// Accepted by uuid.Parse, but indistinguishable from an ordinary slug —
		// treating it as an id would make that name unaddressable.
		{"44444444444444444444444444444444", false},
		{"urn:uuid:44444444-4444-4444-4444-444444444444", false},
		{"4444444444444444444444444444444g", false},
	}
	for _, tt := range tests {
		if got := isID(tt.in); got != tt.want {
			t.Errorf("isID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
