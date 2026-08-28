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
)

// The catalog handlers are the path every deployment travels: org → app →
// stack → version → environment. Each level's error contract matters because
// the CLI resolves slug paths by walking exactly these routes — a 404 where a
// 400 belongs sends a user looking for the wrong bug. These tests drive the
// HTTP surface, not the store, because the store's own tests already cover
// its behaviour; what was untested is the decode/delegate/encode layer.

const goodCompose = `
services:
  api:
    image: nginx:alpine
    x-composectl:
      rollout: swap
`

func TestCatalogLifecycleOverHTTP(t *testing.T) {
	srv := testServer(t)
	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, jsonRequest(http.MethodPost, path, body))
		return rec
	}
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// org
	slug := uniqSlug("cat")
	rec := post("/v1/orgs", `{"slug":"`+slug+`","name":"Catalog"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org: %d %s", rec.Code, rec.Body.String())
	}
	var org struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &org)

	// a duplicate slug is a conflict, not a second org
	if rec := post("/v1/orgs", `{"slug":"`+slug+`","name":"dup"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate org slug should be 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// app under it
	rec = post("/v1/orgs/"+org.ID+"/apps", `{"slug":"web","name":"Web"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d %s", rec.Code, rec.Body.String())
	}
	var app struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &app)

	// app under an org that does not exist: the FK violation is a client
	// mistake, so it must be 404, not 500
	if rec := post("/v1/orgs/"+uuidZero()+"/apps", `{"slug":"x","name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("app under unknown org should be 404, got %d: %s", rec.Code, rec.Body.String())
	}
	// malformed path id: usage error
	if rec := post("/v1/orgs/not-a-uuid/apps", `{"slug":"x","name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed org id should be 400, got %d", rec.Code)
	}
	// invalid slug: the store validates, the handler maps ErrInvalid to 400
	if rec := post("/v1/orgs/"+org.ID+"/apps", `{"slug":"NOT-A-SLUG","name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid slug should be 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// stack under the app
	rec = post("/v1/apps/"+app.ID+"/stacks", `{"slug":"main"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create stack: %d %s", rec.Code, rec.Body.String())
	}
	var stack struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &stack)
	// stacks under an unknown app are 404 — the handler checks the parent
	// exists so the error names the level that is wrong
	if rec := get("/v1/apps/" + uuidZero() + "/stacks"); rec.Code != http.StatusNotFound {
		t.Fatalf("stacks under unknown app should be 404, got %d", rec.Code)
	}

	// environment under the stack
	rec = post("/v1/stacks/"+stack.ID+"/envs", `{"slug":"staging","config":{"LOG_LEVEL":"debug"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create env: %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID     string            `json:"id"`
		Config map[string]string `json:"config"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Config["LOG_LEVEL"] != "debug" {
		t.Fatalf("env config must round-trip, got %+v", env.Config)
	}

	// the reads
	if rec := get("/v1/stacks/" + stack.ID); rec.Code != http.StatusOK {
		t.Fatalf("get stack: %d", rec.Code)
	}
	if rec := get("/v1/envs/" + env.ID); rec.Code != http.StatusOK {
		t.Fatalf("get env: %d", rec.Code)
	}
	if rec := get("/v1/stacks/" + stack.ID + "/envs"); rec.Code != http.StatusOK {
		t.Fatalf("list envs: %d", rec.Code)
	}
	if rec := get("/v1/apps/" + app.ID + "/stacks"); rec.Code != http.StatusOK {
		t.Fatalf("list stacks: %d", rec.Code)
	}
	// unknown ids are 404, not 500
	if rec := get("/v1/envs/" + uuidZero()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown env should be 404, got %d", rec.Code)
	}
}

func uuidZero() string {
	return "00000000-0000-0000-0000-000000000000"
}

// The stack-version push is the API's one raw-body route: compose YAML, not a
// JSON envelope, because `curl --data-binary @compose.yaml` is the natural
// client. Authorship rides in a query parameter. And pushing the same file
// twice must not manufacture version churn — CreateStackVersion dedupes on
// the spec digest, which is only honest if the digest is stable.
func TestStackVersionRawBodyContract(t *testing.T) {
	srv := testServer(t)
	stackID := apiStackUnderFreshOrg(t, srv)

	push := func(compose, createdBy string) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v1/stacks/" + stackID + "/versions"
		if createdBy != "" {
			path += "?created_by=" + createdBy
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(compose))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	rec := push(goodCompose, "craig")
	if rec.Code != http.StatusCreated {
		t.Fatalf("push v1: %d %s", rec.Code, rec.Body.String())
	}
	var v1 struct {
		ID        string `json:"id"`
		Version   int    `json:"version"`
		CreatedBy string `json:"created_by"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v1)
	if v1.Version != 1 || v1.CreatedBy != "craig" {
		t.Fatalf("v1 should be version 1 by craig, got v%d by %q", v1.Version, v1.CreatedBy)
	}

	// Same file again: same row, not a second version. Re-pushing an
	// unmodified stack is what CI does on every merge.
	rec = push(goodCompose, "ci")
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-push: %d %s", rec.Code, rec.Body.String())
	}
	var again struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.ID != v1.ID || again.Version != 1 {
		t.Fatalf("identical digest must dedupe to v1, got v%d (%s)", again.Version, again.ID)
	}

	// A platform-constraint violation is a 422 with the violations listed —
	// all of them at once, so the author fixes everything in one pass.
	rec = push("services:\n  s:\n    image: nginx\n    privileged: true\n    ports: [\"8080:80\"]\n", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("violations should be 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "privileged") || !strings.Contains(rec.Body.String(), "ports") {
		t.Fatalf("expected both violations reported at once, got %s", rec.Body.String())
	}

	// YAML that does not parse at all is a 400, a different failure than
	// violating platform constraints.
	rec = push("\tthis is not yaml: [", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage should be 400, got %d", rec.Code)
	}

	// Versions list back.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stacks/"+stackID+"/versions", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"versions"`) {
		t.Fatalf("list versions: %d %s", rec.Code, rec.Body.String())
	}
}

// The validate endpoint is the fastest feedback loop the platform has — it
// must classify correctly and bound its input.
func TestValidateEndpoint(t *testing.T) {
	srv := testServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/validate", strings.NewReader(goodCompose))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid compose: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Valid   bool   `json:"valid"`
		Digest  string `json:"digest"`
		Summary struct {
			Services  []string `json:"services"`
			Swappable []string `json:"swappable"`
			Ingress   string   `json:"ingress"`
		} `json:"summary"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Valid || len(resp.Summary.Services) != 1 || resp.Summary.Swappable[0] != "api" {
		t.Fatalf("classification wrong: %s", rec.Body.String())
	}
	if len(resp.Digest) != 64 {
		t.Fatalf("digest should be sha256 hex, got %q", resp.Digest)
	}

	// Constraint violations: 422 with details, the shape the CLI prints.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/validate",
		strings.NewReader("services:\n  s:\n    image: nginx\n"))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing rollout declaration should be 422, got %d: %s", rec.Code, rec.Body.String())
	}

	// The body cap: a compose file over 1 MiB is refused, not buffered.
	rec = httptest.NewRecorder()
	big := make([]byte, (1<<20)+10)
	for i := range big {
		big[i] = '#'
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/validate", strings.NewReader(string(big)))
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize body should be 400, got %d", rec.Code)
	}
}

// The deployment handler's job is resolution and fail-fast: latest version
// when none is named, the environment's own stack version when one is,
// 422-before-a-node for missing secrets, and peak memory in the response so
// a rejected placement is explicable.
func TestCreateDeploymentContract(t *testing.T) {
	srv := testServer(t)
	stackID := apiStackUnderFreshOrg(t, srv)

	// push a version whose api service needs a secret
	compose := `
services:
  api:
    image: nginx:alpine
    environment:
      DATABASE_URL: postgres://app:${secret:db_password}@db/app
    x-composectl:
      rollout: swap
`
	req := httptest.NewRequest(http.MethodPost, "/v1/stacks/"+stackID+"/versions", strings.NewReader(compose))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}
	var sv struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sv)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/stacks/"+stackID+"/envs", `{"slug":"prod"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create env: %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)

	// Missing secret: 422 naming the key, before anything reaches a node.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+env.ID+"/deployments", `{}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing secret should be 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "db_password") {
		t.Fatalf("the missing key should be named, got %s", rec.Body.String())
	}

	// Set the secret (store-direct: the secrets handler has its own tests)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	envUUID, err := uuid.Parse(env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.st.SetSecret(ctx, envUUID, "db_password", []byte("ciphertext"), "age1test"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	// Now it deploys: 202 with peak memory.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+env.ID+"/deployments", `{}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	var dep struct {
		ID              string `json:"id"`
		Revision        int    `json:"revision"`
		Slot            string `json:"slot"`
		State           string `json:"state"`
		PeakMemoryBytes int64  `json:"peak_memory_bytes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dep)
	if dep.Revision != 1 || dep.Slot != "blue" || dep.State != "pending" {
		t.Fatalf("first deploy should be revision 1 blue pending, got %+v", dep)
	}
	// Default limits are 250 millicpu / 256 MiB, so one swappable service
	// reserves 2×256 MiB at peak.
	if dep.PeakMemoryBytes != 2*256<<20 {
		t.Fatalf("peak memory should be 2×256MiB for one swappable service, got %d", dep.PeakMemoryBytes)
	}

	// A second concurrent deployment is a 409 — one active per environment.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+env.ID+"/deployments", `{}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second active deployment should be 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// A stack_version_id that is not a UUID is a usage error.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+env.ID+"/deployments",
		`{"stack_version_id":"nope"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad stack_version_id should be 400, got %d", rec.Code)
	}

	// A valid UUID from a *different* stack is a 404, not a silent deploy of
	// someone else's version.
	otherStack := apiStackUnderFreshOrg(t, srv)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/stacks/"+otherStack+"/versions", strings.NewReader(goodCompose)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("push other stack: %d", rec.Code)
	}
	var otherSV struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &otherSV)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+env.ID+"/deployments",
		`{"stack_version_id":"`+otherSV.ID+`"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("version from another stack should be 404, got %d", rec.Code)
	}

	// get + list
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/deployments/"+dep.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"peak_memory_bytes"`) {
		t.Fatalf("get deployment: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/envs/"+env.ID+"/deployments", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deployments"`) {
		t.Fatalf("list deployments: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- helpers

// apiStackUnderFreshOrg creates org → app → stack through the HTTP surface
// and returns the stack id. A fresh org per test keeps the shared dev
// database clean and the failures legible.
func apiStackUnderFreshOrg(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/orgs",
		`{"slug":"`+uniqSlug("apitest")+`","name":"API Test"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org: %d %s", rec.Code, rec.Body.String())
	}
	var org struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &org)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/orgs/"+org.ID+"/apps", `{"slug":"app","name":"App"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d %s", rec.Code, rec.Body.String())
	}
	var app struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &app)

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/apps/"+app.ID+"/stacks", `{"slug":"main"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create stack: %d %s", rec.Code, rec.Body.String())
	}
	var stack struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &stack)
	return stack.ID
}
