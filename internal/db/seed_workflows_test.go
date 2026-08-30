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
