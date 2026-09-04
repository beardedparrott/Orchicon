package scheduler

// Tests for the adapter-aware runtime-serve gate: a run whose worker steps
// all resolve to serve-less adapter kinds (native "orchicon") must arm
// with RuntimeReady=true and carry the runtime.NoServeImage sentinel, so
// the run never gates on (or fails for) the in-container opencode serve it
// never uses. Observed bug: an orchicon-only run failed at start with
// "runtime opencode serve failed to become usable" and sat "waiting for
// dispatch" until the serve deadline expired.
//
// runNeedsServe is DB-backed (worker versions come from worker_versions)
// and is skipped without ORCHICON_TEST_DSN; imageForRun and the
// sentinel round-trip are pure unit tests.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// stubLifecycle is the minimal RuntimeLifecycle stub for runNeedsServe
// tests: ServeDependent mirrors runtime.Lifecycle's real rule
// (opencode-only) so the unit behaves identically without a daemon.
type stubLifecycle struct{}

func (stubLifecycle) ServeDependent(kind string) bool {
	if kind == "" {
		kind = "opencode"
	}
	return kind == "opencode"
}
func (stubLifecycle) EnsureForRun(context.Context, db.WorkflowRunRow) error { return nil }
func (stubLifecycle) EnsureServing(context.Context, db.WorkflowRunRow) error {
	return nil
}
func (stubLifecycle) ReapForRun(context.Context, string) error { return nil }

// TestImageForRun pins the sentinel stamping: serve-needing runs carry
// the resolved image; serve-less runs carry runtime.NoServeImage (which
// the lifecycle no-ops on). The sentinel must NEVER equal a real image
// tag an operator could configure.
func TestImageForRun(t *testing.T) {
	if got := imageForRun("orchicon-runtime:orchicon-dev", true); got != "orchicon-runtime:orchicon-dev" {
		t.Errorf("imageForRun(needsServe) = %q, want the resolved image", got)
	}
	if got := imageForRun("orchicon-runtime:orchicon-dev", false); got != runtime.NoServeImage {
		t.Errorf("imageForRun(serveless) = %q, want %q", got, runtime.NoServeImage)
	}
	if got := imageForRun("", false); got != runtime.NoServeImage {
		t.Errorf("imageForRun(empty, serveless) = %q, want %q", got, runtime.NoServeImage)
	}
	// The sentinel must not collide with any plausible real tag.
	if strings.Contains(runtime.NoServeImage, "runtime:") || strings.Contains(runtime.NoServeImage, ":") {
		t.Errorf("sentinel %q looks like a real image tag — collision risk", runtime.NoServeImage)
	}
}

// TestRunNeedsServeNilRuntime: a headless reconciler (runtime nil) never
// needs a serve regardless of steps — the gate is moot.
func TestRunNeedsServeNilRuntime(t *testing.T) {
	r := &WorkflowReconciler{}
	steps := []workflow.StepWire{
		{ID: "w1", Kind: domain.StepKindTask, Ref: "w_any"},
	}
	if r.runNeedsServe(context.Background(), nil, "tnt_dev", db.WorkflowRunRow{}, steps) {
		t.Error("runNeedsServe with nil runtime = true, want false")
	}
}

// TestRunNeedsServeNonWorkerSteps: decision/parallel/work_item steps carry
// no worker ref → no serve demand even with a wired runtime.
func TestRunNeedsServeNonWorkerSteps(t *testing.T) {
	r := &WorkflowReconciler{runtime: stubLifecycle{}}
	steps := []workflow.StepWire{
		{ID: "d1", Kind: domain.StepKindDecision},
		{ID: "wi", Kind: domain.StepKindWorkItem, Config: `{"work_item_id":"x"}`},
		{ID: "p1", Kind: domain.StepKindParallel},
	}
	if r.runNeedsServe(context.Background(), nil, "tnt_dev", db.WorkflowRunRow{}, steps) {
		t.Error("runNeedsServe(non-worker steps) = true, want false")
	}
}

// DB-backed: a step whose worker version carries an orchicon model_ref
// yields no serve demand; an opencode ref (or an unresolvable worker)
// does. Skipped without ORCHICON_TEST_DSN.
func TestRunNeedsServeAdapterKinds(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed runNeedsServe test")
	}
	ctx := context.Background()
	pool := approvalTestPool(t)
	r := &WorkflowReconciler{pool: pool, runtime: stubLifecycle{}}

	seedWorkerVersion := func(t *testing.T, name, modelRef, runtimeRef string) string {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
		if err != nil {
			t.Fatal(err)
		}
		defer ttx.Rollback(ctx)
		suffix := strings.ToLower(db.NewID())[:12]
		w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: "w-" + suffix, TenantID: approvalTestTenant,
			Name: name + "-" + suffix[:8], Slug: "w-" + suffix,
			Status: domain.WorkerPublished,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: db.NewID(), TenantID: approvalTestTenant, WorkerID: w.ID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: modelRef, RuntimeRef: runtimeRef,
		}); err != nil {
			t.Fatal(err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return w.ID
	}

	nativeWorker := seedWorkerVersion(t, "native", "orchicon/deepseek/deepseek-v4-flash", "")
	ocWorker := seedWorkerVersion(t, "oc", "anthropic/claude-sonnet-4", "")

	nativeOnly := r.runNeedsServe(ctx, nil, approvalTestTenant, db.WorkflowRunRow{}, []workflow.StepWire{
		{ID: "w", Kind: domain.StepKindTask, Ref: nativeWorker},
	})
	if nativeOnly {
		t.Error("native-only run needsServe = true, want false")
	}

	mixed := r.runNeedsServe(ctx, nil, approvalTestTenant, db.WorkflowRunRow{}, []workflow.StepWire{
		{ID: "w", Kind: domain.StepKindTask, Ref: nativeWorker},
		{ID: "w2", Kind: domain.StepKindTask, Ref: ocWorker},
	})
	if !mixed {
		t.Error("mixed run needsServe = false, want true")
	}

	// Unresolvable worker → conservative opencode demand (parity with the
	// pre-fix gate).
	unknown := r.runNeedsServe(ctx, nil, approvalTestTenant, db.WorkflowRunRow{}, []workflow.StepWire{
		{ID: "w", Kind: domain.StepKindTask, Ref: "w-does-not-exist"},
	})
	if !unknown {
		t.Error("unresolvable-worker run needsServe = false, want true (conservative)")
	}
}