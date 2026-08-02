package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
// migrations, constructs the control plane server, wraps it with the
// embedded frontend SPA, and runs until SIGTERM or SIGINT.
// It is the production-like server mode — no Compose management, no
// process forking. Used headless (`orchicon serve --detach`) and as the
// control-plane child of the single-container supervisor (`orchicon
// container`, which spawns `orchicon serve`).
//
// Migrations are run here (the embedded runner writes the
// _orchicon_migrations tracking table) so every boot path — headless,
// detached, or container — stays consistent.
// serveEnvDetached marks a serve subprocess that was forked by `serve
// --detach`; it tells the child to run the server directly instead of
// forking again.
const serveEnvDetached = "ORCHICON_SERVE_DETACHED"

// runServe dispatches `serve` subcommands:
//
//	orchicon serve              run the server in the foreground (blocks)
//	orchicon serve --detach     fork the server into the background, write
//	                            the PID file, wait for /healthz, and return
//	orchicon serve --stop       stop a detached serve via the PID file
//
// --detach exists so scripts and AI agents can start the control plane
// without a command that never returns (a foreground server keeps the
// caller's stdout/stderr pipe open and hangs the session).
func runServe(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--detach", "-d":
			if os.Getenv(serveEnvDetached) == "" {
				return serveDetach()
			}
			// fall through: this is the forked child — run the server.
		case "--stop":
			return serveStop()
		case "--status":
			return serveStatus()
		}
	}
	return serveForeground()
}

// serveForeground runs the server until SIGTERM/SIGINT. Also the body of
// the forked child started by serveDetach (the ORCHICON_SERVE_DETACHED
// env var short-circuits the dispatch above so the child never re-forks).
func serveForeground() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		telemetry.NewOtelSlogHandler(),
	))
	killOrphans()
	log.Info("orchicon serve starting", "version", version.Current().String())

	cfg := config.Default()
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

// serveDetach forks the server into the background (own process group,
// logs to the same file the dev subcommand uses), writes the PID file,
// and returns immediately — the caller must NOT block, because a tool or
// script waiting on this command would hang until the server exits. The
// caller polls /healthz (see serveStatus / docs).
func serveDetach() int {
	if pid, running := procRunning(servePIDFile); running {
		fmt.Fprintf(os.Stderr, "✗ serve is already running (PID %s)\n", pid)
		fmt.Fprintf(os.Stderr, "  Stop it with: %s serve --stop\n", filepath.Base(os.Args[0]))
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(servePIDFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create PID directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(serveLogFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create log directory: %v\n", err)
		return 1
	}
	logFile, err := os.OpenFile(serveLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to open log file: %v\n", err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(os.Args[0], "serve")
	cmd.Env = append(os.Environ(), serveEnvDetached+"=1")
	cmd.Stdin = nil // /dev/null — the child must not inherit the caller's stdin
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setProcAttrBackground(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to start serve: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(servePIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  ! failed to write PID file: %v\n", err)
	}
	// Release the child from our care so nothing waits on it. Start it in
	// its own process group (setProcAttrBackground) and detach.
	_ = cmd.Process.Release()

	fmt.Printf("✓ serve detached (PID %d)\n", pid)
	fmt.Printf("  Logs: %s\n", serveLogFile)
	fmt.Printf("  Check: %s serve --status\n", filepath.Base(os.Args[0]))
	fmt.Printf("  Stop: %s serve --stop\n", filepath.Base(os.Args[0]))
	return 0
}

// serveStop sends SIGTERM to a detached serve and clears the PID file.
func serveStop() int {
	pid, running := procRunning(servePIDFile)
	if !running {
		fmt.Println("serve is not running")
		return 1
	}
	pidNum, err := strconv.Atoi(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ invalid PID file (%s): %v\n", servePIDFile, err)
		return 1
	}
	if proc, err := os.FindProcess(pidNum); err == nil {
		if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintf(os.Stderr, "✗ failed to signal PID %d: %v\n", pidNum, err)
			return 1
		}
	}
	_ = os.Remove(servePIDFile)
	fmt.Printf("✓ serve stopped (PID %d)\n", pidNum)
	return 0
}

// serveStatus prints whether a detached serve is running.
func serveStatus() int {
	if pid, running := procRunning(servePIDFile); running {
		fmt.Printf("serve is running (PID %s)\n", pid)
		return 0
	}
	fmt.Println("serve is not running")
	return 1
}
