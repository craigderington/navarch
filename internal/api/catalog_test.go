package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/store"
)

// GET /v1/orgs/{org}/environments is a cross-tenant surface: it is the only
// route that returns environments without the caller naming a stack, so it is
// the only one where a scoping mistake hands over a whole catalog rather than
// one object. The store owns the scoping; this asserts the route actually
// carries it, because a handler that passed the wrong id would pass every
// store-level test while leaking here.
func TestListOrgEnvironmentsRouteIsScopedToItsOrg(t *testing.T) {
	srv := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mineOrg, mineEnv := apiEnvInOrg(t, srv, ctx, "mine")
	_, theirEnv := apiEnvInOrg(t, srv, ctx, "theirs")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/orgs/"+mineOrg.String()+"/environments", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Environments []struct {
			ID        string `json:"id"`
			AppSlug   string `json:"app_slug"`
			StackSlug string `json:"stack_slug"`
			HomeNode  string `json:"home_node"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var sawMine bool
	for _, e := range got.Environments {
		if e.ID == theirEnv.String() {
			t.Fatalf("environment %s from another organization was returned", e.ID)
		}
		if e.ID == mineEnv.String() {
			sawMine = true
			// The catalog path is why this endpoint exists: without it a client
			// still has to walk the hierarchy to know what it is looking at.
			if e.AppSlug == "" || e.StackSlug == "" {
				t.Fatalf("row is missing its catalog path: %+v", e)
			}
		}
	}
	if !sawMine {
		t.Fatalf("own environment %s missing from %d rows", mineEnv, len(got.Environments))
	}
}

func TestListOrgEnvironmentsRouteRejectsANonUUID(t *testing.T) {
	srv := testServer(t)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/orgs/not-a-uuid/environments", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// apiEnvInOrg builds org → app → stack → env and returns the org and env ids.
func apiEnvInOrg(t *testing.T, srv *Server, ctx context.Context, label string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	slug := uniqSlug(label)
	org, err := srv.st.CreateOrganization(ctx, uniqSlug(label+"-org"), "Org")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	app, err := srv.st.CreateApplication(ctx, org.ID, slug, "App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := srv.st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	env, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	return org.ID, env.ID
}
