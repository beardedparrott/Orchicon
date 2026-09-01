package orchicon

// Regression tests for the PR-Reviewer blockers on the session engine
// (ADR-0007 implementation):
//   - B2: a panic contained at the session boundary (Session.Run recovers
//     and fires OnResult(false, ...) then returns ErrPanic) must NOT
//     double-terminate the execution. NativeBridge.Start must return nil
//     once the terminal OnResult fired, so the reconciler's
//     startExecution does not also markFailedToStart + requeue.
//   - B3: the transcript must live under manifest.ProjectDir (the
//     authoritative project dir from the execution manifest, ADR-0007),
//     not under the bridge's construction-time projectDir — a shared
//     bridge must resolve each execution's transcripts per-execution.
//   - The ContinueSession tenant check is a real cross-tenant refusal
//     (the old check compared prior.TenantID against itself — a
//     tautology that never refused anything).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// B2: a panicking provider, driven through NativeBridge.Start, fails the
// execution with OnResult(false, panic) and Start returns nil (the
// terminal already delivered the verdict — no failed_to_start requeue).
func TestQABridgeStartPanicReturnsNil(t *testing.T) {
	dir := t.TempDir()
	prov := &panicProvider{stream: &panickingStream{panicMsg: "provider callback panicked"}}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), dir, nil)

	exec := testExecRow("exec_panic")
	mf := testManifest("orchicon/mockprov/deepseek-v4-flash")
	mf.ProjectDir = dir

	// The bridge must fail only this execution: Start returns nil, the
	// callbacks saw exactly one terminal OnResult(false), and the
	// transcript is marked failed (resumable, replayable).
	cb := &recordedCallback{}
	err := b.Start(context.Background(), exec, mf, cb)
	if err != nil {
		t.Fatalf("Start = %v, want nil after terminal OnResult (no double-termination)", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Fatalf("OnResult = %+v, want exactly one failed result", results)
	}
	if !strings.Contains(results[0].errMsg, "provider callback panicked") {
		t.Errorf("OnResult error = %q, want panic captured", results[0].errMsg)
	}
	// Transcript exists and is marked failed.
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_panic.jsonl")
	evs, err := Load(path)
	if err != nil {
		t.Fatalf("load transcript: %v", err)
	}
	failed := false
	for _, e := range evs {
		if e.Type == TransState {
			var d struct {
				State string `json:"state"`
			}
			_ = jsonUnmarshal(e.Data, &d)
			if d.State == "failed" {
				failed = true
			}
		}
	}
	if !failed {
		t.Error("transcript not marked failed after contained panic")
	}
}

// B3: NativeBridge.Start must place the session transcript under
// manifest.ProjectDir even when the bridge was constructed with a
// different (or empty) projectDir — the manifest dir is authoritative.
func TestQABridgeStartUsesManifestProjectDir(t *testing.T) {
	manifestDir := t.TempDir()
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "hi"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	b := NewBridge(ProviderResolverFunc(func(ctx context.Context, tenantID, providerID string) (Provider, error) {
		return prov, nil
	}), "bridge-level-dir", nil) // deliberately different from the manifest dir

	exec := testExecRow("exec_mdir")
	mf := testManifest("orchicon/mockprov/deepseek-v4-flash")
	mf.ProjectDir = manifestDir

	cb := &recordedCallback{}
	if err := b.Start(context.Background(), exec, mf, cb); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The transcript must be under the manifest's project dir, NOT under
	// the bridge's construction-time dir.
	want := filepath.Join(manifestDir, ".orchicon", "sessions", "exec_mdir.jsonl")
	if _, err := Load(want); err != nil {
		t.Fatalf("transcript not under manifest.ProjectDir: %v", err)
	}
	notThere := filepath.Join("bridge-level-dir", ".orchicon", "sessions", "exec_mdir.jsonl")
	if _, err := Load(notThere); err == nil {
		t.Error("transcript wrongly placed under bridge construction-time projectDir")
	}
}

// B3: both projectDir sources empty → Start fails fast instead of
// writing a transcript to a nonsense path.
func TestQABridgeStartNoProjectDirFails(t *testing.T) {
	b := NewBridge(nil, "", nil)
	exec := testExecRow("exec_nodir")
	mf := testManifest("orchicon/mockprov/deepseek-v4-flash") // ProjectDir empty
	if err := b.Start(context.Background(), exec, mf, &recordedCallback{}); err == nil {
		t.Fatal("Start with no project dir must fail")
	}
}

// Tautology fix: a prior transcript from a DIFFERENT tenant must be
// refused (the old code compared prior.TenantID to itself).
func TestQAContinueSessionRefusesCrossTenant(t *testing.T) {
	dir := t.TempDir()
	b := NewBridge(nil, dir, nil)
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior_tenant.jsonl")
	writeIdentityTranscript(t, path, Identity{
		ExecutionID: "exec_prior_tenant",
		WorkerID:    "worker_test",
		WorkerName:  "qa-worker",
		TenantID:    "tnt_A",
	})
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		SessionID:   "exec_prior_tenant",
		ExecutionID: "exec_now",
		TenantID:    "tnt_B", // different tenant → must refuse
	})
	if err == nil {
		t.Fatal("cross-tenant continuation must be refused")
	}
	// Same tenant passes.
	_, err = b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		SessionID:   "exec_prior_tenant",
		ExecutionID: "exec_now",
		TenantID:    "tnt_A",
	})
	if err != nil {
		t.Fatalf("same-tenant continuation refused: %v", err)
	}
}

func writeIdentityTranscript(t *testing.T, path string, ident Identity) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tr, err := openTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(TransSession, map[string]any{"identity": ident}); err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
}
