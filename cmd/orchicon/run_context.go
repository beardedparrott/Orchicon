package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
)

// runContext reads a workflow run's on-disk `.orchicon/<runID>/` archive
// (the per-step files written by writeOrchiconFiles) and prints it — either
// the whole archive or a grep-filtered subset. This is the "read on demand
// from the archive" companion to the delta handoff: a worker whose composite
// prompt includes only its direct upstreams can use this tool to pull the
// full detail of ANY earlier step without it being re-embedded into the
// prompt (the context-by-reference contract).
//
// It is the operator/CI-facing form of the same archive the workers read via
// their `read`/`grep` tools; the tool exists so an agent (or a human) can get
// a bounded, indexed dump of the run history without knowing the layout.
//
// Usage:
//
//	orchicon run-context <runID> [--grep <pattern>] [--summary|--steps] [--limit N]
func runRunContext(args []string, log *slog.Logger) int {
	runID := ""
	grep := ""
	mode := "all" // "all" | "steps"
	limit := 0
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--grep="):
			grep = strings.TrimPrefix(a, "--grep=")
		case a == "--steps":
			mode = "steps"
		case a == "--summary":
			mode = "summary"
		case strings.HasPrefix(a, "--limit="):
			fmt.Sscanf(strings.TrimPrefix(a, "--limit="), "%d", &limit)
		default:
			if runID == "" && !strings.HasPrefix(a, "-") {
				runID = a
			}
		}
	}
	if runID == "" {
		fmt.Fprintln(os.Stderr, "usage: orchicon run-context <runID> [--grep <pattern>] [--steps|--summary] [--limit N]")
		return 1
	}

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("db open", "error", err)
		return 1
	}
	defer pool.Close()

	// Resolve the project dir for the run (to reach its .orchicon archive).
	ttx, err := pool.BeginTenantTx(ctx, cfg.DeploymentTenantID)
	if err != nil {
		log.Error("begin tx", "error", err)
		return 1
	}
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, cfg.DeploymentTenantID, runID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		log.Error("run not found", "run", runID, "error", err)
		return 1
	}
	projectDir := ""
	if run.ProjectID != "" {
		ptx, err := pool.BeginTenantTx(ctx, cfg.DeploymentTenantID)
		if err == nil {
			proj, err2 := db.GetProject(ctx, ptx.Tx, cfg.DeploymentTenantID, run.ProjectID)
			if err2 == nil {
				projectDir = proj.ProjectDir
			}
			_ = ptx.Rollback(ctx)
		}
	}
	if projectDir == "" {
		fmt.Fprintln(os.Stderr, "run's project has no project_dir; cannot resolve the archive")
		return 1
	}

	orchDir := filepath.Join(projectDir, ".orchicon", runID)
	if st, err := os.Stat(orchDir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "no .orchicon archive for run %s at %s\n", runID, orchDir)
		return 0
	}

	// Collect the entries to show.
	type entry struct {
		path string
	}
	var entries []entry
	if mode == "steps" || mode == "all" {
		stepDir := filepath.Join(orchDir, "steps")
		if fs, err := os.ReadDir(stepDir); err == nil {
			names := make([]string, 0, len(fs))
			for _, d := range fs {
				names = append(names, d.Name())
			}
			sort.Strings(names)
			for _, n := range names {
				entries = append(entries, entry{path: filepath.Join(stepDir, n)})
			}
		}
	}
	if len(entries) == 0 || mode == "summary" || mode == "all" {
		for _, name := range []string{"status", "summary", "issues", "facts_learned", "touched_files", "worker"} {
			p := filepath.Join(orchDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				entries = append([]entry{{path: p}}, entries...)
			}
		}
	}

	shown := 0
	for _, e := range entries {
		if limit > 0 && shown >= limit {
			fmt.Printf("… (%d more)\n", len(entries)-shown)
			break
		}
		data, err := os.ReadFile(e.path)
		if err != nil {
			continue
		}
		text := string(data)
		if grep != "" && !strings.Contains(text, grep) {
			continue
		}
		fmt.Printf("==== %s ====\n", filepath.Base(e.path))
		fmt.Println(strings.TrimRight(text, "\n"))
		fmt.Println()
		shown++
	}
	if shown == 0 {
		fmt.Println("(no archive entries matched)")
	}
	return 0
}