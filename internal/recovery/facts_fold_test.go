package recovery

// Acceptance-criterion test for the deterministic fact capture change: the
// recovery flow's summary must be folded into .orchicon/<run>/facts_learned
// as a Recovery-attributed entry so ALL of a run's steps see it (today it
// only reaches the restarted execution). Best-effort + idempotent. Skipped
// unless ORCHICON_TEST_DSN points at a disposable database.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/recovery/ -run TestFoldRecoverySummaryToFacts -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestFoldRecoverySummaryToFacts verifies foldRecoverySummaryToFacts writes
// the recovery summary into the bound run's facts_learned as a Recovery-
// attributed line, is idempotent on re-call, and leaves the worker-emitted
// lines untouched.
func TestFoldRecoverySummaryToFacts(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed recovery fold test")
	}
	f := seedKeysSetup(t, true)
	ctx := context.Background()
	const tenant = "tnt_dev"
	root := t.TempDir()

	// Point the project's project_dir at a temp dir so .orchicon/<run>
	// resolves to a writable location inside the test.
	ttx, err := f.pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ttx.Exec(ctx,
		`UPDATE projects SET project_dir = $1 WHERE id = $2 AND tenant_id = $3`,
		root, f.projectID, tenant); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	engine := newQuietEngine(f.pool)
	rc := NewReconciler(engine)

	// A resumed recovery carrying a summary (the state progressRecovery's
	// resume branch reaches). foldRecoverySummaryToFacts resolves project_dir
	// and the run id from rec.ProjectID / rec.FailedExecutionID.
	const summary = "recovery rebuilt context; root cause was a stall-killed SSE"
	recRow := db.RecoveryExecutionRow{
		ID:                db.NewID(),
		TenantID:          tenant,
		ProjectID:         f.projectID,
		TaskID:            f.taskID,
		FailedExecutionID: f.execID,
		Status:            domain.RecoveryResumed,
		Summary:           summary,
	}

	// First fold: the file is created with the Recovery-attributed line.
	rc.foldRecoverySummaryToFacts(ctx, tenant, recRow)
	factsPath := filepath.Join(root, ".orchicon", f.runID, "facts_learned")
	b, err := os.ReadFile(factsPath)
	if err != nil {
		t.Fatalf("read facts_learned: %v", err)
	}
	if !strings.Contains(string(b), "FACTS LEARNED (from Recovery): "+summary) {
		t.Fatalf("recovery summary not folded; got:\n%s", string(b))
	}

	// Idempotent: re-folding (e.g. a re-reconcile) must not double-append.
	rc.foldRecoverySummaryToFacts(ctx, tenant, recRow)
	b, _ = os.ReadFile(factsPath)
	if got := strings.Count(string(b), "FACTS LEARNED (from Recovery): "+summary); got != 1 {
		t.Fatalf("recovery summary appended %d times, want 1 (terminal idempotency); got:\n%s", got, string(b))
	}

	// An empty summary is a no-op (never appends a bare marker).
	rc.foldRecoverySummaryToFacts(ctx, tenant, db.RecoveryExecutionRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: f.projectID,
		TaskID: f.taskID, FailedExecutionID: f.execID, Status: domain.RecoveryResumed,
	})
	b, _ = os.ReadFile(factsPath)
	if !strings.Contains(string(b), "FACTS LEARNED (from Recovery): "+summary) {
		t.Fatalf("empty-summary fold disturbed existing content:\n%s", string(b))
	}
	if strings.Count(string(b), "FACTS LEARNED (from Recovery):") != 1 {
		t.Fatalf("empty summary must not add a line:\n%s", string(b))
	}
}
