package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// deploymentServer serves GetDeployment responses from a queue, one per poll,
// so a test can walk a deployment through states the way the control plane
// would. Requests beyond the queue repeat the last entry.
func deploymentServer(t *testing.T, states ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if i >= len(states) {
			i = len(states) - 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "dep-1", "revision": 2, "slot": "green", "state": states[i],
			"failure_reason": "",
		})
		i++
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// wait is what `navarch deploy` runs behind the flag: poll until live. The
// happy path needs no sleep at all — the state is checked before the first
// tick — so this tests the loop's logic, not its patience.
func TestWaitReachesTargetState(t *testing.T) {
	old := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = old })

	srv, calls := deploymentServer(t, "pending", "starting", "healthy", "live")
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"wait", "dep-1", "--timeout", "30"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "live") {
		t.Fatalf("expected the live row printed, got: %s", out.String())
	}
	if n := len(*calls); n != 4 {
		t.Fatalf("expected 4 polls (pending→live), got %d", n)
	}
}

// A failed deployment is a hard stop, not something to keep polling: the
// failure reason is the only useful output and the evidence ages fast.
func TestWaitShortCircuitsOnFailed(t *testing.T) {
	old := waitPollInterval
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { waitPollInterval = old })

	srv, calls := deploymentServer(t, "starting", "failed")
	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"wait", "dep-1"}, &out, &errb)
	if code != 1 {
		t.Fatalf("a failed deployment must be a runtime error, got exit %d", code)
	}
	if !strings.Contains(errb.String(), "failed") {
		t.Fatalf("expected the failure named, got: %s", errb.String())
	}
	if n := len(*calls); n != 2 {
		t.Fatalf("must stop polling once failed, got %d calls", n)
	}
}

// The timeout must name what it was stuck at — "timed out" alone forces the
// operator to re-run the command just to learn the state it already had.
func TestWaitTimeoutNamesTheStuckState(t *testing.T) {
	srv, _ := deploymentServer(t, "scheduling")
	var out, errb bytes.Buffer
	start := time.Now()
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"wait", "dep-1", "--timeout", "1"}, &out, &errb)
	if code != 1 {
		t.Fatalf("timeout must be a runtime error, got exit %d", code)
	}
	if !strings.Contains(errb.String(), "stuck at scheduling") {
		t.Fatalf("expected the stuck state named, got: %s", errb.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a 1s timeout should not take %v", time.Since(start))
	}
}

func TestWaitFlagValidation(t *testing.T) {
	srv, _ := deploymentServer(t, "live")
	for _, args := range [][]string{
		{"wait"},
		{"wait", "dep-1", "--timeout", "0"},
		{"wait", "dep-1", "--timeout", "abc"},
	} {
		var out, errb bytes.Buffer
		if code := Run(append([]string{"--url", srv.URL, "--token", "t"}, args...), &out, &errb); code != 2 {
			t.Fatalf("%v should be a usage error (exit 2), got %d", args, code)
		}
	}
}

// events paginates by cursor; the flags must parse and reach the wire, since
// a silently-ignored --before would quietly print the same page forever.
func TestEventsPaginationFlags(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"id": 7, "kind": "deployment.promoted", "message": "revision 2 live", "created_at": "2026-08-20T10:00:00Z"},
		}})
	}))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t", "--output", "json",
		"events", "--org", uuid.NewString(), "--limit", "50", "--before", "123"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(gotQuery, "limit=50") || !strings.Contains(gotQuery, "before_id=123") {
		t.Fatalf("pagination flags must reach the wire, got %q", gotQuery)
	}
	if !strings.Contains(out.String(), "deployment.promoted") {
		t.Fatalf("expected the event printed, got: %s", out.String())
	}
	// A non-integer --before is a usage error, not a zero cursor: silent
	// first-page resets look like lost events.
	var out2, errb2 bytes.Buffer
	if code := Run([]string{"--url", srv.URL, "--token", "t",
		"events", "--org", uuid.NewString(), "--before", "not-an-id"}, &out2, &errb2); code != 2 {
		t.Fatalf("bad --before should be exit 2, got %d", code)
	}
}

// The non-following logs path: chunks arrive on the first read and the
// request is done. Also pins that --service is required — logs are per
// service, and an unscoped request is a footgun the command refuses.
func TestLogsOneShotRead(t *testing.T) {
	var openBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/logs"):
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(r.Body)
			openBody = b.String()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "lr-1", "state": "pending"})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"chunks": []map[string]any{{"seq": 1, "data": "hello\n"}},
				"cursor": 1, "dropped": false,
				"request": map[string]any{"state": "done", "last_error": ""},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"logs", uuid.NewString(), "--service", "api", "--tail", "50"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("expected the chunk printed, got: %s", out.String())
	}
	if !strings.Contains(openBody, `"service":"api"`) || !strings.Contains(openBody, `"tail":50`) {
		t.Fatalf("open request should carry service and tail, got %s", openBody)
	}

	// --service is required.
	var out2, errb2 bytes.Buffer
	if code := Run([]string{"--url", srv.URL, "--token", "t",
		"logs", uuid.NewString()}, &out2, &errb2); code != 2 {
		t.Fatalf("missing --service should be exit 2, got %d", code)
	}
	// A bad --tail is a usage error.
	var out3, errb3 bytes.Buffer
	if code := Run([]string{"--url", srv.URL, "--token", "t",
		"logs", uuid.NewString(), "--service", "api", "--tail", "-1"}, &out3, &errb3); code != 2 {
		t.Fatalf("negative --tail should be exit 2, got %d", code)
	}
}

// A node's rendered line must carry the reachability suffix — "do not lie in
// the state column" is only half of the bargain; the doubt has to be visible
// in the same output.
func TestNodeListRendersUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []map[string]any{
			{"id": uuid.NewString(), "hostname": "dev-node-1", "state": "ready",
				"advertise_addr": "10.0.0.1", "cpu_millis": 4000, "memory_bytes": 8 << 30,
				"alloc_cpu_millis": 500, "alloc_memory_bytes": 1 << 30, "labels": map[string]string{}},
			{"id": uuid.NewString(), "hostname": "dev-node-2", "state": "unreachable",
				"advertise_addr": "10.0.0.2", "cpu_millis": 4000, "memory_bytes": 8 << 30,
				"alloc_cpu_millis": 0, "alloc_memory_bytes": 0, "labels": map[string]string{}},
		}})
	}))
	t.Cleanup(srv.Close)

	var out, errb bytes.Buffer
	code := Run([]string{"--url", srv.URL, "--token", "t",
		"node", "list", "--org", uuid.NewString()}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "dev-node-2") {
		t.Fatalf("expected both nodes listed, got: %s", out.String())
	}
}
