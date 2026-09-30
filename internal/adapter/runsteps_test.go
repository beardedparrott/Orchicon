package adapter_test

// runsteps_test.go — THE ONE WALK (AC 2, AC 3).
//
// adapter.ResolveRunSteps is the single traversal over a run's steps that BOTH
// consumers read: the adapter demand set (scheduler.runNeedsServe) and the
// MCP/skills union (mcpsettings' run-scope resolver). The properties pinned
// here are the ones runNeedsServe now INHERITS, so a behaviour change breaks
// this test rather than silently changing the serve gate.

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

func runStepsPool(t *testing.T) (*db.Pool, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed runsteps test")
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
	tenant := "tnt_runsteps_" + db.NewID()[10:22]
	if err := db.SeedDevTenant(ctx, pool, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return pool, tenant
}

func TestResolveRunSteps(t *testing.T) {
	pool, tenant := runStepsPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer ttx.Rollback(ctx)

	mkWorker := func(name, modelRef string) (string, int) {
		suffix := db.NewID()[10:22]
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: "w-" + suffix, TenantID: tenant, Name: name + "-" + suffix[:6],
			Slug: "w-" + suffix, Status: domain.WorkerPublished,
		})
		if err != nil {
			t.Fatalf("create worker %s: %v", name, err)
		}
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: db.NewID(), TenantID: tenant, WorkerID: w.ID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: modelRef,
		}); err != nil {
			t.Fatalf("create worker version %s: %v", name, err)
		}
		return w.ID, 1
	}
	nativeW, _ := mkWorker("native", "orchicon/deepseek/deepseek-v4-flash")
	ocW, _ := mkWorker("oc", "anthropic/claude-sonnet-4")

	steps := []workflow.StepWire{
		{ID: "d1", Kind: domain.StepKindDecision},
		{ID: "p1", Kind: domain.StepKindParallel},
		{ID: "wi", Kind: domain.StepKindWorkItem, Config: `{"work_item_id":"x"}`},
		{ID: "no-ref", Kind: domain.StepKindTask},
		{ID: "w-native", Kind: domain.StepKindTask, Ref: nativeW},
		{ID: "w-oc", Kind: domain.StepKindApproval, Ref: ocW},
		{ID: "w-missing", Kind: domain.StepKindTask, Ref: "w-does-not-exist"},
	}

	res := adapter.ResolveRunSteps(ctx, ttx.Tx, tenant, steps)

	// One entry per WORKER-BEARING step: the decision/parallel/work_item and
	// the empty-Ref step contribute nothing.
	if len(res) != 3 {
		t.Fatalf("resolved %d steps, want 3 (task + approval + unresolvable): %+v", len(res), res)
	}
	if res[0].StepID != "w-native" || res[1].StepID != "w-oc" || res[2].StepID != "w-missing" {
		t.Fatalf("step order/selection wrong: %+v", res)
	}
	// The version ROW is carried (the union reads its permissions jsonb).
	if res[0].Version.ModelRef != "orchicon/deepseek/deepseek-v4-flash" {
		t.Errorf("version row not carried for the native step: %+v", res[0].Version)
	}
	if res[0].ModelRef != "orchicon/deepseek/deepseek-v4-flash" {
		t.Errorf("native model ref = %q", res[0].ModelRef)
	}
	if res[1].ModelRef != "anthropic/claude-sonnet-4" {
		t.Errorf("approval step model ref = %q", res[1].ModelRef)
	}
	// An UNRESOLVABLE worker folds to the zero value: empty ModelRef, no
	// version. The demand set reads that as the CONSERVATIVE default, which is
	// exactly the pre-existing gate's rule.
	if res[2].ModelRef != "" || res[2].Version.ID != "" {
		t.Errorf("unresolvable worker did not fold to the zero value: %+v", res[2])
	}

	// ModelRefs is verbatim and step-ordered, INCLUDING the empty one (the
	// demand set's conservative rule depends on seeing it).
	refs := res.ModelRefs()
	if len(refs) != 3 || refs[0] != "orchicon/deepseek/deepseek-v4-flash" ||
		refs[1] != "anthropic/claude-sonnet-4" || refs[2] != "" {
		t.Errorf("ModelRefs = %v, want the three refs in step order with the empty last", refs)
	}

	// The demand set the gate asks: only the opencode ref (and the
	// conservative default for the unresolvable step) needs a serve.
	demand := adapter.AdapterDemandSet(refs...)
	if !demand.Has("opencode") {
		t.Errorf("demand set lost the opencode kind: %v", demand.Kinds())
	}

	// A step PINNED to a version number resolves BY NUMBER (not by id).
	pinned := []workflow.StepWire{{ID: "pinned", Kind: domain.StepKindTask, Ref: nativeW, WorkerVersion: 1}}
	pres := adapter.ResolveRunSteps(ctx, ttx.Tx, tenant, pinned)
	if len(pres) != 1 || pres[0].ModelRef != "orchicon/deepseek/deepseek-v4-flash" {
		t.Fatalf("pinned-version resolution = %+v", pres)
	}
	if pres[0].WorkerVersion != 1 {
		t.Errorf("pinned version number not carried: %+v", pres[0])
	}
}
