package orchicon

// progress_test.go — native progress monitor + loop parity tests
// (opencode parity: internal/opencode/progress_test.go shapes adapted to
// the in-process session loop).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// --- monitor unit tests -----------------------------------------------------

func TestNativeMonitorNoProgressFatal(t *testing.T) {
	// Sub-second windows clamp the poll interval to 5s (the monitor's
	// floor), so the fake clock is seeded AHEAD of the real constructor
	// timestamps (lastStepFinish = real now): the first tick trips
	// no_progress immediately.
	w := defaultStallWindows()
	w.noProgress = 50 * time.Millisecond
	w.noFileDiff = time.Hour
	w.textLoop = time.Hour
	w.toolHang = time.Hour
	m := newProgressMonitor("exec_x", w)
	cur := time.Now().Add(time.Hour)
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	var reason string
	done := make(chan struct{})
	go func() {
		m.run(func(_ string, r string) { reason = r; close(done) }, func(string, string) {})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("no_progress did not trip within 10s")
	}
	if reason != "stalled:no_progress" {
		t.Fatalf("reason = %q, want stalled:no_progress", reason)
	}
}

func TestNativeMonitorRepetitionTier2Window(t *testing.T) {
	w := defaultStallWindows()
	w.noProgress = time.Hour
	w.noFileDiff = 0 // disabled — no file progress expected in this test
	w.textLoop = 0
	w.toolHang = 0
	w.repetitionN = 5
	w.repetitionW = time.Minute
	m := newProgressMonitor("exec_x", w)
	base := time.Now()
	cur := base
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	// 11 identical SUCCESS calls inside the window: tier 2 (2N) trips at
	// the 11th. No step_finish between them (a step_finish resets).
	var reason string
	for i := 0; i < 12; i++ {
		m.observeToolCall("read", `{"path":"big.go"}`, false)
		if r := m.check(); r != "" {
			reason = r
			break
		}
	}
	if reason == "" {
		t.Fatalf("tier-2 repetition never tripped after 11 identical calls")
	}
	if !strings.HasPrefix(reason, "stalled:repetition:completed:") {
		t.Fatalf("reason = %q, want stalled:repetition:completed:", reason)
	}
}

func TestNativeMonitorRepetitionResetOnProgress(t *testing.T) {
	w := defaultStallWindows()
	w.noProgress = time.Hour
	w.noFileDiff = 0
	w.textLoop = 0
	w.toolHang = 0
	m := newProgressMonitor("exec_x", w)
	base := time.Now()
	cur := base
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	// 15 identical calls, but a step_finish after every 3 — reset-on-progress
	// keeps the count below the tier-2 threshold forever.
	for i := 0; i < 15; i++ {
		m.observeToolCall("read", `{"path":"big.go"}`, false)
		if i%3 == 2 {
			m.observeStepFinish(Usage{InputTokens: 100})
		}
		if r := m.check(); r != "" {
			t.Fatalf("repetition tripped %q despite reset-on-progress", r)
		}
	}
}

func TestNativeMonitorZeroUsageToolLoopTrips(t *testing.T) {
	// The regression that killed the reported execution: identical reads
	// with ZERO reported usage must still trip the (windowed) repetition
	// detector — the monitor no longer requires token growth.
	w := defaultStallWindows()
	w.noProgress = time.Hour
	w.noFileDiff = 0
	w.textLoop = 0
	w.toolHang = 0
	m := newProgressMonitor("exec_x", w)
	cur := time.Now()
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	for i := 0; i < 11; i++ {
		m.observeToolCall("read", `{"path":"chat.go"}`, false)
		if r := m.check(); strings.HasPrefix(r, "stalled:repetition") {
			return // tripped — the failure shape is fixed
		}
	}
	t.Fatalf("zero-usage identical-read loop never tripped")
}

func TestNativeMonitorWriteSignatureCollapsesVolatileContent(t *testing.T) {
	w := defaultStallWindows()
	w.noProgress = time.Hour
	w.noFileDiff = 0
	w.textLoop = 0
	w.toolHang = 0
	m := newProgressMonitor("exec_x", w)
	cur := time.Now()
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	// Same path, different content each retry: the signatures collapse so
	// a blocked-write retry loop still counts as repetition.
	for i := 0; i < 11; i++ {
		m.observeToolCall("write", fmt.Sprintf(`{"filePath":"x.go","content":"attempt %d"}`, i), false)
		if r := m.check(); strings.HasPrefix(r, "stalled:repetition:completed:") {
			return
		}
	}
	t.Fatalf("volatile-content write loop never collapsed to one signature")
}

func TestNativeMonitorToolHangAdvisoryLatched(t *testing.T) {
	w := defaultStallWindows()
	w.noProgress = time.Hour
	w.noFileDiff = 0
	w.textLoop = 0
	w.toolHang = 50 * time.Millisecond
	m := newProgressMonitor("exec_x", w)
	cur := time.Now()
	m.now = func() time.Time { cur = cur.Add(time.Second); return cur }
	m.observeToolStart("bash")
	r1 := m.check()
	if !strings.HasPrefix(r1, "stalled:tool_hang:") {
		t.Fatalf("first check = %q, want tool_hang", r1)
	}
	if r2 := m.check(); r2 != "" {
		t.Fatalf("tool_hang latched once — second check = %q, want empty", r2)
	}
}

func TestIsFatalStallParity(t *testing.T) {
	cases := map[string]bool{
		"stalled:no_progress":          true,
		"stalled:no_progress:x":        true,
		"stalled:no_file_progress":     false,
		"stalled:text_loop:...":        false,
		"stalled:repetition:read":      false,
		"stalled:repetition:completed": false,
		"stalled:tool_hang:bash":       false,
	}
	for reason, want := range cases {
		if got := isFatalStall(reason); got != want {
			t.Errorf("isFatalStall(%q) = %v, want %v", reason, got, want)
		}
	}
}

// --- loop behavior tests ----------------------------------------------------

// AC (leadway): a StopLength turn continues the session via a bounded
// continuation turn instead of failing — the accumulated context survives.
func TestLoopStopLengthContinues(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: long output cut by the cap (bare — no marker).
		{events: []Event{TextDelta{Text: "long monologue"}}, finish: StopLength, usage: Usage{InputTokens: 100, OutputTokens: 4096}, bare: true},
		// Turn 2 (continuation): the model finishes with the marker.
		{events: []Event{TextDelta{Text: "...and the conclusion."}}, finish: StopStop, usage: Usage{InputTokens: 200, OutputTokens: 50}},
	}}
	s, _ := newQATestSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, text, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success (StopLength must continue, not fail)", results)
	}
	joined := strings.Join(text, "")
	if !strings.Contains(joined, "long monologue") || !strings.Contains(joined, "conclusion") {
		t.Fatalf("output lost across the continuation: %q", joined)
	}
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2 (the continuation turn)", prov.requestCount())
	}
}

// AC (bounded): after the continuation budget is spent, StopLength fails
// honestly instead of looping forever.
func TestLoopStopLengthBudgetExhausted(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "cut"}}, finish: StopLength, usage: Usage{InputTokens: 1, OutputTokens: 1}, bare: true},
		{events: []Event{TextDelta{Text: "cut"}}, finish: StopLength, usage: Usage{InputTokens: 1, OutputTokens: 1}, bare: true},
		{events: []Event{TextDelta{Text: "cut"}}, finish: StopLength, usage: Usage{InputTokens: 1, OutputTokens: 1}, bare: true},
	}}
	s, _ := newQATestSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Fatalf("OnResult = %+v, want failure after the continuation budget", results)
	}
	if !strings.Contains(results[0].errMsg, "length") || !strings.Contains(results[0].errMsg, "continuation budget") {
		t.Fatalf("errMsg = %q, want the continuation-budget message", results[0].errMsg)
	}
	// 3 turns max: initial + 2 continuations.
	if prov.requestCount() != 3 {
		t.Fatalf("provider turns = %d, want 3 (initial + 2 continuations)", prov.requestCount())
	}
}

// AC (legacy regression): two UNIQUE-signature tool rounds with real usage
// growth never stall (the old in-loop guard stays inert for healthy runs).
func TestLoopUniqueToolRoundsHealthy(t *testing.T) {
	tools := newMockTools()
	tools.results["noop"] = "ok"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t2", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 120, OutputTokens: 10}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t3", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 140, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 160, OutputTokens: 10}},
	}}
	s, _ := newQATestSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, stalls, _, results := cb.snapshot()
	if len(stalls) != 0 {
		t.Fatalf("stalls = %v, want none on a healthy multi-round run", stalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success", results)
	}
}

// AC (context-window fallback): when live resolution fails, the work
// item's declared window arms the compaction trigger.
func TestContextWindowFallbackFromManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := testManifest("orchicon/mockprov/deepseek-v4-flash")
	manifest.ContextWindow = 65536
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_test"),
		Manifest:   manifest,
		ProjectDir: dir,
		Provider:   &mockProvider{}, // ListModels returns nil → model_not_found
		Log:        nil,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hint := s.resolveContextWindow(context.Background())
	if !hint.Ok || hint.Tokens != 65536 {
		t.Fatalf("hint = %+v, want Ok with the declared 65536-token window", hint)
	}
	if !strings.HasPrefix(hint.Reason, "manifest_fallback:") {
		t.Fatalf("reason = %q, want manifest_fallback provenance", hint.Reason)
	}
}

// AC (output-cap resolution): the per-turn cap is env-tunable and capped
// by the model's known MaxOutput when it is smaller.
func TestTurnMaxTokens(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_test"),
		Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
		ProjectDir: dir,
		Provider:   &mockProvider{},
		Log:        nil,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got := s.turnMaxTokens(context.Background()); got != defaultMaxOutputTokens {
		t.Fatalf("turnMaxTokens = %d, want default %d", got, defaultMaxOutputTokens)
	}
	t.Setenv("ORCHICON_SESSION_MAX_OUTPUT_TOKENS", "8192")
	if got := s.turnMaxTokens(context.Background()); got != 8192 {
		t.Fatalf("turnMaxTokens = %d, want env override 8192", got)
	}
}

// AC (wall-clock parity): explicit 0 disables; unset uses the backstop;
// a value sets the deadline.
func TestNativeWallClockDeadline(t *testing.T) {
	if d, ok := wallClockDeadline([]byte(`{"wall_clock_seconds":0}`)); ok {
		t.Fatalf("explicit 0 must disable, got deadline %v", d)
	}
	if _, ok := wallClockDeadline([]byte(`{"wall_clock_seconds":1500}`)); !ok {
		t.Fatalf("1500s must set a deadline")
	}
	d1, ok1 := wallClockDeadline([]byte(`{}`))
	if !ok1 || time.Until(d1) > defaultWallClockTimeout {
		t.Fatalf("unset must use the %v backstop (got ok=%v until=%v)", defaultWallClockTimeout, ok1, time.Until(d1))
	}
}

// AC (manifest stall windows flow through): the monitor is constructed
// from the ExecutionManifest's tenant-settings fields.
func TestStallWindowsFromManifest(t *testing.T) {
	w := stallWindowsFromManifest(120, 600, 300, 7, 90, -1)
	if w.noProgress != 120*time.Second {
		t.Errorf("noProgress = %v, want 120s", w.noProgress)
	}
	if w.noFileDiff != 600*time.Second {
		t.Errorf("noFileDiff = %v, want 600s", w.noFileDiff)
	}
	if w.textLoop != 300*time.Second {
		t.Errorf("textLoop = %v, want 300s", w.textLoop)
	}
	if w.repetitionN != 7 {
		t.Errorf("repetitionN = %d, want 7", w.repetitionN)
	}
	if w.repetitionW != 90*time.Second {
		t.Errorf("repetitionW = %v, want 90s", w.repetitionW)
	}
	if w.toolHang >= 0 {
		t.Errorf("toolHang = %v, want negative (disabled)", w.toolHang)
	}
}

// AC (nudge-first): an advisory repetition stall nudges the live session
// (queued injection + nudge transcript part) before any escalation.
func TestLoopAdvisoryStallNudgesBeforeEscalation(t *testing.T) {
	s, _ := newQATestSession(t, &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil)
	// The nudge path appends a transcript part; Run normally opens the
	// transcript. This test drives handleStallSignal directly, so open it
	// (same lifecycle as Run) before firing the advisory signal.
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	cb := &recordedCallback{}
	s.handleStallSignal(cb, s.id, "stalled:repetition:completed:read|{}")
	// First advisory: a nudge is queued (injection), surfaced non-fatally,
	// and the session does NOT terminate.
	s.noteMu.Lock()
	nudged := s.nudgesSent
	s.noteMu.Unlock()
	if nudged != 1 {
		t.Fatalf("nudgesSent = %d, want 1 after the first advisory stall", nudged)
	}
	select {
	case r := <-s.stallCh:
		t.Fatalf("advisory stall must not terminate the session, got %q", r)
	default:
	}
	// Second advisory after the cooldown: still nudges (budget 2).
	s.lastNudgeAt = time.Now().Add(-2 * defaultNudgeCooldown)
	s.handleStallSignal(cb, s.id, "stalled:repetition:completed:read|{}")
	s.noteMu.Lock()
	nudged = s.nudgesSent
	s.noteMu.Unlock()
	if nudged != 2 {
		t.Fatalf("nudgesSent = %d, want 2 after the second advisory stall", nudged)
	}
	// Third advisory: the nudge budget is spent → fatal escalation.
	s.lastNudgeAt = time.Now().Add(-3 * defaultNudgeCooldown)
	s.handleStallSignal(cb, s.id, "stalled:repetition:completed:read|{}")
	select {
	case r := <-s.stallCh:
		if !strings.Contains(r, "nudge_budget_spent") {
			t.Fatalf("escalation reason = %q, want nudge_budget_spent suffix", r)
		}
	default:
		t.Fatalf("third advisory must escalate fatally after the nudge budget")
	}
	_ = scheduler.ExecutionManifest{} // keep the import if unused in future edits
}

// AC (fatal-stall parity): a fatal monitor stall delivers the terminal
// OnResult(false, reason) — exactly ONE verdict, mirroring the opencode
// adapter's OnStall(fatal) + finish(false, reason) shape. Recovery owns
// the re-dispatch only when it receives this verdict.
func TestLoopFatalStallFiresTerminalOnResult(t *testing.T) {
	s, _ := newQATestSession(t, &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "thinking slowly…"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil)
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	cb := &recordedCallback{}
	s.handleStallSignal(cb, s.id, "stalled:no_progress")
	select {
	case r := <-s.stallCh:
		if r != "stalled:no_progress" {
			t.Fatalf("stallCh = %q, want stalled:no_progress", r)
		}
	default:
		t.Fatalf("fatal stall must signal stallCh so the loop unwinds")
	}
	_, _, _, _, stalls, stallFatal, results := cb.snapshot()
	if len(stalls) != 1 || !stallFatal[0] {
		t.Fatalf("stalls = %v fatal=%v, want one fatal OnStall", stalls, stallFatal)
	}
	if len(results) != 1 || results[0].succeeded || results[0].errMsg != "stalled:no_progress" {
		t.Fatalf("results = %+v, want exactly one terminal OnResult(false, stalled:no_progress)", results)
	}
}

// AC (terminal-once): a second terminal path (e.g. a concurrent monitor
// escalation racing the loop) never fires a second OnResult — opencode
// finish() first-arrival-wins parity.
func TestLoopTerminalOnResultFiresOnce(t *testing.T) {
	s, _ := newQATestSession(t, &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil)
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	cb := &recordedCallback{}
	if !s.fireTerminalOnce(cb, s.id, false, "first") {
		t.Fatalf("first fireTerminalOnce must deliver")
	}
	if s.fireTerminalOnce(cb, s.id, true, "") {
		t.Fatalf("second fireTerminalOnce must be suppressed (first-arrival-wins)")
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded || results[0].errMsg != "first" {
		t.Fatalf("results = %+v, want exactly the first verdict", results)
	}
}

// AC (nudge reply-window parity): a nudged session that produces NO turn
// activity within the reply window escalates fatally with the
// liveness_probe_no_response reason; continued output clears the probe.
func TestLoopNudgeReplyWindowEscalates(t *testing.T) {
	s, _ := newQATestSession(t, &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil)
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	s.nudgeReplyWindowVal = 30 * time.Millisecond // tiny window for the test
	cb := &recordedCallback{}
	s.handleStallSignal(cb, s.id, "stalled:repetition:completed:read|{}")
	// No reply arrives → the watchdog escalates within ~the window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		cb.mu.Lock()
		results := len(cb.results)
		cb.mu.Unlock()
		if results > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nudge reply window elapsed without a fatal escalation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, _, _, stalls, _, results := cb.snapshot()
	if !strings.Contains(results[len(results)-1].errMsg, "liveness_probe_no_response") {
		t.Fatalf("errMsg = %q, want liveness_probe_no_response", results[len(results)-1].errMsg)
	}
	_ = stalls
}

// AC (nudge reply cleared): turn output after the nudge clears the
// pending probe — the reply IS the liveness evidence (opencode parity).
func TestLoopNudgeReplyClearsProbe(t *testing.T) {
	s, _ := newQATestSession(t, &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "working on it"}}, finish: StopStop, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}, nil)
	if _, err := s.OpenTranscript(); err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	s.nudgeReplyWindowVal = 50 * time.Millisecond
	cb := &recordedCallback{}
	s.handleStallSignal(cb, s.id, "stalled:text_loop:chatter")
	// The nudged turn replies within the window:
	s.nudgeObserved()
	time.Sleep(150 * time.Millisecond)
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (the reply cleared the probe)", results)
	}
}
