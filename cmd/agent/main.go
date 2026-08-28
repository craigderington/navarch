// Command agent runs the composectl node agent: it reconciles the local Docker
// daemon to the control plane's desired state for this node.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/craigderington/navarch/internal/agent"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.Run(ctx, cfg, log); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}
