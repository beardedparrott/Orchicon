package opencode

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// clock is a controllable now() for tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestMonitor(w stallWindows) *progressMonitor {
	c := &clock{t: time.Now()}
	m := newProgressMonitor("exe_test", w)
	m.now = c.now
	m.startedAt = c.t
	m.lastStepFinish = c.t
	m.lastFileDiff = c.t
	return m
}

// TestStallNoProgress verifies the no-progress window trips when no
// step_finish arrives within the window.
func TestStallNoProgress(t *testing.T) {
	w := stallWindows{noProgress: 10 * time.Second, noFileDiff: time.Hour, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Advance past the no-progress window without a step_finish.
	m.now = func() time.Time { return time.Now().Add(11 * time.Second) }
	m.lastStepFinish = m.startedAt.Add(-11 * time.Second) // simulate staleness
	// Simulate: set lastStepFinish far in the past.
	m.mu.Lock()
	m.lastStepFinish = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("expected stalled:no_progress, got %q", reason)
	}
}

// TestStallNoFileDiff verifies the no-file-diff window trips when no
// file_diff arrives within the window.
func TestStallNoFileDiff(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: 10 * time.Second, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	m.mu.Lock()
	m.lastFileDiff = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason != "stalled:no_file_progress" {
		t.Fatalf("expected stalled:no_file_progress, got %q", reason)
	}
}

// TestAdvisoryStallRevives verifies the advisory no_file_progress signal
// is revivable: the monitor keeps running after the trip and reports a
// recovery once a file_diff arrives again, and the cleared warning state
// lets the window trip a second time if file progress stops again.
func TestAdvisoryStallRevives(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: 10 * time.Second, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	m.mu.Lock()
	m.lastFileDiff = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason != "stalled:no_file_progress" {
		t.Fatalf("expected advisory trip, got %q", reason)
	}
	// No file_diff yet — no revival.
	if rec := m.checkRevival(); rec != "" {
		t.Fatalf("expected no revival yet, got %q", rec)
	}
	// A file_diff arrives → the advisory warning clears.
	m.observe("file_diff", map[string]any{"path": "/x"})
	if rec := m.checkRevival(); rec != "recovered:no_file_progress" {
		t.Fatalf("expected revival after file_diff, got %q", rec)
	}
	// warned cleared — a second quiet window can trip the advisory again.
	m.mu.Lock()
	m.lastFileDiff = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason != "stalled:no_file_progress" {
		t.Fatalf("expected advisory to trip again, got %q", reason)
	}
}

// TestStallRepetition verifies the repetition signal trips when the same
// tool_use signature exceeds the threshold within the window. Parts are
// shaped like LegacyEventFromBus emits them (tool_use with args under
// state.input) — the exact repro of execution 01M0B5RWXN9ZH56FXME5MKWRT4
// where 100 identical `bash` calls never tripped because the monitor only
// listened for the dead `tool_call` type.
//
// Result-aware repetition (design B): only ERROR-status tool calls count
// toward the threshold, so the fixture feeds `status: "error"` calls. A
// completed call would reset the counters instead of tripping.
func TestStallRepetition(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Feed the same tool_use 4 times (> threshold of 3).
	for i := 0; i < 4; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "git status"}},
		})
	}
	reason := m.check()
	if len(reason) < len("stalled:repetition:") || reason[:len("stalled:repetition:")] != "stalled:repetition:" {
		t.Fatalf("expected stalled:repetition:..., got %q", reason)
	}
	if !strings.Contains(reason, "git status") {
		t.Fatalf("expected the signature to carry the real args from state.input, got %q", reason)
	}
}

// TestStallRepetitionNoFalsePositive verifies distinct tool_use calls (even
// to the same tool) within the window do NOT trip repetition — different
// state.input args must yield distinct signatures. This is the control that
// proves args are really read from state.input; if args collapsed to a
// constant the varied calls would false-fire. Calls are ERROR-status so they
// count toward the threshold (result-aware repetition); varied erroring
// calls must still not trip.
func TestStallRepetitionNoFalsePositive(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	calls := []map[string]any{
		{"tool": "bash", "state": map[string]any{"status": "error", "input": map[string]any{"command": "git status"}}},
		{"tool": "bash", "state": map[string]any{"status": "error", "input": map[string]any{"command": "git log"}}},
		{"tool": "read", "state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/a"}}},
		{"tool": "read", "state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/b"}}},
		{"tool": "edit", "state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/a", "oldString": "x", "newString": "x2"}}},
		{"tool": "edit", "state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/a", "oldString": "y", "newString": "y2"}}},
	}
	for i := 0; i < 3; i++ { // repeat the varied set; each signature appears 3x = threshold, no trip
		for _, c := range calls {
			m.observe("tool_use", c)
		}
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no repetition on varied calls, got %q", reason)
	}
}

// TestStallRepetitionCompletedResets verifies result-aware repetition
// (design B): a COMPLETED tool call resets the signature history, so
// repeated identical calls that SUCCEED never trip repetition. This is the
// normal build-fix-iterate-debug loop — a worker iterating on a failing
// build (legitimate) must not be reaped.
func TestStallRepetitionCompletedResets(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Many identical COMPLETED calls — each resets the counters, so no trip.
	for i := 0; i < 10; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "completed", "input": map[string]any{"command": "go test ./..."}},
		})
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no repetition on completed calls, got %q", reason)
	}
}

// TestStallRepetitionErroringTrips verifies that only ERROR-status calls
// count toward the threshold: a completed call resets the counters, so
// erroring calls must accumulate WITHOUT an intervening completed call to
// trip.
func TestStallRepetitionErroringTrips(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Two erroring calls, then a completed call (resets), then two more
	// erroring calls — the reset means the signature never exceeds 3.
	for i := 0; i < 2; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "go test ./..."}},
		})
	}
	m.observe("tool_use", map[string]any{
		"tool":  "bash",
		"state": map[string]any{"status": "completed", "input": map[string]any{"command": "go test ./..."}},
	})
	for i := 0; i < 2; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "go test ./..."}},
		})
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no trip after a completed-call reset, got %q", reason)
	}
	// Now 4 consecutive erroring calls (no reset) — trips.
	for i := 0; i < 4; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "go test ./..."}},
		})
	}
	reason := m.check()
	if !strings.HasPrefix(reason, "stalled:repetition:") {
		t.Fatalf("expected stalled:repetition:..., got %q", reason)
	}
}

// TestStallRepetitionBuildFixIterate verifies the build-fix-iterate
// regression: repeated failing `go test` calls INTERRUPTED by edits /
// successes must NOT trip repetition — progress (file_diff, completed
// calls, step_finish) resets the counters.
func TestStallRepetitionBuildFixIterate(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// A realistic loop: run tests (error), edit a file, run tests (error),
	// edit a file, run tests (completed) — the edits and the success reset
	// the counters so the erroring `go test` signature never accumulates.
	for i := 0; i < 5; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "go test ./..."}},
		})
		m.observe("file_diff", map[string]any{"path": "/src/foo.go"})
	}
	m.observe("tool_use", map[string]any{
		"tool":  "bash",
		"state": map[string]any{"status": "completed", "input": map[string]any{"command": "go test ./..."}},
	})
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no repetition on build-fix-iterate loop, got %q", reason)
	}
}

// TestStallRepetitionNormalizedWrite verifies conservative signature
// normalization (design A, gated through B): a worker looping on a blocked
// write to the same path — each retry carrying different volatile content —
// collapses to one signature and trips, while the path is preserved. The
// fixture uses the real opencode write-tool schema ({filePath, content}).
func TestStallRepetitionNormalizedWrite(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// The exact repro: 29 blocked writes to a sibling worktree, each with
	// different content but the same path, all erroring.
	for i := 0; i < 4; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "write",
			"state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/sibling/probe_test.go", "content": "content-" + string(rune('a'+i))}},
		})
	}
	reason := m.check()
	if !strings.HasPrefix(reason, "stalled:repetition:") {
		t.Fatalf("expected stalled:repetition:... on normalized write loop, got %q", reason)
	}
	if !strings.Contains(reason, "/sibling/probe_test.go") {
		t.Fatalf("expected the normalized signature to key on path, got %q", reason)
	}
}

// TestStallRepetitionNormalizedBash verifies bash command scrubbing: a
// worker retrying the same command with different volatile args (timestamps,
// temp IDs) collapses to one signature, while distinct commands stay
// distinct.
func TestStallRepetitionNormalizedBash(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Same command, different temp IDs — must collapse to one signature.
	for i := 0; i < 4; i++ {
		m.observe("tool_use", map[string]any{
			"tool":  "bash",
			"state": map[string]any{"status": "error", "input": map[string]any{"command": "cat /tmp/tmp" + string(rune('a'+i)) + "1234567890"}},
		})
	}
	reason := m.check()
	if !strings.HasPrefix(reason, "stalled:repetition:") {
		t.Fatalf("expected stalled:repetition:... on scrubbed bash loop, got %q", reason)
	}
}

// TestStallRepetitionNormalizedDistinctCommands verifies scrubbing does NOT
// collapse distinct legitimate commands: `git status` and `git log` must
// stay distinct even when erroring.
func TestStallRepetitionNormalizedDistinctCommands(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	calls := []map[string]any{
		{"tool": "bash", "state": map[string]any{"status": "error", "input": map[string]any{"command": "git status"}}},
		{"tool": "bash", "state": map[string]any{"status": "error", "input": map[string]any{"command": "git log"}}},
	}
	for i := 0; i < 3; i++ {
		for _, c := range calls {
			m.observe("tool_use", c)
		}
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no repetition on distinct commands, got %q", reason)
	}
}

// TestStallFiresOnce verifies the monitor fires at most once per execution.
func TestStallFiresOnce(t *testing.T) {
	w := stallWindows{noProgress: 10 * time.Second, noFileDiff: time.Hour, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	m.mu.Lock()
	m.lastStepFinish = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason == "" {
		t.Fatal("expected first stall to fire")
	}
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no second fire, got %q", reason)
	}
}

// TestStallNoTripWhenProgressing verifies no stall when progress is recent.
func TestStallNoTripWhenProgressing(t *testing.T) {
	w := stallWindows{noProgress: 10 * time.Second, noFileDiff: 10 * time.Second, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Recent progress on all signals.
	m.observe("step_finish", map[string]any{"tokens": map[string]any{"input": 10.0}})
	m.observe("file_diff", map[string]any{"path": "/x"})
	m.observe("tool_use", map[string]any{"tool": "read", "state": map[string]any{"input": map[string]any{"file_path": "/a"}}}) // 1 < 3
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no stall, got %q", reason)
	}
}

// TestTokenDeltasPreventNoProgressTrip verifies the no_progress window does
// NOT trip while mid-generation token deltas are streaming: a slow local-model
// generation that streams tokens but never completes a text part must count as
// alive (the exact-300s Aborted root cause). The text_loop guard is
// intentionally NOT reset by deltas — only lastStepFinish advances.
func TestTokenDeltasPreventNoProgressTrip(t *testing.T) {
	w := stallWindows{noProgress: 10 * time.Second, noFileDiff: time.Hour, textLoop: time.Hour, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// A generation that streams deltas well past the no_progress window but
	// completes nothing stays alive.
	for i := 0; i < 12; i++ {
		m.mu.Lock()
		m.lastStepFinish = m.now().Add(-9 * time.Second) // just inside the window
		m.mu.Unlock()
		m.observe("text", map[string]any{"delta": "tok"}) // streamed token
		if reason := m.check(); reason != "" {
			t.Fatalf("expected no stall while deltas stream, got %q", reason)
		}
	}
	// Once the deltas stop, silence DOES trip the window.
	m.mu.Lock()
	m.lastStepFinish = m.now().Add(-11 * time.Second)
	m.mu.Unlock()
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("expected stalled:no_progress after silence, got %q", reason)
	}
}

// TestTokenDeltasDoNotResetTextLoop verifies delta liveness advances ONLY the
// no_progress signal: a pure-token generation that never takes a meaningful
// action still trips text_loop once that window elapses (D4 — the guard
// against an infinite single-step reasoning loop).
func TestTokenDeltasDoNotResetTextLoop(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, textLoop: 10 * time.Second, repetitionN: 100, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// A delta streams (lastStepFinish advances → no_progress stays fresh) but
	// lastMeaningfulAction is untouched → text_loop must still trip.
	m.observe("text", map[string]any{"delta": "tok"})
	m.mu.Lock()
	m.lastMeaningfulAction = m.now().Add(-11 * time.Second) // text_loop window elapsed
	m.mu.Unlock()
	if reason := m.check(); !strings.HasPrefix(reason, "stalled:text_loop") {
		t.Fatalf("expected stalled:text_loop despite delta liveness, got %q", reason)
	}
}

// TestWallClockDeadline verifies the worker's wall_clock_seconds budget
// produces a deadline.
func TestWallClockDeadline(t *testing.T) {
	// Set budget → deadline returned.
	d, ok := wallClockDeadline(nil, []byte(`{"wall_clock_seconds":30}`))
	if !ok || d.IsZero() {
		t.Fatal("expected a deadline")
	}
	if want := time.Duration(30) * time.Second; time.Until(d) > want+time.Second || time.Until(d) < want-time.Second {
		t.Fatalf("deadline = %v from now, want ~%v", time.Until(d), want)
	}
	// No budget → the documented default backstop (3600s) applies so every
	// execution has a hard timeout even when the worker doesn't opt in.
	d, ok = wallClockDeadline(nil, []byte(`{}`))
	if !ok {
		t.Fatal("expected the default deadline when wall_clock_seconds absent")
	}
	if want := defaultWallClockTimeout; time.Until(d) > want+time.Second || time.Until(d) < want-time.Second {
		t.Fatalf("default deadline = %v from now, want ~%v", time.Until(d), want)
	}
	// Zero disables the hard timeout.
	if _, ok := wallClockDeadline(nil, []byte(`{"wall_clock_seconds":0}`)); ok {
		t.Fatal("expected no deadline when wall_clock_seconds=0")
	}
	// Negative is treated as disabled too (never a past deadline).
	if _, ok := wallClockDeadline(nil, []byte(`{"wall_clock_seconds":-5}`)); ok {
		t.Fatal("expected no deadline when wall_clock_seconds negative")
	}
	// Unparseable budgets → fall back to the default backstop (never hang).
	d, ok = wallClockDeadline(nil, []byte(`not-json`))
	if !ok {
		t.Fatal("expected the default deadline on unparseable budgets")
	}
	if want := defaultWallClockTimeout; time.Until(d) > want+time.Second || time.Until(d) < want-time.Second {
		t.Fatalf("unparseable fallback = %v from now, want ~%v", time.Until(d), want)
	}
}

// TestIsFatalStall verifies which stall signals terminate the subprocess
// (routing to recovery) versus advisory-first (nudge-first routing).
func TestIsFatalStall(t *testing.T) {
	// no_progress is FATAL: total silence, no responsive surface to nudge.
	if !isFatalStall("stalled:no_progress") {
		t.Error("isFatalStall(no_progress) = false, want true (fatal)")
	}
	// text_loop / repetition / no_file_progress are ADVISORY-first: the
	// worker is generating text / issuing tool calls, so a nudge can reach
	// it. They escalate to fatal only after the nudge budget is spent.
	advisory := []string{
		"stalled:text_loop:you are talking in circles",
		"stalled:repetition:bash|[\"ls\"]",
		"stalled:no_file_progress",
	}
	for _, r := range advisory {
		if isFatalStall(r) {
			t.Errorf("isFatalStall(%q) = true, want false (advisory-first)", r)
		}
	}
}

// TestStallWindowsFromManifestUnsetAppliesDefault verifies a fresh/
// never-configured manifest (every Stall* field zero — the real state of a
// tenant_settings row that's never been touched in Settings) yields the
// real built-in defaults, not a disabled check. This is the safety case:
// zero must never silently mean "off".
func TestStallWindowsFromManifestUnsetAppliesDefault(t *testing.T) {
	w := stallWindowsFromManifest(scheduler.ExecutionManifest{})
	def := defaultStallWindows()
	if w.noFileDiff != def.noFileDiff {
		t.Fatalf("noFileDiff = %v, want built-in default %v", w.noFileDiff, def.noFileDiff)
	}
	if w.textLoop != def.textLoop {
		t.Fatalf("textLoop = %v, want built-in default %v", w.textLoop, def.textLoop)
	}
	if w.noFileDiff <= 0 || w.textLoop <= 0 {
		t.Fatal("built-in default must be a positive, live window — unset must never resolve to disabled")
	}
}

// TestStallWindowsFromManifestPositiveOverrides verifies an explicit
// positive value overrides the built-in default.
func TestStallWindowsFromManifestPositiveOverrides(t *testing.T) {
	w := stallWindowsFromManifest(scheduler.ExecutionManifest{
		StallNoFileDiffWindowSeconds: 120,
		StallTextLoopWindowSeconds:   90,
	})
	if w.noFileDiff != 120*time.Second {
		t.Fatalf("noFileDiff = %v, want 120s", w.noFileDiff)
	}
	if w.textLoop != 90*time.Second {
		t.Fatalf("textLoop = %v, want 90s", w.textLoop)
	}
}

// TestStallWindowsFromManifestNegativeDisables verifies the fix: a negative
// value (unambiguous, unlike 0 which doubles as "unset") actually disables
// the check, closing the gap where Settings claimed "0 = disabled" but nothing
// enforced it (0 and unset were indistinguishable, so the built-in default
// always applied regardless of what the operator typed). newProgressMonitor
// + a full stall check confirms it end-to-end: no trip even when the window
// has obviously elapsed and no file diff / meaningful action occurred.
func TestStallWindowsFromManifestNegativeDisables(t *testing.T) {
	w := stallWindowsFromManifest(scheduler.ExecutionManifest{
		StallNoFileDiffWindowSeconds: -1,
		StallTextLoopWindowSeconds:   -1,
	})
	if w.noFileDiff > 0 {
		t.Fatalf("noFileDiff = %v, want <= 0 (disabled)", w.noFileDiff)
	}
	if w.textLoop > 0 {
		t.Fatalf("textLoop = %v, want <= 0 (disabled)", w.textLoop)
	}

	c := &clock{t: time.Now()}
	m := newProgressMonitor("exe_disabled_stall", w)
	m.now = c.now
	m.startedAt = c.t
	m.lastStepFinish = c.t
	m.lastFileDiff = c.t
	m.lastMeaningfulAction = c.t
	// Advance 2 hours — far past what the built-in noFileDiff (15min) and
	// textLoop (10min) defaults would have tolerated, isolating the test to
	// those two dimensions by also advancing lastStepFinish (so no_progress,
	// which this manifest leaves live at its default, never trips).
	c.t = c.t.Add(2 * time.Hour)
	m.lastStepFinish = c.t
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no stall with noFileDiff/textLoop disabled, got %q", reason)
	}
}
