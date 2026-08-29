// Command navarch-web is the browser console.
//
// A separate binary on purpose. It is a second consumer of internal/client, the
// same shape the TUI is — it holds the operator's token server-side and gives
// the browser only a session cookie, so no bearer credential ever reaches
// JavaScript. An install that does not want a web surface simply does not run
// it, and internal/api keeps its stated job: decode, delegate, encode.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/craigderington/navarch/internal/transport"
	"github.com/craigderington/navarch/internal/version"
	"github.com/craigderington/navarch/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	api := envOr("NAVARCH_URL", "http://localhost:8417")
	addr := envOr("NAVARCH_WEB_ADDR", "0.0.0.0:8418")

	// The console forwards operator tokens to the control plane, so where it
	// points is the same security decision it is for the CLI and the agent —
	// and it is checked with the same code, so the three cannot drift.
	if err := transport.CheckBaseURL(api); err != nil {
		if !transport.Insecure(os.Getenv("NAVARCH_INSECURE")) {
			log.Error("refusing to start", "err", err,
				"hint", "set NAVARCH_INSECURE=1 if this network really is trusted")
			os.Exit(1)
		}
		log.Warn("sending credentials in the clear to the control plane", "url", api)
	}

	srv, err := web.New(api, log)
	if err != nil {
		log.Error("could not start the console", "err", err)
		os.Exit(1)
	}

	log.Info("navarch console", "version", version.String(), "addr", addr, "api", api)
	s := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("console stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
