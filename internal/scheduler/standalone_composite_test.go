package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBuildStandaloneComposite verifies the standalone (non-workflow)
// dispatch fallback builds a full worker prompt: worker identity, the
// task, and the worker's contract. Project/work-item context paths are
// resolved best-effort — with an unreachable DB the function degrades to
// the sections that don't need DB reads (it must never panic or return
// the bare fallback when worker+task data exists).
func TestBuildStandaloneComposite(t *testing.T) {
	// A closed pool: BeginTenantTx fails immediately, exercising the
	// graceful-degradation path (no project/work-item context available).
	p, err := pgxpool.New(context.Background(), "postgres://nohost:5432/nope?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	pool := &db.Pool{Pool: p}

	out := buildStandaloneComposite(pool, db.ExecutionRow{TenantID: "tnt_test"}, db.WorkItemRow{
		TenantID:           "tnt_test",
		Title:              "Implement feature",
		Description:        "Build the thing.",
		AcceptanceCriteria: "Works end to end.",
	}, db.WorkerVersionRow{
		Role:    "Senior Engineer",
		Skills:  "Go, React",
		Behavior: "Write tests.",
	})

	for _, want := range []string{
		"You are an autonomous worker running inside the Orchicon orchestration platform.",
		"# Worker",
		"Senior Engineer",
		"# Task",
		"Original work item: \"Implement feature\"",
		"Build the thing.",
		"Works end to end.",
		"# Instructions",
		"ORCHICON WORKER SUMMARY:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("standalone composite missing %q; got:\n%s", want, out)
		}
	}
}

// TestBuildStandaloneCompositeWorkItemContextDB verifies the standalone
// path renders the work item's own context_files (files AND directories)
// when a real DB is available — the "just like projects" acceptance
// criterion for standalone dispatch. Skipped without ORCHICON_TEST_DSN.
func TestBuildStandaloneCompositeWorkItemContextDB(t *testing.T) {
	pool := approvalTestPool(t)

	// Project with a directory + a file on disk.
	root := t.TempDir()
	dir := root + "/ctx"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# note\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := buildStandaloneComposite(pool, db.ExecutionRow{TenantID: "tnt_dev"}, db.WorkItemRow{
		TenantID:     "tnt_dev",
		Title:        "Standalone with context",
		ContextFiles: []byte(`["` + dir + `", "` + root + `/note.md"]`),
	}, db.WorkerVersionRow{Role: "Engineer"})

	for _, want := range []string{
		"# Worker",
		"# Task",
		"# Work item context",
		"(directory)",
		`Do NOT attempt to open the directory path itself as a file`,
		"a.go",
		"# note",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("standalone composite missing %q; got:\n%s", want, out)
		}
	}
}
