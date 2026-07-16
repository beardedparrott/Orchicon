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
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS != "windows" {
		// Separate process group so Ctrl-C in the parent does not reach
		// the child. The child only responds to SIGTERM (via dev stop).
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

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
	tailCmd := exec.Command("tail", "-f", devLogFile)
	tailCmd.Stdin = os.Stdin
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	_ = tailCmd.Run() // exits on Ctrl-C

	fmt.Println()
	fmt.Println("◆ Log tail ended. Server continues running in background.")
	fmt.Println("  Run 'orchicon dev stop' to stop the server.")
	fmt.Println("  Run 'orchicon dev logs' to tail logs again.")
	return 0
}

// --- Stop -------------------------------------------------------------------

// devStop signals the background server to shut down gracefully, then tears
// down the Docker Compose stack.
func devStop(log *slog.Logger) int {
	fmt.Println("▸ orchicon dev stop")

	// 1. Signal child to stop gracefully.
	if pid, running := procRunning(devPIDFile); running {
		fmt.Printf("▸ Signaling control plane (PID %s)…\n", pid)
		p, err := strconv.Atoi(strings.TrimSpace(pid))
		if err == nil {
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
	cmd := exec.Command("tail", "-f", devLogFile)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
	fmt.Println()
	return 0
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

// composeDown runs `docker compose down` with the embedded compose file.
func composeDown(ctx context.Context) error {
	if err := runComposeFromTemp(ctx, nil, "down"); err != nil {
		return fmt.Errorf("docker compose down: %w", err)
	}
	return nil
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
