package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestExtractPRFieldsInlineMarker verifies the fallback extracts a
// PR_URL:/PR_STATE: marker glued mid-sentence (the 4.2 / 01M10WE9VWWF5GRQGZSAV57YWB
// case), where the line-anchored pass would silently drop it.
func TestExtractPRFieldsInlineMarker(t *testing.T) {
	output := "The PR was opened at PR_URL: https://github.com/OWNER/REPO/pull/42 and then PR_STATE: merged - see the run notes."
	prURL, prState := extractPRFields(output)
	if prURL != "https://github.com/OWNER/REPO/pull/42" {
		t.Errorf("inline prURL = %q, want %q", prURL, "https://github.com/OWNER/REPO/pull/42")
	}
	if prState != "merged" {
		t.Errorf("inline prState = %q, want %q", prState, "merged")
	}
}

// TestMarkerPRStateInline verifies the whole-output PR_STATE scan recovers a
// marker glued mid-sentence (used by the !verified step-success fallback).
func TestMarkerPRStateInline(t *testing.T) {
	if got := markerPRState("see PR_STATE: merged for the status"); got != "merged" {
		t.Errorf("markerPRState = %q, want %q", got, "merged")
	}
	if got := markerPRState("no marker here"); got != "" {
		t.Errorf("markerPRState no-match = %q, want empty", got)
	}
	if got := markerPRState("PR_STATE: bogus_state"); got != "" {
		t.Errorf("markerPRState invalid-state = %q, want empty", got)
	}
}

// TestDeterministicPRForBranchVerified stubs the gh binary to assert the
// verified path returns the gh-confirmed URL/state, and the unverifiable
// path (empty repo/branch) degrades to verified=false.
func TestDeterministicPRForBranchVerified(t *testing.T) {
	stubGH(t, "verify")
	url, state, verified := deterministicPRForBranch("beardedparrott/Orchicon", "some-branch")
	if !verified {
		t.Fatalf("verified=false, want true for a resolvable repo/branch")
	}
	if url != "https://github.com/OWNER/REPO/pull/777" {
		t.Errorf("url = %q, want %q", url, "https://github.com/OWNER/REPO/pull/777")
	}
	if state != "merged" {
		t.Errorf("state = %q, want %q", state, "merged")
	}
	// Empty repo/branch can never be verified.
	if u, s, v := deterministicPRForBranch("", "some-branch"); u != "" || s != "" || v {
		t.Errorf("empty repo: got (%q,%q,v=%v), want empty/empty/false", u, s, v)
	}
	if u, s, v := deterministicPRForBranch("beardedparrott/Orchicon", ""); u != "" || s != "" || v {
		t.Errorf("empty branch: got (%q,%q,v=%v), want empty/empty/false", u, s, v)
	}
}

// TestPRStepSuccessStampsVerifiedPR verifies that a succeeded PR-requiring
// step on a git_strategy=pr run with a gh-verified PR leaves its execution
// row (and run_context) with the verified pr_url/pr_state.
func TestPRStepSuccessStampsVerifiedPR(t *testing.T) {
	stubGH(t, "verify")
	pool, rc, runID, execID := seedPRStepFixture(t, "PR_URL: https://github.com/OWNER/REPO/pull/999\nPR_STATE: open\nORCHICON WORKER SUMMARY: success\n")
	// The deterministic check verifies PR 777, which must win over the marker 999.
	execRow, runRow := runPRStepSuccess(t, pool, rc, runID, execID)
	if execRow.PrURL == nil || *execRow.PrURL != "https://github.com/OWNER/REPO/pull/777" {
		t.Fatalf("exec pr_url = %v, want verified %q", prURLVal(execRow.PrURL), "https://github.com/OWNER/REPO/pull/777")
	}
	if execRow.PrState == nil || *execRow.PrState != "merged" {
		t.Fatalf("exec pr_state = %v, want %q", prStateVal(execRow.PrState), "merged")
	}
	if got, _ := db.PrFromRunContext(runRow.RunContext); got != "https://github.com/OWNER/REPO/pull/777" {
		t.Fatalf("run_context pr_url = %q, want verified %q", got, "https://github.com/OWNER/REPO/pull/777")
	}
}

// TestPRStepSuccessFallbackMarkerWhenUnverified verifies that when the
// deterministic check cannot verify (gh unavailable), the worker-marker
// fallback still captures the PR — including a marker glued mid-sentence.
func TestPRStepSuccessFallbackMarkerWhenUnverified(t *testing.T) {
	stubGH(t, "fail")
	marker := "opened the PR at PR_URL: https://github.com/OWNER/REPO/pull/555 which is PR_STATE: merged now."
	pool, rc, runID, execID := seedPRStepFixture(t, marker)
	execRow, runRow := runPRStepSuccess(t, pool, rc, runID, execID)
	if execRow.PrURL == nil || *execRow.PrURL != "https://github.com/OWNER/REPO/pull/555" {
		t.Fatalf("exec pr_url = %v, want marker %q", prURLVal(execRow.PrURL), "https://github.com/OWNER/REPO/pull/555")
	}
	if execRow.PrState == nil || *execRow.PrState != "merged" {
		t.Fatalf("exec pr_state = %v, want marker %q", prStateVal(execRow.PrState), "merged")
	}
	if got, _ := db.PrFromRunContext(runRow.RunContext); got != "https://github.com/OWNER/REPO/pull/555" {
		t.Fatalf("run_context pr_url = %q, want marker %q", got, "https://github.com/OWNER/REPO/pull/555")
	}
}

// stubGH writes a fake gh binary that either returns a fixed PR (mode
// "verify") or fails (mode "fail"), so deterministicPRForBranch can be
// driven deterministically in tests.
func stubGH(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\n" +
		"if [ \"$FAKE_GH_MODE\" = \"fail\" ]; then\n" +
		"  echo \"fatal: gh unavailable\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf 'https://github.com/OWNER/REPO/pull/777\\tmerged\\n'\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("FAKE_GH_MODE", mode)
	old := ghBin
	ghBin = script
	t.Cleanup(func() { ghBin = old })
}

// seedPRStepFixture seeds a git_strategy=pr project, a succeeded PR step
// execution, and the surrounding run/step-run/ticket, then returns the pool,
// reconciler, run id, and execution id.
func seedPRStepFixture(t *testing.T, markerOutput string) (*db.Pool, *WorkflowReconciler, string, string) {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	slug := "pr-surface-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "PR Surface Test", Slug: slug,
		Status: domain.ProjectActive, Goals: []byte("[]"),
		ProjectDir:  "/tmp/orchicon/" + slug,
		RepoSlug:    strPtr("beardedparrott/Orchicon"),
		GitStrategy: "pr",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// CreateProject does not INSERT repo_slug; set it via the git-detection
	// updater so the deterministic PR check can resolve the repo from the
	// project row.
	if _, err := db.UpdateProjectGitDetection(ctx, ttx.Tx, approvalTestTenant, proj.ID, proj.Version, false, "beardedparrott/Orchicon"); err != nil {
		t.Fatalf("set project repo_slug: %v", err)
	}
	proj, _ = db.GetProject(ctx, ttx.Tx, approvalTestTenant, proj.ID)

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title: "ticket", Description: "t", AcceptanceCriteria: "t",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		WorktreeBranch: strPtr("pr-surface-branch"),
	})
	if err != nil {
		t.Fatalf("set worktree branch: %v", err)
	}

	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, TaskID: ticket.ID,
		WorkerID: "w_se_devops_engineer", WorkerVersion: 1,
		Status: domain.ExecutionSucceeded, HealthState: domain.HealthHealthy,
		StartedAt: &now, EndedAt: &now,
		WorkflowRunID: run.ID, WorkflowStepID: "step-devops-pr",
		Output: markerOutput,
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	// CreateExecution does not INSERT the output column; the PR-marker
	// fallback scans exec.Output, so persist it explicitly.
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, exec.ID, exec.Version, db.UpdateExecutionFields{Output: &markerOutput}); err != nil {
		t.Fatalf("set execution output: %v", err)
	}
	exec, _ = db.GetExecution(ctx, ttx.Tx, approvalTestTenant, exec.ID)

	result, _ := json.Marshal(map[string]any{"_work_item_id": ticket.ID})
	_, err = db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-devops-pr",
		StepName: "DevOps PR", StepKind: domain.StepKindTask,
		Status: domain.StepRunRunning, Result: result,
		WorkerExecutionID: exec.ID,
	})
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return pool, rc, run.ID, exec.ID
}

// returns the re-read execution row and run row inside the same tx.
func runPRStepSuccess(t *testing.T, pool *db.Pool, rc *WorkflowReconciler, runID, execID string) (db.ExecutionRow, db.WorkflowRunRow) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	run, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, approvalTestTenant, runID, "step-devops-pr")
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	var triggers []recoveryTriggerReq
	terminal, failed, err := rc.pollTaskStep(ctx, ttx.Tx, approvalTestTenant, &run, sr, "",
		map[string]db.WorkflowStepRunRow{sr.StepID: sr}, &triggers)
	if err != nil {
		t.Fatalf("pollTaskStep: %v", err)
	}
	if !terminal || failed {
		t.Fatalf("pollTaskStep %v/%v, want terminal success (terminal=true, failed=false)", terminal, failed)
	}
	execRow, err := db.GetExecution(ctx, ttx.Tx, approvalTestTenant, execID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	runRow, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, runID)
	if err != nil {
		t.Fatalf("re-get run: %v", err)
	}
	return execRow, runRow
}

func prURLVal(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
func prStateVal(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
