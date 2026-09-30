package scheduler

// skill_files_test.go — the worker half of the shared skills render path (AC2/AC3/AC4).
//
// The acceptance criterion is that a skill file selected on a project appears in the WORKER composite prompt
// AND the Ask system prompt, rendered by ONE shared platform-side code path
// (internal/contextfiles.RenderManifest) with NO adapter carrying skill-handling code. This file asserts the
// worker half: the union of the project's skill_files and the VERSION's skill_files renders the `# Skills`
// manifest, and two calls over the same selection are byte-identical (the section sits inside the prompt's
// cached static prefix — ADR-0009 — so an unstable order is a permanent per-turn cache miss).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'SkillFiles' -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// skillFileTestSetup creates a project with a project_dir plus a work item in it, returns the reconciler and
// the tx, and leaves cleanup to the caller's deferred rollback.
func skillFileTestSetup(t *testing.T, slug string) (*WorkflowReconciler, context.Context, *db.Pool, *db.TenantTx, db.ProjectRow, db.WorkItemRow, string) {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "Skills " + slug,
		Slug: slug + "-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: "epic", Title: "skills test", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &WorkflowReconciler{pool: pool}, ctx, pool, ttx, proj, item, root
}

// AC2/AC3 (worker half): a project's skill_files AND a worker version's skill_files BOTH appear in the
// composite prompt, union-ed.
func TestSkillFilesWorkerUnionRendersInCompositePrompt(t *testing.T) {
	r, ctx, _, ttx, proj, item, root := skillFileTestSetup(t, "skills-union")
	defer ttx.Rollback(ctx)

	// A project skill (small file ⇒ inlined) and a version skill (a directory ⇒ read-on-demand manifest).
	projSkill := filepath.Join(root, "project-skill.md")
	if err := os.WriteFile(projSkill, []byte("PROJECT_SKILL_BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verDir := filepath.Join(root, "version-skill-dir")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "vskill.md"), []byte("V\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	projSkills := []byte(`["` + projSkill + `"]`)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
		SkillFiles: &projSkills,
	}); err != nil {
		t.Fatal(err)
	}

	worker := db.WorkerVersionRow{
		WorkerID:   "w1",
		Role:       "Engineer",
		SkillFiles: []byte(`["` + verDir + `"]`),
	}
	out, _, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Skills") {
		t.Fatalf("the composite prompt carries no `# Skills` section:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT_SKILL_BODY") {
		t.Fatalf("the project's skill file did not render into the composite prompt:\n%s", out)
	}
	if !strings.Contains(out, verDir) || !strings.Contains(out, "vskill.md") {
		t.Fatalf("the worker version's skill directory did not render into the composite prompt:\n%s", out)
	}
	// The Skills section must be the MANIFEST form (read on demand), not an inline dump of every file.
	if !strings.Contains(out, "(directory — read on demand)") {
		t.Fatalf("the directory skill did not render as a read-on-demand manifest:\n%s", out)
	}
}

// AC4 (worker half): two builds of the SAME selection are byte-identical.
func TestSkillFilesCompositePromptByteStable(t *testing.T) {
	r, ctx, _, ttx, proj, item, root := skillFileTestSetup(t, "skills-stable")
	defer ttx.Rollback(ctx)

	dir := filepath.Join(root, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"zeta.md", "alpha.md", "mid.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projSkills := []byte(`["` + dir + `"]`)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
		SkillFiles: &projSkills,
	}); err != nil {
		t.Fatal(err)
	}

	worker := db.WorkerVersionRow{
		WorkerID:   "w1",
		Role:       "Engineer",
		SkillFiles: []byte(`["` + filepath.Join(dir, "alpha.md") + `"]`),
	}
	// Distinct reconcilers ⇒ the process prompt cache cannot be what makes them equal.
	a, _, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := (&WorkflowReconciler{pool: r.pool}).buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("two composite prompts over one selection differ:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// AC5-adjacent (worker half): a skill OUTSIDE the project dir is dropped at the render boundary rather than
// rendering a dead "could not read" note — a worker version is project-agnostic, so containment can only be
// enforced here.
func TestSkillFilesOutsideProjectDirNotRendered(t *testing.T) {
	r, ctx, _, ttx, _, item, _ := skillFileTestSetup(t, "skills-outside")
	defer ttx.Rollback(ctx)

	outside := filepath.Join(os.TempDir(), "orchicon-outsider-skill-"+strings.ToLower(db.NewID())+".md")
	if err := os.WriteFile(outside, []byte("OUTSIDE_BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	// Structural-valid (absolute, no "..") so it passes the save-time validator; the containment rule is the
	// render boundary's job for a version.
	worker := db.WorkerVersionRow{
		WorkerID:   "w1",
		Role:       "Engineer",
		SkillFiles: []byte(`["` + outside + `"]`),
	}
	out, _, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "OUTSIDE_BODY") {
		t.Fatalf("a skill outside the project dir was rendered:\n%s", out)
	}
}
