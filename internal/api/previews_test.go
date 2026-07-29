package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
)

func TestPreviewHostnameGeneration(t *testing.T) {
	got := previewHostname("pr-142", "hello", "preview.localhost")
	if got != "pr-142-hello.preview.localhost" {
		t.Errorf("got %q", got)
	}
}

// A DNS label over 63 characters is silently truncated by resolvers, which
// would route the preview nowhere. Reject at creation instead of failing at
// the first request.
func TestPreviewHostnameLabelTooLong(t *testing.T) {
	long := strings.Repeat("a", 60)
	if err := validatePreviewLabel(long, "hello"); err == nil {
		t.Fatal("a >63 character label must be rejected")
	}
	if err := validatePreviewLabel("pr-1", "hello"); err != nil {
		t.Errorf("a short label must be accepted: %v", err)
	}
}

func TestCreatePreviewRejectsExcessiveTTL(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStack(t, srv) // helper below

	body, _ := json.Marshal(map[string]any{
		"slug": "pr-9", "ttl_hours": 1000,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stacks/"+stackID+"/previews",
		strings.NewReader(string(body)))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a TTL above the cap, got %d: %s", rec.Code, rec.Body)
	}
}

func TestCreatePreviewUnknownInheritSourceIs404(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStack(t, srv)

	body, _ := json.Marshal(map[string]any{
		"slug": "pr-10", "inherit_secrets_from": "nope",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/stacks/"+stackID+"/previews",
		strings.NewReader(string(body)))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an unknown inherit source, got %d: %s", rec.Code, rec.Body)
	}
}

// newTestStack builds the org/application/stack/version chain a preview
// hangs off of, and nothing else — no node, no "prod" environment — since
// CreatePreview doesn't touch either. Cleanup mirrors the bottom-up order
// internal/rollout/reaper_test.go's previewStack uses: deployments carry an
// FK to stack_versions without ON DELETE CASCADE, so the environments (whose
// delete cascades their deployments/instances) must go before the org.
func newTestStack(t *testing.T, srv *Server) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	slug := "preview-" + uuid.NewString()[:8]
	org, err := srv.st.CreateOrganization(ctx, slug, "Preview Test")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	app, err := srv.st.CreateApplication(ctx, org.ID, slug, "app")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := srv.st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	// Minimal spec, no secrets: TestCreatePreviewRejectsExcessiveTTL must fail
	// on the TTL check (400), not on a required-secrets 422 first.
	dspec := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"web": {Name: "web", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
	if _, err := srv.st.CreateStackVersion(ctx, stack.ID, "raw", dspec, "t"); err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Environments cascade their deployments/instances on delete, so
		// dropping the org's environments first, then the org, is enough.
		srv.st.Pool().Exec(c, `DELETE FROM environments WHERE stack_id IN (
			SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`, org.ID)
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})

	return stack.ID.String()
}
