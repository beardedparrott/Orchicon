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
	const workerID = "w_ui_developer"
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
	const workerID = "w_ui_design_architect"
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
		    SET agents_md = replace(agents_md, 'orchicon.safety=v13', 'orchicon.safety=v0')
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
	if !strings.Contains(agents, "orchicon.safety=v13") {
		t.Errorf("adopted worker should have been rolled forward to the current marker, got %q", agents[len(agents)-40:])
	}
}

// TestSeedRecreatesUISlugOwner: a stale ULID worker owning a UI canned slug
// (RecreateSlugOwner) is DELETED and recreated fresh under the canned ID —
// the user explicitly wants leftover UUID canned workers gone. The recreated
// worker also carries its own seed model (opencode-go/mimo-v2.5).
func TestSeedRecreatesUISlugOwner(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_ui_developer"
	const slug = "ui-developer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, slug, true)

	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// The stale slug owner is gone.
	var exists string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, userID).Scan(&exists); err == nil {
		t.Errorf("stale slug owner %s should have been deleted", userID)
	}
	// The canned worker exists fresh with the mimo model and the develop
	// branch context.
	var modelRef, agents string
	if err := pool.QueryRow(ctx,
		`SELECT model_ref FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&modelRef); err != nil {
		t.Fatalf("query canned model_ref: %v", err)
	}
	if modelRef != "opencode-go/mimo-v2.5" {
		t.Errorf("UI worker should default to opencode-go/mimo-v2.5, got %q", modelRef)
	}
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned agents: %v", err)
	}
	if !strings.Contains(agents, "Branch off `develop`") {
		t.Errorf("UI worker agents_md should carry the develop-first git workflow")
	}
}

// TestSeedCannedWorkersCarryDevOnlyGuard: every canned worker's agents_md
// must carry the DEV-ONLY instruction (never touch the PROD instance), the
// safety marker at the current version, and the develop-first git workflow.
func TestSeedCannedWorkersCarryDevOnlyGuard(t *testing.T) {
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
	if !strings.Contains(agents, "DEV-ONLY") {
		t.Errorf("canned worker must carry the DEV-ONLY prod guard")
	}
	if !strings.Contains(agents, "orchicon-cnt-prod") || !strings.Contains(agents, "orchicon-cnt-dev") {
		t.Errorf("DEV-ONLY guard must name both instances so the rule is unambiguous")
	}
	if !strings.Contains(agents, "orchicon.safety=v13") {
		t.Errorf("canned worker must carry the current safety marker (orchicon.safety=v13)")
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
		"orchicon.safety=v13",
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
		"orchicon.safety=v13",
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
