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
	"github.com/jackc/pgx/v5/pgxpool"
)

func recoveryResultJSON(summary, execID, workerID string) []byte {
	m := map[string]any{}
	if summary != "" {
		m["_recovery_summary"] = summary
	}
	if execID != "" {
		m["_recovery_execution_id"] = execID
	}
	if workerID != "" {
		m["_recovery_worker_id"] = workerID
	}
	b, _ := json.Marshal(m)
	return b
}

// TestRecoverySeedForMatch verifies the predicate returns a seed for a
// same-worker recovery-resumed dispatch, reading keys from either the
// step-run result (workflow) or the work item results (standalone).
func TestRecoverySeedForMatch(t *testing.T) {
	runResult := recoveryResultJSON("exec abc failed", "exec-1", "w1")
	seed := recoverySeedFor(runResult, nil, "w1")
	if seed == nil {
		t.Fatal("same-worker workflow seed should be non-nil")
	}
	if seed.FailedExecID != "exec-1" || seed.FailedWorkerID != "w1" || seed.Summary != "exec abc failed" {
		t.Errorf("seed fields wrong: %+v", seed)
	}

	// Standalone: keys in the work item's Results.
	seed = recoverySeedFor(nil, runResult, "w1")
	if seed == nil {
		t.Fatal("same-worker standalone seed should be non-nil")
	}
}

// TestRecoverySeedForNoMatch verifies the predicate is nil for a
// different worker, missing keys, or a fresh (non-recovery) dispatch.
func TestRecoverySeedForNoMatch(t *testing.T) {
	runResult := recoveryResultJSON("exec abc failed", "exec-1", "w1")

	if seed := recoverySeedFor(runResult, nil, "w2"); seed != nil {
		t.Error("different worker must not get a seed")
	}
	if seed := recoverySeedFor(nil, nil, "w1"); seed != nil {
		t.Error("no recovery keys must not get a seed")
	}
	if seed := recoverySeedFor([]byte(`{}`), nil, "w1"); seed != nil {
		t.Error("empty result must not get a seed")
	}
	if seed := recoverySeedFor(recoveryResultJSON("", "exec-1", "w1"), nil, "w1"); seed != nil && seed.FailedExecID != "exec-1" {
		t.Error("exec+worker keys alone must still seed (summary is optional)")
	}
	// Missing exec id → no seed (cannot locate the dead session).
	if seed := recoverySeedFor(recoveryResultJSON("summary only", "", "w1"), nil, "w1"); seed != nil {
		t.Error("missing execution id must not get a seed")
	}
	// Missing worker id → no seed.
	if seed := recoverySeedFor(recoveryResultJSON("summary only", "exec-1", ""), nil, "w1"); seed != nil {
		t.Error("missing worker id must not get a seed")
	}
}

// TestEnsureRecoveryFileReference verifies the append is idempotent and
// only applies when a seed exists.
func TestEnsureRecoveryFileReference(t *testing.T) {
	seed := &recoverySeed{Summary: "s", FailedExecID: "e", FailedWorkerID: "w1"}
	prompt := "# Task\n\nDo the thing.\n\n"
	out := ensureRecoveryFileReference(prompt, seed)
	if !strings.Contains(out, recoveryFileReferenceMarker) {
		t.Errorf("reference not appended:\n%s", out)
	}
	if !strings.Contains(out, "Summary: s") {
		t.Errorf("summary missing from block:\n%s", out)
	}
	// Idempotent: a second call must not duplicate the block.
	out2 := ensureRecoveryFileReference(out, seed)
	if strings.Count(out2, "## Recovery") != 1 {
		t.Errorf("reference block duplicated; got %d copies", strings.Count(out2, "## Recovery"))
	}
	// No seed → unchanged.
	if got := ensureRecoveryFileReference(prompt, nil); got != prompt {
		t.Error("nil seed must not modify the prompt")
	}
}

// TestBuildRecoveryFileContent verifies the file shape: header first,
// ## Recovery section with the directive, transcript tail, and the footer
// as the LAST lines (the system-side cleanup matcher depends on it).
func TestBuildRecoveryFileContent(t *testing.T) {
	seed := &recoverySeed{Summary: "exec abc failed", FailedExecID: "exec-1", FailedWorkerID: "w1"}
	content := buildRecoveryFileContent("Architect", seed, "ASSISTANT: did stuff\n\n")

	if !strings.HasPrefix(content, "This is for 'Architect' worker only because you stalled in a previous session. If you are not this worker, do not read the rest of this file.") {
		t.Errorf("header must be the first line; got prefix:\n%s", content[:120])
	}
	for _, want := range []string{
		"## Recovery",
		"exec abc failed",
		"If you are already done with your work, please print your ORCHICON WORKER SUMMARY.",
		"rm .orchicon/worker.recovery",
		"## Dead session transcript (tail)",
		"ASSISTANT: did stuff",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q", want)
		}
	}
	// Footer must be the last lines.
	if !strings.HasSuffix(content, "# recovery-execution-id: exec-1\n# worker-id: w1\n") {
		t.Errorf("footer must be the last lines; tail:\n%s", content[len(content)-160:])
	}
}

// TestBuildRecoveryFileContentEmptyTail verifies a missing transcript still
// yields a usable file (summary + directive + footer).
func TestBuildRecoveryFileContentEmptyTail(t *testing.T) {
	seed := &recoverySeed{Summary: "exec abc failed", FailedExecID: "exec-1", FailedWorkerID: "w1"}
	content := buildRecoveryFileContent("w1", seed, "")
	if strings.Contains(content, "## Dead session transcript") {
		t.Error("empty tail should omit the transcript section")
	}
	if !strings.Contains(content, "If you are already done with your work") {
		t.Error("directive missing")
	}
	if !strings.HasSuffix(content, "# recovery-execution-id: exec-1\n# worker-id: w1\n") {
		t.Errorf("footer must still be the last lines")
	}
}

// TestBuildStandaloneCompositeRecoveryBlock verifies the standalone path
// renders the ## Recovery file reference for a same-worker recovery seed
// and nothing for a fresh/different-worker dispatch.
func TestBuildStandaloneCompositeRecoveryBlock(t *testing.T) {
	p, err := pgxpool.New(context.Background(), "postgres://nohost:5432/nope?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	pool := &db.Pool{Pool: p}
	exec := db.ExecutionRow{TenantID: "tnt_test"}

	// Same-worker recovery → reference present.
	withSeed := buildStandaloneComposite(pool, exec, db.WorkItemRow{
		TenantID: "tnt_test",
		Title:    "Recover me",
		Results:  recoveryResultJSON("exec abc failed", "exec-1", "w1"),
	}, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"})
	if !strings.Contains(withSeed, recoveryFileReferenceMarker) {
		t.Errorf("standalone composite must reference the recovery file when the seed matches")
	}
	if !strings.Contains(withSeed, "## Recovery") {
		t.Errorf("standalone composite missing ## Recovery block")
	}

	// Different worker → no reference.
	diffWorker := buildStandaloneComposite(pool, exec, db.WorkItemRow{
		TenantID: "tnt_test",
		Title:    "Recover me",
		Results:  recoveryResultJSON("exec abc failed", "exec-1", "w1"),
	}, db.WorkerVersionRow{WorkerID: "w2", Role: "Engineer"})
	if strings.Contains(diffWorker, recoveryFileReferenceMarker) {
		t.Error("different-worker standalone composite must not reference the recovery file")
	}
	if strings.Contains(diffWorker, "## Recovery") {
		t.Error("different-worker standalone composite must not show a recovery block")
	}

	// Fresh dispatch → no reference.
	fresh := buildStandaloneComposite(pool, exec, db.WorkItemRow{TenantID: "tnt_test", Title: "Fresh"}, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"})
	if strings.Contains(fresh, recoveryFileReferenceMarker) {
		t.Error("fresh dispatch must not reference the recovery file")
	}
}

// TestBuildCompositePromptRecoveryBlock verifies the workflow path renders
// the ## Recovery file reference for a same-worker seed and keeps the
// summary-only narrative (NO file reference) for a different worker.
func TestBuildCompositePromptRecoveryBlock(t *testing.T) {
	ctx := context.Background()
	item := db.WorkItemRow{
		Title:          "Recover step",
		Status:         "running",
		RuntimeImage:   "orchicon-dev:latest",
		WorkflowStepID: "step1",
	}
	runs := map[string]db.WorkflowStepRunRow{
		"step1": {
			ID:     "sr1",
			Status: domain.StepRunRecovering,
			Result: recoveryResultJSON("exec abc failed", "exec-1", "w1"),
		},
	}
	steps := []workflow.StepWire{{ID: "step1", Name: "Senior Engineer", Kind: "task"}}
	r := &WorkflowReconciler{}

	// Same worker → file reference block present.
	out, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"}, steps, runs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, recoveryFileReferenceMarker) {
		t.Errorf("same-worker workflow composite must reference the recovery file:\n%s", out)
	}
	if !strings.Contains(out, "## Recovery") {
		t.Errorf("same-worker workflow composite missing ## Recovery block")
	}

	// Different worker → summary-only narrative, no file reference.
	diff, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{WorkerID: "w2", Role: "Engineer"}, steps, runs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, recoveryFileReferenceMarker) {
		t.Error("different-worker workflow composite must not reference the recovery file")
	}
	if !strings.Contains(diff, "A previous execution of this step failed and was recovered. Recovery summary:") {
		t.Errorf("different-worker composite must keep the summary-only narrative:\n%s", diff)
	}
}
func TestRemoveRecoveryFileMatching(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, recoveryFileDir)
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(orchDir, recoveryFileName)
	r := &TaskReconciler{}

	// Mismatch (newer recovery owns the file) → left in place.
	content := buildRecoveryFileContent("w1", &recoverySeed{FailedExecID: "exec-9", FailedWorkerID: "w1"}, "")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r.removeRecoveryFileMatching(dir, "exec-1", "w1")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mismatched recovery file must not be deleted")
	}

	// Match → deleted.
	r.removeRecoveryFileMatching(dir, "exec-9", "w1")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("matching recovery file should be deleted")
	}
}

// TestSeedRecoveryFileDispatch verifies the dispatch-time hook end to end:
// a same-worker recovery writes the file (header + directive + footer) and
// appends the composite reference; a non-recovery dispatch sweeps any stale
// file and leaves the prompt untouched. A closed pool exercises the
// no-DB path (worker name falls back to the worker id, empty transcript).
func TestSeedRecoveryFileDispatch(t *testing.T) {
	p, err := pgxpool.New(context.Background(), "postgres://nohost:5432/nope?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	pool := &db.Pool{Pool: p}
	r := &TaskReconciler{pool: pool, log: slog.Default()}
	ctx := context.Background()
	dir := t.TempDir()
	orchDir := filepath.Join(dir, recoveryFileDir)

	exec := db.ExecutionRow{TenantID: "tnt_test", ProjectID: "proj"}
	version := db.WorkerVersionRow{WorkerID: "w1", Version: 1}
	prompt := "# Task\n\nDo the thing.\n\n"

	// Same-worker recovery dispatch.
	task := db.WorkItemRow{TenantID: "tnt_test", Title: "Recover me", Results: recoveryResultJSON("exec abc failed", "exec-1", "w1")}
	out := r.seedRecoveryFile(ctx, exec, task, version, dir, nil, prompt)
	if !strings.Contains(out, recoveryFileReferenceMarker) {
		t.Errorf("dispatch prompt must reference the recovery file")
	}
	b, err := os.ReadFile(filepath.Join(orchDir, recoveryFileName))
	if err != nil {
		t.Fatalf("recovery file not written: %v", err)
	}
	content := string(b)
	if !strings.HasPrefix(content, "This is for 'w1' worker only because you stalled in a previous session.") {
		t.Errorf("header missing; got:\n%s", content[:120])
	}
	for _, want := range []string{"## Recovery", "exec abc failed", "If you are already done with your work"} {
		if !strings.Contains(content, want) {
			t.Errorf("recovery file missing %q", want)
		}
	}
	if !strings.HasSuffix(content, "# recovery-execution-id: exec-1\n# worker-id: w1\n") {
		t.Errorf("footer must be the last lines")
	}

	// A different worker assigned to the same task → no file reference and the
	// existing file (which belongs to w1) is swept.
	other := db.WorkerVersionRow{WorkerID: "w2", Version: 1}
	out2 := r.seedRecoveryFile(ctx, exec, task, other, dir, nil, prompt)
	if out2 != prompt {
		t.Error("different-worker dispatch must not touch the prompt")
	}
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); !os.IsNotExist(err) {
		t.Error("different-worker dispatch must sweep the stale recovery file")
	}

	// Fresh dispatch → sweep again.
	r.seedRecoveryFile(ctx, exec, task, version, dir, nil, prompt)
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); err != nil {
		t.Fatal("recovery file should be rewritten for the same worker")
	}
	fresh := db.WorkItemRow{TenantID: "tnt_test", Title: "Fresh task"}
	out3 := r.seedRecoveryFile(ctx, exec, fresh, version, dir, nil, prompt)
	if out3 != prompt {
		t.Error("fresh dispatch must not touch the prompt")
	}
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); !os.IsNotExist(err) {
		t.Error("fresh dispatch must sweep the stale recovery file")
	}
}

// TestSeedRecoveryFileWorkflowResultKey verifies the workflow path passes
// the step-run result (which carries the seed keys) rather than the work
// item's Results.
func TestSeedRecoveryFileWorkflowResultKey(t *testing.T) {
	stepResult := recoveryResultJSON("exec abc failed", "exec-1", "w1")
	seed := recoverySeedFor(stepResult, nil, "w1")
	if seed == nil {
		t.Fatal("workflow step-run result must seed")
	}
	if seed.FailedExecID != "exec-1" {
		t.Errorf("seed exec id = %q, want exec-1", seed.FailedExecID)
	}
}
