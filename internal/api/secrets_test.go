package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/secrets"
	"github.com/craig/composectl/internal/store"
)

// uniqSlug builds a slug that satisfies the store's lowercase-alphanumeric-
// with-dashes rule while staying unique across test runs against the same
// database.
func uniqSlug(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// seedEnvWithNode builds the minimal catalog chain a secret needs: the
// bootstrapped dev org, an app, a stack and an environment, plus a node
// registered with a real age recipient so handleSetSecret has something to
// encrypt to. Returns the environment id (string, for building URLs) and the
// registered node.
func seedEnvWithNode(t *testing.T, srv *Server) (string, *store.Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	org, err := srv.st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug(dev): %v", err)
	}

	slug := uniqSlug("secrets")
	app, err := srv.st.CreateApplication(ctx, org.ID, slug, "Secrets Test App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := srv.st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	env, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		StackID: stack.ID, Slug: slug,
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.0.0.9",
		CPUMillis: 1000, MemoryBytes: 1 << 30, AgeRecipient: id.Recipient(),
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return env.ID.String(), node
}

func TestSetAndListSecret(t *testing.T) {
	srv := testServer(t)
	// need an env and a node with a recipient in the dev org
	// (build via srv.st; see helper below)
	envID, _ := seedEnvWithNode(t, srv)

	body, _ := json.Marshal(map[string]string{"key": "db_password", "value": "hunter2"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/"+envID+"/secrets", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/envs/"+envID+"/secrets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("hunter2")) {
		t.Fatal("list must never leak the value")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("db_password")) {
		t.Fatalf("list missing the key: %s", rec.Body.String())
	}
}

// TestSetSecretWithNoReadyNodeIsUnprocessable makes sure a fresh environment
// with no agent registered fails loudly rather than silently storing a
// secret nothing can ever decrypt. It deliberately uses its own org rather
// than the shared "dev" one bootstrapped by testServer: ListReadyNodes is
// org-scoped, and other tests in this package register ready nodes into
// "dev" whose 30-second heartbeat window can still be open here, which would
// make this test pass or fail depending on run order.
func TestSetSecretWithNoReadyNodeIsUnprocessable(t *testing.T) {
	srv := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	org, err := srv.st.CreateOrganization(ctx, uniqSlug("nonode-org"), "No Node Org")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	slug := uniqSlug("nonode")
	app, err := srv.st.CreateApplication(ctx, org.ID, slug, "No Node App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := srv.st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	env, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		StackID: stack.ID, Slug: slug,
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"key": "db_password", "value": "hunter2"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/"+env.ID.String()+"/secrets", bytes.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with no ready node, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSecret(t *testing.T) {
	srv := testServer(t)
	envID, _ := seedEnvWithNode(t, srv)

	body, _ := json.Marshal(map[string]string{"key": "api_token", "value": "s3cr3t"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/"+envID+"/secrets", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/envs/"+envID+"/secrets/api_token", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/envs/"+envID+"/secrets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("api_token")) {
		t.Fatalf("deleted key still listed: %s", rec.Body.String())
	}
}

func TestSetSecretWithMalformedKeyIsRejected(t *testing.T) {
	srv := testServer(t)
	envID, _ := seedEnvWithNode(t, srv)

	tests := []string{
		"bad key!",        // spaces not allowed
		"x}${secret:y",    // special characters not allowed
		"",                // empty key not allowed
		"key with spaces", // spaces not allowed
	}

	for _, malformedKey := range tests {
		body, _ := json.Marshal(map[string]string{"key": malformedKey, "value": "hunter2"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/"+envID+"/secrets", bytes.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed key %q: expected 400, got %d: %s", malformedKey, rec.Code, rec.Body.String())
		}
	}
}
