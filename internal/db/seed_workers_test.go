package db_test

import (
	"context"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// These tests exercise the canned-worker seeder's version handling against
// a real Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (migrations + dev workers are applied on every run):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run 'TestSeed.*' -v
//
// They guard the draft-preservation contract: a user draft on a canned
// worker must never be force-published by a boot re-seed.

func seedTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed seed tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

func insertDraftVersion(t *testing.T, pool *db.Pool, workerID string, version int) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID:               db.NewID(),
		TenantID:         "tnt_dev",
		WorkerID:         workerID,
		Version:          version,
		Status:           "draft",
		RuntimeRef:       "opencode",
		ModelRef:         "opencode-go/deepseek-v4-flash",
		ContextSources:   []byte("[]"),
		Permissions:      []byte("{}"),
		GatedTools:       []byte("[]"),
		BudgetOverrides:  []byte("{}"),
		Labels:           []byte("{}"),
		ConcurrencyLimit: 1,
	}); err != nil {
		t.Fatalf("insert draft v%d: %v", version, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func workerVersionStatus(t *testing.T, pool *db.Pool, workerID string, version int) string {
	t.Helper()
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
		workerID, version).Scan(&status)
	if err != nil {
		t.Fatalf("query v%d status: %v", version, err)
	}
	return status
}

func workerVersionModel(t *testing.T, pool *db.Pool, workerID string, version int) string {
	t.Helper()
	var model string
	err := pool.QueryRow(context.Background(),
		`SELECT model_ref FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
		workerID, version).Scan(&model)
	if err != nil {
		t.Fatalf("query v%d model_ref: %v", version, err)
	}
	return model
}

func setWorkerVersionModel(t *testing.T, pool *db.Pool, workerID string, version int, model string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx,
		`UPDATE worker_versions SET model_ref = $3 WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
		workerID, version, model); err != nil {
		t.Fatalf("set v%d model_ref: %v", version, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// resetWorker deletes the canned worker (cascade versions) and re-seeds it so
// each test starts from a clean, fresh seed state — independent of residue
// from a previous run against the same disposable DB.
func resetWorker(t *testing.T, pool *db.Pool, workerID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx,
		`DELETE FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev'`, workerID); err != nil {
		t.Fatalf("delete versions: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID); err != nil {
		t.Fatalf("delete worker: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}

// TestSeedLeavesUserDraftUntouched: a user-created draft on a canned worker
// (alongside the seed's published v1) must survive a boot re-seed as a
// draft — never force-published.
func TestSeedLeavesUserDraftUntouched(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_se_senior_software_engineer"
	resetWorker(t, pool, workerID)

	insertDraftVersion(t, pool, workerID, 2)

	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := workerVersionStatus(t, pool, workerID, 1); got != "published" {
		t.Errorf("seed v1 should stay published, got %q", got)
	}
	if got := workerVersionStatus(t, pool, workerID, 2); got != "draft" {
		t.Errorf("user draft v2 must NOT be force-published, got %q", got)
	}
}

// TestSeedPublishesLatestDraftWhenNoPublishedVersion: when a canned worker
// has lost every published version, the seeder promotes the latest draft
// (and only that one) so the worker stays dispatchable, and current_version
// follows.
func TestSeedPublishesLatestDraftWhenNoPublishedVersion(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const workerID = "w_se_principal_architect"
	resetWorker(t, pool, workerID)

	// Remove the seed's published v1 so the worker has no published version.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND status = 'published'`,
		workerID); err != nil {
		t.Fatalf("delete published versions: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	insertDraftVersion(t, pool, workerID, 2)
	insertDraftVersion(t, pool, workerID, 3)

	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := workerVersionStatus(t, pool, workerID, 3); got != "published" {
		t.Errorf("latest draft v3 should be promoted when nothing is published, got %q", got)
	}
	if got := workerVersionStatus(t, pool, workerID, 2); got != "draft" {
		t.Errorf("non-latest draft v2 should stay draft, got %q", got)
	}
	var curVer int
	if err := pool.QueryRow(ctx,
		`SELECT current_version FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID).Scan(&curVer); err != nil {
		t.Fatalf("query current_version: %v", err)
	}
	if curVer != 3 {
		t.Errorf("current_version should follow the promoted draft, got %d want 3", curVer)
	}
}

// replaceCannedWorkerWithUserShell deletes a canned worker and re-creates it
// as a user-created worker with a ULID id but the SAME slug, simulating the
// prod situation where the user built the worker before it was canned.
// If withContent is true the shell's version carries a non-empty role.
func replaceCannedWorkerWithUserShell(t *testing.T, pool *db.Pool, cannedID, slug string, withContent bool) string {
	t.Helper()
	ctx := context.Background()
	userID := "usr_" + cannedID
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	// Clear residue from a previous test that used the same generated ids
	// (the adoption + skip tests share one canned worker) so tests are
	// order-independent.
	if _, err := ttx.Exec(ctx,
		`DELETE FROM worker_versions WHERE worker_id IN ($1, $2) AND tenant_id = 'tnt_dev'`, cannedID, userID); err != nil {
		t.Fatalf("delete versions: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workers WHERE id IN ($1, $2) AND tenant_id = 'tnt_dev'`, cannedID, userID); err != nil {
		t.Fatalf("delete workers: %v", err)
	}
	role := ""
	if withContent {
		role = "A customized worker the user owns."
	}
	if _, err := ttx.Exec(ctx,
		`INSERT INTO workers (id, tenant_id, name, slug, description, purpose, status, current_version, created_by)
		 VALUES ($1, 'tnt_dev', $2, $3, '', '', 'published', 1, 'someone')`, userID, slug, slug); err != nil {
		t.Fatalf("insert user worker: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`INSERT INTO worker_versions (id, tenant_id, worker_id, version, version_note, status,
			runtime_ref, model_ref, role, skills, behavior, agents_md,
			context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
			concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
		 VALUES ($1, 'tnt_dev', $2, 1, 'user', 'published', 'opencode', 'opencode-go/deepseek-v4-flash',
			$3, '', '', '', '[]', '{}', '[]', '{}', '', 1, '', '{}', now(), now())`,
		"vusr_"+cannedID, userID, role); err != nil {
		t.Fatalf("insert user version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return userID
}

// TestSeedAdoptsEmptySlugOwner: a user-created worker that owns the canned
// slug but is an empty shell is adopted by the seeder — its ID is preserved
// (workflow step refs stay valid) and its version gets the canned profile.
func TestSeedAdoptsEmptySlugOwner(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", false)

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// The canned worker must NOT be created (the slug is owned), and the user
	// worker keeps its ID but now carries the canned profile.
	var exists string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, cannedID).Scan(&exists); err == nil {
		t.Errorf("canned worker %s should not be created when the slug is adopted", cannedID)
	}
	var roleLen int
	if err := pool.QueryRow(ctx,
		`SELECT length(role) FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		userID).Scan(&roleLen); err != nil {
		t.Fatalf("query adopted role: %v", err)
	}
	if roleLen == 0 {
		t.Errorf("adopted worker v1 should carry the canned role, got empty")
	}
}

// TestSeedSkipsCustomizedSlugOwner: a user-created worker that owns the canned
// slug AND has real content is left untouched — the seeder neither adopts it
// nor crashes the batch.
func TestSeedSkipsCustomizedSlugOwner(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", true)

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var role string
	if err := pool.QueryRow(ctx,
		`SELECT role FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		userID).Scan(&role); err != nil {
		t.Fatalf("query user role: %v", err)
	}
	if role != "A customized worker the user owns." {
		t.Errorf("customized worker must keep its own role, got %q", role)
	}
}

// TestSeedKeepsSyncingAdoptedWorker: once an empty shell is adopted and filled
// by the seeder, it is seed-managed (carries the safety marker) — subsequent
// boots must KEEP syncing it (e.g. roll the marker forward), not treat it as a
// user worker and skip it.
func TestSeedKeepsSyncingAdoptedWorker(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", false)

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed (adopt): %v", err)
	}
	// Simulate the adopted worker having been created under an OLDER seed: its
	// content carries the marker but with a stale version.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`UPDATE worker_versions
		    SET agents_md = replace(agents_md, 'orchicon.safety=v22', 'orchicon.safety=v0')
		  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`, userID); err != nil {
		t.Fatalf("stale marker: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		userID).Scan(&agents); err != nil {
		t.Fatalf("query adopted agents: %v", err)
	}
	if !strings.Contains(agents, "orchicon.safety=v22") {
		t.Errorf("adopted worker should have been rolled forward to the current marker, got %q", agents[len(agents)-40:])
	}
}

// TestSeedVisionWorkersCarryPlaywright: the Vision canned workers must
// carry the Playwright visual-verification block and the current safety
// marker, and — after the git-neutral change — must NOT carry hardcoded
// branch-workflow guidance in their AGENTS.md (per-run prompt blocks keyed on
// worktree_status provide it instead).
func TestSeedVisionWorkersCarryPlaywright(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	for _, canned := range []struct{ id string }{
		{"w_se_sse_vision"},
		{"w_se_architect_vision"},
		{"w_se_qa_vision"},
	} {
		if err := db.SeedDevWorkers(ctx, pool); err != nil {
			t.Fatalf("seed: %v", err)
		}
		var agents string
		if err := pool.QueryRow(ctx,
			`SELECT agents_md FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			canned.id).Scan(&agents); err != nil {
			t.Fatalf("query %s: %v", canned.id, err)
		}
		for _, want := range []string{
			"Browser automation (Playwright) — VISUAL verification",
			"read the screenshot back with your Read tool",
			"orchicon.safety=v22",
		} {
			if !strings.Contains(agents, want) {
				t.Errorf("%s agents_md missing %q", canned.id, want)
			}
		}
		// Git-neutral: no hardcoded branch-workflow guidance in AGENTS.md.
		for _, forbid := range []string{"Git workflow", "Git awareness", "integration branch where all work lands"} {
			if strings.Contains(agents, forbid) {
				t.Errorf("%s agents_md must be git-neutral (no %q) so non-repo runs aren't told a branch exists", canned.id, forbid)
			}
		}
	}
}

// TestSeedCannedWorkersCarrySandboxPlaneGuard: every canned worker's agents_md
// must carry the sandbox-vs-plane instruction, must NOT carry prod/dev
// instance wording, and must carry the safety marker at the current version.
func TestSeedCannedWorkersCarrySandboxPlaneGuard(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_senior_software_engineer"

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned agents: %v", err)
	}
	if !strings.Contains(agents, "Sandbox vs plane") {
		t.Errorf("canned worker must carry the sandbox-vs-plane instruction")
	}
	for _, forbid := range []string{"DEV-ONLY", "orchicon-cnt-prod", "orchicon-cnt-dev"} {
		if strings.Contains(agents, forbid) {
			t.Errorf("canned worker must not carry prod/dev instance wording (contains %q)", forbid)
		}
	}
	if !strings.Contains(agents, "orchicon.safety=v22") {
		t.Errorf("canned worker must carry the current safety marker (orchicon.safety=v22)")
	}
}

// TestSeedVisionWorkersAreFullStack: the Vision canned workers are copies of
// their non-UI counterparts (senior SSE, principal architect, QA engineer) —
// they must NOT carry the old UI-only specialist identity that gated them out
// of backend work (the "UI Developer"-style limiting framing is retired along
// with the UI workers).
func TestSeedVisionWorkersAreFullStack(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	for _, canned := range []struct{ id string }{
		{"w_se_sse_vision"},
		{"w_se_architect_vision"},
		{"w_se_qa_vision"},
	} {
		if err := db.SeedDevWorkers(ctx, pool); err != nil {
			t.Fatalf("seed: %v", err)
		}
		var role, skills, agents string
		if err := pool.QueryRow(ctx,
			`SELECT role, skills, agents_md FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			canned.id).Scan(&role, &skills, &agents); err != nil {
			t.Fatalf("query %s: %v", canned.id, err)
		}
		blob := role + "\n" + skills + "\n" + agents

		// The old limiting identity is gone.
		for _, gone := range []string{
			"specializes in UI",
			"specialist is UI",
			"whose specialty is UI",
			"specialize in UI/UX",
			"you also happen to be",
			"a developer first",
			"an architect first",
			"a QA engineer first",
		} {
			if strings.Contains(strings.ToLower(blob), strings.ToLower(gone)) {
				t.Errorf("%s seed still carries limiting UI-only identity %q", canned.id, gone)
			}
		}
		// model_ref is wiped — workers fall back to the tenant default_worker_model.
	}
}

// TestSeedDesignApproverCarriesDesignReviewContract: the canned Design
// Approver's seed content must statically review the PLAN only — no
// implementation expectations, no predecessor-type inference. The review
// type is fixed by the worker, not guessed at runtime (the split-approver
// fix: two workflows steps no longer share one context-switching worker).
func TestSeedDesignApproverCarriesDesignReviewContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_design_approver"

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned Design Approver agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v22",
		"review the design/architecture PLAN only",
		"plan is sound and complete; implementation may begin",
		"plan does not meet the bar",
		"acceptance criterion",
	}
	for _, c := range checks {
		if !strings.Contains(agents, c) {
			t.Errorf("Design Approver agents_md missing %q", c)
		}
	}
	// The old context-switching inference must be gone from the design
	// approver: it never identifies a previous worker or reviews an
	// implementation.
	for _, gone := range []string{"Identify the previous worker", "Previous worker was an implementer", "review the outcome of the QA/review loop"} {
		if strings.Contains(agents, gone) {
			t.Errorf("Design Approver agents_md should no longer contain %q", gone)
		}
	}
}

// TestSeedCodeApproverCarriesCodeReviewContract: the canned Code Approver's
// seed content must statically verify DONE-ness of the completed
// implementation after QA/PR — not the design, and not inferred from who
// ran before it.
func TestSeedCodeApproverCarriesCodeReviewContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_code_approver"

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned Code Approver agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v22",
		"review the completed IMPLEMENTATION",
		"do not re-review it",
		"implementation is done and meets the acceptance criteria",
		"implementation is not done",
		"acceptance criterion",
	}
	for _, c := range checks {
		if !strings.Contains(agents, c) {
			t.Errorf("Code Approver agents_md missing %q", c)
		}
	}
	// The old context-switching inference must be gone from the code
	// approver: it never identifies a previous worker or reviews a plan.
	for _, gone := range []string{"Identify the previous worker", "Previous worker was a planner", "There is no implementation to inspect"} {
		if strings.Contains(agents, gone) {
			t.Errorf("Code Approver agents_md should no longer contain %q", gone)
		}
	}
}

// TestSeedDevOpsCarriesMergeConflictResolutionContract: the canned DevOps
// worker's seed content must instruct detecting a merge conflict AND resolving
// it (merge develop in, fix with semantic edits, add/commit/push, re-attempt),
// reporting only success or failure — no conflict signal.
func TestSeedDevOpsCarriesMergeConflictResolutionContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_devops_engineer"

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned DevOps agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v22",
		"Merge conflicts — detect AND resolve",
		"git merge origin/develop",
		"git add",
		"git commit",
		"git push",
		"gh pr merge",
	}
	for _, c := range checks {
		if !strings.Contains(agents, c) {
			t.Errorf("DevOps Engineer agents_md missing %q", c)
		}
	}
	// Must NOT contain the old conflict-routing language.
	for _, forbid := range []string{"do NOT resolve", "conflict — merged by develop", "Integrator", "routes the run to the Integrator"} {
		if strings.Contains(agents, forbid) {
			t.Errorf("DevOps Engineer agents_md must not contain %q (merge conflicts are resolved, not routed)", forbid)
		}
	}
}

// TestSeedFreshCannedWorkersHaveBlankModelRef: freshly seeded canned workers
// must carry an EMPTY model_ref so dispatch inherits the tenant
// default_worker_model — model selection is user-owned after creation.
func TestSeedFreshCannedWorkersHaveBlankModelRef(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_se_design_approver"
	resetWorker(t, pool, workerID)

	if model := workerVersionModel(t, pool, workerID, 1); model != "" {
		t.Errorf("freshly seeded canned worker must have blank model_ref, got %q", model)
	}
	// Re-seed again: the blank model_ref must be stable across boots (no
	// force-align back to a seed default).
	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if model := workerVersionModel(t, pool, workerID, 1); model != "" {
		t.Errorf("canned worker model_ref must stay blank after re-seed, got %q", model)
	}
}

// TestSeedUserModelEditSurvivesReseed: a user's model edit on a canned worker
// version must survive a boot re-seed — the seeder never overrides model_ref.
func TestSeedUserModelEditSurvivesReseed(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_se_senior_software_engineer"
	resetWorker(t, pool, workerID)

	setWorkerVersionModel(t, pool, workerID, 1, "anthropic/claude-sonnet-4")

	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if model := workerVersionModel(t, pool, workerID, 1); model != "anthropic/claude-sonnet-4" {
		t.Errorf("user model edit must survive re-seed, got %q want anthropic/claude-sonnet-4", model)
	}
}

// TestSeedRollForwardPreservesModelRef: when the safety-context roll-forward
// appends a new published version (current published version is NOT v1), the
// new version must carry the current version's model_ref (user-owned), not the
// seed's.
func TestSeedRollForwardPreservesModelRef(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const workerID = "w_se_devops_engineer"
	resetWorker(t, pool, workerID)

	// Build a user-created published v2 (seed's v1 stays as the base), carrying
	// a user-chosen model and a stale safety marker so the seeder rolls forward.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var v1id string
	if err := ttx.QueryRow(ctx,
		`SELECT id FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		workerID).Scan(&v1id); err != nil {
		t.Fatalf("query v1: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`INSERT INTO worker_versions
		    (id, tenant_id, worker_id, version, version_note, status,
		     runtime_ref, model_ref, role, skills, behavior, agents_md,
		     context_sources, permissions, gated_tools, budget_overrides,
		     execution_policy_ref, concurrency_limit, recovery_workflow_ref,
		     labels, published_at, created_at)
		 SELECT $1, 'tnt_dev', worker_id, 2, 'user version', 'published',
		        runtime_ref, 'google/gemini-2.5-pro', role, skills, behavior,
		        replace(agents_md, 'orchicon.safety=v22', 'orchicon.safety=v0'),
		        context_sources, permissions, gated_tools, budget_overrides,
		        execution_policy_ref, concurrency_limit, recovery_workflow_ref,
		        labels, now(), now()
		   FROM worker_versions
		  WHERE id = $2 AND tenant_id = 'tnt_dev'`,
		db.NewID(), v1id); err != nil {
		t.Fatalf("insert published v2: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`UPDATE workers SET current_version = 2 WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID); err != nil {
		t.Fatalf("bump current_version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// v2 carries a stale marker -> the seeder rolls a new published version
	// forward that must preserve v2's model_ref.
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var curVer int
	if err := pool.QueryRow(ctx,
		`SELECT current_version FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID).Scan(&curVer); err != nil {
		t.Fatalf("query current_version: %v", err)
	}
	if curVer != 3 {
		t.Fatalf("roll-forward should create v3, got current_version=%d", curVer)
	}
	if model := workerVersionModel(t, pool, workerID, 3); model != "google/gemini-2.5-pro" {
		t.Errorf("roll-forward version must preserve the current version's model_ref, got %q want google/gemini-2.5-pro", model)
	}
}

// TestSeedAutomationResearchTrioSeededWithRoleAndGenericPurposes: the
// Automation Research trio (Planner/Analyst/Synthesizer) is canned with
// the automation-research role and project-agnostic purposes — the
// per-run product targets live in the bound work item's brief, not in the
// workers. Asserts: role_ref bound, generic wording, no Orchicon-project
// bias phrases, the web-research runtime image, and the markers +
// worktree-hygiene rule on the seeded profile.
func TestSeedAutomationResearchTrioSeededWithRoleAndGenericPurposes(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}

	trio := []struct {
		id     string
		slug   string
		expect string // purpose fragment that must be present
	}{
		{"01M13DYHKHEF71MVGY07GMGMJ6", "automation-research-planner", "capability landscape"},
		{"01M13DYJWHCYHWQ1X85J1BWWZ1", "automation-research-analyst", "project codebase"},
		{"01M13DYM3A7CTY8ECP4R7M33SR", "automation-research-synthesizer", "project codebase"},
	}
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	for _, tc := range trio {
		var purpose, roleRef string
		if err := ttx.QueryRow(ctx,
			`SELECT purpose, role_ref FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`,
			tc.id).Scan(&purpose, &roleRef); err != nil {
			t.Fatalf("query worker %s: %v", tc.slug, err)
		}
		if roleRef != "r_se_automation_research" {
			t.Errorf("%s role_ref = %q, want r_se_automation_research", tc.slug, roleRef)
		}
		if !strings.Contains(purpose, tc.expect) {
			t.Errorf("%s purpose missing %q", tc.slug, tc.expect)
		}
		// Project-agnostic: the Orchicon-project bias phrases must be gone.
		for _, bias := range []string{"the Orchicon codebase", "compare Orchicon against", "competitors to analyze"} {
			if strings.Contains(purpose, bias) {
				t.Errorf("%s purpose still carries project bias %q", tc.slug, bias)
			}
		}
		var agents, runtimeRef string
		if err := ttx.QueryRow(ctx,
			`SELECT agents_md, runtime_ref FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			tc.id).Scan(&agents, &runtimeRef); err != nil {
			t.Fatalf("query version %s: %v", tc.slug, err)
		}
		if runtimeRef != "orchicon-runtime:web-research" {
			t.Errorf("%s runtime_ref = %q, want orchicon-runtime:web-research", tc.slug, runtimeRef)
		}
		if !strings.Contains(agents, "Sandbox vs plane") || !strings.Contains(agents, "orchicon.safety=v22") {
			t.Errorf("%s agents_md missing seed markers", tc.slug)
		}
		if !strings.Contains(agents, "Worktree hygiene") {
			t.Errorf("%s agents_md missing the worktree hygiene rule", tc.slug)
		}
	}
}
