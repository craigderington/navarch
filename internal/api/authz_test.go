package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// unscopedRoutes are the routes that legitimately do not check org membership.
// Every one needs a reason, because this allowlist is the only way a route
// escapes the check below — an entry added without one is how a tenant leak
// ships.
var unscopedRoutes = map[string]string{
	"GET /healthz": "public liveness probe; touches no tenant data",
	"GET /metrics": "fleet-wide counters and enums, no tenant identifiers; " +
		"authenticated by the shared service token so a scraper needs no identity",
	"POST /v1/orgs": "self-serve; the creator is enrolled as owner in the same request",
	"GET /v1/orgs":  "scoped by construction — it lists only the caller's own orgs",
	"GET /v1/whoami": "the answer is the caller themselves; it addresses no object, " +
		"and it is how an operator tells \"wrong org\" from \"wrong id\" given that " +
		"the two are deliberately indistinguishable everywhere else",
	"POST /v1/validate": "parses a compose file the caller supplied and stores nothing; " +
		"it addresses no object, so there is no org to check",
	"POST /v1/nodes/register": "an agent enrolling has no identity yet; an operator " +
		"calling it by hand is checked against the named org inside the handler",
	"POST /v1/nodes/{id}/heartbeat":    "node token only; confined by nodeAgentPathID",
	"GET /v1/nodes/{id}/desired-state": "node token only; confined by nodeAgentPathID",
	"POST /v1/nodes/{id}/report":       "node token only; confined by nodeAgentPathID",
	"POST /v1/nodes/{id}/logs":         "node token only; confined by nodeAgentPathID",
	"GET /v1/nodes":                    "org comes from a query parameter, checked in the handler",
}

// Every operator route must refuse an operator who is not in the owning org,
// and must refuse with 404 rather than 403.
//
// The list of routes comes from the mux itself, not from this file. A route
// added later without an authorize call fails here rather than shipping open,
// which is the entire point: the risk is not a helper that checks wrongly, it
// is a handler that forgets to call one, and that is invisible until a tenant
// finds it.
//
// The server is built WITH a bearer token deliberately. A test server without
// one skips authentication entirely, so every assertion below would pass
// against a completely unauthorized server — the exact shape of the bug that
// let every per-node endpoint 401 for a sprint while the suite stayed green.
func TestEveryOperatorRouteIsOrgScoped(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"))
	st := srv.st

	victim := newScopedFixture(t, st)
	intruder := newScopedOperator(t, st)

	checked := 0
	for _, pattern := range srv.Patterns() {
		if why, ok := unscopedRoutes[pattern]; ok {
			if why == "" {
				t.Errorf("%s is exempt with no reason given", pattern)
			}
			continue
		}
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("unparseable route pattern %q", pattern)
		}
		concrete := victim.fill(t, path)
		checked++

		t.Run(pattern, func(t *testing.T) {
			req := httptest.NewRequest(method, concrete, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+intruder.token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("an operator outside the org got %d, want 404: %s",
					rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
	if checked == 0 {
		t.Fatal("no routes were checked — the pattern list or the allowlist is wrong")
	}
	t.Logf("checked %d org-scoped routes", checked)
}

// The other half: the owning operator must actually get through. Without this,
// a handler that answered 404 unconditionally would pass the test above.
func TestTheOwningOperatorIsNotRefused(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"))
	f := newScopedFixture(t, srv.st)

	for _, path := range []string{
		"/v1/orgs/" + f.orgID.String() + "/apps",
		"/v1/apps/" + f.appID.String() + "/stacks",
		"/v1/stacks/" + f.stackID.String() + "/envs",
		"/v1/envs/" + f.envID.String(),
		"/v1/deployments/" + f.deployID.String(),
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+f.op.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized {
			t.Fatalf("the owning operator was refused %s with %d: %s",
				path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// ------------------------------------------------------------------ fixtures

type scopedOperator struct {
	id    uuid.UUID
	token string
}

func newScopedOperator(t *testing.T, st *store.Store) *scopedOperator {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	op, err := st.CreateOperator(ctx, "op-"+uuid.NewString()[:8]+"@example.com", "Scoped")
	if err != nil {
		t.Fatalf("CreateOperator: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st.Pool().Exec(c, `DELETE FROM operators WHERE id=$1`, op.ID)
	})
	tok, err := st.IssueOperatorToken(ctx, op.ID, "test", nil)
	if err != nil {
		t.Fatalf("IssueOperatorToken: %v", err)
	}
	return &scopedOperator{id: op.ID, token: tok.Plaintext}
}

// scopedFixture is one org with a full catalog graph beneath it, plus an
// operator who owns it. Every {placeholder} in a route pattern resolves to
// something in here, so the intruder's request names a real object and the only
// reason it can fail is membership.
type scopedFixture struct {
	op                                             *scopedOperator
	orgID, appID, stackID, envID, deployID, nodeID uuid.UUID
	logID                                          uuid.UUID
}

func newScopedFixture(t *testing.T, st *store.Store) *scopedFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	slug := "authz-" + uuid.NewString()[:8]
	org, err := st.CreateOrganization(ctx, slug, "Authz")
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exec := func(q string) { st.Pool().Exec(c, q, org.ID) }
		exec(`UPDATE environments SET live_deployment_id=NULL WHERE stack_id IN (
			SELECT s.id FROM stacks s JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`)
		exec(`DELETE FROM log_requests WHERE environment_id IN (
			SELECT e.id FROM environments e JOIN stacks s ON e.stack_id=s.id
			JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`)
		exec(`DELETE FROM service_instances WHERE node_id IN (SELECT id FROM nodes WHERE org_id=$1)`)
		exec(`DELETE FROM deployments WHERE environment_id IN (
			SELECT e.id FROM environments e JOIN stacks s ON e.stack_id=s.id
			JOIN applications a ON s.app_id=a.id WHERE a.org_id=$1)`)
		exec(`DELETE FROM nodes WHERE org_id=$1`)
		exec(`DELETE FROM organizations WHERE id=$1`)
	})

	op := newScopedOperator(t, st)
	if err := st.AddOrgMember(ctx, org.ID, op.id, "owner"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}

	app, err := st.CreateApplication(ctx, org.ID, slug, "App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	sv, err := st.CreateStackVersion(ctx, stack.ID, "raw", &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{"app": {
			Name: "app", Image: "nginx:alpine", Swappable: true,
			Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20},
		}},
	}, "t")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := st.CreateEnvironment(ctx, store.CreateEnvironmentParams{StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := st.CreateDeployment(ctx, store.CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "t",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	node, err := st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.9.9.9",
		CPUMillis: 4000, MemoryBytes: 8 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	return &scopedFixture{
		op: op, orgID: org.ID, appID: app.ID, stackID: stack.ID,
		envID: env.ID, deployID: dep.ID, nodeID: node.ID,
		// No log request is created: GET/DELETE /v1/logs/{id} resolve through
		// OrgForLogRequest, which returns ErrNotFound for an unknown id — the
		// same 404 a non-member gets, which is exactly the property under test.
		logID: uuid.New(),
	}
}

// fill turns a route pattern into a concrete path against this fixture. {id}
// means different objects on different routes, so it is chosen by prefix rather
// than by name.
func (f *scopedFixture) fill(t *testing.T, path string) string {
	t.Helper()
	rep := strings.NewReplacer(
		"{org}", f.orgID.String(),
		"{app}", f.appID.String(),
		"{stack}", f.stackID.String(),
		"{env}", f.envID.String(),
		"{operator}", f.op.id.String(),
		"{key}", "some-key",
	)
	out := rep.Replace(path)
	if strings.Contains(out, "{id}") {
		var id uuid.UUID
		switch {
		case strings.HasPrefix(out, "/v1/deployments/"):
			id = f.deployID
		case strings.HasPrefix(out, "/v1/nodes/"):
			id = f.nodeID
		case strings.HasPrefix(out, "/v1/logs/"):
			id = f.logID
		default:
			t.Fatalf("no fixture object for {id} in %q — add one rather than exempting the route", path)
		}
		out = strings.ReplaceAll(out, "{id}", id.String())
	}
	if strings.Contains(out, "{") {
		t.Fatalf("unfilled placeholder in %q — %s", path, out)
	}
	return out
}
