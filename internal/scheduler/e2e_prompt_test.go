package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// E2E: a project whose context_files contains a DIRECTORY renders the
// directory as a read-on-demand manifest in the workflow composite prompt
// (AC: workers can view/scan directories in executions).
func TestE2EDirectoryInProjectContextPrompt(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	root := t.TempDir()
	dir := filepath.Join(root, "srcdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello\n"), 0o644)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "E2E Dir",
		Slug: "e2e-dir-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Set context_files the way the API does (UpdateProject).
	cf := []byte(`["` + dir + `"]`)
	proj, err = db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
		ContextFiles: &cf,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: "epic", Title: "E2E dir context", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := &WorkflowReconciler{pool: pool}
	out, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, db.WorkerVersionRow{Role: "Engineer"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are an autonomous worker running inside the Orchicon orchestration platform.",
		"srcdir (directory — read on demand)",
		"read on demand",
		"a.go",
		"b.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("workflow prompt missing %q; got:\n%s", want, out)
		}
	}
}

// E2E: a work item whose OWN context_files include a directory AND a file
// renders them as a "# Work item context" section in the composite prompt
// (AC: work item context "just like projects").
func TestE2EWorkItemContextFilesInPrompt(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	root := t.TempDir()
	dir := filepath.Join(root, "widir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "w.go"), []byte("package w\n"), 0o644)
	file := filepath.Join(root, "wi-note.md")
	os.WriteFile(file, []byte("## wi note\n"), 0o644)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "E2E WI",
		Slug: "e2e-wi-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	wiCF := []byte(`["` + dir + `", "` + file + `"]`)
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: "epic", Title: "E2E WI context", Status: "pending",
		ContextFiles: wiCF,
	})
	if err != nil {
		t.Fatal(err)
	}

	r := &WorkflowReconciler{pool: pool}
	out, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, db.WorkerVersionRow{Role: "Engineer"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Work item context",
		"widir (directory — read on demand)",
		"read on demand",
		"w.go",
		"## wi note",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("work item context missing %q; got:\n%s", want, out)
		}
	}
}
