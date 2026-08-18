package scheduler

// Concurrent step-run dispatch tests (architecture-notes/
// concurrent-step-run-dispatch.md): parallel-branch children of a
// `parallel` step dispatch CONCURRENTLY, each in its own branch worktree,
// while gates/loop decisions still fan in on ALL inputs and a failed
// branch never smears a still-running sibling. DB-backed tests are
// skipped unless ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestParallelBranch|TestDispatchInline|TestSetDispatchConcurrency|TestBranchWorktree|TestBranchExecutionCwd|TestD4' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// --- unit tests (no DB) -----------------------------------------------------

// TestParallelBranchChildIDs verifies the DAG-shape helper: only steps whose
// depends_on references a `parallel` step are branch children; the parallel
// marker and serial steps are not.
func TestParallelBranchChildIDs(t *testing.T) {
	steps := []workflow.StepWire{
		{ID: "sse", Kind: "task"},
		{ID: "par", Kind: "parallel", DependsOn: []string{"sse"}},
		{ID: "branch-a", Kind: "task", DependsOn: []string{"par"}},
		{ID: "branch-b", Kind: "task", DependsOn: []string{"par"}},
		{ID: "fan-in", Kind: "loop_decision", DependsOn: []string{"branch-a", "branch-b"}},
		{ID: "serial", Kind: "task", DependsOn: []string{"sse"}},
	}
	got := parallelBranchChildIDs(steps)
	for _, id := range []string{"branch-a", "branch-b"} {
		if !got[id] {
			t.Errorf("parallelBranchChildIDs(%s) = false, want true (parallel-branch child)", id)
		}
	}
	for _, id := range []string{"sse", "par", "fan-in", "serial"} {
		if got[id] {
			t.Errorf("parallelBranchChildIDs(%s) = true, want false (not a branch child)", id)
		}
	}
}

// barrierDispatcher is a TaskDispatcher stub that waits until `expected`
// dispatches are all IN FLIGHT before any returns, so tests can assert
// bounded concurrency without timing flakes.
type barrierDispatcher struct {
	mu       sync.Mutex
	expected int
	calls    []string
	done     chan struct{}
}

func (d *barrierDispatcher) DispatchTask(_ context.Context, _ string, stepRunID string) error {
	d.mu.Lock()
	d.calls = append(d.calls, stepRunID)
	if len(d.calls) == d.expected {
		close(d.done)
	}
	d.mu.Unlock()
	<-d.done
	return nil
}

func (d *barrierDispatcher) stepRuns() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// newInFlightPeak attaches the test-only dispatchOverlap hook to a
// reconciler and returns a reader for the peak number of DispatchTask calls
// observed in flight during a fan-out.
func newInFlightPeak(rec *WorkflowReconciler) func() int {
	var mu sync.Mutex
	peak := 0
	rec.dispatchOverlap = func(cur int) {
		mu.Lock()
		defer mu.Unlock()
		if cur > peak {
			peak = cur
		}
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

// TestDispatchInlineConcurrent is the D1 acceptance test: the post-commit
// inline fan-out runs independent dispatches CONCURRENTLY (all in flight at
// once) and waits for all of them.
func TestDispatchInlineConcurrent(t *testing.T) {
	rec := &WorkflowReconciler{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	const n = 3
	rec.taskDispatcher = &barrierDispatcher{expected: n, done: make(chan struct{})}
	rec.dispatchConcurrency = 4
	peak := newInFlightPeak(rec)

	steps := make([]dispatchReq, n)
	for i := range steps {
		steps[i] = dispatchReq{taskID: fmt.Sprintf("t%d", i), stepRunID: fmt.Sprintf("sr%d", i)}
	}
	rec.dispatchInline(context.Background(), steps)

	if p := peak(); p < 2 {
		t.Errorf("peak in-flight dispatches = %d, want >= 2 (concurrent fan-out)", p)
	}
}

// instantDispatcher records calls without blocking (used where a barrier
// would deadlock, e.g. asserting a serializing bound).
type instantDispatcher struct {
	mu    sync.Mutex
	calls []string
}

func (d *instantDispatcher) DispatchTask(_ context.Context, _ string, stepRunID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, stepRunID)
	return nil
}

func (d *instantDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// TestDispatchInlineRespectsBound verifies the fan-out is bounded: with the
// limit at 1, concurrent dispatches serialize and peak in-flight is exactly 1.
func TestDispatchInlineRespectsBound(t *testing.T) {
	rec := &WorkflowReconciler{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	const n = 5
	rec.taskDispatcher = &instantDispatcher{}
	rec.SetDispatchConcurrency(1)
	var mu sync.Mutex
	peak := 0
	rec.dispatchOverlap = func(cur int) {
		mu.Lock()
		defer mu.Unlock()
		if cur > peak {
			peak = cur
		}
	}

	steps := make([]dispatchReq, n)
	for i := range steps {
		steps[i] = dispatchReq{taskID: "t", stepRunID: fmt.Sprintf("sr%d", i)}
	}
	rec.dispatchInline(context.Background(), steps)

	if p := peak; p != 1 {
		t.Errorf("peak in-flight dispatches = %d, want 1 (bound serializes)", p)
	}
	if got := rec.taskDispatcher.(*instantDispatcher).count(); got != n {
		t.Errorf("dispatchInline dispatched %d, want %d", got, n)
	}
}

// TestWorkflowSetDispatchConcurrencyClamps verifies the WorkflowReconciler's
// setter clamps to [1, 64]; zero falls back to the default at fan-out time.
func TestWorkflowSetDispatchConcurrencyClamps(t *testing.T) {
	rec := &WorkflowReconciler{}
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, 1}, {-5, 1}, {1000, 64}, {6, 6}, {64, 64},
	} {
		rec.SetDispatchConcurrency(tc.in)
		if got := rec.dispatchConcurrency; got != tc.want {
			t.Errorf("SetDispatchConcurrency(%d) → %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := (&WorkflowReconciler{}).dispatchInlineLimit(3); got != 3 {
		t.Errorf("unset dispatchInlineLimit(3) = %d, want 3 (default 4 clamped to the batch)", got)
	}
	if got := (&WorkflowReconciler{}).dispatchInlineLimit(10); got != defaultDispatchConcurrency {
		t.Errorf("unset dispatchInlineLimit(10) = %d, want default %d", got, defaultDispatchConcurrency)
	}
	if got := (&WorkflowReconciler{dispatchConcurrency: 2}).dispatchInlineLimit(3); got != 2 {
		t.Errorf("dispatchInlineLimit = %d, want 2", got)
	}
	if got := (&WorkflowReconciler{dispatchConcurrency: 8}).dispatchInlineLimit(3); got != 3 {
		t.Errorf("dispatchInlineLimit = %d, want 3 (clamped to the batch)", got)
	}
}

// TestBranchWorktreeName verifies the branch-worktree branch name: a
// git-safe sub-branch of the run branch, unique PER STEP RUN (loop
// iterations re-create step runs for the same step), with the step ID
// segment slugified so user-authored (git-ref-invalid) IDs never break
// `git worktree add`.
func TestBranchWorktreeName(t *testing.T) {
	r := &WorktreeReconciler{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	run := db.WorkflowRunRow{ID: "01J1234567ABCDEFGHJKLMNPQR", WorktreeBranch: "refactor-export-pipeline-abcd1234"}
	sr := db.WorkflowStepRunRow{ID: "01J1234567ABCDEFGHJKLMNPQRSTUVWXY", StepID: "step-pr-reviewer"}

	got := r.branchWorktreeName(context.Background(), "tnt_dev", run, sr)
	if !strings.HasPrefix(got, run.WorktreeBranch+"-") {
		t.Errorf("branch %q does not carry the run branch prefix", got)
	}
	if !strings.Contains(got, "step-pr-reviewer") {
		t.Errorf("branch %q lost its step identity", got)
	}
	if !strings.HasSuffix(got, "-"+strings.ToLower(sr.ID[10:])) {
		t.Errorf("branch %q lost its step-run suffix (must be unique per step RUN)", got)
	}
	if len(got) >= 243 {
		t.Errorf("branch %q is too long (%d) for git's ref limit", got, len(got))
	}

	// A user-authored, git-ref-invalid step ID must be sanitized.
	bad := db.WorkflowStepRunRow{ID: sr.ID, StepID: "step:weird [id]..suffix/"}
	if b := r.branchWorktreeName(context.Background(), "tnt_dev", run, bad); strings.ContainsAny(b, ` ~^:?*[\`) || strings.Contains(b, "..") {
		t.Errorf("branch %q contains git-ref-forbidden characters", b)
	}

	// Two step runs for the SAME step (loop iterations) must never collide.
	sr2 := db.WorkflowStepRunRow{ID: "01J1234567ZYXWVUTSRQPONMLKJIHGF", StepID: "step-pr-reviewer"}
	a := r.branchWorktreeName(context.Background(), "tnt_dev", run, sr)
	b := r.branchWorktreeName(context.Background(), "tnt_dev", run, sr2)
	if a == b {
		t.Errorf("loop-iteration step runs of the same step collided on branch %q", a)
	}

	// A maximal run branch + maximal step slug still fits the ref limit.
	longRun := db.WorkflowRunRow{ID: run.ID, WorktreeBranch: strings.Repeat("a-very-long-slug-component-", 12) + strings.Repeat("x", 50)}
	longSR := db.WorkflowStepRunRow{ID: sr.ID, StepID: strings.Repeat("y", 120)}
	if l := r.branchWorktreeName(context.Background(), "tnt_dev", longRun, longSR); len(l) >= 243 {
		t.Errorf("long branch %q is %d bytes — git refs cap at 243", l, len(l))
	}
}

// --- shared DB + git fixture -------------------------------------------------

// parallelBranchDAG is the canonical concurrent fan-out shape:
// SSE → Parallel → {branch-a, branch-b} → Loop Decision (fan-in).
func parallelBranchDAG() []workflow.StepWire {
	return []workflow.StepWire{
		{ID: "step-sse", Name: "Senior Software Engineer", Kind: "task", Ref: "w_se_senior_software_engineer",
			Config: `{"recovery":{"strategy":"retry","max_attempts":1}}`},
		{ID: "step-parallel", Name: "Parallel", Kind: "parallel", DependsOn: []string{"step-sse"}},
		{ID: "step-branch-a", Name: "PR Reviewer", Kind: "task", Ref: "w_se_pr_reviewer",
			DependsOn: []string{"step-parallel"}, Config: `{"recovery":{"strategy":"retry","max_attempts":1}}`},
		{ID: "step-branch-b", Name: "QA Engineer", Kind: "task", Ref: "w_se_qa_engineer",
			DependsOn: []string{"step-parallel"}, Config: `{"recovery":{"strategy":"retry","max_attempts":1}}`},
		{ID: "step-loop", Name: "Loop Decision", Kind: "loop_decision",
			DependsOn: []string{"step-branch-a", "step-branch-b"}, Config: `{"max_iterations":3,"loop_branch":"step-sse"}`},
	}
}

// branchDispatchEnv is the shared fixture for the concurrent-dispatch
// integration tests: a git-backed project, a published workflow version with
// a parallel fan-out, a bound ticket, a running run whose SSE + Parallel
// steps are already terminal-success and whose two branch steps are READY
// (so a single reconcileRun pass dispatches them), plus the reconcilers.
type branchDispatchEnv struct {
	t          *testing.T
	pool       *db.Pool
	repo       string
	proj       db.ProjectRow
	ticketID   string
	run        db.WorkflowRunRow
	rec        *WorkflowReconciler
	wrec       *WorktreeReconciler
	stepRuns   map[string]db.WorkflowStepRunRow // stepID → row
	notifierMu sync.Mutex
	notifier   []string
}

func newBranchDispatchEnv(t *testing.T) *branchDispatchEnv {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := &branchDispatchEnv{t: t, pool: pool,
		rec:      NewWorkflowReconciler(pool, logger, nil, nil, nil, nil),
		wrec:     NewWorktreeReconciler(pool, logger),
		stepRuns: map[string]db.WorkflowStepRunRow{},
	}
	env.rec.SetWorktreeNotifier(func(_ context.Context, key string) {
		env.notifierMu.Lock()
		defer env.notifierMu.Unlock()
		env.notifier = append(env.notifier, key)
	})
	env.repo = newTestRepo(t)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Concurrent Dispatch Project", Slug: "concurrent-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"), ProjectDir: env.repo,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env.proj = proj

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Concurrent branch dispatch ticket",
		Description: "the shared input reference", AcceptanceCriteria: "branches run in parallel",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	env.ticketID = ticket.ID

	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "Concurrent Branch Workflow",
		CurrentVersion: 1, Status: "published", Type: "template",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	stepsJSON, _ := json.Marshal(parallelBranchDAG())
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, VersionNote: "parallel fan-out", Status: "published",
		Steps: stepsJSON, Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}

	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: 1, ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("set runtime_ready: %v", err)
	}
	env.run = run

	// Bind the ticket to the run the way StartWorkflow does: startExecution
	// resolves the run (and the step run's worktree) off task.WorkflowRunID.
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, ticket.ID, ticket.Version, db.UpdateWorkItemFields{
		WorkflowRunID: &run.ID,
	}); err != nil {
		t.Fatalf("bind ticket to run: %v", err)
	}

	// Seed step runs. SSE + Parallel are pre-completed so a single pass
	// reaches the branches; the branch steps are seeded READY (deps
	// satisfied) so the pass dispatches exactly them.
	seedStatus := map[string]string{
		"step-sse":      domain.StepRunSucceeded,
		"step-parallel": domain.StepRunSucceeded,
		"step-branch-a": domain.StepRunReady,
		"step-branch-b": domain.StepRunReady,
		"step-loop":     domain.StepRunPending,
	}
	for _, s := range parallelBranchDAG() {
		sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
			StepID: s.ID, StepName: s.Name, StepKind: s.Kind,
			Status: seedStatus[s.ID], Result: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create step run %s: %v", s.ID, err)
		}
		env.stepRuns[s.ID] = sr
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return env
}

func (e *branchDispatchEnv) getStepRun(t *testing.T, stepID string) db.WorkflowStepRunRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := e.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("get step run: begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, e.stepRuns[stepID].ID)
	if err != nil {
		t.Fatalf("get step run %s: %v", stepID, err)
	}
	return sr
}

func (e *branchDispatchEnv) notifierKeys() []string {
	e.notifierMu.Lock()
	defer e.notifierMu.Unlock()
	return append([]string(nil), e.notifier...)
}

func (e *branchDispatchEnv) branchPath(stepID string) string {
	return filepath.Join(e.repo, worktreeDirName, e.run.ID, e.stepRuns[stepID].ID)
}

// TestParallelBranchHeldUntilWorktreeReady is the D2 readiness-gate test:
// a parallel-branch child stays READY (never dispatches) until the
// WorktreeReconciler provisions its OWN branch worktree; the branch
// notifier fires post-commit with the "<runID>:<stepRunID>" composite key;
// and once provisioned, both branches dispatch CONCURRENTLY in one pass.
func TestParallelBranchHeldUntilWorktreeReady(t *testing.T) {
	env := newBranchDispatchEnv(t)
	ctx := context.Background()

	// Pass 1: branches are ready but their worktrees are pending → held.
	barrier := &barrierDispatcher{done: make(chan struct{})}
	env.rec.taskDispatcher = barrier
	if err := env.rec.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (held pass): %v", err)
	}
	if calls := barrier.stepRuns(); len(calls) != 0 {
		t.Fatalf("branches dispatched before their worktrees were ready: %v", calls)
	}
	for _, stepID := range []string{"step-branch-a", "step-branch-b"} {
		if got := env.getStepRun(t, stepID).Status; got != domain.StepRunReady {
			t.Errorf("branch %s status = %q, want ready (held by the gate)", stepID, got)
		}
	}
	keys := env.notifierKeys()
	if len(keys) != 2 {
		t.Fatalf("branch notifier fired %d keys, want 2 (one per held branch): %v", len(keys), keys)
	}
	for _, stepID := range []string{"step-branch-a", "step-branch-b"} {
		want := env.run.ID + ":" + env.stepRuns[stepID].ID
		found := false
		for _, k := range keys {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("branch notifier missing key %q (got %v)", want, keys)
		}
	}

	// Provision the run + both branch worktrees.
	if res := env.wrec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("provision worktrees: %v", res.Error)
	}
	for _, stepID := range []string{"step-branch-a", "step-branch-b"} {
		sr := env.getStepRun(t, stepID)
		if sr.WorktreeStatus != domain.WorktreeReady {
			t.Fatalf("branch %s worktree_status = %q, want ready", stepID, sr.WorktreeStatus)
		}
		if _, err := os.Stat(env.branchPath(stepID)); err != nil {
			t.Fatalf("branch worktree for %s missing at %s: %v", stepID, env.branchPath(stepID), err)
		}
	}

	// Pass 2: both branches dispatch CONCURRENTLY in a single pass.
	barrier2 := &barrierDispatcher{expected: 2, done: make(chan struct{})}
	env.rec.taskDispatcher = barrier2
	env.rec.SetDispatchConcurrency(4)
	peak := newInFlightPeak(env.rec)
	if err := env.rec.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (dispatch pass): %v", err)
	}
	calls := barrier2.stepRuns()
	if len(calls) != 2 {
		t.Fatalf("branches dispatched %d step runs, want 2: %v", len(calls), calls)
	}
	if p := peak(); p < 2 {
		t.Errorf("peak in-flight branch dispatches = %d, want >= 2 (concurrent)", p)
	}
	for _, stepID := range []string{"step-branch-a", "step-branch-b"} {
		if got := env.getStepRun(t, stepID).Status; got != domain.StepRunRunning {
			t.Errorf("branch %s status = %q, want running after dispatch", stepID, got)
		}
	}
}

// TestBranchWorktreeDistinctPerStepRun is the AC1 filesystem-isolation
// test: each parallel-branch child records its OWN worktree path + branch
// (never shared, never the run worktree), and the branches are distinct.
func TestBranchWorktreeDistinctPerStepRun(t *testing.T) {
	env := newBranchDispatchEnv(t)
	ctx := context.Background()
	if res := env.wrec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("provision worktrees: %v", res.Error)
	}
	run := env.run
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	run, err = db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}

	srA := env.getStepRun(t, "step-branch-a")
	srB := env.getStepRun(t, "step-branch-b")
	if srA.WorktreeStatus != domain.WorktreeReady || srB.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("branch worktrees not ready: a=%s b=%s", srA.WorktreeStatus, srB.WorktreeStatus)
	}
	if srA.WorktreePath == srB.WorktreePath {
		t.Errorf("branches share worktree path %q — must be one per step run", srA.WorktreePath)
	}
	if srA.WorktreeBranch == srB.WorktreeBranch {
		t.Errorf("branches share worktree branch %q — git forbids two worktrees on one branch", srA.WorktreeBranch)
	}
	if srA.WorktreePath == run.WorktreePath || srB.WorktreePath == run.WorktreePath {
		t.Errorf("a branch worktree collides with the run worktree %q", run.WorktreePath)
	}
	wantA := filepath.Join(env.repo, worktreeDirName, env.run.ID, srA.ID)
	if srA.WorktreePath != wantA {
		t.Errorf("branch-a path = %q, want %q", srA.WorktreePath, wantA)
	}
	for _, branch := range []string{srA.WorktreeBranch, srB.WorktreeBranch} {
		if !strings.HasPrefix(branch, run.WorktreeBranch+"-") {
			t.Errorf("branch %q does not carry the run branch prefix %q", branch, run.WorktreeBranch)
		}
	}
	// Non-branch steps never get a branch worktree.
	for _, stepID := range []string{"step-sse", "step-parallel", "step-loop"} {
		if got := env.getStepRun(t, stepID).WorktreeStatus; got != domain.WorktreePending {
			t.Errorf("non-branch step %s worktree_status = %q, want pending (no branch worktree)", stepID, got)
		}
	}
}

// TestBranchWorktreePrunedAtStepRunTerminal verifies the D2 lifecycle: when
// a branch step run is terminal, the scan reaps its worktree (path cleared,
// branch retained) and never touches the run worktree or live branches.
func TestBranchWorktreePrunedAtStepRunTerminal(t *testing.T) {
	env := newBranchDispatchEnv(t)
	ctx := context.Background()
	if res := env.wrec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("provision worktrees: %v", res.Error)
	}
	if _, err := os.Stat(env.branchPath("step-branch-a")); err != nil {
		t.Fatalf("branch-a worktree missing before prune: %v", err)
	}

	// branch-a terminal, branch-b still live.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	srA := env.getStepRun(t, "step-branch-a")
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, srA.ID, srA.Version, db.UpdateWorkflowStepRunFields{
		Status: strPtr(domain.StepRunSucceeded),
	}); err != nil {
		t.Fatalf("mark branch-a succeeded: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if res := env.wrec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("prune pass: %v", res.Error)
	}
	srA = env.getStepRun(t, "step-branch-a")
	if srA.WorktreeStatus != domain.WorktreePruned {
		t.Errorf("branch-a worktree_status = %q, want pruned", srA.WorktreeStatus)
	}
	if srA.WorktreePath != "" {
		t.Errorf("branch-a worktree_path = %q, want empty after prune", srA.WorktreePath)
	}
	if srA.WorktreeBranch == "" {
		t.Errorf("branch-a worktree_branch was cleared; branch deletion stays with the DevOps merge step")
	}
	if _, err := os.Stat(env.branchPath("step-branch-a")); !os.IsNotExist(err) {
		t.Errorf("branch-a worktree dir still exists after prune")
	}
	// The live sibling's worktree survives.
	srB := env.getStepRun(t, "step-branch-b")
	if srB.WorktreeStatus != domain.WorktreeReady {
		t.Errorf("live branch-b worktree_status = %q, want ready (not pruned)", srB.WorktreeStatus)
	}
	if _, err := os.Stat(env.branchPath("step-branch-b")); err != nil {
		t.Errorf("live branch-b worktree was pruned: %v", err)
	}
}

// TestBranchExecutionCwd verifies D2's cwd wiring: a branch execution
// resolves its working directory from the STEP RUN's own branch worktree
// (manifest.WorktreePath = branch path), and the execution row carries the
// branch worktree state — not the run worktree's.
func TestBranchExecutionCwd(t *testing.T) {
	env := newBranchDispatchEnv(t)
	ctx := context.Background()
	if res := env.wrec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("provision worktrees: %v", res.Error)
	}
	srA := env.getStepRun(t, "step-branch-a")

	// Stamp the step run the way dispatchStep would, then dispatch via the
	// REAL TaskReconciler so the execution row + startExecution cwd both
	// go through production paths.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	result, _ := json.Marshal(map[string]any{
		"_work_item_id":   env.ticketID,
		"_prompt":         "composite prompt",
		"_worker_id":      "w_se_pr_reviewer",
		"_worker_version": 1,
	})
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, srA.ID, srA.Version, db.UpdateWorkflowStepRunFields{
		Result: &result,
	}); err != nil {
		t.Fatalf("stamp step run: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.CreateAdapter(ctx, ttx.Tx, db.AdapterRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready",
		MaxConcurrentExecutions: 64, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	bridge := &manifestCaptureBridge{}
	taskRec := NewTaskReconciler(env.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), bridge)
	if err := taskRec.reconcileOne(ctx, env.ticketID, srA.ID); err != nil {
		t.Fatalf("reconcileOne (branch dispatch): %v", err)
	}

	// The execution row carries the branch worktree state.
	execs := executionsForStepRun(t, env.pool, srA.ID)
	if len(execs) != 1 {
		t.Fatalf("branch-a produced %d executions, want 1", len(execs))
	}
	exec := execs[0]
	if exec.WorktreeStatus == nil || *exec.WorktreeStatus != domain.WorktreeReady {
		t.Errorf("execution worktree_status = %v, want ready", exec.WorktreeStatus)
	}
	if exec.WorktreePath == nil || *exec.WorktreePath != srA.WorktreePath {
		t.Errorf("execution worktree_path = %v, want branch-a path %q", exec.WorktreePath, srA.WorktreePath)
	}
	if exec.WorktreeBranch == nil || *exec.WorktreeBranch != srA.WorktreeBranch {
		t.Errorf("execution worktree_branch = %v, want branch-a branch %q", exec.WorktreeBranch, srA.WorktreeBranch)
	}

	// The manifest cwd is the branch worktree (startExecution runs in a
	// goroutine — poll briefly for the manifest).
	deadline := time.Now().Add(5 * time.Second)
	for {
		bridge.mu.Lock()
		n := len(bridge.manifests)
		bridge.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startExecution produced %d manifests, want 1", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	man := bridge.manifests[0]
	if man.WorktreePath != srA.WorktreePath {
		t.Errorf("manifest.WorktreePath = %q, want branch-a worktree %q", man.WorktreePath, srA.WorktreePath)
	}
	if man.ProjectDir != env.repo {
		t.Errorf("manifest.ProjectDir = %q, want %q (project root stays the mount root)", man.ProjectDir, env.repo)
	}
}

func executionsForStepRun(t *testing.T, pool *db.Pool, stepRunID string) []db.ExecutionRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, stepRunID)
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	if sr.WorkerExecutionID == "" {
		return nil
	}
	exec, err := db.GetExecution(ctx, ttx.Tx, approvalTestTenant, sr.WorkerExecutionID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	return []db.ExecutionRow{exec}
}

// TestD4FailedBranchDoesNotSmearRunningSibling is the AC4 test: when one
// branch fails while a sibling is still RUNNING, the run stays running and
// the sibling is never smeared with a shared skipped mark — it reaches its
// OWN terminal mark, and only then does the run fail. A not-yet-dispatched
// (pending) step IS still skipped so the failure surfaces immediately.
func TestD4FailedBranchDoesNotSmearRunningSibling(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rec := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "D4 Project", Slug: "d4-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"), ProjectDir: nonRepoDir(t),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "D4 shared ticket",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "D4 Workflow",
		CurrentVersion: 1, Status: "published", Type: "template",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	steps := []workflow.StepWire{
		{ID: "step-a", Name: "Failed Branch", Kind: "task", Ref: "w_se_pr_reviewer"},
		{ID: "step-b", Name: "Running Sibling", Kind: "task", Ref: "w_se_qa_engineer"},
		{ID: "step-c", Name: "Pending Downstream", Kind: "task", Ref: "w_se_devops_engineer", DependsOn: []string{"step-a"}},
	}
	stepsJSON, _ := json.Marshal(steps)
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, VersionNote: "D4", Status: "published",
		Steps: stepsJSON, Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatalf("create version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: 1, ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("set runtime_ready: %v", err)
	}

	// step-a: terminal FAILED.
	srA, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-a", StepName: "Failed Branch", StepKind: "task",
		Status: domain.StepRunFailed, Result: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create step-a: %v", err)
	}
	// step-b: RUNNING with a linked running execution.
	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		TaskID: ticket.ID, Status: domain.ExecutionRunning,
		HealthState: domain.HealthHealthy, StartedAt: &now,
		WorkflowRunID: run.ID, WorkflowStepID: "step-b",
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	srB, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-b", StepName: "Running Sibling", StepKind: "task",
		Status: domain.StepRunRunning, WorkerExecutionID: exec.ID,
		Result: func() []byte { r, _ := json.Marshal(map[string]string{"_work_item_id": ticket.ID}); return r }(),
	})
	if err != nil {
		t.Fatalf("create step-b: %v", err)
	}
	// step-c: PENDING, blocked on the failed step-a.
	srC, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-c", StepName: "Pending Downstream", StepKind: "task",
		Status: domain.StepRunPending, Result: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create step-c: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	_ = srA

	getRun := func() db.WorkflowRunRow {
		t.Helper()
		ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
		if err != nil {
			t.Fatal(err)
		}
		defer ttx2.Rollback(ctx)
		r, err := db.GetWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	getSR := func(id string) db.WorkflowStepRunRow {
		t.Helper()
		ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
		if err != nil {
			t.Fatal(err)
		}
		defer ttx2.Rollback(ctx)
		sr, err := db.GetWorkflowStepRun(ctx, ttx2.Tx, approvalTestTenant, id)
		if err != nil {
			t.Fatal(err)
		}
		return sr
	}

	// Pass 1: the failed branch + the running sibling coexist; the run is
	// NOT failed yet and the RUNNING sibling is NOT skipped.
	if err := rec.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	if got := getSR(srB.ID).Status; got != domain.StepRunRunning {
		t.Errorf("running sibling status = %q, want running (not smeared)", got)
	}
	if got := getSR(srC.ID).Status; got != domain.StepRunSkipped {
		t.Errorf("pending downstream status = %q, want skipped (fail-fast for not-yet-dispatched)", got)
	}
	if got := getRun().Status; got != domain.WorkflowRunRunning {
		t.Errorf("run status = %q, want running (waiting for the in-flight sibling)", got)
	}

	// Pass 2: the sibling reaches its OWN terminal mark → the run fails.
	ttx, err = pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, exec.ID, exec.Version, db.UpdateExecutionFields{
		Status:  strPtr(domain.ExecutionSucceeded),
		EndedAt: &now,
	}); err != nil {
		t.Fatalf("complete sibling execution: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := rec.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	if got := getSR(srB.ID).Status; got != domain.StepRunSucceeded {
		t.Errorf("running sibling status = %q, want succeeded (its OWN terminal mark)", got)
	}
	if got := getRun().Status; got != domain.WorkflowRunFailed {
		t.Errorf("run status = %q, want failed once all branches are terminal", got)
	}
}
