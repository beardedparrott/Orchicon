package runtime

// lifecycle_union_test.go — AC 1 / AC 2 at the WIRING level.
//
// Child 1's union_run_test.go proves the RESOLVER half (ResolveRunUnion walks
// the run and unions every step version's own-set). This proves the SERVE
// CONFIG half: buildCreateRequest — the single place the container's serve
// config is built — resolves the RUN union through the same ScopeResolver and
// hands it to the config builder. A per-execution resolution cannot produce
// the union asserted here, which is the whole point of precomputing it.

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
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestBuildCreateRequestBakesRunUnionForTwoSteps is the DB-gated proof that
// the container's serve config is built from the RUN union: a 2-step run whose
// steps pin DIFFERENT worker versions with DIFFERENT inline MCP own-sets must
// yield a serve config carrying BOTH (AC 1), so a step-A execution receives a
// server only step B defined (AC 2).
func TestBuildCreateRequestBakesRunUnionForTwoSteps(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed run-union wiring test")
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
	// contend on the same row and deadlock.
	tenantID := "tnt_rununion_wire_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID, Name: "Run Union Wire",
		Slug: "run-union-wire-" + db.NewID()[10:22], Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Two workers, each with ONE published version carrying a DIFFERENT inline
	// mcp_servers own-set plus a DIFFERENT inline skill file.
	mkWorker := func(id string) string {
		suffix := db.NewID()[10:22]
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: "w-" + suffix, TenantID: tenantID, Name: id + "-" + suffix[:6],
			Slug: "w-" + suffix, Status: domain.WorkerPublished,
		})
		if err != nil {
			t.Fatalf("create worker %s: %v", id, err)
		}
		perms, _ := json.Marshal(map[string]any{
			"mcp_servers": []map[string]any{{
				"id": id, "type": "stdio", "command": []string{"npx", "-y", id},
			}},
			"skill_files": []map[string]any{{"path": "skills/" + id + ".md", "content": "x"}},
		})
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: db.NewID(), TenantID: tenantID, WorkerID: w.ID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: "opencode/deepseek/deepseek-v4-flash-free",
			Permissions: perms,
		}); err != nil {
			t.Fatalf("create worker version %s: %v", id, err)
		}
		return w.ID
	}
	wA := mkWorker("inline-a")
	wB := mkWorker("inline-b")

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	// A project-owned definition, so the union's project half is present too.
	svc := mcpsettings.New(pool, nil, nil)
	projSrv, err := svc.Create(ctx, tenantID, mcpsettings.CreateInput{
		Name: "project-owned-wire", ProjectID: proj.ID,
		Transport: mcpsettings.TransportStdio, Command: "npx",
		Args: []string{"-y", "project-owned"}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create project-owned definition: %v", err)
	}

	wtx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin run tx: %v", err)
	}
	defer wtx.Rollback(ctx)
	stepsJSON, _ := json.Marshal([]workflow.StepWire{
		{ID: "step-a", Kind: domain.StepKindTask, Ref: wA},
		{ID: "step-b", Kind: domain.StepKindTask, Ref: wB},
	})
	wf, err := db.CreateWorkflow(ctx, wtx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: proj.ID,
		Name: "Run Union Wire WF", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, wtx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: tenantID, WorkflowID: wf.ID, Version: 1,
		Status: "published", Steps: stepsJSON, Inputs: []byte("[]"), Outputs: []byte("[]"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, wtx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenantID, WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RuntimeImage: "orchicon-runtime:local",
		RunContext:   []byte("{}"), CurrentStep: "",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	// THE ONE CONFIG BUILDER is captured here: the test asserts on the union
	// the PLANE hands it, which is exactly what the real
	// opencode.RuntimeServeConfig consumes. (A test file in package runtime
	// cannot import internal/opencode — opencode imports runtime — so the
	// seam is captured instead of the builder being called.)
	var got mcpclient.Resolution
	var called bool
	lc := NewLifecycle(nil, pool, slog.Default(),
		func(image, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution) string {
			called = true
			got = union
			return "{}"
		}, nil)
	lc.SetScopeResolver(mcpsettings.NewResolver(pool))

	req, err := lc.buildCreateRequest(ctx, run)
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	if !called {
		t.Fatalf("the serve config builder was not called for an opencode run")
	}
	if req.ServeConfig == "" {
		t.Fatalf("the create request carries no serve config")
	}

	// BOTH steps' own-sets + the project-owned definition are in the RUN union.
	has := func(id string) bool {
		for _, s := range got.Servers {
			if s.Spec.ID == id || s.EntryID == id {
				return true
			}
		}
		return false
	}
	if !has("inline-a") {
		t.Errorf("the run union is missing step A's inline definition (AC 1): %+v", unionIDs(got))
	}
	if !has("inline-b") {
		t.Errorf("the run union is missing step B's inline definition (AC 2 — a step-A execution must receive step B's server): %+v", unionIDs(got))
	}
	if !has(projSrv.ID) {
		t.Errorf("the run union is missing the project-owned definition: %+v", unionIDs(got))
	}
	// The skill-file union rides along (AC 1's second half).
	paths := map[string]bool{}
	for _, s := range got.Skills {
		paths[s.Path] = true
	}
	if !paths["skills/inline-a.md"] || !paths["skills/inline-b.md"] {
		t.Errorf("the run skill union = %+v, want both step versions' files", got.Skills)
	}
	// Provenance is carried for the plane-side log (AC 4).
	for _, s := range got.Servers {
		if s.FromID == "" {
			t.Errorf("resolved server %q carries no provenance", s.Spec.ID)
		}
	}
}

// TestRunServeConfigProviderIsDeterministicForARun proves AC 3 at the wiring
// level: the provider takes ONLY a run id, and two calls for the same run
// return the SAME config — the self-heal path can therefore never disagree
// with the run-start one.
func TestRunServeConfigProviderIsDeterministicForARun(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed determinism test")
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
	// The provider reads the run under the plane's dev tenant (the reconciler's
	// single-dev-tenant assumption), so seed and use THAT tenant.
	if err := db.SeedDevTenant(ctx, pool, devTenantID); err != nil {
		t.Fatalf("seed dev tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, devTenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: devTenantID, Name: "Determinism",
		Slug: "determinism-" + db.NewID()[10:22], Status: "active", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// One opencode worker + one task step, so the run DEMANDS opencode (a run
	// with no opencode demand has no serve config by design).
	suffix := db.NewID()[10:22]
	w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: "w-" + suffix, TenantID: devTenantID, Name: "det-" + suffix[:6],
		Slug: "w-" + suffix, Status: domain.WorkerPublished,
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
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: devTenantID, ProjectID: proj.ID,
		Name: "Det WF", CurrentVersion: 1, Status: "published", Type: "one_shot",
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
		RuntimeImage: "orchicon-runtime:local", RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	lc := NewLifecycle(nil, pool, slog.Default(),
		func(image, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution) string {
			return `{"tag":"` + image + `"}`
		}, nil)
	lc.SetScopeResolver(mcpsettings.NewResolver(pool))

	first, ok := lc.RunServeConfig(ctx, run.ID)
	if !ok {
		t.Fatalf("RunServeConfig reported no config for an opencode run")
	}
	second, ok := lc.RunServeConfig(ctx, run.ID)
	if !ok {
		t.Fatalf("RunServeConfig failed on the second call")
	}
	if first != second {
		t.Fatalf("the run serve config is not deterministic:\n%s\n---\n%s", first, second)
	}
	// An unknown run yields (false) — the caller sends NO config rather than
	// inventing one.
	if cfg, ok := lc.RunServeConfig(ctx, "run-does-not-exist"); ok || cfg != "" {
		t.Fatalf("an unknown run must report no config, got ok=%v cfg=%q", ok, cfg)
	}
}

func unionIDs(r mcpclient.Resolution) []string {
	out := make([]string, 0, len(r.Servers))
	for _, s := range r.Servers {
		out = append(out, s.Spec.ID+"("+s.FromID+")")
	}
	return out
}
