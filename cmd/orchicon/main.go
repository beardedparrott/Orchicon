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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcp"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
)

func main() {
	// Wire the OTel slog bridge so the default-mode control plane
	// (not just the dev subcommand) also streams its structured log
	// records into the Telemetry logs tab. The OTel handler is a
	// no-op until telemetry.Setup binds the global LoggerProvider
	// inside server.New.
	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		telemetry.NewOtelSlogHandler(),
	))

	// Subcommand dispatch. If the first arg matches a known subcommand,
	// dispatch to it; otherwise run the control plane (default).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runServe(os.Args[2:]))
		case "container":
			os.Exit(runContainer(os.Args[2:]))
		case "mcp":
			os.Exit(runMCP(context.Background(), os.Args[2:], log))
		case "db":
			os.Exit(runDB(os.Args[2:], log))
		case "runtime-daemon":
			os.Exit(exitOnErr(runRuntimeDaemon(os.Args[2:], log)))
		case "runtime-supervisor":
			os.Exit(exitOnErr(runRuntimeSupervisor(os.Args[2:], log)))
		case "runtime-client":
			os.Exit(exitOnErr(runRuntimeClient(os.Args[2:], log)))
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

func runDB(args []string, log *slog.Logger) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: orchicon db <backup|restore|list|prune>\n")
		return 1
	}

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid config", "error", err)
		return 1
	}

	dir, err := backup.DefaultDir()
	if err != nil {
		log.Error("backup directory", "error", err)
		return 1
	}

	ctx := context.Background()

	switch args[0] {
	case "backup":
		info, err := backup.Create(ctx, cfg.PostgresDSN, dir)
		if err != nil {
			log.Error("backup failed", "error", err)
			return 1
		}
		fmt.Printf("Created: %s (%d bytes)\n", info.Name, info.SizeBytes)
		return 0

	case "restore":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: orchicon db restore <backup-name>\n")
			fmt.Fprintf(os.Stderr, "Use \"orchicon db list\" to see available backups.\n")
			return 1
		}
		path := args[1]
		if !strings.Contains(path, string(filepath.Separator)) {
			path = filepath.Join(dir, path)
		}
		if err := backup.Restore(ctx, cfg.PostgresDSN, path); err != nil {
			log.Error("restore failed", "error", err)
			return 1
		}
		fmt.Println("Restore complete.")
		return 0

	case "list":
		backups, err := backup.List(dir)
		if err != nil {
			log.Error("list backups", "error", err)
			return 1
		}
		if len(backups) == 0 {
			fmt.Println("No backups found.")
			return 0
		}
		for _, b := range backups {
			age := time.Since(b.CreatedAt).Truncate(time.Second)
			sz := b.SizeBytes
			var unit string
			switch {
			case sz > 1<<30:
				sz /= 1 << 30
				unit = "GB"
			case sz > 1<<20:
				sz /= 1 << 20
				unit = "MB"
			case sz > 1<<10:
				sz /= 1 << 10
				unit = "KB"
			default:
				unit = "B"
			}
			fmt.Printf("%-40s %4d %-2s  %s ago\n", b.Name, sz, unit, age)
		}
		return 0

	case "prune":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: orchicon db prune <days>\n")
			return 1
		}
		var days int
		if _, err := fmt.Sscanf(args[1], "%d", &days); err != nil || days < 1 {
			fmt.Fprintf(os.Stderr, "days must be a positive integer\n")
			return 1
		}
		removed, err := backup.Prune(dir, days)
		if err != nil {
			log.Error("prune failed", "error", err)
			return 1
		}
		fmt.Printf("Pruned %d backup(s) older than %d day(s).\n", removed, days)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown db subcommand: %s\n", args[0])
		fmt.Fprintf(os.Stderr, "Usage: orchicon db <backup|restore|list|prune>\n")
		return 1
	}
}

// exitOnErr turns an error into a process exit code (1 on error, 0 nil).
// Used by the runtime subcommands, which return error values.
func exitOnErr(err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func printHelp() {
	bin := filepath.Base(os.Args[0])
	fmt.Printf(`%s %s — Orchicon control plane

Usage:
  %s                Run the control plane (API + relay + reconcilers)
  %s serve          Run the control plane with embedded frontend (headless)
  %s serve --detach Fork the server into the background (PID file + /healthz)
  %s serve --stop   Stop a detached server
  %s container      Run the whole stack as PID 1 inside the single-container image
  %s mcp            Start the MCP stdio server (for opencode tool integration)
  %s db backup      Create a database snapshot
  %s db list        List available backups
  %s db restore     Restore from a backup
  %s db prune       Remove backups older than N days
`, bin, version.Current().Tag, bin, bin, bin, bin, bin, bin, bin, bin, bin, bin)

	fmt.Printf(`
  %s version       Print version info

The binary embeds the single-container runtime configs, migrations, and
the frontend bundle. Run the full stack with `+"`docker run`"+` (see
DOCUMENTATION.md §Single-Container Deployment) or `+"`%s container`"+` as
the container's PID-1 supervisor.
`, bin, bin)
}

func runMCP(ctx context.Context, args []string, log *slog.Logger) int {
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("db open", "error", err)
		return 1
	}
	defer pool.Close()

	toolReg := askorchicon.NewToolRegistry(pool, log)
	mcpSrv := mcp.New(log, pool, mcp.NewAskOrchiconRegistry(toolReg))

	log.Info("mcp server started (stdio transport)")
	if err := mcpSrv.Run(ctx); err != nil {
		log.Error("mcp server", "error", err)
		return 1
	}
	return 0
}
