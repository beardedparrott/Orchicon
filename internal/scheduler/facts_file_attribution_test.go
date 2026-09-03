package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestFactsFileCarriesStepAttribution verifies the acceptance criterion for
// facts dedup: after the embedded "## Facts learned (this run)" prompt block
// was removed, the .orchicon/<run>/facts_learned file must record the
// originating step per fact (e.g. `FACTS LEARNED (from <step-name>): <fact>`)
// so downstream workers keep the same attribution. DB-backed (needs a real
// workflow_step_runs row keyed by worker_execution_id); skipped without
// ORCHICON_TEST_DSN.
func TestFactsFileCarriesStepAttribution(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	root := t.TempDir()
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "Facts file",
		Slug: "facts-file-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "facts attribution", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-facts", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	step := workflow.StepWire{ID: "step-swe", Name: "Senior Software Engineer", Kind: domain.StepKindTask}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: step.ID,
		StepName: step.Name, StepKind: step.Kind,
		Status: domain.StepRunRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Tie the step run to an execution (worker_execution_id).
	const execID = "exec-facts-test"
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE workflow_step_runs SET worker_execution_id = $1
		  WHERE id = $2 AND tenant_id = $3`, execID, sr.ID, approvalTestTenant); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := &TaskReconciler{pool: pool, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	exec := db.ExecutionRow{
		ID: execID, TenantID: approvalTestTenant, ProjectID: proj.ID,
		TaskID: item.ID, WorkflowRunID: run.ID, WorkflowStepID: step.ID,
		WorkerID: "w_se_senior_software_engineer", Status: "succeeded",
	}
	results := map[string]any{
		"_summary": "Implemented the thing.\nFACTS LEARNED: the first established fact.\nFACTS LEARNED: a second established fact.\n",
	}
	r.writeOrchiconFiles(ctx, exec, item, true, results)

	b, err := os.ReadFile(filepath.Join(root, ".orchicon", run.ID, "facts_learned"))
	if err != nil {
		t.Fatalf("read facts_learned: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "FACTS LEARNED (from Senior Software Engineer): the first established fact.") {
		t.Errorf("facts_learned missing step attribution; got:\n%s", got)
	}
	if !strings.Contains(got, "FACTS LEARNED (from Senior Software Engineer): a second established fact.") {
		t.Errorf("facts_learned missing step attribution on second fact; got:\n%s", got)
	}
	// The summary itself is capped but facts must still be extractable and the
	// summary file must exist.
	if sb, err := os.ReadFile(filepath.Join(root, ".orchicon", run.ID, "summary")); err != nil {
		t.Errorf("summary file missing: %v", err)
	} else if !strings.Contains(string(sb), "Implemented the thing.") {
		t.Errorf("summary file missing content: %q", string(sb))
	}
}

// TestFactsFileExtractsFromTranscriptOnTerminal verifies the deterministic
// terminal-time backstop: an execution that terminaled with NO emitted
// FACTS LEARNED summary (e.g. stall-killed) still has facts folded into
// .orchicon/<run>/facts_learned from its persisted execution_session_parts
// transcript by writeOrchiconFiles. DB-backed; skipped without
// ORCHICON_TEST_DSN.
func TestFactsFileExtractsFromTranscriptOnTerminal(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	root := t.TempDir()
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "Facts transcript terminal",
		Slug: "facts-term-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "stalled facts", Status: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-facts-term", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	step := workflow.StepWire{ID: "step-swe", Name: "Pragmatic Engineer", Kind: domain.StepKindTask}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: step.ID,
		StepName: step.Name, StepKind: step.Kind,
		Status: domain.StepRunRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	const execID = "exec-stalled"
	if _, err := ttx.Tx.Exec(ctx,
		`UPDATE workflow_step_runs SET worker_execution_id = $1
		  WHERE id = $2 AND tenant_id = $3`, execID, sr.ID, approvalTestTenant); err != nil {
		t.Fatal(err)
	}
	// Seed the transcript: recon, a tool use, then a stalled FINAL assistant
	// block carrying a FACTS LEARNED under part.text (the shape the
	// transcript renderer + extractor read).
	finalPart, _ := json.Marshal(map[string]any{
		"text": "",
		"part": map[string]any{"text": "all groundwork before the stall\nFACTS LEARNED: the stall hides the backend here."},
	})
	parts := []db.SessionPart{
		{ExecutionID: execID, TenantID: approvalTestTenant, Seq: 1,
			Kind: db.SessionPartText, Payload: []byte(`{"text":"recon grind","part":{"text":"recon grind"}}`)},
		{ExecutionID: execID, TenantID: approvalTestTenant, Seq: 2,
			Kind: db.SessionPartToolUse, Payload: []byte(`{"text":"","part":{"tool":"bash"}}`)},
		{ExecutionID: execID, TenantID: approvalTestTenant, Seq: 3,
			Kind: db.SessionPartText, Payload: finalPart},
	}
	if err := db.AppendExecutionSessionParts(ctx, ttx.Tx, approvalTestTenant, parts); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := &TaskReconciler{pool: pool, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	exec := db.ExecutionRow{
		ID: execID, TenantID: approvalTestTenant, ProjectID: proj.ID,
		TaskID: item.ID, WorkflowRunID: run.ID, WorkflowStepID: step.ID,
		WorkerID: "w_se_engineer", Status: "failed",
	}
	// No _summary: the worker emitted nothing before dying.
	r.writeOrchiconFiles(ctx, exec, item, false, map[string]any{})

	b, err := os.ReadFile(filepath.Join(root, ".orchicon", run.ID, "facts_learned"))
	if err != nil {
		t.Fatalf("read facts_learned: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "FACTS LEARNED (from Pragmatic Engineer): the stall hides the backend here.") {
		t.Errorf("transcript fact not extracted; got:\n%s", got)
	}
}
