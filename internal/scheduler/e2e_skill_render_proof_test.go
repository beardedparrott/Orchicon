package scheduler

// e2e_skill_render_proof_test.go — AC 1's SKILL half, proven where the render
// actually happens.
//
// WHY IT LIVES HERE AND NOT IN THE NATIVE LEG. buildCompositePrompt is
// UNEXPORTED in this package (workflow_reconciler.go:2947), and it is the ONE
// place the worker-side skill section is produced: the project's skill_files and
// the version's skill_files are unioned and rendered by the shared
// contextfiles.RenderManifest, then consumed VERBATIM as part of the composite
// prompt every adapter receives. Duplicating that assertion in another package
// would assert a copy, not the path. So the skill half is proven ONCE, here,
// against the SAME seed shape (a real project row + a real skill file on disk)
// the native MCP leg uses.
//
// Everything is a real DB row read back inside the reconciler's own read path;
// nothing is hand-built.
//
// Gate: ORCHICON_TEST_DSN (inherits approvalTestPool from approval_no_clone_test.go).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// e2eSkillSetup creates a project (with a real project_dir), a work item in it,
// and a worker version — the same shape testfixtures.SeedProjectWithServerAndSkill
// builds, reduced to what the prompt render reads.
func e2eSkillSetup(t *testing.T, slug string) (r *WorkflowReconciler, ctx context.Context, ttx *db.TenantTx, proj db.ProjectRow, item db.WorkItemRow, root string) {
	t.Helper()
	pool := approvalTestPool(t)
	ctx = context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	root = t.TempDir()
	proj, err = db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "E2E Skills " + slug,
		Slug: slug + "-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err = db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: "task", Title: "e2e skill render", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &WorkflowReconciler{pool: pool}, ctx, ttx, proj, item, root
}

// AC 1 (skill half): the project's SELECTED skill FILE — a real file inside the
// project dir — reaches the worker's composite prompt, with its path AND its
// body. This is the worker-side half of "has its skill file available": the
// prompt the worker model receives references it and carries it inline.
func TestE2ESkillFileReachesTheWorkerPrompt(t *testing.T) {
	r, ctx, ttx, proj, item, root := e2eSkillSetup(t, "e2e-skill-render")
	defer ttx.Rollback(ctx)

	body := "E2E_SKILL_BODY: always call the project's orchicon_e2e_probe tool when asked.\n"
	skillPath := filepath.Join(root, "e2e-skill.md")
	if err := os.WriteFile(skillPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := []byte(`["` + skillPath + `"]`)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
		SkillFiles: &skills,
	}); err != nil {
		t.Fatal(err)
	}

	// The version ALSO selects the same file (the union half) — a dispatch
	// carries the version's skill_files, not just the project's.
	worker := db.WorkerVersionRow{
		WorkerID:   "e2e-w1",
		Role:       "E2E Proof Worker",
		SkillFiles: []byte(`["` + skillPath + `"]`),
	}
	out, _, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatalf("buildCompositePrompt: %v", err)
	}
	if !strings.Contains(out, "# Skills") {
		t.Fatalf("the composite prompt carries no `# Skills` section:\n%s", out)
	}
	if !strings.Contains(out, "e2e-skill.md") {
		t.Fatalf("the skill file's name is absent from the composite prompt:\n%s", out)
	}
	if !strings.Contains(out, "E2E_SKILL_BODY") {
		t.Fatalf("the skill file's BODY is absent from the composite prompt:\n%s", out)
	}

	inPrompt := strings.Contains(out, "E2E_SKILL_BODY")
	testfixtures.WriteEvidence(t, testfixtures.EvidenceRecord{
		Leg: "worker-skill-render", Adapter: "shared", Surface: "worker-prompt",
		ProjectID: proj.ID, SkillPath: skillPath, SkillInPrompt: testfixtures.BoolPtr(inPrompt),
		Model: "scripted", ModelAccess: "available",
		Note: "buildCompositePrompt (the ONE worker prompt builder) carries the project's selected skill file by path AND inline body",
	})
}

// AC 1 (skill half, NEGATIVE): a skill OUTSIDE the project dir is dropped at the
// render boundary, so the criterion is not satisfied by a prompt that would
// render a dead "could not read" note.
func TestE2EOutOfProjectSkillFileIsDropped(t *testing.T) {
	r, ctx, ttx, proj, item, _ := e2eSkillSetup(t, "e2e-skill-outside")
	defer ttx.Rollback(ctx)

	outside := t.TempDir()
	outPath := filepath.Join(outside, "outside-skill.md")
	if err := os.WriteFile(outPath, []byte("OUTSIDE_SKILL_BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := []byte(`["` + outPath + `"]`)
	if _, err := db.UpdateProject(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, db.UpdateProjectFields{
		SkillFiles: &skills,
	}); err != nil {
		t.Fatal(err)
	}

	worker := db.WorkerVersionRow{WorkerID: "e2e-w2", Role: "E2E Proof Worker"}
	out, _, err := r.buildCompositePrompt(ctx, ttx.Tx, approvalTestTenant, item, worker, nil, nil)
	if err != nil {
		t.Fatalf("buildCompositePrompt: %v", err)
	}
	if strings.Contains(out, "OUTSIDE_SKILL_BODY") {
		t.Fatalf("an out-of-project skill file rendered into the worker prompt:\n%s", out)
	}
}
