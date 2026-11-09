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

// Passing it means the fixture states which tenant it is testing — and, because the seed writes
// ONLY to what it is given.
const seedTenant = "tnt_dev"

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
	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
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

// resetWorker deletes the canned worker (cascade versions) and re-seeds it so each test starts from
// a clean, fresh seed state.
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
	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}

// TestSeedLeavesUserDraftUntouched: a user-created draft on a canned worker (alongside the seed's
// published v1) must survive a boot re-seed as a draft — never force-published.
func TestSeedLeavesUserDraftUntouched(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_se_senior_software_engineer"
	resetWorker(t, pool, workerID)

	insertDraftVersion(t, pool, workerID, 2)

	if err := db.SeedDevWorkers(context.Background(), pool, seedTenant); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := workerVersionStatus(t, pool, workerID, 1); got != "published" {
		t.Errorf("seed v1 should stay published, got %q", got)
	}
	if got := workerVersionStatus(t, pool, workerID, 2); got != "draft" {
		t.Errorf("user draft v2 must NOT be force-published, got %q", got)
	}
}

// TestSeedPublishesLatestDraftWhenNoPublishedVersion: when a canned worker has lost every published
// version, the seeder promotes the latest draft (and only that one) so the worker stays.
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

	if err := db.SeedDevWorkers(context.Background(), pool, seedTenant); err != nil {
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

// replaceCannedWorkerWithUserShell deletes a canned worker and re-creates it as a user-created
// worker with a ULID id but the SAME slug.
func replaceCannedWorkerWithUserShell(t *testing.T, pool *db.Pool, cannedID, slug string, withContent bool) string {
	t.Helper()
	ctx := context.Background()
	userID := "usr_" + cannedID
	// Restore the canned worker after the test: without this cleanup the usr_-prefixed shell (and the
	// seeder's adoption of it) leaks into every later test that queries the canned ID at version 1.
	t.Cleanup(func() {
		ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
		if err != nil {
			t.Fatalf("cleanup begin tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		if _, err := ttx.Exec(ctx,
			`DELETE FROM worker_versions WHERE worker_id IN ($1, $2) AND tenant_id = 'tnt_dev'`, cannedID, userID); err != nil {
			t.Fatalf("cleanup delete versions: %v", err)
		}
		if _, err := ttx.Exec(ctx,
			`DELETE FROM workers WHERE id IN ($1, $2) AND tenant_id = 'tnt_dev'`, cannedID, userID); err != nil {
			t.Fatalf("cleanup delete workers: %v", err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("cleanup commit: %v", err)
		}
		if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
			t.Fatalf("cleanup re-seed: %v", err)
		}
	})
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	// Clear residue from a previous test that used the same generated ids (the adoption + skip tests
	// share one canned worker) so tests are order-independent.
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
			model_ref, role, skills, behavior, agents_md,
			context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
			concurrency_limit, recovery_workflow_ref, labels, published_at, created_at)
		 VALUES ($1, 'tnt_dev', $2, 1, 'user', 'published', 'opencode-go/deepseek-v4-flash',
			$3, '', '', '', '[]', '{}', '[]', '{}', '', 1, '', '{}', now(), now())`,
		"vusr_"+cannedID, userID, role); err != nil {
		t.Fatalf("insert user version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return userID
}

// TestSeedAdoptsEmptySlugOwner: a user-created worker that owns the canned slug but is an empty
// shell is adopted by the seeder.
func TestSeedAdoptsEmptySlugOwner(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", false)

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
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

// TestSeedSkipsCustomizedSlugOwner: a user-created worker that owns the canned slug AND has real
// content is left untouched — the seeder neither adopts it nor crashes the batch.
func TestSeedSkipsCustomizedSlugOwner(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", true)

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
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

// TestSeedKeepsSyncingAdoptedWorker: once an empty shell is adopted and filled by the seeder, it is
// seed-managed (carries the safety marker).
func TestSeedKeepsSyncingAdoptedWorker(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_qa_engineer"
	userID := replaceCannedWorkerWithUserShell(t, pool, cannedID, "qa-engineer", false)

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
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
		    SET agents_md = replace(agents_md, 'orchicon.safety=v24', 'orchicon.safety=v0')
		  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`, userID); err != nil {
		t.Fatalf("stale marker: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Content tests read version 1 — reset the worker so the seed's own v1 is what's read (this box's
	// dev DB carries stale old-seed v1s under rolled-forward current versions; row 260's hygiene.
	resetWorker(t, pool, cannedID)
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		userID).Scan(&agents); err != nil {
		t.Fatalf("query adopted agents: %v", err)
	}
	if !strings.Contains(agents, "orchicon.safety=v24") {
		t.Errorf("adopted worker should have been rolled forward to the current marker, got %q", agents[len(agents)-40:])
	}
}

// TestSeedVisionWorkersAreRetired: the Vision canned workers (SSE/Architect/ QA) were retired
// 2026-09-02 — visual UI verification moved into the base QA Engineer via the Playwright block.
func TestSeedVisionWorkersAreRetired(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, id := range []string{"w_se_sse_vision", "w_se_architect_vision", "w_se_qa_vision"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev')`, id,
		).Scan(&exists); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if exists {
			t.Errorf("retired Vision worker %s must be deleted by the seeder, still exists", id)
		}
	}
}

// TestSeedBaseQACarriesPlaywright: the base QA Engineer absorbed the Vision workers' role — it must
// carry the Playwright visual-verification block and the current safety marker at its current.
func TestSeedBaseQACarriesPlaywright(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	resetWorker(t, pool, "w_se_qa_engineer")
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions
		  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		"w_se_qa_engineer").Scan(&agents); err != nil {
		t.Fatalf("query QA agents: %v", err)
	}
	for _, want := range []string{
		"Browser automation (Playwright) — VISUAL verification",
		"read the screenshot back with your Read tool",
		"orchicon.safety=v24",
	} {
		if !strings.Contains(agents, want) {
			t.Errorf("base QA agents_md missing %q", want)
		}
	}
}

// TestSeedQACarriesSurfaceImpactCheck: the QA Engineer's UI-verification trigger is the MANDATORY
// surface-impact check.
func TestSeedQACarriesSurfaceImpactCheck(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	resetWorker(t, pool, "w_se_qa_engineer")
	var agents, purpose, skills string
	if err := pool.QueryRow(ctx,
		`SELECT v.agents_md, w.purpose, v.skills
		   FROM workers w
		   JOIN worker_versions v ON v.worker_id = w.id AND v.tenant_id = w.tenant_id
		  WHERE w.id = $1 AND w.tenant_id = 'tnt_dev' AND v.version = 1`,
		"w_se_qa_engineer").Scan(&agents, &purpose, &skills); err != nil {
		t.Fatalf("query QA worker: %v", err)
	}
	for _, want := range []string{
		"Surface-impact check — mandatory, before any verdict",
		"Diff file types are not the test",
		"Verifying only the backend while a criterion references displayed data is an incomplete pass",
		"State your surface-impact determination",
		"Surface-impact analysis",
	} {
		if !strings.Contains(agents, want) && !strings.Contains(purpose, want) && !strings.Contains(skills, want) {
			t.Errorf("base QA missing surface-impact wording %q", want)
		}
	}
	if strings.Contains(agents, "UI changes get visual verification") {
		t.Errorf("base QA still carries the old conditional UI trigger %q", "UI changes get visual verification")
	}
	for _, w := range []string{"w_se_senior_software_engineer", "w_se_pr_reviewer", "w_se_principal_architect"} {
		resetWorker(t, pool, w)
		var agents string
		if err := pool.QueryRow(ctx,
			`SELECT agents_md FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			w).Scan(&agents); err != nil {
			t.Fatalf("query %s agents: %v", w, err)
		}
		if strings.Contains(agents, "Surface-impact check — mandatory, before any verdict") {
			t.Errorf("%s must NOT carry the QA-only surface-impact step", w)
		}
	}
}

// TestSeedCannedWorkersCarrySandboxPlaneGuard: every canned worker's agents_md must carry the
// sandbox-vs-plane instruction, must NOT carry prod/dev instance wording.
func TestSeedCannedWorkersCarrySandboxPlaneGuard(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_senior_software_engineer"

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Content tests read version 1 — reset so the seed's own v1 is what's read (stale old-seed v1s
	// under rolled-forward current versions; row 260's hygiene).
	resetWorker(t, pool, cannedID)
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
	if !strings.Contains(agents, "orchicon.safety=v24") {
		t.Errorf("canned worker must carry the current safety marker (orchicon.safety=v24)")
	}
}

// TestSeedSDLCWorkersAreTimeBoxedWorkhorses: the four SDLC workers (SSE, PR Reviewer, QA,
// Architect) carry the workhorse contract — hard time-box in the prompt, the roll-forward marker.
func TestSeedSDLCWorkersAreTimeBoxedWorkhorses(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]struct {
		box    string
		budget string
	}{
		"w_se_principal_architect":      {"Hard time-box: 20 minutes", "1500"},
		"w_se_senior_software_engineer": {"Hard time-box: 45 minutes", "3600"},
		"w_se_pr_reviewer":              {"Hard time-box: 30 minutes", "2400"},
		"w_se_qa_engineer":              {"Hard time-box: 30 minutes", "2400"},
	}
	for id, tc := range want {
		resetWorker(t, pool, id)
		var agents, budget string
		if err := pool.QueryRow(ctx,
			`SELECT agents_md, budget_overrides->>'wall_clock_seconds' FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			id).Scan(&agents, &budget); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if !strings.Contains(agents, tc.box) {
			t.Errorf("%s agents_md missing %q", id, tc.box)
		}
		if !strings.Contains(agents, "orchicon.safety=v24") {
			t.Errorf("%s agents_md missing the safety marker", id)
		}
		if budget != tc.budget {
			t.Errorf("%s budget_overrides = %s, want %s", id, budget, tc.budget)
		}
	}
	// The QA Engineer rolls on its own marker so QA-only wording changes
	// never re-roll the other three SDLC workers.
	var qaMarker, sseMarker string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions
		  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		"w_se_qa_engineer").Scan(&qaMarker); err != nil {
		t.Fatalf("query QA agents: %v", err)
	}
	if !strings.Contains(qaMarker, db.QASurfaceImpactMarker) {
		t.Errorf("QA agents_md missing its roll-forward marker %q", db.QASurfaceImpactMarker)
	}
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions
		  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		"w_se_senior_software_engineer").Scan(&sseMarker); err != nil {
		t.Fatalf("query SSE agents: %v", err)
	}
	if strings.Contains(sseMarker, db.QASurfaceImpactMarker) {
		t.Errorf("SSE must NOT carry the QA-only roll-forward marker")
	}
}

// TestSeedPreExistingFailureRemedyContract pins the SPLIT between the implementer and the gates, which
// replaced the old shared "remedy it yourself" contract.
//
// WHY IT CHANGED. The remedy contract used to be shared by all three quality workers, and it authorised
// each of them to "remove or correct" a failing test when their own investigation concluded it no longer
// protected anything. That is a defensible instruction for the IMPLEMENTER — it owns the code — and a
// catastrophic one for a REVIEWER: a gate that may edit the suite to reach green is not a gate, and the
// failure is invisible because the edit appears in the same diff as the fix. Combined with the reviewers
// also being told to repair every bug they found, no step in the pipeline could ever return `failure` for
// the class of defect the pipeline exists to catch.
//
// The contract now differs by ROLE, and this test is what keeps them from drifting back together:
//
//   - SSE (implementer): owns the suite. May fix a failing test's CAUSE, and may correct or remove the
//     test itself — but the decision and its rationale must be recorded, so it is visible.
//   - PR Reviewer / QA (gates): NEVER edit the suite. A pre-existing failure is reported as context with
//     the base-commit evidence, and a wrong test is a FINDING for the engineer, not an edit by the gate.
func TestSeedPreExistingFailureRemedyContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	if err := db.SeedDevWorkers(ctx, pool, "tnt_dev"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// --- The IMPLEMENTER keeps ownership of the suite. ---
	t.Run("implementer owns the suite", func(t *testing.T) {
		resetWorker(t, pool, "w_se_senior_software_engineer")
		var agents string
		if err := pool.QueryRow(ctx,
			`SELECT agents_md FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			"w_se_senior_software_engineer").Scan(&agents); err != nil {
			t.Fatalf("query: %v", err)
		}
		if !strings.Contains(agents, db.PreExistingRemedyMarker) {
			t.Errorf("the SSE agents_md must carry the remedy-contract marker %q", db.PreExistingRemedyMarker)
		}
		if !strings.Contains(agents, "FACTS LEARNED") {
			t.Error("the SSE must require recording the remedy decision as a FACTS LEARNED line")
		}
		for _, bad := range []string{"propose", "PROPOSAL", "escalate to a human"} {
			if strings.Contains(agents, bad) {
				t.Errorf("the SSE remedy contract must be autonomous — found %q", bad)
			}
		}
		if !strings.Contains(agents, "You own the decision and execute it") {
			t.Error("the SSE must carry the autonomous ownership wording")
		}
	})

	// --- The GATES must never edit the suite.
	t.Run("gates never edit the suite", func(t *testing.T) {
		for _, id := range []string{"w_se_pr_reviewer", "w_se_qa_engineer"} {
			resetWorker(t, pool, id)
			var agents string
			if err := pool.QueryRow(ctx,
				`SELECT agents_md FROM worker_versions
				  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
				id).Scan(&agents); err != nil {
				t.Fatalf("query %s: %v", id, err)
			}
			if strings.Contains(agents, db.PreExistingRemedyMarker) {
				t.Errorf("%s must NOT carry the remedy marker — a gate that repairs what it finds is not a gate", id)
			}
			for _, bad := range []string{"fix it yourself", "fix ALL of them", "you FIX all code bugs", "remove or correct it yourself"} {
				if strings.Contains(agents, bad) {
					t.Errorf("%s must not be instructed to repair (%q) — it must reject instead", id, bad)
				}
			}
			for _, want := range []string{"do not fix", "Never delete, skip or weaken a test"} {
				if !strings.Contains(agents, want) {
					t.Errorf("%s must state %q so the gate cannot substitute repair for a verdict", id, want)
				}
			}
		}
	})

	// --- The roll-forward fragments must be per-worker, or the new contract never reaches a published
	// worker. A shared marker would also re-roll workers whose contract did not change. ---
	t.Run("gates carry their own roll markers", func(t *testing.T) {
		for _, tc := range []struct{ id, marker string }{
			{"w_se_pr_reviewer", db.ReviewerVerdictMarker},
			{"w_se_qa_engineer", db.QAVerifierMarker},
		} {
			resetWorker(t, pool, tc.id)
			var agents string
			if err := pool.QueryRow(ctx,
				`SELECT agents_md FROM worker_versions
				  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
				tc.id).Scan(&agents); err != nil {
				t.Fatalf("query %s: %v", tc.id, err)
			}
			if !strings.Contains(agents, tc.marker) {
				t.Errorf("%s agents_md missing its own roll-forward marker %q — an already-published worker would never re-roll", tc.id, tc.marker)
			}
		}
		// And the two markers must be DIFFERENT, so a change to one gate never re-rolls the other.
		if db.ReviewerVerdictMarker == db.QAVerifierMarker {
			t.Error("the reviewer and verifier markers must be distinct")
		}
	})

	// The Architect and DevOps are not quality gates for the suite.
	for _, id := range []string{"w_se_principal_architect", "w_se_devops_engineer"} {
		var agents string
		if err := pool.QueryRow(ctx,
			`SELECT agents_md FROM worker_versions
			  WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			id).Scan(&agents); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if strings.Contains(agents, db.PreExistingRemedyMarker) {
			t.Errorf("%s must NOT carry the remedy-contract marker", id)
		}
	}
}

// TestSeedDesignApproverCarriesDesignReviewContract: the canned Design Approver's seed content must
// statically review the PLAN only — no implementation expectations, no predecessor-type inference.
func TestSeedDesignApproverCarriesDesignReviewContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_design_approver"

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned Design Approver agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v24",
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
	// The old context-switching inference must be gone from the design approver: it never identifies a
	// previous worker or reviews an implementation.
	for _, gone := range []string{"Identify the previous worker", "Previous worker was an implementer", "review the outcome of the QA/review loop"} {
		if strings.Contains(agents, gone) {
			t.Errorf("Design Approver agents_md should no longer contain %q", gone)
		}
	}
}

// TestSeedCodeApproverCarriesCodeReviewContract: the canned Code Approver's seed content must
// statically verify DONE-ness of the completed implementation after QA/PR — not the design.
func TestSeedCodeApproverCarriesCodeReviewContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_code_approver"

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Content tests read version 1 — reset so the seed's own v1 is what's read (stale old-seed v1s
	// under rolled-forward current versions; row 260's hygiene).
	resetWorker(t, pool, cannedID)
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned Code Approver agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v24",
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

// TestSeedDevOpsCarriesMergeConflictResolutionContract: the canned DevOps worker's seed content
// must instruct detecting a merge conflict AND resolving it (merge develop.
func TestSeedDevOpsCarriesMergeConflictResolutionContract(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const cannedID = "w_se_devops_engineer"

	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var agents string
	if err := pool.QueryRow(ctx,
		`SELECT agents_md FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
		cannedID).Scan(&agents); err != nil {
		t.Fatalf("query canned DevOps agents: %v", err)
	}
	checks := []string{
		"orchicon.safety=v24",
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

// TestSeedFreshCannedWorkersHaveBlankModelRef: freshly seeded canned workers must carry an EMPTY
// model_ref so dispatch inherits the tenant default_worker_model.
func TestSeedFreshCannedWorkersHaveBlankModelRef(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_se_design_approver"
	resetWorker(t, pool, workerID)

	if model := workerVersionModel(t, pool, workerID, 1); model != "" {
		t.Errorf("freshly seeded canned worker must have blank model_ref, got %q", model)
	}
	// Re-seed again: the blank model_ref must be stable across boots (no
	// force-align back to a seed default).
	if err := db.SeedDevWorkers(context.Background(), pool, seedTenant); err != nil {
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

	if err := db.SeedDevWorkers(context.Background(), pool, seedTenant); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if model := workerVersionModel(t, pool, workerID, 1); model != "anthropic/claude-sonnet-4" {
		t.Errorf("user model edit must survive re-seed, got %q want anthropic/claude-sonnet-4", model)
	}
}

// TestSeedRollForwardPreservesModelRef: when the safety-context roll-forward appends a new
// published version (current published version is NOT v1).
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
		     model_ref, role, skills, behavior, agents_md,
		     context_sources, permissions, gated_tools, budget_overrides,
		     execution_policy_ref, concurrency_limit, recovery_workflow_ref,
		     labels, published_at, created_at)
		 SELECT $1, 'tnt_dev', worker_id, 2, 'user version', 'published',
		        'google/gemini-2.5-pro', role, skills, behavior,
		        replace(agents_md, 'orchicon.safety=v24', 'orchicon.safety=v0'),
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
	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
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

// TestSeedAutomationResearchTrioSeededWithRoleAndGenericPurposes: the Automation Research trio
// (Planner/Analyst/Synthesizer) is canned with the automation-research role and project-agnostic.
func TestSeedAutomationResearchTrioSeededWithRoleAndGenericPurposes(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	if err := db.SeedDevWorkers(ctx, pool, seedTenant); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}

	trio := []struct {
		id     string
		slug   string
		expect string // purpose fragment that must be present
	}{
		{"01M13DYHKHEF71MVGY07GMGMJ6", "automation-research-planner", "market capability"},
		{"01M13DYJWHCYHWQ1X85J1BWWZ1", "automation-research-analyst", "project codebase"},
		{"01M13DYM3A7CTY8ECP4R7M33SR", "automation-research-synthesizer", "project codebase"},
	}
	// Content tests read version 1 — reset the workers so the seed's own v1 is what's read (this box's
	// dev DB carries stale old-seed v1s under rolled-forward current versions; row 260's hygiene.
	for _, tc := range trio {
		resetWorker(t, pool, tc.id)
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
		var agents, modelRef string
		if err := ttx.QueryRow(ctx,
			`SELECT agents_md, model_ref FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = 1`,
			tc.id).Scan(&agents, &modelRef); err != nil {
			t.Fatalf("query version %s: %v", tc.slug, err)
		}
		// Worker-level runtime_ref is retired (ADR-0003): the trio seeds an empty model_ref (dispatch
		// derives the kind at run time), so the version row must carry no model_ref and no runtime_ref
		// column.
		if modelRef != "" {
			t.Errorf("%s model_ref = %q, want empty (runtime_ref retired; dispatch kind derives at run time)", tc.slug, modelRef)
		}
		if !strings.Contains(agents, "Sandbox vs plane") || !strings.Contains(agents, "orchicon.safety=v24") {
			t.Errorf("%s agents_md missing seed markers", tc.slug)
		}
		if !strings.Contains(agents, "Worktree hygiene") {
			t.Errorf("%s agents_md missing the worktree hygiene rule", tc.slug)
		}
	}
}

// ---------------------------------------------------------------------------
// THE MERGE GATE — canned workers must not land a red PR on develop.
//
// THE INCIDENT. PR #850 merged with TWO RED CI JOBS (fe-lint, windows desktop + MSI,
// both failing a whole-project typecheck) and left `develop` unable to build. The
// prompts were not silent about verification — the Quick worker's said "never report
// success with failing build, failing tests" — but two things defeated it:
//
//   1. NO PRECONDITION ON THE MERGE. The ship step ran `gh pr create` then straight to
//      `gh pr merge --squash --admin`, and `--admin` BYPASSES checks. Verification was an
//      ACTIVITY, never a gate on the command that lands the branch.
//   2. THE SCOPE WAS THE WORKER'S OWN CHOICE ("the packages you touched"), which is
//      exactly the subset that misses a whole-repo gate.
//
// These tests pin the gate's presence and its stack-agnosticism: they are CANNED workers
// and must never name one project's toolchain.
// ---------------------------------------------------------------------------

// cannedSeedByName returns a canned worker's NAME, its RollMarker, and its persisted
// agents_md, read through the exported tooling view (this is an external test package).
func cannedSeedByName(t *testing.T, name string) (string, string, string) {
	t.Helper()
	for i := range db.CannedWorkers() {
		w := db.CannedWorkers()[i]
		if w.Name == name {
			return w.Name, w.RollMarker, db.SeedAgentsMD(&w)
		}
	}
	t.Fatalf("no canned worker named %q", name)
	return "", "", ""
}

// TestEveryMergingCannedWorkerCarriesTheMergeGate: a worker that MERGES into develop must carry the
// gate.
func TestEveryMergingCannedWorkerCarriesTheMergeGate(t *testing.T) {
	for _, name := range []string{"Quick Software Engineer", "DevOps Engineer"} {
		_, _, md := cannedSeedByName(t, name)
		if !strings.Contains(md, db.MergeGateMarker) {
			t.Errorf("%s: agents_md carries no merge gate (%q) — a worker that merges into "+
				"develop without a check precondition is how a red PR lands on the integration "+
				"branch", name, db.MergeGateMarker)
		}
		// The gate must be a PRECONDITION, not a suggestion: the words that make it
		// non-optional are the ones that matter.
		for _, required := range []string{"do not merge", "still-running"} {
			if !strings.Contains(md, required) {
				t.Errorf("%s: the merge gate does not say %q — a gate that does not forbid "+
					"merging on a pending or red check is not a gate", name, required)
			}
		}
	}
}

// TestMergeGateNamesNoToolchain: these are CANNED workers used against arbitrary projects.
func TestMergeGateNamesNoToolchain(t *testing.T) {
	_, _, quickMD := cannedSeedByName(t, "Quick Software Engineer")
	for _, banned := range []string{"tsc", "npm", "node_modules", "make fe", "go test", "yarn", "pnpm"} {
		if strings.Contains(quickMD, banned) {
			t.Errorf("the merge gate names %q — canned workers run against OTHER projects, so "+
				"the gate must describe the project's own checks, not one repo's toolchain", banned)
		}
	}
	// And it must actually point at the project's definition of verified.
	if !strings.Contains(quickMD, "project's CI") {
		t.Errorf("the merge gate does not name the project's CI as the definition of verified")
	}
}

// TestShippingDevOpsWorkerHasItsOwnRollMarker: the shipping DevOps worker shares
// sdlcWorkhorseMarker with the SSE / Reviewer / Architect.
func TestShippingDevOpsWorkerHasItsOwnRollMarker(t *testing.T) {
	_, marker, md := cannedSeedByName(t, "DevOps Engineer")
	if marker == "" {
		t.Fatal("the shipping DevOps worker has NO RollMarker — the seeder only rolls a canned " +
			"worker forward when its agents_md lacks the fragment, so this gate would never reach " +
			"the published version")
	}
	if marker == db.SDLCWorkhorseMarker {
		t.Errorf("the shipping DevOps worker reuses sdlcWorkhorseMarker, so this change would " +
			"re-roll the SSE / Reviewer / Architect too")
	}
	if !strings.Contains(md, marker) {
		t.Errorf("its RollMarker %q does not appear in its own agents_md, so the roll-forward "+
			"check can never be satisfied and it would re-roll on every boot", marker)
	}
}

// TestQuickWorkerRollsForwardToTheGate: the Quick worker's marker must be the NEW fragment,
// or workers already carrying the old one are never updated.
func TestQuickWorkerRollsForwardToTheGate(t *testing.T) {
	_, marker, md := cannedSeedByName(t, "Quick Software Engineer")
	if !strings.Contains(md, marker) {
		t.Errorf("the Quick worker's RollMarker %q is absent from its agents_md — the seeder "+
			"would never consider it up to date (and would re-roll it every boot)", marker)
	}
	if marker == "SHIPPING IS IDEMPOTENT" {
		t.Error("the Quick worker's marker still pins the OLD generation, so published versions " +
			"carrying that fragment would never receive this gate")
	}
}

// TestEveryRollMarkerAppearsInItsOwnSeed is a pure, DB-free guard on the roll-forward MECHANISM.
func TestEveryRollMarkerAppearsInItsOwnSeed(t *testing.T) {
	for _, w := range db.CannedWorkers() {
		if w.RollMarker == "" {
			continue
		}
		agents := db.SeedAgentsMD(&w)
		if !strings.Contains(agents, w.RollMarker) {
			t.Errorf("%s: roll marker %q is not present in its own seeded agents_md.\n"+
				"The seeder re-syncs only when the STORED content lacks this fragment, and it looks at "+
				"agents_md alone — a marker that lives in Role (or anywhere else) strands every "+
				"already-published worker on the old contract.", w.Name, w.RollMarker)
		}
		if !strings.Contains(agents, db.SafetyMarker) {
			t.Errorf("%s: seeded agents_md is missing the global safety marker", w.Name)
		}
		if !strings.Contains(agents, db.SandboxPlaneMarker) {
			t.Errorf("%s: seeded agents_md is missing the sandbox-plane marker", w.Name)
		}
	}
}

// TestGateWorkersDoNotShareAMarker pins that the two gates roll forward independently.
func TestGateWorkersDoNotShareAMarker(t *testing.T) {
	if db.ReviewerVerdictMarker == db.QAVerifierMarker {
		t.Fatal("the reviewer and verifier roll markers must differ")
	}
	if db.ReviewerVerdictMarker == db.SDLCWorkhorseMarker || db.QAVerifierMarker == db.SDLCWorkhorseMarker {
		t.Fatal("a gate marker must not collide with the shared work-budget marker")
	}
}
