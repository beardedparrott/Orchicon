package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/logging"
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

// serveEnvLogFile tells the serve child where to write its rotating log
// file (set by serveDetach). When unset, logs go to stdout/stderr.
const serveEnvLogFile = "ORCHICON_SERVE_LOG_FILE"

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
//
// When ORCHICON_SERVE_LOG_FILE is set (detached mode), the JSON slog
// output goes through a rotating file writer (size + time ceiling,
// retention pruning — internal/logging) instead of stdout, and the log
// file is dup2'd onto fds 1/2 so panics and stray prints land in the
// current log. The log rotation config is layered from env/config at
// boot; the server live-applies Settings → Defaults changes afterwards.
func serveForeground() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var logOut io.Writer = os.Stdout
	var rotator *logging.RotatingWriter
	if logPath := os.Getenv(serveEnvLogFile); logPath != "" {
		cfg := config.Default()
		rw, err := logging.New(loggingFromEnv(cfg, logPath))
		if err != nil {
			// Detached: stdout/stderr are /dev/null, so a silent fallback
			// would lose every log line. Surface the failure at the log
			// path the operator is already watching, then bail.
			if ferr, e2 := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); e2 == nil {
				fmt.Fprintf(ferr, "failed to open rotating log file: %v\n", err)
				ferr.Close()
			}
			fmt.Fprintf(os.Stderr, "✗ failed to open rotating log file %s: %v\n", logPath, err)
			return 1
		}
		rotator = rw
		logOut = rw
		logging.RedirectStdStreams(rw.Current())
		rw.SetOnRotate(func() { logging.RedirectStdStreams(rw.Current()) })
		defer rw.Close()
	}

	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}),
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

	srv, err := server.New(cfg, log, rotator)
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
// logs to the rotating file at serveLogFile), writes the PID file, and
// returns immediately — the caller must NOT block, because a tool or
// script waiting on this command would hang until the server exits. The
// caller polls /healthz (see serveStatus / docs).
//
// The child (not the parent) owns the log file: serveForeground opens a
// rotating writer on ORCHICON_SERVE_LOG_FILE, redirects its own fd 1/2
// to it, and live-applies Settings → Defaults log management. The parent
// only passes the path and creates the directory.
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

	cmd := exec.Command(os.Args[0], "serve")
	cmd.Env = append(os.Environ(), serveEnvDetached+"=1", serveEnvLogFile+"="+serveLogFile)
	cmd.Stdin = nil // /dev/null — the child must not inherit the caller's stdin
	cmd.Stdout = nil
	cmd.Stderr = nil
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

// loggingFromEnv layers the log rotation config for a given log file
// path. Precedence: ORCHICON_LOG_* env vars, then built-in defaults. The
// DB-backed Settings → Defaults values are applied live by the server
// after boot (server.New + Run). logPath wins as the directory/base even
// when ORCHICON_LOG_DIR is set, because it is the path serveDetach
// actually opened for the PID/log contract.
func loggingFromEnv(cfg config.Config, logPath string) logging.Config {
	c := logging.DefaultConfig()
	if d := os.Getenv("ORCHICON_LOG_DIR"); d != "" {
		c.Dir = d
	}
	if v := os.Getenv("ORCHICON_LOG_MAX_SIZE_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxSizeBytes = n << 20
		}
	}
	if v := os.Getenv("ORCHICON_LOG_ROLL_INTERVAL_HOURS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.RollInterval = time.Duration(n) * time.Hour
		}
	}
	if v := os.Getenv("ORCHICON_LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.RetentionDays = int(n)
		}
	}
	if v := os.Getenv("ORCHICON_LOG_MAX_FILES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxFiles = int(n)
		}
	}
	dir, base := filepath.Dir(logPath), filepath.Base(logPath)
	if dir != "" {
		c.Dir = dir
	}
	if base != "" {
		c.BaseName = base
	}
	return c
}
