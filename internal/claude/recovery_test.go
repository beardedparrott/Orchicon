package claude

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// resumeHarness spawns a FRESH fake subprocess per spawn and captures every
// argv, so a re-attach (`--resume <sid>`) and a restart-then-resume can be
// asserted without a real claude process.
type resumeHarness struct {
	b *Bridge

	mu    sync.Mutex
	argvs [][]string
	fakes []*fakeProc
}

func newResumeHarness(t *testing.T) *resumeHarness {
	t.Helper()
	rh := &resumeHarness{b: New(quietLogger())}
	rh.b.SetSpawnOverride(func(_ context.Context, spec procSpec) (ProcSession, error) {
		rh.mu.Lock()
		defer rh.mu.Unlock()
		rh.argvs = append(rh.argvs, append([]string(nil), spec.Argv...))
		fp := newFakeProc()
		rh.fakes = append(rh.fakes, fp)
		return fp, nil
	})
	return rh
}

func (rh *resumeHarness) spawned() int {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return len(rh.fakes)
}

func (rh *resumeHarness) argv(i int) []string {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	if i >= len(rh.argvs) {
		return nil
	}
	return append([]string(nil), rh.argvs[i]...)
}

func (rh *resumeHarness) fake(i int) *fakeProc {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	if i >= len(rh.fakes) {
		return nil
	}
	return rh.fakes[i]
}

// TestClaudeBridgeDeclaresEveryOptionalCapability pins the declared contract
// surface: the bridge implements Start plus every optional capability this work
// item adds or re-verifies — including ChatTurnClient, which Ask chat requires
// and which this assertion previously required to be ABSENT.
func TestClaudeBridgeDeclaresEveryOptionalCapability(t *testing.T) {
	b := New(quietLogger())
	for name, ok := range map[string]bool{
		"AdapterBridge":    implements[scheduler.AdapterBridge](b),
		"MessageInjector":  implements[scheduler.MessageInjector](b),
		"SessionContinuer": implements[scheduler.SessionContinuer](b),
		"Aborter":          implements[scheduler.Aborter](b),
		"LivenessReporter": implements[scheduler.LivenessReporter](b),
		"ContextCompacter": implements[scheduler.ContextCompacter](b),
		"SessionOwnerKind": implements[scheduler.SessionOwnerKind](b),
		"ChatTurnClient":   implements[scheduler.ChatTurnClient](b),
	} {
		if !ok {
			t.Fatalf("claude bridge capability %s = false", name)
		}
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// TestClaudeOmittedCapabilitiesFailActionablyNeverPanic pins the contract
// rule: a capability requested for an execution the bridge cannot serve
// returns an actionable error (and never nil-panics).
func TestClaudeOmittedCapabilitiesFailActionablyNeverPanic(t *testing.T) {
	b := New(quietLogger())
	ctx := context.Background()

	if err := b.SendExecutionMessage(ctx, "exec-none", "hi"); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("SendExecutionMessage error = %v, want an actionable no-live-session error", err)
	}
	if err := b.CompactExecution(ctx, "exec-none", "anthropic", "claude-sonnet-5", "scope"); err == nil || !strings.Contains(err.Error(), "no live session to compact") {
		t.Fatalf("CompactExecution error = %v, want an actionable compaction error", err)
	}
	if _, err := b.ContinueSession(ctx, scheduler.ContinueSessionOpts{ExecutionID: "exec-none"}); err == nil {
		t.Fatal("ContinueSession with neither a live session nor a recorded session id must fail actionably")
	}
	if b.IsExecutionActive("exec-none") {
		t.Fatal("a bridge that cannot report liveness for an execution must NOT claim it is alive (fail-closed)")
	}
	if err := b.AbortExecution(ctx, "exec-none", "cancelled"); err != nil {
		t.Fatalf("AbortExecution for an unknown execution must be a safe no-op, got %v", err)
	}
}

// TestContinueSessionReusesTheLiveSession proves a follow-up runs in the SAME
// conversation while the session is live: the message goes onto the same live
// stdin and the SAME session identity comes back.
func TestContinueSessionReusesTheLiveSession(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	h, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-live-follow-up",
		Goal:        "first turn",
	}, rec)
	fp.push(initLine)
	waitFor(t, func() bool {
		s := h.b.liveSession("exec-live-follow-up")
		return s != nil && s.sessionIdentity() == "sess-1"
	}, "session live and bound to its identity")

	sid, err := h.b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec-live-follow-up",
		Message:     "what is left?",
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	if sid != "sess-1" {
		t.Fatalf("session id = %q, want the SAME live identity sess-1", sid)
	}
	if got := fp.turnCount(); got != 2 {
		t.Fatalf("stdin turns = %d, want the follow-up written onto the live session", got)
	}
	// Both the injected follow-up and its result boundary resolve the turn.
	fp.push(resultOne)
	fp.push(resultOne)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session never reached a terminal turn")
	}
}

// TestContinueSessionResumesTheRecordedIdentityAfterARestart proves recovery
// end-to-end: with no live session (a control-plane restart, a lost runtime
// container, an execution that already finished) the follow-up re-attaches by
// SESSION ID — `--resume <sid>` — instead of starting a fresh conversation.
func TestContinueSessionResumesTheRecordedIdentityAfterARestart(t *testing.T) {
	rh := newResumeHarness(t)
	projDir := t.TempDir()
	sid, err := rh.b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exec-resume",
		TenantID:    "t1",
		SessionID:   "sess-prior",
		Message:     "continue the task",
		ModelRef:    "claude/anthropic/claude-sonnet-5",
		ProjectDir:  projDir,
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	if sid != "sess-prior" {
		t.Fatalf("session id = %q, want the recorded identity", sid)
	}
	waitFor(t, func() bool { return rh.spawned() == 1 }, "resumed session spawn")
	argv := strings.Join(rh.argv(0), " ")
	if !strings.Contains(argv, "--resume sess-prior") {
		t.Fatalf("argv %q must re-attach the SAME session identity", argv)
	}
	fp := rh.fake(0)
	waitFor(t, func() bool { return fp.turnCount() == 1 }, "follow-up turn write")
	if len(turnsContaining(fp, "continue the task")) == 0 {
		t.Fatal("the follow-up message was not the resumed session's turn")
	}
	// The resumed session completes normally (the reply lands through the
	// session's own transcript fan-out).
	fp.push(initLine)
	fp.push(resultOne)
	waitFor(t, func() bool { return !rh.b.IsExecutionActive("exec-resume") }, "follow-up session finished")
}

// TestSessionSurvivesARestartResumingBySessionID proves the same identity
// re-attaches across a control-plane restart: a SECOND Start carrying
// ContinueFromSessionID resumes the prior transcript rather than starting a
// fresh conversation.
func TestSessionSurvivesARestartResumingBySessionID(t *testing.T) {
	rh := newResumeHarness(t)
	rec := &captureCallbacks{}
	rh.b.SetSessionStore(rec.recordParts)

	first := make(chan error, 1)
	go func() {
		first <- rh.b.Start(context.Background(), db.ExecutionRow{ID: "exec-1", TenantID: "t1"}, scheduler.ExecutionManifest{
			ExecutionID: "exec-1",
			Goal:        "original goal",
			ModelRef:    "claude/anthropic/claude-sonnet-5",
			ProjectDir:  t.TempDir(),
		}, rec)
	}()
	waitFor(t, func() bool { return rh.spawned() == 1 }, "first session spawn")
	fp1 := rh.fake(0)
	fp1.push(initLine)
	fp1.push(resultOne)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first session never terminated")
	}
	if argv := strings.Join(rh.argv(0), " "); strings.Contains(argv, "--resume") {
		t.Fatalf("a fresh session must not resume: %q", argv)
	}

	// The runtime container is gone; a fresh Start re-attaches the transcript
	// of the SAME session identity.
	second := make(chan error, 1)
	go func() {
		second <- rh.b.Start(context.Background(), db.ExecutionRow{ID: "exec-2", TenantID: "t1"}, scheduler.ExecutionManifest{
			ExecutionID:           "exec-2",
			Goal:                  "resumed goal",
			ModelRef:              "claude/anthropic/claude-sonnet-5",
			ProjectDir:            t.TempDir(),
			SequenceContinue:      true,
			ContinueFromSessionID: "sess-1",
		}, rec)
	}()
	waitFor(t, func() bool { return rh.spawned() == 2 }, "resumed session spawn")
	if argv := strings.Join(rh.argv(1), " "); !strings.Contains(argv, "--resume sess-1") {
		t.Fatalf("resumed argv %q must carry --resume sess-1", argv)
	}
	fp2 := rh.fake(1)
	fp2.push(initLine)
	fp2.push(resultOne)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("resumed Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resumed session never terminated")
	}
}

// TestAbortSignalsIntThenTerm pins the cancellation contract for a stdio
// session: SIGINT first (so an unfinished turn stays resumable), SIGTERM only
// after the grace window.
func TestAbortSignalsIntThenTerm(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	s := newDirectSession(t, scheduler.ExecutionManifest{ExecutionID: "exec-abort-signal"}, rec, fp)
	s.abortWindow = 30 * time.Millisecond // per-session grace: no shared state
	s.abort()
	waitFor(t, func() bool { return len(fp.signals()) >= 2 }, "INT then TERM")
	sigs := fp.signals()
	if sigs[0] != "INT" || sigs[1] != "TERM" {
		t.Fatalf("signals = %v, want INT then TERM after the grace window", sigs)
	}
}

// TestCompactExecutionCompactsALiveSession drives the bridge-level compact
// API (the AC's Compact(provider, model, remaining_scope) surface) against a
// live fake session: the directive turn is written and the compaction is
// recorded, with no HTTP path involved.
func TestCompactExecutionCompactsALiveSession(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	h, done := budgetSession(t, fp, scheduler.ExecutionManifest{
		ExecutionID: "exec-compact-api",
		Goal:        "bridge-level goal",
		Budgets:     []byte(noAutoCompactBudgets),
	}, rec)
	// Two priced turns inject the ladder's own warnings (keeping the session
	// alive) but cannot compact: this budget opts EVERY dimension out of
	// compact_dims, so the only compaction here is the explicit bridge call.
	fp.push(initLine)
	fp.push(cacheHeavyResult)
	fp.push(cacheHeavyResult)
	waitFor(t, func() bool {
		s := h.b.liveSession("exec-compact-api")
		return s != nil && s.stepSnapshot() >= 2
	}, "two completed turns (min-turn floor armed)")
	if got := budgetCompactedParts(rec); len(got) != 0 {
		t.Fatalf("the ladder compacted despite compact_dims opting every dimension out: %+v", got)
	}

	if err := h.b.CompactExecution(context.Background(), "exec-compact-api", "anthropic", "claude-sonnet-5", "PRESERVE-THIS-SCOPE"); err != nil {
		t.Fatalf("CompactExecution: %v", err)
	}
	if len(turnsContaining(fp, "PRESERVE-THIS-SCOPE")) == 0 {
		t.Fatal("the compact directive did not carry the remaining scope through")
	}
	if len(budgetCompactedParts(rec)) == 0 {
		t.Fatal("the bridge-level compaction was not recorded in the transcript")
	}
	// The session survives its own compaction and can still terminate once
	// the outstanding queued turns (two ladder warnings + the directive) have
	// each produced a boundary.
	for i := 0; i < 4; i++ {
		fp.push(freeResult)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the compacted session never terminated")
	}
}
