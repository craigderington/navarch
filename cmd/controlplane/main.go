// Command controlplane runs the composectl API server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/craig/composectl/internal/api"
	"github.com/craig/composectl/internal/config"
	"github.com/craig/composectl/internal/store"
)

func main() {
	probe := flag.Bool("healthcheck", false,
		"probe the running server's /healthz and exit 0 (healthy) or 1")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if *probe {
		if err := healthCheck(); err != nil {
			log.Error("healthcheck failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthCheck probes the already-running server over loopback and reports
// the result as an exit code. Compose invokes this as the container's
// healthcheck: the distroless image ships no shell and no curl, so the
// binary has to be able to test itself.
func healthCheck() error {
	host, port, err := net.SplitHostPort(config.ListenAddr())
	if err != nil {
		return fmt.Errorf("parse listen addr: %w", err)
	}
	// The configured value is a *bind* address. A wildcard bind is not a
	// valid destination to dial, so probe loopback instead.
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// /healthz pings the database, so a 503 here means the process is up
	// but cannot serve — still unhealthy as far as Compose is concerned.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewServer(st, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("control plane listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
