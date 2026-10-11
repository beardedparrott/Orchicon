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

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/backup"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcp"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
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
		case "backfill-pr":
			os.Exit(runBackfillPR(os.Args[2:], log))
		case "claude-hook":
			os.Exit(runClaudeHook(os.Stdin, os.Stdout))
		case "run-context":
			os.Exit(runRunContext(os.Args[2:], log))
		case "runtime-daemon":
			os.Exit(exitOnErr(runRuntimeDaemon(os.Args[2:], log)))
		case "runtime-supervisor":
			os.Exit(exitOnErr(runRuntimeSupervisor(os.Args[2:], log)))
		case "runtime-client":
			os.Exit(exitOnErr(runRuntimeClient(os.Args[2:], log)))
		case "install":
			os.Exit(exitOnErr(runInstall(os.Args[2:], log)))
		case "restart":
			os.Exit(exitOnErr(runRestart(os.Args[2:], log)))
		case "version", "--version", "-v":
			fmt.Println(version.Current().String())
			return
		case "notices":
			os.Exit(runNotices(os.Args[2:]))
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

	srv, err := server.New(cfg, log, nil)
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

// runNotices prints the embedded third-party license + notice text shipped
// in the binary (Apache-2.0 attribution for vendored libraries).
func runNotices(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: orchicon notices")
		return 0
	}
	entries, err := assets.ThirdPartyFS.ReadDir("third_party/oidc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "notices: %v\n", err)
		return 1
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := assets.ThirdPartyFS.ReadFile("third_party/oidc/" + e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "notices: read %s: %v\n", e.Name(), err)
			return 1
		}
		fmt.Printf("=== %s ===\n\n%s\n\n", e.Name(), data)
	}
	return 0
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

	// THE ROWS ARE DATA, AND THE COMMAND COLUMN IS MEASURED FROM THEM.
	//
	// Both halves of that matter, and the first version of this function had neither. The list was
	// a Printf with one positional argument per line (fifteen of them), maintained by hand — which
	// is how `install` came to be absent from the help entirely. And the gap after each command was
	// literal spaces, so the columns only lined up for a binary whose name happened to fit: with a
	// longer name (a test binary, or `/usr/local/bin/orchicon` → still `orchicon`) the longest rows
	// lost their gap and the line read as one run-on sentence.
	//
	// So: rows first, then the width is the longest command plus a gap. The rendered help is
	// correct for ANY binary name.
	type row struct {
		section string // non-empty starts a new section
		cmd     string
		desc    []string
	}

	rows := []row{
		{section: "Setting up and driving an instance:"},
		{cmd: bin + " install", desc: []string{
			"Set up this machine: pull the images, create the instance, start the plane.",
			"IDEMPOTENT — re-running converges an existing instance and upgrades a superseded",
			"plane. It does NOT restart a plane that is already healthy on the installed binary,",
			"which is why it is not the way to pick up an environment change."}},
		{cmd: bin + " restart", desc: []string{
			"Restart the instance so its components re-read their environment.",
			"Use this after installing an adapter CLI or changing PATH: a plane inherits its",
			"environment when it STARTS, so `install` alone will not refresh it."}},
		{cmd: bin + " version", desc: []string{"Print version info"}},
		{cmd: bin + " notices", desc: []string{"Print third-party license + notice text"}},

		{section: "Running the control plane directly:"},
		{cmd: bin, desc: []string{"Run the control plane (API + relay + reconcilers)"}},
		{cmd: bin + " serve", desc: []string{"Run the control plane with embedded frontend (headless)"}},
		{cmd: bin + " serve --detach", desc: []string{"Fork the server into the background (PID file + /healthz)"}},
		{cmd: bin + " serve --status", desc: []string{"Report whether a detached server is running"}},
		{cmd: bin + " serve --stop", desc: []string{"Stop a detached server"}},
		{cmd: bin + " container", desc: []string{"Run the whole stack as PID 1 inside the single-container image"}},

		{section: "Operating an instance:"},
		{cmd: bin + " db", desc: []string{"Database snapshots: backup | list | restore | prune"}},
		{cmd: bin + " backfill-pr", desc: []string{"Backfill real PR URLs for completed git-backed runs"}},
		{cmd: bin + " run-context", desc: []string{"Print a run's .orchicon archive (steps/summary) for context-by-reference"}},

		{section: "Integration helpers (invoked by the runtime, not usually by hand):"},
		{cmd: bin + " mcp", desc: []string{"Run the MCP stdio server (for opencode tool integration)"}},
		{cmd: bin + " claude-hook", desc: []string{"Run the PreToolUse permission hook for a claude worker session"}},

		// INTERNAL COMMANDS ARE LISTED TOO. They are dispatched and they are real, so an operator can
		// reach them; hiding them would recreate exactly the gap this help had (a command that exists
		// and runs, absent from the list). The heading is what says "not for hand use".
		{section: "Internal (started by the installer and the daemon; not for hand use):"},
		{cmd: bin + " runtime-daemon", desc: []string{"Run the runtime daemon (owns the Docker socket)"}},
		{cmd: bin + " runtime-supervisor", desc: []string{"Run a workflow container's supervisor"}},
		{cmd: bin + " runtime-client", desc: []string{"Talk to the runtime daemon over its socket"}},
	}

	width := 0
	for _, r := range rows {
		if len(r.cmd) > width {
			width = len(r.cmd)
		}
	}
	col := width + 2

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — Orchicon control plane\n", bin, version.Current().Tag)
	for _, r := range rows {
		if r.section != "" {
			fmt.Fprintf(&b, "\n%s\n", r.section)
			continue
		}
		fmt.Fprintf(&b, "  %-*s%s\n", col, r.cmd, r.desc[0])
		for _, extra := range r.desc[1:] {
			fmt.Fprintf(&b, "  %-*s%s\n", col, "", extra)
		}
	}

	fmt.Fprintf(&b, "\nAlmost every command reads ORCHICON_INSTANCE to choose the instance (default: dev).\n")
	fmt.Fprintf(&b, "\nThe binary embeds the single-container runtime configs, migrations, and the frontend\n"+
		"bundle. Run the full stack with `docker run` (see ARCHITECTURE.md §Single-Container\n"+
		"Deployment) or `%s container` as the container's PID-1 supervisor.\n", bin)

	fmt.Print(b.String())
}

func runMCP(ctx context.Context, args []string, log *slog.Logger) int {
	// Worktree-only mode: a worker's composite file tools (batch_read /
	// batch_grep / batch_write) run against a base directory and need no
	// Postgres or valid plane config. The control plane sets
	// ORCHICON_MCP_WORKTREE_DIR when it wires this sidecar into a worker's
	// opencode config, so the sidecar can run inside a DB-less runtime.
	if wd := os.Getenv("ORCHICON_MCP_WORKTREE_DIR"); wd != "" {
		pr := os.Getenv("ORCHICON_MCP_PROJECT_DIR")
		mcpSrv := mcp.New(log, nil, mcp.NewWorktreeRegistry(wd, pr))
		log.Info("mcp server started (worktree tools, stdio transport)", "worktree", wd, "project", pr)
		if err := mcpSrv.Run(ctx); err != nil {
			log.Error("mcp server", "error", err)
			return 1
		}
		return 0
	}

	// Plane-channel mode: the `orchicon-plane` MCP sidecar (role-bound
	// worker executions) talks to the REAL instance's Connect API with a
	// short-lived, role-scoped credential — no Postgres, no sandbox. The
	// control plane sets ORCHICON_PLANE_URL + ORCHICON_PLANE_TOKEN when it
	// mints the credential for the run.
	if url := os.Getenv("ORCHICON_PLANE_URL"); url != "" && os.Getenv("ORCHICON_PLANE_TOKEN") != "" {
		mcpSrv := mcp.New(log, nil, mcp.NewPlaneRegistry(url, os.Getenv("ORCHICON_PLANE_TOKEN"), log))
		log.Info("mcp server started (plane channel, stdio transport)", "url", url)
		if err := mcpSrv.Run(ctx); err != nil {
			log.Error("mcp server", "error", err)
			return 1
		}
		return 0
	}

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	// Secrets KEK for the MCP surface's secret tools: explicit override
	// wins, else the per-instance data-dir key (load-or-create on first
	// boot) — same resolution as the control plane.
	secretsKEK, kekErr := secretcrypto.ResolveKEK(cfg.SecretsKEK, cfg.DataDir)
	if kekErr != nil {
		log.Warn("secrets KEK unavailable (secret tools disabled)", "error", kekErr)
	}

	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("db open", "error", err)
		return 1
	}
	defer pool.Close()

	toolReg := askorchicon.NewToolRegistry(pool, log, secretsKEK)
	mcpSrv := mcp.New(log, pool, mcp.NewAskOrchiconRegistry(toolReg))

	log.Info("mcp server started (stdio transport)")
	if err := mcpSrv.Run(ctx); err != nil {
		log.Error("mcp server", "error", err)
		return 1
	}
	return 0
}
