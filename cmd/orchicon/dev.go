// Command orchicon dev manages the full local development stack from within
// the binary itself (AGENTS.md §Dev Control Script). It replaces the
// repo-local scripts/dev.sh for installed binaries: the Docker Compose
// stack, migrations, and frontend bundle are embedded via go:embed so the
// user needs only Docker + the orchicon binary.
//
// Control plane runs in the background (forked child process); logs go to
// .dev/logs/orchicon.log; dev start tails the log after launching.
// Ctrl-C on the tail does not kill the server — run orchicon dev stop
// for a clean shutdown.
//
// Usage:
//
//	orchicon dev start    compose up → migrate → fork background server → tail log
//	orchicon dev stop     signal server → compose down
//	orchicon dev status   show what's running
//	orchicon dev restart  stop then start
//	orchicon dev logs     tail control plane logs
//
// When scripts/dev.sh detects the binary on PATH, it delegates here.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
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
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/version"
)

const (
	devPIDFile  = ".dev/pids/orchicon.pid"
	devLogFile  = ".dev/logs/orchicon.log"
	devEnvChild = "ORCHICON_DEV_CHILD"
)

// runDev dispatches to the dev subcommand. Returns an exit code.
func runDev(args []string) int {
	if len(args) == 0 {
		printDevUsage()
		return 1
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	switch args[0] {
	case "start":
		return devStart(log)
	case "stop":
		return devStop(log)
	case "status":
		return devStatus(log)
	case "restart":
		_ = devStop(log)
		return devStart(log)
	case "logs":
		return devLogs(log)
	case "--help", "-h":
		printDevUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown dev subcommand: %s\n", args[0])
		printDevUsage()
		return 1
	}
}

func printDevUsage() {
	fmt.Fprintf(os.Stderr, `orchicon dev — manage the local development stack

Usage:
  orchicon dev start     Start the full stack (compose → migrate → serve in background)
  orchicon dev stop      Stop the stack (signal server → compose down)
  orchicon dev status    Show what's running
  orchicon dev restart   Stop then start
  orchicon dev logs      Tail control plane logs

The binary embeds the Docker Compose stack, migrations, and the frontend
bundle, so no Go, Node, or source checkout is required — only Docker.
`)
}

// --- Start ------------------------------------------------------------------

// devStart dispatches to parent or child based on environment.
func devStart(log *slog.Logger) int {
	if os.Getenv(devEnvChild) != "" {
		return devStartChild()
	}
	return devStartParent()
}

// devStartChild runs the control plane server. Called from the background
// child process. Output goes to .dev/logs/orchicon.log via inherited FDs.
func devStartChild() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("orchicon control plane starting", "version", version.Current().String())

	cfg := config.Default()
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
	log.Info("orchicon control plane stopped")
	return 0
}

// devStartParent brings up the compose stack, runs migrations, starts the
// server in the background, waits for it to be healthy, then tails the log
// file. Ctrl-C stops the tail only — the server keeps running.
func devStartParent() int {
	fmt.Printf("orchicon dev start %s\n\n", version.Current().String())

	// 1. Check if already running.
	if pid, running := procRunning(devPIDFile); running {
		fmt.Fprintf(os.Stderr, "✗ orchicon is already running (PID %s)\n", pid)
		fmt.Fprintf(os.Stderr, "  Run 'orchicon dev logs' to tail its logs.\n")
		fmt.Fprintf(os.Stderr, "  Run 'orchicon dev stop' to stop it.\n")
		return 1
	}

	// 2. Start Docker Compose stack.
	fmt.Println("▸ Starting dev stack (Docker Compose)…")
	if err := composeUp(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Compose up failed: %v\n", err)
		return 1
	}

	fmt.Println("▸ Waiting for containers to be healthy…")
	if err := waitForContainer("postgres", 60); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Postgres did not become healthy: %v\n", err)
		return 1
	}
	fmt.Println("  ✓ postgres healthy")

	if err := waitForContainer("nats", 30); err != nil {
		fmt.Fprintf(os.Stderr, "  ! nats not healthy: %v\n", err)
	} else {
		fmt.Println("  ✓ nats healthy")
	}

	// 3. Apply migrations.
	fmt.Println("▸ Applying migrations…")
	ctx := context.Background()
	cfg := config.Default()
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Failed to connect to database: %v\n", err)
		return 1
	}
	defer pool.Close()

	if err := db.SeedDevTenant(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "  ! Seed dev tenant: %v\n", err)
	}
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Migrations failed: %v\n", err)
		return 1
	}
	fmt.Println("  ✓ Migrations applied")

	// 4. Ensure .dev/ directories exist.
	if err := os.MkdirAll(filepath.Dir(devPIDFile), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Failed to create PID directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(devLogFile), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Failed to create log directory: %v\n", err)
		return 1
	}

	// 5. Fork the server process in the background.
	fmt.Println("▸ Starting control plane…")

	logFile, err := os.OpenFile(devLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Failed to open log file: %v\n", err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(os.Args[0], "dev", "start")
	cmd.Env = append(os.Environ(), devEnvChild+"=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setProcAttrBackground(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Failed to start server process: %v\n", err)
		return 1
	}

	pid := cmd.Process.Pid
	if err := os.WriteFile(devPIDFile, []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "  ! Failed to write PID file: %v\n", err)
	}
	fmt.Printf("  ✓ Server process started (PID %d)\n", pid)

	// 6. Wait for healthz.
	fmt.Print("▸ Waiting for control plane to be ready…")
	healthURL := "http://localhost" + cfg.HTTPAddr + "/healthz"
	healthy := false
	for i := 0; i < 30; i++ {
		if probeHTTP(context.Background(), healthURL) {
			healthy = true
			break
		}
		time.Sleep(time.Second)
	}
	if !healthy {
		fmt.Fprintf(os.Stderr, "\n✗ Control plane did not become healthy within 30s\n")
		_ = cmd.Process.Kill()
		_ = os.Remove(devPIDFile)
		_ = os.Remove(devLogFile)
		return 1
	}
	fmt.Println(" ✓")

	// 7. Print endpoint info.
	fmt.Println()
	fmt.Println("  ✓ Orchicon is running")
	fmt.Printf("    Control plane:  http://localhost%s\n", cfg.HTTPAddr)
	fmt.Println("    SigNoz UI:      http://localhost:3301")
	fmt.Println("    NATS monitor:   http://localhost:8222")
	fmt.Printf("    Logs:           %s\n", devLogFile)
	fmt.Printf("    PID:            %d\n", pid)
	fmt.Println()
	fmt.Println("→ Tailing control plane logs (Ctrl+C to stop tailing; server continues in background)")
	fmt.Println()

	// 8. Tail log file until Ctrl-C.
	followFile(devLogFile, os.Stdout)

	fmt.Println()
	fmt.Println("◆ Log tail ended. Server continues running in background.")
	fmt.Println("  Run 'orchicon dev stop' to stop the server.")
	fmt.Println("  Run 'orchicon dev logs' to tail logs again.")
	return 0
}

// --- Stop -------------------------------------------------------------------

// devStop signals the background server to shut down gracefully, then tears
// down the Docker Compose stack.
//
// Orphan handling: after the PID-file process is dealt with we scan for any
// other running `orchicon` processes (by executable name) and SIGKILL them.
// This catches the common "I left a stray orchicon running somewhere"
// case which `dev stop` couldn't otherwise see — and which holds the
// binary file lock so `mv` of a newer binary returns "Text file busy".
// This is the user's installer-time escape valve: we use SIGKILL
// directly because by the time we get here the user has already asked
// for a stop, the PID-file process had its 15s grace period, and any
// remaining orphans are almost always stuck (their parent context is
// gone, they're blocked on a syscall that won't return, etc.) — SIGTERM
// would just bounce. SIGKILL is the only signal that guarantees the
// mmap'd binary is released so a new binary can be installed.
func devStop(log *slog.Logger) int {
	fmt.Println("▸ orchicon dev stop")

	// 1. Signal child to stop gracefully.
	var stoppedPID int
	if pid, running := procRunning(devPIDFile); running {
		fmt.Printf("▸ Signaling control plane (PID %s)…\n", pid)
		p, err := strconv.Atoi(strings.TrimSpace(pid))
		if err == nil {
			stoppedPID = p
			proc, err := os.FindProcess(p)
			if err == nil {
				_ = proc.Signal(syscall.SIGTERM)
				// Wait up to 15s for graceful shutdown.
				for i := 0; i < 30; i++ {
					if proc.Signal(syscall.Signal(0)) != nil {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
				if proc.Signal(syscall.Signal(0)) == nil {
					fmt.Println("  ! Process did not exit in time, sending SIGKILL")
					_ = proc.Kill()
				}
				fmt.Println("  ✓ Control plane stopped")
			}
		}
	} else if pid != "" {
		fmt.Printf("  ! PID file exists but process is not running (removing stale PID file)\n")
	} else {
		fmt.Println("  - Control plane not running (no PID file)")
	}
	_ = os.Remove(devPIDFile)

	// 1b. Belt-and-suspenders: kill any orphan orchicon processes that
	// the PID file didn't know about (e.g. a previous install left a
	// running binary whose PID file was clobbered). Excludes the PID
	// we just stopped (and us, internally) so we don't double-kill.
	killOrphanOrchiconProcs(stoppedPID)

	// 2. Compose down.
	fmt.Println("▸ Stopping dev stack (Docker Compose)…")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := composeDown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "  ! Compose down: %v\n", err)
	} else {
		fmt.Println("  ✓ Dev stack stopped")
	}

	return 0
}

// --- Status -----------------------------------------------------------------

// devStatus checks what's running and probes key endpoints.
func devStatus(log *slog.Logger) int {
	fmt.Println("Orchicon dev status")
	fmt.Println()

	// Control plane via PID file.
	fmt.Println("Control plane:")
	if pid, running := procRunning(devPIDFile); running {
		if probeHTTP(context.Background(), "http://localhost:8080/healthz") {
			fmt.Printf("  ✓ Running (PID %s) — healthy\n", pid)
		} else {
			fmt.Printf("  ! Running (PID %s) — not responding on :8080\n", pid)
		}
	} else if _, exists := os.Stat(devPIDFile); exists == nil {
		fmt.Println("  ! PID file exists but process is not running (stale)")
	} else {
		fmt.Println("  - Not running")
	}

	// Docker Compose services.
	fmt.Println()
	fmt.Println("Docker Compose services:")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runComposeFromTemp(ctx, log, "ps", "--format", "table {{.Name}}\t{{.Service}}\t{{.Status}}"); err != nil {
		fmt.Fprintf(os.Stderr, "  ! Compose ps failed: %v\n", err)
	}

	// Endpoint probes.
	fmt.Println()
	fmt.Println("Endpoints:")
	endpoints := []struct {
		url, label string
	}{
		{"http://localhost:8080/healthz", "Control plane"},
		{"http://localhost:8222/healthz", "NATS"},
		{"http://localhost:3301/api/v1/health", "SigNoz"},
	}
	for _, ep := range endpoints {
		if probeHTTP(ctx, ep.url) {
			fmt.Printf("  ✓ %-16s %s  ok\n", ep.label, ep.url)
		} else {
			fmt.Printf("  ✗ %-16s %s  unreachable\n", ep.label, ep.url)
		}
	}
	return 0
}

// --- Logs -------------------------------------------------------------------

// devLogs tails the control plane log file.
func devLogs(log *slog.Logger) int {
	if _, err := os.Stat(devLogFile); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "✗ Log file not found: %s\n", devLogFile)
		fmt.Fprintf(os.Stderr, "  Is Orchicon running? Run 'orchicon dev start' first.\n")
		return 1
	}
	fmt.Println("◆ Tailing control plane logs (Ctrl+C to stop)")
	followFile(devLogFile, os.Stdout)
	fmt.Println()
	return 0
}

// followFile reads path from the current end and prints new content to w as
// it is written (like tail -f).  Returns when reading fails (e.g. file
// deleted) or when interrupted (the caller should handle signals).  Works on
// any OS — no external command needed.
func followFile(path string, w io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Cannot open log file: %v\n", err)
		return
	}
	defer f.Close()

	// Seek to end so we only show new content.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Cannot seek log file: %v\n", err)
		return
	}

	br := bufio.NewReader(f)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// No new data yet — wait briefly then retry.
				time.Sleep(200 * time.Millisecond)
				continue
			}
			// File was deleted or truncated — stop following.
			return
		}
		fmt.Fprint(w, line)
	}
}

// --- Helpers ----------------------------------------------------------------

// procRunning reads the PID file and checks whether the corresponding
// process is alive. Returns the PID string and a bool indicating if it's
// running.
func procRunning(pidFile string) (string, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return "", false
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		return "", false
	}
	p, err := strconv.Atoi(pid)
	if err != nil {
		return pid, false
	}
	proc, err := os.FindProcess(p)
	if err != nil {
		return pid, false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return pid, false
	}
	return pid, true
}

// --- Compose helpers --------------------------------------------------------

// extractComposeDir writes the embedded deploy/compose/ directory to a
// fresh temp directory and returns its path. The docker-compose.yml uses
// relative mounts (e.g. ./clickhouse-cluster.xml:/etc/...) so docker
// compose must run from a directory that contains all the side files;
// extracting the whole tree (not just the YAML) is what makes those
// mounts land correctly. See assets.go for why.
//
// The embed.FS root is the repo root, so the embedded paths start with
// "deploy/compose/" — we strip that prefix when laying files out so the
// docker-compose.yml lands at the top of the temp dir.
//
// Caller is responsible for os.RemoveAll on the returned dir when done.
func extractComposeDir() (string, error) {
	dir, err := os.MkdirTemp("", "orchicon-compose-*")
	if err != nil {
		return "", fmt.Errorf("create temp compose dir: %w", err)
	}
	const prefix = "deploy/compose/"
	err = fs.WalkDir(assets.ComposeFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !strings.HasPrefix(path, prefix) {
			return nil
		}
		rel := strings.TrimPrefix(path, prefix)
		if rel == "" || d.IsDir() {
			return nil
		}
		target := filepath.Join(dir, rel)
		data, err := assets.ComposeFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// runComposeFromTemp extracts the embedded compose tree, runs `docker
// compose` with the given sub-args from that directory, and removes the
// temp dir on return. Used by composeUp / composeDown / composeStatus.
//
// The project name is pinned to "orchicon" (`-p orchicon`) so successive
// invocations target the same containers regardless of the per-invocation
// temp dir the compose YAML lives in.
func runComposeFromTemp(ctx context.Context, log *slog.Logger, args ...string) error {
	dir, err := extractComposeDir()
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && log != nil {
			log.Warn("remove temp compose dir", "dir", dir, "error", rmErr)
		}
	}()

	fullArgs := append([]string{
		"compose",
		"-p", "orchicon",
		"-f", filepath.Join(dir, "docker-compose.yml"),
	}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// composeUp extracts the embedded compose tree to a temp dir and runs
// `docker compose up -d` from there. The dir is removed on return.
func composeUp(ctx context.Context) error {
	if err := runComposeFromTemp(ctx, nil, "up", "-d"); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// composeDown tears down all orchicon Docker containers. It tries three
// approaches in order of preference so that a subsequent `orchicon start`
// never fails with "Conflict: container name already in use".
func composeDown(ctx context.Context) error {
	// 1. Polite: docker compose down with the embedded compose file.
	err := runComposeFromTemp(ctx, nil, "down", "--remove-orphans")
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "  ! compose down failed, force-removing containers: %v\n", err)

	// 2. Forceful: remove every container belonging to the orchicon
	// compose project by label. This catches containers that were
	// started outside the compose file or have irregular names.
	prune := exec.CommandContext(ctx, "docker", "container", "prune",
		"--force", "--filter", "label=com.docker.compose.project=orchicon")
	prune.Stderr = os.Stderr
	prune.Stdout = os.Stdout
	_ = prune.Run()

	// 3. Nuclear: any container whose name starts with "orchicon-" that
	// might have slipped through (e.g. from a legacy run without compose
	// labels). Idempotent — `docker rm -f` is a no-op if the container
	// does not exist.
	known := []string{
		"orchicon-postgres", "orchicon-nats", "orchicon-clickhouse",
		"orchicon-signoz-schema-migrator", "orchicon-otel-collector", "orchicon-signoz",
	}
	for _, name := range known {
		rm := exec.CommandContext(ctx, "docker", "rm", "-f", name)
		rm.Stderr = os.Stderr
		rm.Stdout = os.Stdout
		_ = rm.Run()
	}

	return fmt.Errorf("docker compose down failed (containers force-removed): %w", err)
}

// waitForContainer polls `docker inspect` for the container's health
// status.
func waitForContainer(service string, maxRetries int) error {
	for i := 0; i < maxRetries; i++ {
		cmd := exec.Command("docker", "inspect",
			"--format", "{{.State.Health.Status}}",
			"orchicon-"+service)
		out, err := cmd.Output()
		if err == nil {
			status := strings.TrimSpace(string(out))
			if status == "healthy" {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s did not become healthy after %d retries", service, maxRetries)
}

// probeHTTP returns true if the URL returns a 2xx status code.
func probeHTTP(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// withFrontend wraps the API handler so that non-API requests serve the
// embedded SPA. API routes (starting with /orchicon.api.v1) and
// health/version endpoints are passed through to the API handler. All
// other paths serve from the embedded frontend/dist, falling back to
// index.html for client-side routing (docs/10 §9).
func withFrontend(apiHandler http.Handler, log *slog.Logger) http.Handler {
	spaFS, err := fs.Sub(assets.FrontendFS, assets.FrontendDir)
	if err != nil {
		log.Warn("frontend embed unavailable, serving API only", "error", err)
		return apiHandler
	}
	fileServer := http.StripPrefix("/", http.FileServer(http.FS(spaFS)))
	indexHTML, _ := fs.ReadFile(spaFS, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/orchicon.api.v1") ||
			strings.HasPrefix(path, "/auth/") ||
			strings.HasPrefix(path, "/signoz") ||
			path == "/healthz" || path == "/versionz" {
			apiHandler.ServeHTTP(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		f, err := spaFS.Open(cleanPath)
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		if len(indexHTML) > 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}

		apiHandler.ServeHTTP(w, r)
	})
}
