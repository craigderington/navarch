package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Variadic options so a test needing extra wiring — a log buffer, say — does not
// have to build its own Server and drift from this one's setup.
func testServer(t *testing.T, opts ...ServerOption) *Server {
	t.Helper()
	dsn := os.Getenv("COMPOSECTL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://composectl:composectl@localhost:5473/composectl?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable — run make up: %v", err)
	}
	t.Cleanup(st.Close)
	srv := NewServer(st, slogDiscard(), opts...)
	srv.BootstrapDevOrg(ctx)
	cleanupDevNodes(t, st)
	return srv
}

// cleanupDevNodes deletes every node this test adds to the bootstrapped "dev"
// org, which is the same org the dev fleet and every demo run in.
//
// Left behind, those rows are not merely untidy. Placement scores by spread, so
// a freshly registered fixture node — zero environments homed on it — is the
// *most* attractive node in the fleet, and nothing drives it: the next demo's
// deployment is placed there and sits in `scheduling` until the 30-second
// heartbeat window closes and it is marked unreachable. That is a passing system
// looking broken, on a surface where the honest failures are already subtle.
// Slice A's scoring is what turned an old flakiness (see the comment on
// TestSetSecretWithNoReadyNodeIsUnprocessable, which dodges this by using its
// own org) into a hazard for anything sharing the database.
//
// Scoped by difference rather than by name so it covers registrations made
// through the HTTP handler, where the test never holds the id, and any site
// added later that this file does not know about.
func cleanupDevNodes(t *testing.T, st *store.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, err := st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		return // no dev org, nothing this test can pollute
	}
	before := map[uuid.UUID]bool{}
	if nodes, err := st.ListNodes(ctx, dev.ID); err == nil {
		for _, n := range nodes {
			before[n.ID] = true
		}
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		nodes, err := st.ListNodes(c, dev.ID)
		if err != nil {
			return
		}
		for _, n := range nodes {
			if before[n.ID] {
				continue
			}
			// Instances first: service_instances.node_id has no ON DELETE
			// CASCADE, the same ordering hazard the store fixtures document.
			if _, err := st.Pool().Exec(c, `DELETE FROM service_instances WHERE node_id=$1`, n.ID); err != nil {
				t.Errorf("cleanup instances for node %s: %v", n.ID, err)
				continue
			}
			if _, err := st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, n.ID); err != nil {
				t.Errorf("cleanup node %s: %v", n.ID, err)
			}
		}
	})
}

// Every other test here builds a Server with no bearer token, which makes
// ServeHTTP skip authorization entirely — so the per-node token path was
// never exercised and shipped returning 401 for every agent request. These
// cases configure a token deliberately.
//
// The failure it guards: authorization runs before the mux matches a route,
// so r.PathValue("id") is empty there and the node id has to be parsed from
// the path directly.
func TestNodeTokenAuthorizesAgentEndpoints(t *testing.T) {
	srv := testServer(t)
	WithBearerToken("shared-service-token")(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	org, err := srv.st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	// A real operator, and a member of the node's own org, so the refusals
	// below are about the credential being the wrong *kind* rather than about
	// membership. An operator with every right to the node is still not the
	// node, and a node endpoint must say so.
	op := newScopedOperator(t, srv.st)
	if err := srv.st.AddOrgMember(ctx, org.ID, op.id, "owner"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: "auth-" + uuid.NewString()[:8],
		AdvertiseAddr: "10.9.9.9", CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if node.Token == "" {
		t.Fatal("first registration must issue a plaintext node token")
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
	})

	path := "/v1/nodes/" + node.ID.String() + "/desired-state"

	t.Run("node token is accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+node.Token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for a valid node token, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// The operator token must not reach a node endpoint: it would let anyone
	// holding it pull another node's desired-state ciphertext.
	t.Run("operator token is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+op.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for the operator token, got %d", rec.Code)
		}
	})

	t.Run("another node's token is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes/"+uuid.NewString()+"/desired-state", nil)
		req.Header.Set("Authorization", "Bearer "+node.Token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for a mismatched node, got %d", rec.Code)
		}
	})

	t.Run("malformed node id is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes/not-a-uuid/desired-state", nil)
		req.Header.Set("Authorization", "Bearer "+op.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("malformed id must not fall through to the operator token, got %d", rec.Code)
		}
	})
}

func TestRegisterNodeHandler(t *testing.T) {
	srv := testServer(t)
	body, _ := json.Marshal(map[string]any{
		"org": "dev", "hostname": "test-" + time.Now().Format("150405.000"),
		"advertise_addr": "10.1.2.3", "cpu_millis": 2000, "memory_bytes": 1 << 31,
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/nodes/register", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID == "" {
		t.Fatal("expected a node id in the response")
	}
}

// A malformed age recipient must be rejected at registration with a 400.
// Accepted, it surfaces much later as a 500 on the first secret written for
// an environment homed to that node — confined to that node's environments,
// which reads as a control-plane bug rather than a bad key.
func TestRegisterNodeRejectsMalformedRecipient(t *testing.T) {
	srv := testServer(t)
	for _, recipient := range []string{"not-a-recipient", "age1shortcut", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample"} {
		body, _ := json.Marshal(map[string]any{
			"org": "dev", "hostname": "badkey-" + time.Now().Format("150405.000"),
			"advertise_addr": "10.1.2.3", "cpu_millis": 2000, "memory_bytes": 1 << 31,
			"age_recipient": recipient,
		})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/nodes/register", bytes.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("recipient %q should be a 400, got %d: %s", recipient, rec.Code, rec.Body.String())
		}
	}
}

// newReportableInstance builds the full chain a service_instance hangs off of
// -- org, app, stack, version, environment, deployment, node -- and returns
// the node and the one instance row scheduled onto it.
func newReportableInstance(t *testing.T, srv *Server) (nodeID, instanceID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	slug := "report-" + uuid.NewString()[:8]
	org, err := srv.st.CreateOrganization(ctx, slug, "Report Test")
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
	env, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{
		StackID: stack.ID, Slug: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := srv.st.CreateDeployment(ctx, store.CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: dspec, CreatedBy: "t"})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.1.2.5",
		CPUMillis: 2000, MemoryBytes: 1 << 31})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if err := srv.st.CreateServiceInstances(ctx, dep.ID, node.ID, []store.NewInstance{
		{ServiceName: "web", Swappable: true, ImageRef: "nginx:alpine"}}); err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}
	if err := srv.st.Pool().QueryRow(ctx,
		`SELECT id FROM service_instances WHERE deployment_id=$1`, dep.ID).Scan(&instanceID); err != nil {
		t.Fatalf("read instance id: %v", err)
	}

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM environments WHERE stack_id=$1`, stack.ID)
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
		srv.st.Pool().Exec(c, `DELETE FROM organizations WHERE id=$1`, org.ID)
	})
	return node.ID, instanceID
}

// A report for an instance row that has since been deleted must not fail the
// whole batch. Every preview reap produces exactly this: the agent reconciled
// those instances at the top of the tick and is holding reports for rows the
// reaper has cascaded away since. Aborting on the first one 404s the request,
// which makes reconcileTick bail out -- skipping the heartbeat and dropping
// the reports for every other environment on the node.
func TestInstanceReportSkipsVanishedRowsAndKeepsTheBatch(t *testing.T) {
	srv := testServer(t)
	nodeID, live := newReportableInstance(t, srv)

	// The vanished row comes first, so a first-error abort would drop the
	// live one behind it.
	body, _ := json.Marshal(map[string]any{"instances": []map[string]any{
		{"instance_id": uuid.New(), "state": "running", "health_status": "healthy"},
		{"instance_id": live, "state": "running", "health_status": "healthy", "set_started": true},
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/nodes/"+nodeID.String()+"/report", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("a report for a deleted instance must not fail the batch, got %d: %s", rec.Code, rec.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var state, health string
	if err := srv.st.Pool().QueryRow(ctx,
		`SELECT state::text, COALESCE(health_status,'') FROM service_instances WHERE id=$1`,
		live).Scan(&state, &health); err != nil {
		t.Fatalf("read reported instance: %v", err)
	}
	if state != "running" || health != "healthy" {
		t.Errorf("the surviving instance's report must still have been applied, got state=%s health=%s", state, health)
	}
}

func TestInstanceReportCannotUpdateAnotherNodesInstance(t *testing.T) {
	srv := testServer(t)
	_, instanceID := newReportableInstance(t, srv)

	body, _ := json.Marshal(map[string]any{"instances": []map[string]any{
		{"instance_id": instanceID, "state": "running", "health_status": "healthy"},
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/nodes/"+uuid.NewString()+"/report", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stale or foreign reports are ignored, got %d: %s", rec.Code, rec.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var state string
	if err := srv.st.Pool().QueryRow(ctx,
		`SELECT state::text FROM service_instances WHERE id=$1`, instanceID).Scan(&state); err != nil {
		t.Fatalf("read instance: %v", err)
	}
	if state != "pending" {
		t.Fatalf("foreign node changed instance state to %q", state)
	}
}

func TestRollbackUnknownEnvIsNotFound(t *testing.T) {
	srv := testServer(t)
	body := bytes.NewReader([]byte(`{"to_revision":1}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/envs/"+uuid.NewString()+"/rollback", body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 rolling back an env with no deployments, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRollbackInvalidEnvIsBadRequest(t *testing.T) {
	srv := testServer(t)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/envs/not-a-uuid/rollback",
		bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad env id, got %d", rec.Code)
	}
}

// Uncordon is an operator action, not an agent one. The distinction is not
// cosmetic: nodeAgentPathID claims only heartbeat, desired-state and report, and
// anything it does not claim falls through to the operator-token branch. A new
// /v1/nodes/{id}/... route is exactly the shape that could land on the wrong
// side of that split — which is how every per-node endpoint once returned 401
// unconditionally — so the branch is pinned here rather than assumed.
func TestUncordonIsAnOperatorEndpoint(t *testing.T) {
	srv := testServer(t)
	WithBearerToken("operator-token")(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	org, err := srv.st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: "uncordon-" + uuid.NewString()[:8],
		AdvertiseAddr: "10.9.9.10", CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
	})
	if err := srv.st.DrainNode(ctx, node.ID); err != nil {
		t.Fatalf("DrainNode: %v", err)
	}
	// An operator route now needs an operator, not the shared token — that
	// token opens node registration and metrics and nothing else. The operator
	// must be a member of the node's org or the route answers 404 rather than
	// 401, which would pass the "a node token is refused" case below for
	// entirely the wrong reason.
	op := newScopedOperator(t, srv.st)
	if err := srv.st.AddOrgMember(ctx, org.ID, op.id, "owner"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}
	path := "/v1/nodes/" + node.ID.String() + "/uncordon"

	t.Run("a node token is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+node.Token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a node must not be able to uncordon itself, got %d", rec.Code)
		}
	})

	t.Run("the operator token works and reports the derived state", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+op.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		// The response reports what the row actually says, not a fixed "ready":
		// the store derives the state from the last heartbeat, and an API that
		// answered "ready" regardless would contradict the next node list.
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		n, err := srv.st.GetNode(ctx, node.ID)
		if err != nil {
			t.Fatalf("GetNode: %v", err)
		}
		if body["status"] != string(n.State) {
			t.Fatalf("reported %q but the row says %q", body["status"], n.State)
		}
		if n.State == store.NodeDraining {
			t.Fatal("the node is still draining after a successful uncordon")
		}
	})
}

// The operator half of S7, over HTTP: a node's proposed key does nothing until
// somebody with an identity approves it.
//
// The route is org-scoped like every other node route — TestEveryOperatorRoute
// IsOrgScoped covers that — so what is left to check here is the transition
// itself and the refusal when there is nothing to approve.
func TestRotateRecipientIsAnOperatorAction(t *testing.T) {
	srv := testServer(t, WithBearerToken("shared-service-token"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	org, err := srv.st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	op := newScopedOperator(t, srv.st)
	if err := srv.st.AddOrgMember(ctx, org.ID, op.id, "owner"); err != nil {
		t.Fatalf("AddOrgMember: %v", err)
	}

	host := "rotate-" + uuid.NewString()[:8]
	register := func(recipient string) *store.Node {
		t.Helper()
		n, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
			OrgID: org.ID, Hostname: host, AdvertiseAddr: "10.9.9.11",
			CPUMillis: 1000, MemoryBytes: 1 << 30, AgeRecipient: recipient,
		})
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", recipient, err)
		}
		return n
	}
	node := register("age1original")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.st.Pool().Exec(c, `DELETE FROM nodes WHERE id=$1`, node.ID)
	})
	path := "/v1/nodes/" + node.ID.String() + "/rotate-recipient"

	// Nothing pending: a conflict, so "rotated" never means "did nothing".
	if rec := doAs(t, srv, http.MethodPost, path, op.token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 with nothing pending, got %d: %s", rec.Code, rec.Body.String())
	}

	// The node proposes a new key. Over HTTP, the effective one must not move.
	register("age1rotated")
	got, err := srv.st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.AgeRecipient != "age1original" || got.PendingAgeRecipient != "age1rotated" {
		t.Fatalf("re-registration should propose, not assign: %+v", got)
	}

	// A node cannot approve itself: its own token does not reach this route at
	// all, which is what keeps the proposal and the approval separate parties.
	if rec := doAs(t, srv, http.MethodPost, path, node.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a node token must not reach an operator route, got %d", rec.Code)
	}
	if rec := doAs(t, srv, http.MethodPost, path, "shared-service-token", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the shared service token must not approve a rotation, got %d", rec.Code)
	}
	if got, _ := srv.st.GetNode(ctx, node.ID); got.AgeRecipient != "age1original" {
		t.Fatal("a refused request changed the effective recipient")
	}

	// The operator approves, and only then does it take effect.
	rec := doAs(t, srv, http.MethodPost, path, op.token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got, err = srv.st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.AgeRecipient != "age1rotated" || got.PendingAgeRecipient != "" {
		t.Fatalf("approval should have promoted the pending key: %+v", got)
	}
}
