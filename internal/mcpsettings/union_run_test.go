package mcpsettings_test

// union_run_test.go — THE RUN-LEVEL UNION (AC 1, AC 2, AC 3).
//
// The RUN scope is the one to get right BY CONSTRUCTION: its result is applied
// ONCE per container (child 4), so it must be derived from the RUN, never from
// an executing worker. The test seeds a TWO-STEP run whose steps use DIFFERENT
// worker versions with DIFFERENT inline own-sets — the result must contain
// BOTH, which a per-execution resolution cannot produce.

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

func TestResolveRunUnionTwoStepsTwoWorkers(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed run-union test")
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

	// UNIQUE PER RUN: a fixed tenant id makes concurrent test processes
	// contend on the same tenants row and deadlock.
	tenant := "tnt_rununion_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "Run Union",
		Slug: "run-union-" + db.NewID()[10:22], Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Two workers, each with ONE published version carrying a DIFFERENT
	// inline mcp_servers own-set plus a DIFFERENT inline skill file.
	mkWorker := func(name, mcpID, skillPath string) string {
		suffix := db.NewID()[10:22]
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: "w-" + suffix, TenantID: tenant, Name: name + "-" + suffix[:6],
			Slug: "w-" + suffix, Status: domain.WorkerPublished,
		})
		if err != nil {
			t.Fatalf("create worker %s: %v", name, err)
		}
		perms, _ := json.Marshal(map[string]any{
			"mcp_servers": []map[string]any{{
				"id": mcpID, "type": "stdio", "command": []string{"npx", "-y", mcpID},
			}},
			"skill_files": []map[string]any{{"path": skillPath, "content": "x"}},
		})
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: db.NewID(), TenantID: tenant, WorkerID: w.ID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
			Permissions: perms,
		}); err != nil {
			t.Fatalf("create worker version %s: %v", name, err)
		}
		return w.ID
	}
	wA := mkWorker("worker-a", "inline-a", "skills/a.md")
	wB := mkWorker("worker-b", "inline-b", "skills/b.md")

	// A project-OWNED definition, so the union's project half is present too.
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}
	svc := mcpsettings.New(pool, nil, nil)
	projSrv, err := svc.Create(ctx, tenant, mcpsettings.CreateInput{
		Name: "project-owned", ProjectID: proj.ID,
		Transport: mcpsettings.TransportStdio, Command: "npx",
		Args: []string{"-y", "project-owned"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create project-owned definition: %v", err)
	}

	// The run's published version: TWO TASK steps, one per worker, with NO
	// pinned version (so dispatch resolves the latest published — the same
	// resolution the demand set sees).
	wtx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin run tx: %v", err)
	}
	defer wtx.Rollback(ctx)
	stepsJSON, _ := json.Marshal([]workflow.StepWire{
		{ID: "step-a", Kind: domain.StepKindTask, Ref: wA},
		{ID: "step-b", Kind: domain.StepKindTask, Ref: wB},
	})
	wf, err := db.CreateWorkflow(ctx, wtx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID,
		Name: "Run Union WF", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, wtx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: tenant, WorkflowID: wf.ID, Version: 1,
		Status: "published", Steps: stepsJSON, Inputs: []byte("[]"), Outputs: []byte("[]"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, wtx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenant, WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), CurrentStep: "",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	// THE CALL. Note the signature: it takes the RUN, never a worker.
	res, err := mcpsettings.NewResolver(pool).ResolveRunUnion(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("ResolveRunUnion: %v", err)
	}

	// BOTH step workers' own-sets are present — the property a per-execution
	// resolution CANNOT produce (it would resolve exactly one worker).
	if !runUnionHas(res, "inline-a") {
		t.Errorf("the union is missing step A's inline definition: %+v", idsOf(res))
	}
	if !runUnionHas(res, "inline-b") {
		t.Errorf("the union is missing step B's inline definition: %+v", idsOf(res))
	}
	// And the project-owned definition.
	if !runUnionHas(res, projSrv.ID) {
		t.Errorf("the union is missing the project-owned definition: %+v", idsOf(res))
	}
	// Project rows come first (order-stable), then the inline specs in STEP
	// order.
	if res.Servers[0].EntryID != projSrv.ID {
		t.Errorf("union order = %+v, want the project-owned row first", idsOf(res))
	}
	var inlineOrder []string
	for _, s := range res.Servers {
		if s.EntryID == "" {
			inlineOrder = append(inlineOrder, s.Spec.ID)
		}
	}
	if len(inlineOrder) != 2 || inlineOrder[0] != "inline-a" || inlineOrder[1] != "inline-b" {
		t.Errorf("inline order = %v, want [inline-a inline-b] (step order)", inlineOrder)
	}
	// Each carries PROVENANCE.
	for _, s := range res.Servers {
		if s.FromID == "" {
			t.Errorf("resolved server %q carries no provenance", s.Spec.ID)
		}
	}

	// THE SKILLS UNION: both step versions' skill files.
	paths := map[string]bool{}
	for _, s := range res.Skills {
		paths[s.Path] = true
	}
	if !paths["skills/a.md"] || !paths["skills/b.md"] {
		t.Errorf("skills union = %+v, want both step versions' files", res.Skills)
	}

	// THE SCOPE SHAPE: ScopeRun resolves the same thing through ResolveScope,
	// so both entry points agree.
	byScope, err := mcpsettings.NewResolver(pool).ResolveScope(withTenant(ctx, tenant), mcpclient.ScopeRef{
		Kind: mcpclient.ScopeRun, RunID: run.ID,
	})
	if err != nil {
		t.Fatalf("ResolveScope(run): %v", err)
	}
	if len(byScope.Servers) != len(res.Servers) {
		t.Errorf("ResolveScope(run) = %d servers, ResolveRunUnion = %d — the two entries must agree",
			len(byScope.Servers), len(res.Servers))
	}
}

func runUnionHas(r mcpclient.Resolution, id string) bool {
	for _, s := range r.Servers {
		if s.EntryID == id || s.Spec.ID == id {
			return true
		}
	}
	return false
}

func idsOf(r mcpclient.Resolution) []string {
	out := make([]string, 0, len(r.Servers))
	for _, s := range r.Servers {
		out = append(out, s.EntryID+"|"+s.Spec.ID)
	}
	return out
}

// withTenant scopes ctx with the tenant the resolver reads
// (mcpclient.ScopeResolver implementations read tenant.FromContext).
func withTenant(ctx context.Context, tenantID string) context.Context {
	return tenant.WithID(ctx, tenantID)
}

// TestResolveRunUnionDedupesASharedInlineID pins the union's DEDUP contract
// for inline (worker-owned) definitions: an inline definition has no
// mcp_servers row, so its ScopedServer.EntryID is "" — a seen-set seeded from
// EntryID alone is empty on every call and two steps declaring the SAME inline
// id would each resolve it. The run scope is exactly where that arises (two
// worker versions can declare one shared server name), and a duplicate would
// open the same MCP connection twice under one namespace.
func TestResolveRunUnionDedupesASharedInlineID(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed run-union dedup test")
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

	tenant := "tnt_rundedup_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "Dedup", Slug: "dedup-" + db.NewID()[10:22],
		Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// TWO workers whose versions declare the SAME inline id.
	mkWorker := func() string {
		suffix := db.NewID()[10:22]
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: "w-" + suffix, TenantID: tenant, Name: "w-" + suffix[:6],
			Slug: "w-" + suffix, Status: domain.WorkerPublished,
		})
		if err != nil {
			t.Fatalf("create worker: %v", err)
		}
		perms, _ := json.Marshal(map[string]any{
			"mcp_servers": []map[string]any{{
				"id": "shared-inline", "type": "stdio", "command": []string{"npx", "-y", "shared"},
			}},
		})
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: db.NewID(), TenantID: tenant, WorkerID: w.ID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
			Permissions: perms,
		}); err != nil {
			t.Fatalf("create worker version: %v", err)
		}
		return w.ID
	}
	wA, wB := mkWorker(), mkWorker()
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	wtx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin run tx: %v", err)
	}
	defer wtx.Rollback(ctx)
	stepsJSON, _ := json.Marshal([]workflow.StepWire{
		{ID: "step-a", Kind: domain.StepKindTask, Ref: wA},
		{ID: "step-b", Kind: domain.StepKindTask, Ref: wB},
	})
	wf, err := db.CreateWorkflow(ctx, wtx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID,
		Name: "Dedup WF", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, wtx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: tenant, WorkflowID: wf.ID, Version: 1,
		Status: "published", Steps: stepsJSON, Inputs: []byte("[]"), Outputs: []byte("[]"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, wtx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenant, WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning, RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	res, err := mcpsettings.NewResolver(pool).ResolveRunUnion(ctx, tenant, run.ID)
	if err != nil {
		t.Fatalf("ResolveRunUnion: %v", err)
	}
	n := 0
	for _, s := range res.Servers {
		if s.Spec.ID == "shared-inline" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a shared inline id resolved %d times, want 1 (deduped): %+v", n, idsOf(res))
	}
}
