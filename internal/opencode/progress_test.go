package opencode

import (
	"testing"
	"time"
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

// TestStallRepetition verifies the repetition signal trips when the same
// tool_call signature exceeds the threshold within the window.
func TestStallRepetition(t *testing.T) {
	w := stallWindows{noProgress: time.Hour, noFileDiff: time.Hour, repetitionN: 3, repetitionW: time.Minute}
	m := newTestMonitor(w)
	// Feed the same tool_call 4 times (> threshold of 3).
	for i := 0; i < 4; i++ {
		m.observe("tool_call", map[string]any{"tool": "read_file", "args": map[string]any{"path": "/x"}})
	}
	reason := m.check()
	if len(reason) < len("stalled:repetition:") || reason[:len("stalled:repetition:")] != "stalled:repetition:" {
		t.Fatalf("expected stalled:repetition:..., got %q", reason)
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
	m.observe("tool_call", map[string]any{"tool": "read", "args": map[string]any{}}) // 1 < 3
	if reason := m.check(); reason != "" {
		t.Fatalf("expected no stall, got %q", reason)
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
	// Unparseable budgets → no deadline (fail safe).
	if _, ok := wallClockDeadline(nil, []byte(`not-json`)); ok {
		t.Fatal("expected no deadline on unparseable budgets")
	}
}

// TestIsFatalStall verifies which stall signals terminate the subprocess
// (routing to recovery) versus advisory-only.
func TestIsFatalStall(t *testing.T) {
	fatal := []string{
		"stalled:no_progress",
		"stalled:text_loop:you are talking in circles",
		"stalled:repetition:bash|[\"ls\"]",
	}
	for _, r := range fatal {
		if !isFatalStall(r) {
			t.Errorf("isFatalStall(%q) = false, want true (genuine hang/loop)", r)
		}
	}
	// no_file_progress is advisory: a reviewer may legitimately produce
	// output without touching files (SSE case — flagged yet completed).
	if isFatalStall("stalled:no_file_progress") {
		t.Error("isFatalStall(no_file_progress) = true, want false (advisory)")
	}
}
