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
	"github.com/craig/composectl/internal/store"
)

// env8 is part of the generated hostname because {slug}-{stack} alone is not
// unique: stacks.slug is only UNIQUE (app_id, slug), so two apps — in the same
// org or different ones — each with a stack "main" and a preview "pr-1" would
// generate the same hostname, and the router would pick between them
// arbitrarily. That is a cross-tenant misroute into someone else's branch with
// inherited secrets, so the environment's own id has to be in the name.
func TestPreviewHostnameGeneration(t *testing.T) {
	got := previewHostname("pr-142", "hello", "93fa144e", "preview.localhost")
	if got != "pr-142-hello-93fa144e.preview.localhost" {
		t.Errorf("got %q", got)
	}
}

// Two previews that agree on slug and stack slug must still differ, or the
// collision this format exists to prevent is back.
func TestPreviewHostnameDisambiguatesIdenticalSlugs(t *testing.T) {
	a := previewHostname("pr-1", "main", "93fa144e", "preview.localhost")
	b := previewHostname("pr-1", "main", "0b17c3d2", "preview.localhost")
	if a == b {
		t.Fatalf("identical slug+stack in different environments must not collide, both %q", a)
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

// The check must measure what the label actually becomes, env8 suffix
// included. Counting only slug+stack is wrong in the lenient direction: it
// admits names that resolvers then truncate, which is exactly the failure
// mode the limit exists to catch.
//
// 48 + 1 + len("hello") + 1 + 8 == 63 exactly, so 48 is the last accepted
// slug length and 49 must be refused.
func TestPreviewHostnameLabelAccountsForEnv8(t *testing.T) {
	if err := validatePreviewLabel(strings.Repeat("a", 48), "hello"); err != nil {
		t.Errorf("a label that is exactly 63 characters with env8 must be accepted: %v", err)
	}
	if err := validatePreviewLabel(strings.Repeat("a", 49), "hello"); err == nil {
		t.Fatal("a label that only exceeds 63 characters once env8 is appended must be rejected")
	}
}

// The hostname the API reports must be the one env8 was derived from — the
// handler generates the environment id itself so it can build the hostname
// before the row exists, and a mismatch there would mean Traefik routes a
// name no container's labels agree with.
func TestCreatePreviewHostnameCarriesItsOwnEnv8(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStack(t, srv)

	body, _ := json.Marshal(map[string]any{"slug": "pr-env8"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/stacks/"+stackID+"/previews", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		EnvironmentID string `json:"environment_id"`
		Hostname      string `json:"hostname"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	env8 := resp.EnvironmentID[:8]
	if !strings.HasPrefix(resp.Hostname, "pr-env8-") {
		t.Errorf("hostname %q must start with the preview slug", resp.Hostname)
	}
	if !strings.Contains(resp.Hostname, "-"+env8+".") {
		t.Errorf("hostname %q must carry env8 %q of the environment it created", resp.Hostname, env8)
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

// newTestStackNeedingSecret is newTestStack's counterpart for the 422
// fail-fast: its spec references ${secret:name}, so a preview of it is only
// viable if that key is actually inherited. It also creates a "staging"
// environment to inherit from, deliberately holding a *different* key, so a
// source that exists but lacks the required secret can be exercised too.
func newTestStackNeedingSecret(t *testing.T, srv *Server) (stackID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	slug := "needsecret-" + uuid.NewString()[:8]
	org, err := srv.st.CreateOrganization(ctx, slug, "Preview Secret Test")
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
	dspec := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"web": {Name: "web", Image: "nginx:alpine", Swappable: true,
				SecretEnv: map[string]string{"WHOAMI_NAME": "${secret:name}"},
				Limits:    spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
	if _, err := srv.st.CreateStackVersion(ctx, stack.ID, "raw", dspec, "t"); err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	staging, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		StackID: stack.ID, Slug: "staging"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	// Some other key, so the source is a real environment with real secrets
	// and the 422 is about the missing one, not about an empty source.
	if err := srv.st.SetSecret(ctx, staging.ID, "unrelated", []byte("ct"), "age1x"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM environments WHERE stack_id=$1`, stack.ID)
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})
	return stack.ID
}

// countEnvs reads the catalog through the public surface, which is what the
// user would see -- a half-built preview left behind is only a problem
// because it shows up here and in the reaper's queue.
func countEnvs(t *testing.T, srv *Server, stackID uuid.UUID) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stacks/"+stackID.String()+"/envs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list envs: want 200, got %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Environments []struct {
			ID string `json:"id"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode env list: %v", err)
	}
	return len(resp.Environments)
}

// The 422 is a stated design guarantee of this slice, not just an error code:
// the check runs *before* CreatePreview, so a preview whose secrets could
// never resolve is never built at all. Checking after creation would leave a
// half-built environment behind for the reaper to collect and the user to
// wonder about -- so asserting on the status alone would miss the half of
// this that matters.
func TestCreatePreviewMissingSecretIs422AndCreatesNothing(t *testing.T) {
	srv := testServer(t)
	stackID := newTestStackNeedingSecret(t, srv)
	before := countEnvs(t, srv, stackID)

	cases := []struct {
		name string
		body map[string]any
	}{
		// No inheritance at all: the preview would start with no secrets.
		{"no source", map[string]any{"slug": "pr-nosrc"}},
		// A source that exists but never set the key the spec needs.
		{"source lacking the key", map[string]any{
			"slug": "pr-wrongsrc", "inherit_secrets_from": "staging"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/v1/stacks/"+stackID.String()+"/previews", strings.NewReader(string(body))))

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"name"`) {
				t.Errorf("the response must name the missing secret, got %s", rec.Body)
			}
			if got := countEnvs(t, srv, stackID); got != before {
				t.Errorf("a rejected preview must create no environment: %d before, %d after", before, got)
			}
		})
	}
}

// newNodeWithReapedPreview builds a fresh org with a stack, registers a node
// in it, creates a preview, ages the preview past its TTL, and reaps it --
// leaving a tombstone in that org for the node to pick up. TombstonesForNode
// is org-scoped, so the node and the preview must share an org for the
// assertion to mean anything.
func newNodeWithReapedPreview(t *testing.T, srv *Server) (nodeID, env8 string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	slug := "teardown-" + uuid.NewString()[:8]
	org, err := srv.st.CreateOrganization(ctx, slug, "Teardown Test")
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
	dspec := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"web": {Name: "web", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
	sv, err := srv.st.CreateStackVersion(ctx, stack.ID, "raw", dspec, "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}

	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: "teardown-" + uuid.NewString()[:8],
		AdvertiseAddr: "10.1.2.3", CPUMillis: 2000, MemoryBytes: 1 << 31,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	env, _, err := srv.st.CreatePreview(ctx, store.CreatePreviewParams{
		StackID: stack.ID, Slug: "pr-" + uuid.NewString()[:8],
		Hostname: uuid.NewString()[:8] + ".preview.localhost",
		TTL:      time.Hour, StackVersionID: sv.ID, ResolvedSpec: sv.Spec,
	})
	if err != nil {
		t.Fatalf("CreatePreview: %v", err)
	}

	if _, err := srv.st.Pool().Exec(ctx,
		`UPDATE environments SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		env.ID); err != nil {
		t.Fatalf("age the preview: %v", err)
	}
	if _, err := srv.st.ExpireEnvironmentsForOrg(ctx, org.ID); err != nil {
		t.Fatalf("ExpireEnvironments: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The node has no ON DELETE CASCADE from service_instances, and
		// deployments has none from stack_versions -- delete bottom-up:
		// environments (cascades deployments/instances), then the node,
		// then the org.
		srv.st.Pool().Exec(c, `DELETE FROM environments WHERE stack_id IN (
			SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`, org.ID)
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})

	return node.ID.String(), env.ID.String()[:8]
}

func TestDesiredStateIncludesTeardownEnvs(t *testing.T) {
	srv := testServer(t)
	nodeID, env8 := newNodeWithReapedPreview(t, srv) // helper below

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes/"+nodeID+"/desired-state", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		TeardownEnvs []string `json:"teardown_envs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, e := range resp.TeardownEnvs {
		if e == env8 {
			found = true
		}
	}
	if !found {
		t.Errorf("want %s in teardown_envs, got %v", env8, resp.TeardownEnvs)
	}
}

// newNodeInFreshOrg registers a node in its own org with no stack and no
// preview -- just enough to prove a node with no relationship to org A's
// tombstone can call desired-state at all.
func newNodeInFreshOrg(t *testing.T, srv *Server) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	slug := "teardown-b-" + uuid.NewString()[:8]
	org, err := srv.st.CreateOrganization(ctx, slug, "Teardown Test Org B")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: "teardown-b-" + uuid.NewString()[:8],
		AdvertiseAddr: "10.1.2.4", CPUMillis: 2000, MemoryBytes: 1 << 31,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// No stack/environments here, so node then org is already bottom-up.
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})
	return node.ID.String()
}

// TestDesiredStateExcludesOtherOrgsTeardownEnvs pins the org-scoping join in
// TombstonesForNode: a node must only ever be told to destroy environments
// its own org could have been running. The endpoint hands out destructive
// instructions (tear down pinned containers and named volumes), so a
// cross-org leak here is a tenant-isolation bug, not an ordinary miss.
func TestDesiredStateExcludesOtherOrgsTeardownEnvs(t *testing.T) {
	srv := testServer(t)
	nodeA, env8 := newNodeWithReapedPreview(t, srv)
	nodeB := newNodeInFreshOrg(t, srv)

	// Confirm org A's own node still sees the tombstone first. Without this,
	// a passing assertion below could mean either "scoping works" or
	// "the tombstone doesn't exist" -- and only the first is what this test
	// claims to prove.
	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, httptest.NewRequest(http.MethodGet, "/v1/nodes/"+nodeA+"/desired-state", nil))
	if recA.Code != http.StatusOK {
		t.Fatalf("org A desired-state: want 200, got %d: %s", recA.Code, recA.Body)
	}
	var respA struct {
		TeardownEnvs []string `json:"teardown_envs"`
	}
	if err := json.Unmarshal(recA.Body.Bytes(), &respA); err != nil {
		t.Fatalf("decode org A response: %v", err)
	}
	orgASeesIt := false
	for _, e := range respA.TeardownEnvs {
		if e == env8 {
			orgASeesIt = true
		}
	}
	if !orgASeesIt {
		t.Fatalf("org A's own node must still see tombstone %s (got %v); "+
			"otherwise the negative assertion below is vacuous", env8, respA.TeardownEnvs)
	}

	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, httptest.NewRequest(http.MethodGet, "/v1/nodes/"+nodeB+"/desired-state", nil))
	if recB.Code != http.StatusOK {
		t.Fatalf("org B desired-state: want 200, got %d: %s", recB.Code, recB.Body)
	}
	var respB struct {
		TeardownEnvs []string `json:"teardown_envs"`
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &respB); err != nil {
		t.Fatalf("decode org B response: %v", err)
	}
	for _, e := range respB.TeardownEnvs {
		if e == env8 {
			t.Errorf("org B's node must not see org A's tombstone %s, got %v", env8, respB.TeardownEnvs)
		}
	}
}
