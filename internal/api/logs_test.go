package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/craig/composectl/internal/logbuf"
	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

// Two routes end in "/logs" and they must authorize differently: an operator
// opens a tail with the operator token, and only the node running the container
// delivers the answer with its own node token. Wrong in either direction is
// bad — a node token that could open tails reads any environment, and an
// operator token demanded on the delivery path stops every agent answering.
func TestLogRoutesSplitAcrossOperatorAndNodeTokens(t *testing.T) {
	nodeID := uuid.New()
	tests := []struct {
		name      string
		method    string
		path      string
		wantAgent bool
	}{
		{"node delivery is an agent path", http.MethodPost, "/v1/nodes/" + nodeID.String() + "/logs", true},
		{"opening a tail is not", http.MethodPost, "/v1/envs/" + uuid.New().String() + "/logs", false},
		{"reading a tail is not", http.MethodGet, "/v1/logs/" + uuid.New().String(), false},
		{"closing a tail is not", http.MethodDelete, "/v1/logs/" + uuid.New().String(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, isAgent := nodeAgentPathID(httptest.NewRequest(tt.method, tt.path, nil))
			if isAgent != tt.wantAgent {
				t.Fatalf("agent-path classification = %v, want %v", isAgent, tt.wantAgent)
			}
			if tt.wantAgent && id != nodeID {
				t.Fatalf("node id = %s, want %s", id, nodeID)
			}
		})
	}
}

// The full round trip through HTTP: a node delivers, the requester reads it, and
// the cursor advances so a follow does not reprint what it already showed.
func TestLogDeliveryReachesTheRequester(t *testing.T) {
	buf := logbuf.New()
	srv := testServer(t, WithLogBuffer(buf))
	envID, nodeID := seedRunningInstance(t, srv)

	reqID := openTail(t, srv, envID, `{"service":"api","follow":true}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/nodes/"+nodeID+"/logs",
		`{"deliveries":[{"request_id":"`+reqID+`","data":"line one\n"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body.String())
	}

	var page struct {
		Chunks []struct {
			Data string `json:"data"`
		} `json:"chunks"`
		Cursor int64 `json:"cursor"`
	}
	readPage(t, srv, reqID, 0, &page)
	if len(page.Chunks) != 1 || page.Chunks[0].Data != "line one\n" {
		t.Fatalf("delivered output did not reach the requester: %+v", page.Chunks)
	}
	if page.Cursor == 0 {
		t.Fatal("cursor must advance, or a follow reprints everything each poll")
	}

	var second struct {
		Chunks []struct {
			Data string `json:"data"`
		} `json:"chunks"`
	}
	readPage(t, srv, reqID, page.Cursor, &second)
	if len(second.Chunks) != 0 {
		t.Fatalf("re-reading at the cursor must return nothing, got %+v", second.Chunks)
	}
}

// The buffer is optional wiring. Without one the endpoints must still answer and
// report no output: refusing the request, or quietly persisting content instead,
// are both worse than an honest empty page.
func TestGetLogsWithoutBufferStillAnswers(t *testing.T) {
	srv := testServer(t) // deliberately no WithLogBuffer
	envID, _ := seedRunningInstance(t, srv)
	reqID := openTail(t, srv, envID, `{"service":"api"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/logs/"+reqID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("read without a buffer should still answer, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"chunks"`) {
		t.Fatalf("expected an empty chunk list, got %s", rec.Body.String())
	}
}

// A node may only answer for its own containers. Ownership is enforced by
// CompleteLogRequest's node scoping — and it must gate the *content* write
// too, not just the row update. Without that gate a foreign node's delivery
// is refused as a completion while its forged content still lands in the
// buffer and is served to the operator as if their own node had produced
// it. Forged log output is a real attack: "credentials rotated, re-auth
// required" in a tail is acted on, not read.
func TestLogDeliveryFromAnotherNodeDoesNotCompleteTheRequest(t *testing.T) {
	buf := logbuf.New()
	srv := testServer(t, WithLogBuffer(buf))
	envID, _ := seedRunningInstance(t, srv)
	reqID := openTail(t, srv, envID, `{"service":"api"}`)

	other := uuid.New().String()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/nodes/"+other+"/logs",
		`{"deliveries":[{"request_id":"`+reqID+`","data":"forged\n"}]}`))
	// The batch is accepted — one bogus entry must not fail a real agent's
	// whole delivery — but neither the row nor the buffer may accept it.
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery batch should be accepted, got %d", rec.Code)
	}
	var page struct {
		Request struct {
			State string `json:"state"`
		} `json:"request"`
		Chunks []struct {
			Data string `json:"data"`
		} `json:"chunks"`
	}
	readPage(t, srv, reqID, 0, &page)
	if page.Request.State != string(store.LogPending) {
		t.Fatalf("a foreign node completed the request: state=%s", page.Request.State)
	}
	if len(page.Chunks) != 0 {
		t.Fatalf("a foreign node's content reached the requester: %+v", page.Chunks)
	}
}

// A node may not forge deliveries for request ids that never existed either.
// The buffer must not allocate an entry for them: its slots are bounded, so
// a node spraying random UUIDs must not be able to occupy the lot and starve
// legitimate deliveries out of the buffer.
func TestLogDeliveryForUnknownRequestBuffersNothing(t *testing.T) {
	buf := logbuf.New()
	srv := testServer(t, WithLogBuffer(buf))
	envID, nodeID := seedRunningInstance(t, srv)
	openTail(t, srv, envID, `{"service":"api"}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/nodes/"+nodeID+"/logs",
		`{"deliveries":[{"request_id":"`+uuid.New().String()+`","data":"noise\n"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("delivery batch should be accepted, got %d", rec.Code)
	}
	if n := buf.Len(); n != 0 {
		t.Fatalf("delivery for an unknown request must not allocate a buffer entry, got %d", n)
	}
}

// Closing is what stops a followed request keeping its node reading Docker every
// tick for output nobody collects.
func TestCloseLogRequestEndsTheTail(t *testing.T) {
	buf := logbuf.New()
	srv := testServer(t, WithLogBuffer(buf))
	envID, nodeID := seedRunningInstance(t, srv)
	reqID := openTail(t, srv, envID, `{"service":"api","follow":true}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/nodes/"+nodeID+"/logs",
		`{"deliveries":[{"request_id":"`+reqID+`","data":"buffered\n"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("deliver: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/logs/"+reqID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body.String())
	}
	// The buffered content goes with it: content outliving the instruction that
	// justified holding it is the one thing this design promises not to do.
	if n := buf.Len(); n != 0 {
		t.Fatalf("closing must free the buffer, %d entries left", n)
	}
}

// ---------------------------------------------------------------- helpers

func jsonRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func openTail(t *testing.T, srv *Server, envID, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, jsonRequest(http.MethodPost, "/v1/envs/"+envID+"/logs", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("open tail: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	return out.ID
}

func readPage(t *testing.T, srv *Server, reqID string, cursor int64, into any) {
	t.Helper()
	path := "/v1/logs/" + reqID
	if cursor > 0 {
		path += "?cursor=" + strconv.FormatInt(cursor, 10)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("read logs: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode log page: %v", err)
	}
}

// seedRunningInstance builds a catalog chain with one deployment whose "api"
// instance has reported a container, which is what a log request resolves
// against. Returns the environment and node ids as strings, for URLs.
func seedRunningInstance(t *testing.T, srv *Server) (envID, nodeID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	org, err := srv.st.GetOrganizationBySlug(ctx, "dev")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	slug := uniqSlug("logs")
	app, err := srv.st.CreateApplication(ctx, org.ID, slug, "Logs Test App")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	stack, err := srv.st.CreateStack(ctx, app.ID, slug)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	ds := &spec.DeploymentSpec{
		SpecVersion: spec.SpecVersion,
		Services: map[string]spec.Service{
			"api": {Name: "api", Image: "nginx:alpine", Swappable: true,
				Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		},
	}
	sv, err := srv.st.CreateStackVersion(ctx, stack.ID, "raw", ds, "tester")
	if err != nil {
		t.Fatalf("CreateStackVersion: %v", err)
	}
	env, err := srv.st.CreateEnvironment(ctx, store.CreateEnvironmentParams{StackID: stack.ID, Slug: slug})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	dep, err := srv.st.CreateDeployment(ctx, store.CreateDeploymentParams{
		EnvironmentID: env.ID, StackVersionID: sv.ID, ResolvedSpec: sv.Spec, CreatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	node, err := srv.st.RegisterNode(ctx, store.RegisterNodeParams{
		OrgID: org.ID, Hostname: slug, AdvertiseAddr: "10.0.0.11",
		CPUMillis: 1000, MemoryBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if err := srv.st.CreateServiceInstances(ctx, dep.ID, node.ID, []store.NewInstance{
		{ServiceName: "api", Swappable: true, ImageRef: "nginx:alpine"},
	}); err != nil {
		t.Fatalf("CreateServiceInstances: %v", err)
	}
	if err := srv.st.UpdateDeploymentState(ctx, dep.ID, store.DeployScheduling, ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	desired, err := srv.st.DesiredStateForNode(ctx, node.ID)
	if err != nil || len(desired) == 0 {
		t.Fatalf("DesiredStateForNode: %v (%d rows)", err, len(desired))
	}
	if err := srv.st.ReportInstance(ctx, node.ID, desired[0].InstanceID, store.ObservedInstance{
		State: store.InstanceRunning, ContainerID: "c-" + slug, HealthStatus: "healthy", SetStarted: true,
	}); err != nil {
		t.Fatalf("ReportInstance: %v", err)
	}
	return env.ID.String(), node.ID.String()
}
