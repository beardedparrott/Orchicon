package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
)

// runServe loads configuration from the environment, constructs the control
// plane server, wraps it with the embedded frontend SPA, and runs until
// SIGTERM or SIGINT. It is the production-like server mode — no Compose
// management, no dev-child process forking. Used by the prod instance
// (scripts/dev-prod.sh) for the dogfooding dual-instance setup.
func runServe() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		telemetry.NewOtelSlogHandler(),
	))
	log.Info("orchicon serve starting", "version", version.Current().String())

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		return 1
	}

	handler := withFrontend(srv.Handler(), log)
	srv.SetHandler(handler)

	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "error", err)
		return 1
	}
	log.Info("orchicon serve stopped")
	return 0
}
