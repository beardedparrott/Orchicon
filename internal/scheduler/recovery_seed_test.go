package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
	withSeed, _ := buildStandaloneComposite(pool, exec, db.WorkItemRow{
		TenantID: "tnt_test",
		Title:    "Recover me",
		Results:  recoveryResultJSON("exec abc failed", "exec-1", "w1"),
	}, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"}, "", "", "")
	if !strings.Contains(withSeed, recoveryFileReferenceMarker) {
		t.Errorf("standalone composite must reference the recovery file when the seed matches")
	}
	if !strings.Contains(withSeed, "## Recovery") {
		t.Errorf("standalone composite missing ## Recovery block")
	}

	// Different worker → no reference.
	diffWorker, _ := buildStandaloneComposite(pool, exec, db.WorkItemRow{
		TenantID: "tnt_test",
		Title:    "Recover me",
		Results:  recoveryResultJSON("exec abc failed", "exec-1", "w1"),
	}, db.WorkerVersionRow{WorkerID: "w2", Role: "Engineer"}, "", "", "")
	if strings.Contains(diffWorker, recoveryFileReferenceMarker) {
		t.Error("different-worker standalone composite must not reference the recovery file")
	}
	if strings.Contains(diffWorker, "## Recovery") {
		t.Error("different-worker standalone composite must not show a recovery block")
	}

	// Fresh dispatch → no reference.
	fresh, _ := buildStandaloneComposite(pool, exec, db.WorkItemRow{TenantID: "tnt_test", Title: "Fresh"}, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"}, "", "", "")
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
	out, _, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{WorkerID: "w1", Role: "Engineer"}, steps, runs)
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
	diff, _, err := r.buildCompositePrompt(ctx, nil, "tnt_test", item, db.WorkerVersionRow{WorkerID: "w2", Role: "Engineer"}, steps, runs)
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

// TestSeedRecoveryFileDispatch verifies the dispatch-time HARD GATE end to
// end: a same-worker recovery writes + verifies the file (header + directive
// + fail-fast + footer) and appends the composite reference; a fresh or
// different-worker dispatch leaves the prompt AND any existing file
// untouched (no blind sweep — a file may belong to another in-flight
// recovery); a foreign-seed file with no resolvable (terminal) owner blocks
// the dispatch with an error. A closed pool exercises the no-DB path (the
// fast-path keys resolve the seed; worker name falls back to the worker id,
// empty transcript).
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
	out, err := r.seedRecoveryFile(ctx, exec, task, version, dir, nil, prompt)
	if err != nil {
		t.Fatalf("same-worker seed must succeed: %v", err)
	}
	if !strings.Contains(out, recoveryFileReferenceMarker) {
		t.Errorf("dispatch prompt must reference the recovery file")
	}
	if !strings.Contains(out, "recovery seed file missing") {
		t.Errorf("dispatch prompt must carry the worker fail-fast directive")
	}
	b, err := os.ReadFile(filepath.Join(orchDir, recoveryFileName))
	if err != nil {
		t.Fatalf("recovery file not written: %v", err)
	}
	content := string(b)
	if !strings.HasPrefix(content, "This is for 'w1' worker only because you stalled in a previous session.") {
		t.Errorf("header missing; got:\n%s", content[:120])
	}
	for _, want := range []string{"## Recovery", "exec abc failed", "If you are already done with your work", "recovery seed file missing"} {
		if !strings.Contains(content, want) {
			t.Errorf("recovery file missing %q", want)
		}
	}
	if !strings.HasSuffix(content, "# recovery-execution-id: exec-1\n# worker-id: w1\n") {
		t.Errorf("footer must be the last lines")
	}

	// Idempotent same-footer reuse: a second same-worker dispatch succeeds
	// and leaves the file in place.
	out2, err := r.seedRecoveryFile(ctx, exec, task, version, dir, nil, prompt)
	if err != nil {
		t.Fatalf("same-footer reuse must succeed: %v", err)
	}
	if !strings.Contains(out2, recoveryFileReferenceMarker) {
		t.Error("reuse must keep the reference")
	}
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); err != nil {
		t.Fatal("same-footer reuse must not delete the file")
	}

	// A different worker assigned to the same task → no file reference, and
	// the existing file (which belongs to w1) is LEFT ALONE (never swept —
	// it may be a live foreign seed).
	other := db.WorkerVersionRow{WorkerID: "w2", Version: 1}
	out3, err := r.seedRecoveryFile(ctx, exec, task, other, dir, nil, prompt)
	if err != nil {
		t.Fatalf("different-worker dispatch must not error: %v", err)
	}
	if out3 != prompt {
		t.Error("different-worker dispatch must not touch the prompt")
	}
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); err != nil {
		t.Error("different-worker dispatch must NOT sweep an existing file")
	}

	// Fresh dispatch → prompt unchanged, existing file left alone.
	fresh := db.WorkItemRow{TenantID: "tnt_test", Title: "Fresh task"}
	out4, err := r.seedRecoveryFile(ctx, exec, fresh, version, dir, nil, prompt)
	if err != nil {
		t.Fatalf("fresh dispatch must not error: %v", err)
	}
	if out4 != prompt {
		t.Error("fresh dispatch must not touch the prompt")
	}
	if _, err := os.Stat(filepath.Join(orchDir, recoveryFileName)); err != nil {
		t.Error("fresh dispatch must NOT sweep an existing file")
	}

	// Foreign-seed collision: a file whose footer belongs to ANOTHER
	// recovery with no resolvable owner (closed pool → owner lookup fails)
	// must BLOCK the dispatch with an error — never clobber, never launch.
	if err := os.RemoveAll(orchDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := buildRecoveryFileContent("w1", &recoverySeed{FailedExecID: "exec-9", FailedWorkerID: "w1"}, "")
	if err := os.WriteFile(filepath.Join(orchDir, recoveryFileName), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.seedRecoveryFile(ctx, exec, task, version, dir, nil, prompt); err == nil {
		t.Fatal("foreign-seed file with unresolvable owner must block the dispatch")
	}
	if got, _ := os.ReadFile(filepath.Join(orchDir, recoveryFileName)); string(got) != foreign {
		t.Error("foreign-seed file must not be clobbered")
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

// TestRecoveringStepResult verifies the recovering-transition result records
// the dead-execution identity + strategy + worker pin so the seed stays
// resolvable after WorkerExecutionID is cleared at re-dispatch.
func TestRecoveringStepResult(t *testing.T) {
	prev, _ := json.Marshal(map[string]any{"_worker_id": "w1", "_worker_version": 1})
	res := recoveringStepResult(context.Background(), nil, "tnt_test", "wi-1", "exec-9", "summarize_restart", prev)
	var m map[string]any
	if err := json.Unmarshal(res, &m); err != nil {
		t.Fatal(err)
	}
	if m["_work_item_id"] != "wi-1" {
		t.Errorf("_work_item_id = %v", m["_work_item_id"])
	}
	if m["_failed_execution_id"] != "exec-9" {
		t.Errorf("_failed_execution_id = %v", m["_failed_execution_id"])
	}
	if m["_recovery_strategy"] != "summarize_restart" {
		t.Errorf("_recovery_strategy = %v", m["_recovery_strategy"])
	}
	if m["_worker_id"] != "w1" {
		t.Errorf("_worker_id must be preserved for the gate's seed resolution, got %v", m["_worker_id"])
	}
	// Empty strategy normalizes to retry (readStepRecoveryConfig parity).
	res2 := recoveringStepResult(context.Background(), nil, "tnt_test", "wi-1", "exec-9", "", nil)
	var m2 map[string]any
	_ = json.Unmarshal(res2, &m2)
	if m2["_recovery_strategy"] != "retry" {
		t.Errorf("empty strategy must normalize to retry, got %v", m2["_recovery_strategy"])
	}
}

// TestRecoveryIdentityFromResult verifies the resolution order:
// _failed_execution_id wins over _recovery_execution_id, and both are read
// from the step-run result before the work-item results.
func TestRecoveryIdentityFromResult(t *testing.T) {
	stepResult := recoveryResultJSON("s", "exec-1", "w1")
	withFailed := map[string]any{"_failed_execution_id": "exec-0", "_recovery_execution_id": "exec-1"}
	runResult, _ := json.Marshal(withFailed)
	if got := recoveryIdentityFromResult(runResult, stepResult); got != "exec-0" {
		t.Errorf("_failed_execution_id must win; got %q", got)
	}
	if got := recoveryIdentityFromResult(stepResult, nil); got != "exec-1" {
		t.Errorf("_recovery_execution_id fallback; got %q", got)
	}
	if got := recoveryIdentityFromResult(nil, stepResult); got != "exec-1" {
		t.Errorf("wiResults fallback; got %q", got)
	}
	if got := recoveryIdentityFromResult(nil, nil); got != "" {
		t.Errorf("no keys → empty; got %q", got)
	}
}

// TestParseRecoveryFileFooter verifies the footer parser round-trips the
// file's owner identity.
func TestParseRecoveryFileFooter(t *testing.T) {
	content := buildRecoveryFileContent("w1", &recoverySeed{FailedExecID: "exec-1", FailedWorkerID: "w1"}, "")
	execID, workerID := parseRecoveryFileFooter(content)
	if execID != "exec-1" || workerID != "w1" {
		t.Errorf("footer = (%q, %q), want (exec-1, w1)", execID, workerID)
	}
	if e, w := parseRecoveryFileFooter("no footer here\n"); e != "" || w != "" {
		t.Errorf("missing footer must parse empty, got (%q, %q)", e, w)
	}
}

// TestRecoveryFileReferenceBlockFailFast verifies the composite ## Recovery
// block carries the worker-side fail-fast directive (the last line of
// defense against a cold restart).
func TestRecoveryFileReferenceBlockFailFast(t *testing.T) {
	block := recoveryFileReferenceBlock(&recoverySeed{Summary: "s", FailedExecID: "e", FailedWorkerID: "w1"})
	for _, want := range []string{"recovery seed file missing", "do NOT redo work", "fail"} {
		if !strings.Contains(block, want) {
			t.Errorf("recovery block missing fail-fast text %q:\n%s", want, block)
		}
	}
	if !strings.Contains(recoveryDirective, "recovery seed file missing") {
		t.Error("recoveryDirective must carry the fail-fast instruction")
	}
}

// TestResolveRecoverySeedFastPath verifies the shared resolver returns the
// fast-path seed without touching the DB (nil tx), and nil for a
// fresh/different-worker dispatch.
func TestResolveRecoverySeedFastPath(t *testing.T) {
	runResult := recoveryResultJSON("exec abc failed", "exec-1", "w1")
	if seed := resolveRecoverySeed(context.Background(), nil, "tnt_test", "wi-1", runResult, nil, "w1"); seed == nil {
		t.Fatal("fast-path same-worker seed must resolve without a DB")
	} else if seed.FailedExecID != "exec-1" {
		t.Errorf("seed exec = %q", seed.FailedExecID)
	}
	if seed := resolveRecoverySeed(context.Background(), nil, "tnt_test", "wi-1", runResult, nil, "w2"); seed != nil {
		t.Error("different worker must not resolve")
	}
	if seed := resolveRecoverySeed(context.Background(), nil, "tnt_test", "wi-1", nil, nil, "w1"); seed != nil {
		t.Error("no keys, no DB → must not resolve")
	}
}

// ---- bootstrap-churn defense (substance-filtered resume tail) ----

// churnOpenerUserPart / churnOpenerTextPart build the recovery-opener
// announcement parts the bootstrap-churn loop produces.
func churnOpenerUserPart(seq int64, variant string) db.SessionPart {
	txt := "I'm resuming a recovered session, let me read the recovery file to continue (" + variant + ")."
	return db.SessionPart{Seq: seq, Kind: db.SessionPartUserMessage, Payload: []byte(`{"text":` + strconv.Quote(txt) + `,"source":"goal"}`)}
}

func churnOpenerTextPart(seq int64, variant string) db.SessionPart {
	txt := "Resuming the recovered session — the recovery seed file is present, proceeding. (" + variant + ")"
	return db.SessionPart{Seq: seq, Kind: db.SessionPartText, Payload: []byte(`{"part":{"text":` + strconv.Quote(txt) + `}}`)}
}

// TestTailIsBootstrapChurn verifies the churn floor: >=2 recovery-opener
// parts with no tool_use, no file-diff evidence, and no substantive text;
// and that productive tails never classify as churn.
func TestTailIsBootstrapChurn(t *testing.T) {
	churn := []db.SessionPart{}
	var seq int64 = 1
	for i := 0; i < 5; i++ {
		churn = append(churn, churnOpenerUserPart(seq, "v1"), churnOpenerTextPart(seq+1, "v2"))
		seq += 2
	}
	if !tailIsBootstrapChurn(churn) {
		t.Error("recovery-opener-only tail must classify as churn")
	}

	// Neutral kinds (errors, steps) in between don't mask churn.
	withNeutral := append(append([]db.SessionPart{}, churn...), db.SessionPart{Seq: seq, Kind: db.SessionPartError, Payload: []byte(`{"error":"boom"}`)})
	if !tailIsBootstrapChurn(withNeutral) {
		t.Error("neutral error parts must not mask churn")
	}

	// A single opener is not repetition -> not churn.
	if tailIsBootstrapChurn([]db.SessionPart{churnOpenerUserPart(1, "v1")}) {
		t.Error("a single opener must not classify as churn")
	}

	// Any tool_use part -> productive, never churn.
	withTool := append(append([]db.SessionPart{}, churn...), db.SessionPart{Seq: seq, Kind: db.SessionPartToolUse, Payload: []byte(`{"part":{"tool":"bash"}}`)})
	if tailIsBootstrapChurn(withTool) {
		t.Error("tool_use in the tail must prevent churn classification")
	}

	// Substantive non-opener text -> productive.
	substantive := append(append([]db.SessionPart{}, churn...), db.SessionPart{Seq: seq, Kind: db.SessionPartText, Payload: []byte(`{"part":{"text":"` + strings.Repeat("real work happened here ", 8) + `"}}`)})
	if tailIsBootstrapChurn(substantive) {
		t.Error("substantive assistant text must prevent churn classification")
	}

	// File-diff evidence -> productive.
	withDiff := append(append([]db.SessionPart{}, churn...), db.SessionPart{Seq: seq, Kind: db.SessionPartText, Payload: []byte(`{"part":{"text":"+++ b/main.go\n+added"}}`)})
	if tailIsBootstrapChurn(withDiff) {
		t.Error("file-diff evidence must prevent churn classification")
	}

	// Empty/short input -> not churn.
	if tailIsBootstrapChurn(nil) {
		t.Error("empty tail must not classify as churn")
	}
}

// TestLastProductiveWindow verifies the walkback lands on the last
// productive part and drops the churn after it.
func TestLastProductiveWindow(t *testing.T) {
	parts := []db.SessionPart{
		churnOpenerUserPart(1, "v1"),
		{Seq: 2, Kind: db.SessionPartToolUse, Payload: []byte(`{"part":{"tool":"bash"}}`)},
		{Seq: 3, Kind: db.SessionPartText, Payload: []byte(`{"part":{"text":"` + strings.Repeat("productive ", 30) + `"}}`)},
		churnOpenerUserPart(4, "v2"),
		churnOpenerTextPart(5, "v3"),
		churnOpenerUserPart(6, "v4"),
	}
	win, ok := lastProductiveWindow(parts, 60)
	if !ok {
		t.Fatal("productive window must be found")
	}
	if len(win) != 3 || win[len(win)-1].Seq != 3 {
		t.Errorf("window must end at the last productive part (seq 3); got len=%d last=%d", len(win), win[len(win)-1].Seq)
	}

	// All churn -> not found.
	if _, ok := lastProductiveWindow([]db.SessionPart{churnOpenerUserPart(1, "v"), churnOpenerTextPart(2, "v")}, 60); ok {
		t.Error("all-churn transcript must report no productive window")
	}

	// Window respects the part cap.
	var long []db.SessionPart
	for i := int64(1); i <= 100; i++ {
		long = append(long, db.SessionPart{Seq: i, Kind: db.SessionPartToolUse, Payload: []byte(`{"part":{"tool":"bash"}}`)})
	}
	win, ok = lastProductiveWindow(long, 10)
	if !ok || len(win) != 10 || win[0].Seq != 91 {
		t.Errorf("window cap: ok=%v len=%d first=%v", ok, len(win), win[0].Seq)
	}
}

// TestResolveRecoveryTailContent verifies the substance filter end to end:
// a productive tail seeds unchanged; a churn tail with a productive window
// walks back to it (with the churn note); a churn-only transcript reports
// churnOnly so the prior seed is carried instead of the churn.
func TestResolveRecoveryTailContent(t *testing.T) {
	// Genuinely productive tail -> unchanged (no churn note).
	productive := []db.SessionPart{
		{Seq: 1, Kind: db.SessionPartToolUse, Payload: []byte(`{"part":{"tool":"bash"}}`)},
		{Seq: 2, Kind: db.SessionPartText, Payload: []byte(`{"part":{"text":"` + strings.Repeat("did the thing ", 20) + `"}}`)},
	}
	tail, churnOnly := resolveRecoveryTailContent(productive, nil, 64*1024)
	if churnOnly || tail == "" || strings.Contains(tail, "bootstrap churn") {
		t.Errorf("productive tail must seed unchanged: churnOnly=%v tail=%q", churnOnly, tail)
	}

	// Churn tail with an EARLIER productive window (the churn is what the
	// raw tail sees; the walkback reaches past it in the deeper scan).
	tailParts := []db.SessionPart{churnOpenerUserPart(10, "v1"), churnOpenerTextPart(11, "v2"), churnOpenerUserPart(12, "v3")}
	fullScan := append(append([]db.SessionPart{}, productive...), tailParts...)
	tail, churnOnly = resolveRecoveryTailContent(tailParts, fullScan, 64*1024)
	if churnOnly {
		t.Fatal("productive window exists — must not report churnOnly")
	}
	if !strings.Contains(tail, "TOOL CALL: bash") || !strings.Contains(tail, "last productive window") {
		t.Errorf("walkback must carry the productive window + churn note:\n%s", tail)
	}
	if strings.Contains(tail, "continue (v1)") || strings.Contains(tail, "proceeding. (v2)") {
		t.Error("the churn AFTER the productive window must be dropped")
	}

	// All-churn transcript -> churnOnly (explicit churn marker path).
	allChurn := []db.SessionPart{}
	for i := int64(1); i <= 6; i++ {
		allChurn = append(allChurn, churnOpenerUserPart(i, "v1"), churnOpenerTextPart(i+100, "v2"))
	}
	tail, churnOnly = resolveRecoveryTailContent(allChurn, allChurn, 64*1024)
	if !churnOnly || tail != "" {
		t.Errorf("all-churn transcript must report churnOnly with no tail; got churnOnly=%v tail=%q", churnOnly, tail)
	}

	// Churn tail with NO deeper scan -> churnOnly (degraded but never churn-fed).
	tail, churnOnly = resolveRecoveryTailContent(allChurn, nil, 64*1024)
	if !churnOnly || tail != "" {
		t.Errorf("missing scan must degrade to churnOnly; got churnOnly=%v tail=%q", churnOnly, tail)
	}
}

// TestExtractRecoveryTailSection verifies the prior-seed tail extraction
// round-trips the section content and stops at the footer.
func TestExtractRecoveryTailSection(t *testing.T) {
	content := buildRecoveryFileContent("w1", &recoverySeed{FailedExecID: "exec-1", FailedWorkerID: "w1"}, "ASSISTANT: did stuff\n\nTOOL CALL: bash\n\n")
	got := extractRecoveryTailSection(content)
	if got != "ASSISTANT: did stuff\n\nTOOL CALL: bash" {
		t.Errorf("extracted = %q", got)
	}
	if extractRecoveryTailSection("no section here") != "" {
		t.Error("missing section must extract empty")
	}
}

// TestChurnOnlySeedCarriesPriorTail verifies the churn-only seed decision
// at the content level: a churn-only successor must NOT overwrite the prior
// seed's transcript — the carried content names the carried tail and the
// NEW footer, while a churn-only successor with no prior transcript carries
// the explicit churn marker. The header/footer/directive contract is
// unchanged in both cases.
func TestChurnOnlySeedCarriesPriorTail(t *testing.T) {
	prior := buildRecoveryFileContent("Architect", &recoverySeed{Summary: "prior failed", FailedExecID: "exec-0", FailedWorkerID: "w1"}, "TOOL CALL: bash\n\nASSISTANT: real prior work\n\n")
	priorTail := extractRecoveryTailSection(prior)
	if priorTail == "" {
		t.Fatal("prior tail must extract")
	}

	// Churn-only successor carries the prior tail forward under its own footer.
	seed := &recoverySeed{Summary: "exec-1 failed", FailedExecID: "exec-1", FailedWorkerID: "w1"}
	carried := recoveryChurnCarriedNote + priorTail + "\n"
	content := buildRecoveryFileContent("w1", seed, carried)
	for _, want := range []string{
		"carried forward from the prior recovery seed",
		"TOOL CALL: bash",
		"ASSISTANT: real prior work",
		"# recovery-execution-id: exec-1\n# worker-id: w1\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("carried seed missing %q", want)
		}
	}
	if strings.Contains(content, "# recovery-execution-id: exec-0") {
		t.Error("carried seed must carry the NEW footer, not the prior one")
	}

	// Churn-only successor with no prior transcript -> explicit churn marker.
	content = buildRecoveryFileContent("w1", seed, recoveryChurnMarkerNote)
	if !strings.Contains(content, "no productive transcript window exists to seed") {
		t.Errorf("churn marker missing:\n%s", content)
	}
	if !strings.HasSuffix(content, "# recovery-execution-id: exec-1\n# worker-id: w1\n") {
		t.Error("footer must remain the last lines")
	}
}
