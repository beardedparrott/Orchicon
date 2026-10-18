package scheduler

// Work-item execution_mode override: persistence, the resolution order
// (item -> project -> tenant default), the ephemeral/Quick Work default, and
// the arm gate that preserves the "local mode never calls EnsureForRun"
// lifecycle invariant.
//
// DB-backed; skipped without ORCHICON_TEST_DSN (a disposable sandbox DSN —
// never the live plane).

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/jackc/pgx/v5"
)

// recordingLifecycle records EnsureForRun calls so "never provisions a
// container for a local item" is an assertion, not a comment.
type recordingLifecycle struct {
	stubLifecycle
	ensureCalls int
	lastRun     db.WorkflowRunRow
}

func (l *recordingLifecycle) EnsureForRun(_ context.Context, run db.WorkflowRunRow) error {
	l.ensureCalls++
	l.lastRun = run
	return nil
}

// mkModeProject creates a project already in the given execution mode.
func mkModeProject(t *testing.T, ctx context.Context, tx pgx.Tx, id, slug, mode string) db.ProjectRow {
	t.Helper()
	p, err := db.CreateProject(ctx, tx, db.ProjectRow{
		ID: id, TenantID: approvalTestTenant, Name: id, Slug: slug,
		Status: "active", Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	if p.ExecutionMode == mode {
		return p
	}
	m := mode
	upd, err := db.UpdateProject(ctx, tx, approvalTestTenant, p.ID, p.Version, db.UpdateProjectFields{ExecutionMode: &m})
	if err != nil {
		t.Fatalf("set project %s mode: %v", id, err)
	}
	return upd
}

// TestWorkItemExecutionModePersists pins AC1: the override is persisted, NULL
// means inherit, and clearing returns the row to NULL (never "").
func TestWorkItemExecutionModePersists(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	p := mkModeProject(t, ctx, ttx.Tx, "prj_mode_persist", "mode-persist", db.ExecutionModeRuntime)

	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_mode_persist", TenantID: approvalTestTenant, ProjectID: p.ID,
		Kind: domain.WorkItemKindTask, Title: "persist", Status: domain.WorkItemReady,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if wi.ExecutionMode != nil {
		t.Fatalf("new item execution_mode = %q, want NULL (inherit)", *wi.ExecutionMode)
	}

	local := db.ExecutionModeLocal
	upd, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, wi.ID, wi.Version, db.UpdateWorkItemFields{ExecutionMode: &local})
	if err != nil {
		t.Fatalf("set override: %v", err)
	}
	if upd.ExecutionMode == nil || *upd.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("override round-trip = %v, want local", upd.ExecutionMode)
	}

	clr, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, wi.ID, upd.Version, db.UpdateWorkItemFields{ClearExecutionMode: true})
	if err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if clr.ExecutionMode != nil {
		t.Fatalf("cleared execution_mode = %q, want NULL", *clr.ExecutionMode)
	}
}

// TestExecutionModeResolutionOrder pins AC2 at each level, in both directions.
func TestExecutionModeResolutionOrder(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	localPrj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_local", "mode-local", db.ExecutionModeLocal)
	rtPrj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_rt", "mode-rt", db.ExecutionModeRuntime)
	local, rt := db.ExecutionModeLocal, db.ExecutionModeRuntime

	cases := []struct {
		name    string
		itemID  string
		project db.ProjectRow
		mode    *string
		want    string
	}{
		{"inherit local project", "wi_rl_nil", localPrj, nil, db.ExecutionModeLocal},
		{"inherit runtime project", "wi_rr_nil", rtPrj, nil, db.ExecutionModeRuntime},
		{"runtime item in local project", "wi_lr_rt", localPrj, &rt, db.ExecutionModeRuntime},
		{"local item in runtime project", "wi_rl_local", rtPrj, &local, db.ExecutionModeLocal},
	}
	for _, tc := range cases {
		wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
			ID: tc.itemID, TenantID: approvalTestTenant, ProjectID: tc.project.ID,
			Kind: domain.WorkItemKindTask, Title: tc.name, Status: domain.WorkItemReady,
			ExecutionMode: tc.mode,
		})
		if err != nil {
			t.Fatalf("%s: create item: %v", tc.name, err)
		}
		if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, wi.ProjectID); got != tc.want {
			t.Errorf("%s: resolved = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEphemeralResolvesLocal pins AC7: a Quick Work (ephemeral) item gets a
// CONCRETE local mode at create — never the undefined "inherit" state — and an
// explicit caller value still wins.
func TestEphemeralResolvesLocal(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_ephemeral", "mode-ephemeral", db.ExecutionModeRuntime)

	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_ephemeral_mode", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "quick work", Status: domain.WorkItemReady,
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create ephemeral item: %v", err)
	}
	if wi.ExecutionMode == nil || *wi.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("ephemeral item mode = %v, want concrete local (stamped at create)", wi.ExecutionMode)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, prj.ID); got != db.ExecutionModeLocal {
		t.Errorf("ephemeral effective mode = %q, want local even in a runtime project", got)
	}

	rt := db.ExecutionModeRuntime
	forced, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_ephemeral_forced", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "quick work forced", Status: domain.WorkItemReady,
		Ephemeral: true, ExecutionMode: &rt,
	})
	if err != nil {
		t.Fatalf("create forced ephemeral item: %v", err)
	}
	if forced.ExecutionMode == nil || *forced.ExecutionMode != db.ExecutionModeRuntime {
		t.Fatalf("forced ephemeral mode = %v, want explicit runtime", forced.ExecutionMode)
	}
}

// TestLocalItemInRuntimeProjectSkipsEnsureForRun pins AC5: an item that
// resolves to local NEVER provisions a container, even inside a runtime
// project ("local mode is the explicit opt-out and never reaches here").
func TestLocalItemInRuntimeProjectSkipsEnsureForRun(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_skip", "mode-skip", db.ExecutionModeRuntime)
	local := db.ExecutionModeLocal
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_local_in_rt", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "local in runtime", Status: domain.WorkItemReady,
		ExecutionMode: &local,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	rl := &recordingLifecycle{}
	r := &WorkflowReconciler{runtime: rl, log: slog.Default()}
	runLocalMode := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, prj.ID) == db.ExecutionModeLocal
	if !runLocalMode {
		t.Fatal("a local item inside a runtime project must resolve local (the arm gate would provision a container)")
	}
	if r.shouldEnsureRunFor(runLocalMode) {
		// Simulate the arm site: it would call EnsureForRun at this point.
		if err := r.runtime.EnsureForRun(ctx, db.WorkflowRunRow{ID: "run_should_not_exist", WorkItemID: wi.ID, ProjectID: prj.ID}); err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	if rl.ensureCalls != 0 {
		t.Fatalf("EnsureForRun called %d times for a local item, want 0", rl.ensureCalls)
	}

	// The run-level path (runExecutionMode) must agree: same resolver.
	run := db.WorkflowRunRow{ID: "run_local_item", ProjectID: prj.ID, WorkItemID: wi.ID}
	if got := runExecutionMode(ctx, ttx.Tx, approvalTestTenant, run); got != db.ExecutionModeLocal {
		t.Fatalf("runExecutionMode = %q, want local", got)
	}
	// And the prompt block agrees, so a worker is told the truth.
	if got := projectExecutionModeForPrompt(ctx, ttx.Tx, approvalTestTenant, wi); got != db.ExecutionModeLocal {
		t.Fatalf("projectExecutionModeForPrompt = %q, want local", got)
	}
}

// TestRuntimeItemInLocalProjectEnsuresContainer pins AC6: an item-level opt-IN
// inside a local project really provisions a container, and the provisioning
// opt-in is honoured — with no daemon wired the failure is LOUD, never a silent
// in-process degrade.
func TestRuntimeItemInLocalProjectEnsuresContainer(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_rt_item", "mode-rt-item", db.ExecutionModeLocal)
	rt := db.ExecutionModeRuntime
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_rt_in_local", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "runtime in local", Status: domain.WorkItemReady,
		ExecutionMode: &rt,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	rl := &recordingLifecycle{}
	r := &WorkflowReconciler{runtime: rl, log: slog.Default()}
	runLocalMode := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, prj.ID) == db.ExecutionModeLocal
	if runLocalMode {
		t.Fatal("a runtime item inside a local project must resolve runtime (it must be containerised)")
	}
	if !r.shouldEnsureRunFor(runLocalMode) {
		t.Fatal("a runtime item with a wired runtime must provision a container")
	}
	if err := r.runtime.EnsureForRun(ctx, db.WorkflowRunRow{ID: "run_rt_item", WorkItemID: wi.ID, ProjectID: prj.ID}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if rl.ensureCalls != 1 {
		t.Fatalf("EnsureForRun calls = %d, want 1", rl.ensureCalls)
	}
	if got := runExecutionMode(ctx, ttx.Tx, approvalTestTenant, db.WorkflowRunRow{ID: "run_rt_item", ProjectID: prj.ID, WorkItemID: wi.ID}); got != db.ExecutionModeRuntime {
		t.Fatalf("runExecutionMode = %q, want runtime", got)
	}

	// The daemon/Podman opt-in is HONOURED, not bypassed: headless serve has no
	// daemon to provision from, so runtime.Lifecycle fails LOUD instead of
	// silently running in-process.
	headless := runtime.NewLifecycle(nil, nil, slog.Default(), nil, nil)
	err = headless.EnsureForRun(ctx, db.WorkflowRunRow{ID: "run_rt_item", WorkItemID: wi.ID, ProjectID: prj.ID})
	if err == nil {
		t.Fatal("a runtime-mode run with no daemon must fail LOUD, not degrade to in-process")
	}
	if !strings.Contains(err.Error(), "execution_mode") {
		t.Errorf("the LOUD failure must name the fix (project or work item execution_mode); got %q", err)
	}
}

// TestProjectOnlyFallbackPreservesExistingBehaviour pins that items without an
// override behave exactly as before the override existed.
func TestProjectOnlyFallbackPreservesExistingBehaviour(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_mode_fallback", "mode-fallback", db.ExecutionModeLocal)
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: "wi_mode_fallback", TenantID: approvalTestTenant, ProjectID: prj.ID,
		Kind: domain.WorkItemKindTask, Title: "fallback", Status: domain.WorkItemReady,
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, wi.ID, prj.ID); got != db.ExecutionModeLocal {
		t.Errorf("no-override item mode = %q, want the project's local", got)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, "", prj.ID); got != db.ExecutionModeLocal {
		t.Errorf("project-only mode = %q, want local", got)
	}
	if got := executionModeFor(ctx, ttx.Tx, approvalTestTenant, "", ""); got != db.DefaultExecutionMode {
		t.Errorf("no project, no item = %q, want tenant default", got)
	}
}
