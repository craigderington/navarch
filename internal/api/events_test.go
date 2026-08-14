package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craig/composectl/internal/metrics"
	"github.com/craig/composectl/internal/store"
	"github.com/google/uuid"
)

func TestMetricsEndpoint(t *testing.T) {
	srv := testServer(t)
	srv.metrics = metrics.New()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type=%q", got)
	}
	if !strings.Contains(rec.Body.String(), "composectl_database_up 1") {
		t.Fatalf("missing database gauge: %s", rec.Body)
	}
}

func TestOrganizationEventsEndpoint(t *testing.T) {
	srv := testServer(t)
	nodeID, _ := newReportableInstance(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var orgID uuid.UUID
	if err := srv.st.Pool().QueryRow(ctx, `SELECT org_id FROM nodes WHERE id=$1`, nodeID).Scan(&orgID); err != nil {
		t.Fatalf("node org: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/orgs/"+orgID.String()+"/events?limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Events []store.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].Kind != "deployment.created" {
		t.Fatalf("unexpected events: %+v", body.Events)
	}
}
