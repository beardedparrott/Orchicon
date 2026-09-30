package runtime

// lifecycle_planecred_test.go — AC 3 for the PLANE-CHANNEL case.
//
// The sibling TestRunServeConfigProviderIsDeterministicForARun uses a worker
// with NO role_ref, so mintPlaneCredential short-circuits to nil and never
// exercises its mint. But mintPlaneCredential MINTS A FRESH RANDOM API KEY on
// every call, and RunServeConfig is invoked from the adapter's PER-EXECUTION
// self-heal path — so a plane-channel run (a published, role-bound worker)
// emitted a DIFFERENT config on every dispatch. That (a) breaks the
// determinism AC 3 requires, (b) changes the daemon's serve-config pool key so
// the run's already-warmed container is not reused, and (c) leaks one
// api_keys row per execution. This test pins the fix (the per-run
// planeCredCache memo).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

func TestRunServeConfigDeterministicWithPlaneCredential(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed plane-credential determinism test")
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
	if err := db.SeedDevTenant(ctx, pool, devTenantID); err != nil {
		t.Fatalf("seed dev tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, devTenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	role, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{
		TenantID: devTenantID, Name: "plane-det-" + db.NewID()[10:22],
		Scope: "tenant", Entitlements: []string{"work_items.read"},
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: devTenantID, Name: "Plane Determinism",
		Slug: "plane-det-" + db.NewID()[10:22], Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// A PUBLISHED, ROLE-BOUND worker: this is what grants the plane channel
	// (deny-by-default). Without RoleRef the mint short-circuits and the bug
	// this test exists for is invisible.
	suffix := db.NewID()[10:22]
	w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: "w-" + suffix, TenantID: devTenantID, Name: "plane-det-" + suffix[:6],
		Slug: "w-" + suffix, Status: domain.WorkerPublished, RoleRef: role.ID,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: devTenantID, WorkerID: w.ID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "opencode/deepseek/deepseek-v4-flash-free",
	}); err != nil {
		t.Fatalf("create worker version: %v", err)
	}
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: devTenantID, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "plane-det ticket",
		Description: "d", AcceptanceCriteria: "a", Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: devTenantID, ProjectID: proj.ID,
		Name: "Plane Det WF", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	stepsJSON, _ := json.Marshal([]workflow.StepWire{
		{ID: "step-a", Kind: domain.StepKindTask, Ref: w.ID},
	})
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: devTenantID, WorkflowID: wf.ID, Version: 1,
		Status: "published", Steps: stepsJSON, Inputs: []byte("[]"), Outputs: []byte("[]"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: devTenantID, WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RuntimeImage: "orchicon-runtime:local", RunContext: []byte("{}"), WorkItemID: wi.ID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The builder returns the PLANE TOKEN, so a fresh mint on any call makes the
	// two values differ — exactly the AC 3 violation this test pins.
	lc := NewLifecycle(nil, pool, slog.Default(),
		func(image, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution) string {
			if planeEnv == nil {
				return "NO-PLANE-CHANNEL"
			}
			return "PLANE:" + planeEnv["ORCHICON_PLANE_TOKEN"]
		}, nil)
	lc.SetScopeResolver(mcpclient.NoopScopeResolver{})

	first, ok := lc.RunServeConfig(ctx, run.ID)
	if !ok {
		t.Fatalf("RunServeConfig reported no config for a plane-channel run")
	}
	if first == "NO-PLANE-CHANNEL" {
		t.Fatalf("the run did not resolve a plane channel — the fixture did not exercise the mint")
	}
	second, ok := lc.RunServeConfig(ctx, run.ID)
	if !ok {
		t.Fatalf("RunServeConfig failed on the second call")
	}
	if first != second {
		t.Fatalf("the plane-channel run serve config is NOT deterministic (a fresh key is minted per call):\n%q\n%q", first, second)
	}

	// The mint ran ONCE: the second call reused the memoized credential rather
	// than leaking another api_keys row.
	var keyCount int
	kq, kerr := pool.BeginTenantTx(ctx, devTenantID)
	if kerr != nil {
		t.Fatalf("begin key-count tx: %v", kerr)
	}
	defer kq.Rollback(ctx)
	if qerr := kq.Tx.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE tenant_id=$1 AND name=$2`,
		devTenantID, "automation:run:"+run.ID).Scan(&keyCount); qerr != nil {
		t.Fatalf("count api keys: %v", qerr)
	}
	if keyCount != 1 {
		t.Fatalf("expected exactly one minted plane key for the run (memoized), got %d", keyCount)
	}
}
