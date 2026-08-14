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

func testServer(t *testing.T) *Server {
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
	srv := NewServer(st, slogDiscard())
	srv.BootstrapDevOrg(ctx)
	return srv
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
