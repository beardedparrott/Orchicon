package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
)

// killOrphans kills any leftover opencode and orchicon mcp processes from
// a prior crash. These can accumulate when the server is killed before the
// ChatStream subprocess exits (e.g. during a forced binary replacement).
func killOrphans() {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return
	}
	for _, name := range []string{"opencode", "orchicon mcp"} {
		// Split so the argument becomes "-f" (pattern match) + the name.
		args := []string{"-x"}
		if strings.Contains(name, " ") {
			args = []string{"-f"} // use -f for multi-word patterns
		}
		out, err := exec.Command(pgrep, append(args, name)...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || pid == os.Getpid() {
				continue
			}
			if proc, err := os.FindProcess(pid); err == nil {
				proc.Signal(syscall.SIGTERM)
			}
		}
	}
}

// runServe loads configuration from the environment, applies pending
// migrations (same as devStartParent), constructs the control plane server,
// wraps it with the embedded frontend SPA, and runs until SIGTERM or SIGINT.
// It is the production-like server mode — no Compose management, no dev-child
// process forking. Used by the prod instance (scripts/dev-prod.sh) for the
// dogfooding dual-instance setup.
//
// Migrations are run here (not just in devStartParent) so that both
// orchicon-prod serve and scripts/dev-prod.sh's downstream serve call
// consistently use the embedded migration runner — never mixing it with
// atlas migrate apply which writes to a different tracking table.
func runServe() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		telemetry.NewOtelSlogHandler(),
	))
	killOrphans()
	log.Info("orchicon serve starting", "version", version.Current().String())

	cfg := config.Default()
	applyProdDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	// Run embedded migrations before the server starts, using the same
	// tracking table (_orchicon_migrations) that devStartParent uses.
	// This ensures consistency regardless of whether the user calls
	// orchicon-prod start (parent path) or orchicon-prod serve directly.
	if cfg.MigrateOnBoot {
		pool, err := db.Open(ctx, cfg.PostgresDSN)
		if err != nil {
			log.Error("failed to connect for migrations", "error", err)
			return 1
		}
		if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
			log.Error("migrations failed", "error", err)
			pool.Close()
			return 1
		}
		pool.Close()
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
