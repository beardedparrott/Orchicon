// Command orchicon dev manages the full local development stack from within
// the binary itself (AGENTS.md §Dev Control Script). It replaces the
// repo-local scripts/dev.sh for installed binaries: the Docker Compose
// stack, migrations, and frontend bundle are embedded via go:embed so the
// user needs only Docker + the orchicon binary.
//
// Usage:
//
//	orchicon dev start    compose up → wait healthy → migrate → serve (control plane + embedded frontend)
//	orchicon dev stop     compose down
//	orchicon dev status   show what's running
//	orchicon dev restart  stop then start
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
  orchicon dev start     Start the full stack (compose → migrate → serve)
  orchicon dev stop      Stop the stack (compose down)
  orchicon dev status    Show what's running
  orchicon dev restart   Stop then start

The binary embeds the Docker Compose stack, migrations, and the frontend
bundle, so no Go, Node, or source checkout is required — only Docker.
`)
}

// devStart brings up the compose stack, applies migrations, and serves
// the control plane + embedded frontend in the foreground. It blocks
// until SIGINT/SIGTERM.
func devStart(log *slog.Logger) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("orchicon dev start", "version", version.Current().String())

	// 1. Write embedded compose file and bring up the stack.
	if err := composeUp(ctx, log); err != nil {
		log.Error("compose up failed", "error", err)
		return 1
	}

	// 2. Wait for Postgres to be healthy.
	log.Info("waiting for postgres…")
	if err := waitForHTTP("http://localhost:8080/healthz", 0); err != nil {
		// Postgres doesn't have an HTTP healthz; use docker healthcheck.
		if err := waitForContainer("postgres", 60); err != nil {
			log.Error("postgres did not become healthy", "error", err)
			return 1
		}
	}
	if err := waitForContainer("nats", 30); err != nil {
		log.Warn("nats did not become healthy in time", "error", err)
	}
	log.Info("dev stack is healthy (Postgres, NATS)")

	// 3. Apply migrations from the embedded SQL files.
	cfg := config.Default()
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("failed to open db", "error", err)
		return 1
	}
	defer pool.Close()

	if err := db.SeedDevTenant(ctx, pool); err != nil {
		log.Warn("seed dev tenant failed (continuing)", "error", err)
	}
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		log.Error("migrations failed", "error", err)
		return 1
	}
	log.Info("migrations applied")

	// 4. Serve the control plane + embedded frontend.
	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		return 1
	}

	// Wrap the server's handler to also serve the embedded frontend.
	handler := withFrontend(srv.Handler(), log)
	srv.SetHandler(handler)

	log.Info("orchicon dev is serving",
		"http", cfg.HTTPAddr,
		"frontend", "embedded",
		"signoz", "http://localhost:3301",
		"nats_monitor", "http://localhost:8222")

	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "error", err)
		return 1
	}

	// 5. Tear down compose on clean shutdown.
	// Use a fresh context here — srv.Run returns when the signal ctx is
	// cancelled (SIGINT/SIGTERM), so ctx is already Done.  composeDown
	// calls exec.CommandContext which would immediately fail on a done
	// context, leaving containers running.
	log.Info("stopping dev stack…")
	downCtx, downCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer downCancel()
	if err := composeDown(downCtx, log); err != nil {
		log.Warn("compose down failed", "error", err)
	}
	log.Info("orchicon dev stopped")
	return 0
}

// devStop tears down the compose stack.
func devStop(log *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log.Info("stopping dev stack…")
	if err := composeDown(ctx, log); err != nil {
		log.Error("compose down failed", "error", err)
		return 1
	}
	log.Info("dev stack stopped")
	return 0
}

// devStatus checks what's running and probes key endpoints.
func devStatus(log *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println("Orchicon dev status")
	fmt.Println()

	// Docker Compose services.
	fmt.Println("Docker Compose services:")
	if err := runComposeFromTemp(ctx, log, "ps", "--format", "table {{.Name}}\t{{.Service}}\t{{.Status}}"); err != nil {
		log.Warn("compose ps failed", "error", err)
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
			// Not under deploy/compose/ (e.g. the repo root, the deploy
			// directory itself). Skip — we only want the compose tree.
			return nil
		}
		rel := strings.TrimPrefix(path, prefix)
		if rel == "" || d.IsDir() {
			// The "deploy/compose" directory itself, or an empty path.
			// Either way nothing to write.
			return nil
		}
		target := filepath.Join(dir, rel)
		data, err := assets.ComposeFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		// Read-only files like *.sql / *.xml / *.yaml are fine with 0o644.
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
// `orchicon dev start` invocations target the same containers (and
// `orchicon dev stop` cleanly tears them down) regardless of the
// per-invocation temp dir the compose YAML lives in. Without this,
// each start picks a fresh project name and the next start collides
// on the hardcoded container names in docker-compose.yml.
func runComposeFromTemp(ctx context.Context, log *slog.Logger, args ...string) error {
	dir, err := extractComposeDir()
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			log.Warn("remove temp compose dir", "dir", dir, "error", rmErr)
		}
	}()

	fullArgs := append([]string{
		"compose",
		"-p", "orchicon",
		"-f", filepath.Join(dir, "docker-compose.yml"),
	}, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Dir = dir // run from the dir so relative mounts resolve
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// composeUp extracts the embedded compose tree to a temp dir and runs
// `docker compose up -d` from there. The dir is removed on return.
func composeUp(ctx context.Context, log *slog.Logger) error {
	if err := runComposeFromTemp(ctx, log, "up", "-d"); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// composeDown runs `docker compose down` with the embedded compose file.
func composeDown(ctx context.Context, log *slog.Logger) error {
	if err := runComposeFromTemp(ctx, log, "down"); err != nil {
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

// waitForHTTP polls a URL until it returns 200 or maxRetries is exceeded.
// If maxRetries is 0, it returns immediately (used as a no-op sentinel).
func waitForHTTP(url string, maxRetries int) error {
	if maxRetries == 0 {
		return fmt.Errorf("no retries")
	}
	for i := 0; i < maxRetries; i++ {
		if probeHTTP(context.Background(), url) {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s did not respond after %d retries", url, maxRetries)
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
	// http.FileServer uses r.URL.Path verbatim when opening files from the
	// FS, and embed.FS rejects paths with a leading slash. StripPrefix
	// rewrites the path in the request so FileServer sees "assets/…"
	// instead of "/assets/…"; without this every asset path falls
	// through to the SPA index.html and the browser parses HTML as JS
	// (a blank page).
	fileServer := http.StripPrefix("/", http.FileServer(http.FS(spaFS)))
	indexHTML, _ := fs.ReadFile(spaFS, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pass through API + auth + health/version routes. The /auth/*
		// endpoints (dev-login, refresh, oidc, session — docs/07 §6.1)
		// are mounted on the API mux; without this pass-through they
		// would be shadowed by the SPA's index.html fallback and login
		// would silently serve HTML instead of minting a token.
		path := r.URL.Path
		if strings.HasPrefix(path, "/orchicon.api.v1") ||
			strings.HasPrefix(path, "/auth/") ||
			path == "/healthz" || path == "/versionz" {
			apiHandler.ServeHTTP(w, r)
			return
		}

		// Try to serve the file from the embedded FS. The check itself
		// uses the slash-less path (embed.FS rejects leading slashes)
		// so we don't fall through to the index.html branch; the actual
		// fileServer (above) is wrapped in http.StripPrefix so it also
		// sees a slash-less path internally.
		cleanPath := strings.TrimPrefix(path, "/")
		f, err := spaFS.Open(cleanPath)
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// File not found — serve index.html for client-side routing.
		if len(indexHTML) > 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}

		// No frontend embedded — fall back to API handler (dev mode).
		apiHandler.ServeHTTP(w, r)
	})
}
