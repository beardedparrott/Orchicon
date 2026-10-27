package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestSeedRetiresRetiredWorkflows verifies the workflow seeder's cleanup
// pass: templates that were canned in an earlier build but removed from
// cannedWorkflows are hard-deleted on boot — but ONLY while they are still
// seed-managed (their current version is the original seed version). A
// workflow the user has forked (new versions) is left untouched.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed seed tests.
func TestSeedRetiresRetiredWorkflows(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	// Seed the current canned templates so the retire pass has a baseline.
	if err := db.SeedDevWorkflows(ctx, pool); err != nil {
		t.Fatalf("seed dev workflows: %v", err)
	}

	// Simulate a legacy install: two retired templates still at their
	// original seed version (pristine — expect deletion) and one that the
	// user forked by publishing a new version (expect preservation).
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	// Clear any residue from a previous run against the same disposable DB.
	retiredIDs := []string{"01KZB0CONFLICT000000000001", "wf_devops_per_branch", "wf_devops_per_branch_nogit"}
	for _, id := range retiredIDs {
		if _, err := ttx.Exec(ctx,
			`DELETE FROM workflow_versions WHERE tenant_id = 'tnt_dev' AND workflow_id = $1`, id); err != nil {
			t.Fatalf("cleanup versions for %s: %v", id, err)
		}
		if _, err := ttx.Exec(ctx,
			`DELETE FROM workflows WHERE tenant_id = 'tnt_dev' AND id = $1`, id); err != nil {
			t.Fatalf("cleanup workflow %s: %v", id, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ttx, err = pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	insertLegacy := func(workflowID, versionID string, currentVer int) {
		t.Helper()
		if _, err := ttx.Exec(ctx,
			`INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, type)
			 VALUES ($1, 'tnt_dev', '', 'Legacy', $2, 'published', 'template')`, workflowID, currentVer); err != nil {
			t.Fatalf("insert legacy workflow %s: %v", workflowID, err)
		}
		if _, err := ttx.Exec(ctx,
			`INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, version_note, status, steps, inputs, outputs)
			 VALUES ($1, 'tnt_dev', $2, $3, '', 'published', '[]'::jsonb, '{}'::jsonb, '{}'::jsonb)`,
			versionID, workflowID, currentVer); err != nil {
			t.Fatalf("insert legacy version %s: %v", versionID, err)
		}
	}

	insertLegacy("01KZB0CONFLICT000000000001", "wfv_coding_template_ai_approval_architect_conflict_v1", 1)
	insertLegacy("wf_devops_per_branch_nogit", "wfv_devops_per_branch_nogit_v1", 1)
	insertLegacy("wf_devops_per_branch", "user-forked-v1", 2) // forked: current version is NOT the seed version

	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Re-run the seeder: it must retire the pristine templates and leave
	// the user-forked one alone.
	if err := db.SeedDevWorkflows(ctx, pool); err != nil {
		t.Fatalf("re-seed dev workflows: %v", err)
	}

	assertWorkflowGone := func(workflowID string) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev'`, workflowID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", workflowID, err)
		}
		if n != 0 {
			t.Errorf("retired workflow %s still present after re-seed", workflowID)
		}
	}
	assertWorkflowGone("01KZB0CONFLICT000000000001")
	assertWorkflowGone("wf_devops_per_branch_nogit")

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM workflows WHERE id = 'wf_devops_per_branch' AND tenant_id = 'tnt_dev'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("user-forked workflow wf_devops_per_branch was deleted, want preserved (n=%d)", n)
	}
}

// TestSeedAutomationResearchWorkflow: the Automation Research template is
// seeded with the live research worker refs wired to the canned trio.
func TestSeedAutomationResearchWorkflow(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	if err := db.SeedDevWorkflows(ctx, pool); err != nil {
		t.Fatalf("seed dev workflows: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	var steps string
	if err := ttx.QueryRow(ctx,
		`SELECT steps FROM workflow_versions WHERE workflow_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		"01M17EZC170ZR7SZAJET4Z5RY1").Scan(&steps); err != nil {
		t.Fatalf("query workflow steps: %v", err)
	}
	for _, ref := range []string{
		"01M13DYHKHEF71MVGY07GMGMJ6",
		"01M13DYJWHCYHWQ1X85J1BWWZ1",
		"01M13DYM3A7CTY8ECP4R7M33SR",
	} {
		if !strings.Contains(steps, ref) {
			t.Errorf("steps missing worker ref %s", ref)
		}
	}
	if !strings.Contains(steps, `"kind": "end"`) {
		t.Errorf("steps missing End step")
	}
}

// ---------------------------------------------------------------------------
// QUICK WORK INHERITS ITS GIT STRATEGY — it does not pin PR.
//
// REPORTED: "change the seed for the Quick Work workflow to say 'inherit' as opposed to PR
// mode so people aren't stuck with that mode and it's driven from their project."
//
// The pin was load-bearing and wrong: a workflow-level strategy BEATS the project
// (db.effectiveGitStrategyValue — workflow wins over project), so every install carried a
// template that forced PR mode no matter what the operator configured.
//
// The half that is easy to miss: seeding "" only fixes FRESH installs. The seeder's
// propagation only ever wrote a value when the seed PINNED one, so an existing host kept
// "pr" forever — which is exactly the machine that reported this. UnpinGitStrategy exists
// for that, and these tests assert both halves.
// ---------------------------------------------------------------------------

// cannedWorkflowByName returns a canned workflow definition by name.
func cannedWorkflowByName(t *testing.T, name string) db.CannedWorkflowView {
	t.Helper()
	for _, w := range db.CannedWorkflowViews() {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no canned workflow named %q", name)
	return db.CannedWorkflowView{}
}

// TestQuickWorkSeedDoesNotPinAGitStrategy: the seed must INHERIT.
func TestQuickWorkSeedDoesNotPinAGitStrategy(t *testing.T) {
	w := cannedWorkflowByName(t, "Quick Work")
	if w.GitStrategy != "" {
		t.Errorf("Quick Work pins git_strategy=%q — an empty value means INHERIT, so the bound "+
			"project decides. A pin here overrides the operator's own project setting, which is "+
			"what left them stuck in PR mode.", w.GitStrategy)
	}
}

// TestQuickWorkSeedUnpinsAnExistingPin: seeding "" is not enough — a host that already has
// "pr" stored must be cleared back to NULL, or the fix reaches fresh installs only.
func TestQuickWorkSeedUnpinsAnExistingPin(t *testing.T) {
	w := cannedWorkflowByName(t, "Quick Work")
	if !w.UnpinGitStrategy {
		t.Fatal("Quick Work does not set UnpinGitStrategy, so an EXISTING install that was " +
			"seeded with \"pr\" keeps it forever — the seeder only writes when the seed pins a " +
			"value, and this machine is the one that reported the problem")
	}
}

// TestNoCannedWorkflowPinsAGitStrategy is the rule rather than the instance: a canned
// template is shared by every project, so pinning a strategy on one overrides whatever the
// operator configured. If a future workflow genuinely needs a pin, this test is where that
// decision gets argued.
func TestNoCannedWorkflowPinsAGitStrategy(t *testing.T) {
	for _, w := range db.CannedWorkflowViews() {
		if w.GitStrategy != "" {
			t.Errorf("canned workflow %q pins git_strategy=%q — a shared template must inherit "+
				"the bound project's strategy, or every install is forced into one operator's "+
				"per-workflow choice", w.Name, w.GitStrategy)
		}
	}
}

// TestUnpinClearsAStoredStrategyDB proves the propagation against a REAL row: seed a workflow
// pinned to "pr", run the seeder, and assert the pin is gone and the project now decides.
func TestUnpinClearsAStoredStrategyDB(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev" // SeedDevWorkflows is tenant-fixed

	if err := db.SeedDevWorkflows(ctx, pool); err != nil {
		t.Fatalf("seed workflows: %v", err)
	}

	// A row that some earlier release pinned to "pr" — the state of a real, existing install.
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	const qwID = "01M1ERHNCNF38MP3SEV1GTH26G"
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE workflows SET git_strategy = 'pr' WHERE id = $1 AND tenant_id = $2`,
		qwID, tenant); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Re-seed — this is what a boot does.
	if err := db.SeedDevWorkflows(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	var stored *string
	if err := ttx2.Tx.QueryRow(ctx,
		`SELECT git_strategy FROM workflows WHERE id = $1 AND tenant_id = $2`,
		qwID, tenant).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != nil {
		t.Fatalf("after re-seeding, Quick Work still stores git_strategy=%q — an existing install "+
			"stays pinned to PR mode and the operator is stuck with exactly what was reported", *stored)
	}
}
