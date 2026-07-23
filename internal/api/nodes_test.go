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
