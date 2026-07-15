// Command orchicon is the Orchicon control plane binary.
//
// It is a single Go binary (docs/01_Architecture_Vision.md §2) that
// serves the API, runs reconcilers, the outbox relay, recovery engine,
// policy engine, and AI gateway. v0.1 ships a minimal HTTP server with
// health/version endpoints; later phases add the full surface.
//
// Subcommands:
//
//	(default)        Run the control plane (serve API + relay + reconcilers)
//	dev              Manage the full local dev stack (compose → migrate → serve)
//	version          Print version info
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Subcommand dispatch. If the first arg matches a known subcommand,
	// dispatch to it; otherwise run the control plane (default).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "dev":
			os.Exit(runDev(os.Args[2:]))
		case "start":
			os.Exit(runDev([]string{"start"}))
		case "stop":
			os.Exit(runDev([]string{"stop"}))
		case "status":
			os.Exit(runDev([]string{"status"}))
		case "restart":
			os.Exit(runDev([]string{"restart"}))
		case "version", "--version", "-v":
			fmt.Println(version.Current().String())
			return
		case "--help", "-h":
			printHelp()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
			printHelp()
			os.Exit(1)
		}
	}

	log.Info("orchicon control plane", "version", version.Current().String())

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		os.Exit(1)
	}
	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
	log.Info("orchicon stopped")
}

func printHelp() {
	fmt.Printf(`orchicon %s — Orchicon control plane

Usage:
  orchicon              Run the control plane (API + relay + reconcilers)
  orchicon dev start    Start the full dev stack (compose → migrate → serve)
  orchicon dev stop     Stop the dev stack
  orchicon dev status   Show what's running

Short aliases:
  orchicon start        Same as "orchicon dev start"
  orchicon stop         Same as "orchicon dev stop"

  orchicon version      Print version info

The binary embeds the Docker Compose stack, migrations, and the frontend
bundle, so `+"`orchicon start`"+` is the complete one-command experience.
`, version.Current().Tag)
}
