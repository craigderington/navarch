package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craigderington/navarch/internal/metrics"
)

// ---------------------------------------------------------------- healthCheck

// startProbeServer runs a real HTTP server on an ephemeral loopback port and
// returns its port. healthCheck dials a real socket — it is the container
// healthcheck, and the thing it asserts is that the process can actually
// serve, which an httptest recorder cannot stand in for.
func startProbeServer(t *testing.T, status int) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// The configured listen address is a *bind* address, and a wildcard bind is
// not a valid destination to dial — the probe must rewrite it to loopback or
// the container reports unhealthy while the server is serving fine.
func TestHealthCheckProbesLoopbackForWildcardBinds(t *testing.T) {
	for _, bind := range []string{":%d", "0.0.0.0:%d", "[::]:%d"} {
		t.Run(bind, func(t *testing.T) {
			port := startProbeServer(t, http.StatusOK)
			t.Setenv("COMPOSECTL_LISTEN_ADDR", fmt.Sprintf(bind, port))
			if err := healthCheck(); err != nil {
				t.Fatalf("expected healthy, got: %v", err)
			}
		})
	}
}

// A 503 from /healthz means the process is up but cannot serve — still
// unhealthy as far as Compose is concerned.
func TestHealthCheckFailsWhenUnhealthy(t *testing.T) {
	port := startProbeServer(t, http.StatusServiceUnavailable)
	t.Setenv("COMPOSECTL_LISTEN_ADDR", fmt.Sprintf("127.0.0.1:%d", port))
	if err := healthCheck(); err == nil {
		t.Fatal("expected an error from a 503 healthz")
	}
}

func TestHealthCheckFailsWhenUnreachable(t *testing.T) {
	// Reserve a port and release it, so the address is genuinely closed
	// rather than guessing at an unused one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	t.Setenv("COMPOSECTL_LISTEN_ADDR", fmt.Sprintf("127.0.0.1:%d", port))
	if err := healthCheck(); err == nil {
		t.Fatal("expected an error from an unreachable server")
	}
}

func TestHealthCheckRejectsUnparseableAddr(t *testing.T) {
	t.Setenv("COMPOSECTL_LISTEN_ADDR", "not-a-host-port")
	if err := healthCheck(); err == nil {
		t.Fatal("expected a parse error")
	}
}

// ------------------------------------------------------------------- runLoop

func TestRunLoopTicksUntilCancelled(t *testing.T) {
	var ticks atomic.Int32
	tick := func(context.Context) error { ticks.Add(1); return nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, 2*time.Millisecond, slog.Default(), metrics.New(), "test", tick)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runLoop did not return after cancel")
	}
	if ticks.Load() < 2 {
		t.Fatalf("expected at least 2 ticks, got %d", ticks.Load())
	}
}

// A tick that fails must not stop the loop: the scheduler, controller and
// reaper run unattended, and a transient database error at 2am must not
// quietly become "the scheduler stopped until someone noticed".
func TestRunLoopSurvivesTickErrors(t *testing.T) {
	var ticks atomic.Int32
	tick := func(context.Context) error { ticks.Add(1); return errors.New("boom") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, 2*time.Millisecond, slog.Default(), metrics.New(), "test", tick)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runLoop did not return after cancel")
	}
	if ticks.Load() < 2 {
		t.Fatalf("expected the loop to keep ticking past errors, got %d ticks", ticks.Load())
	}
}

// The loop's tick timeout exists so a slow database cannot wedge the loop;
// a tick that fails must still be observed — through the Prometheus surface
// an operator would actually scrape, not an internal accessor.
func TestRunLoopObservesTickErrorsInMetrics(t *testing.T) {
	reg := metrics.New()
	tick := func(context.Context) error { return errors.New("boom") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, 2*time.Millisecond, slog.Default(), reg, "test", tick)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	var buf bytes.Buffer
	reg.WritePrometheus(&buf, metrics.Gauges{})
	if !strings.Contains(buf.String(), `composectl_loop_runs_total{name="test",result="error"}`) {
		t.Fatalf("expected an error counter for loop \"test\" in Prometheus output:\n%s", buf.String())
	}
}
