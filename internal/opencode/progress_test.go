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
	// No budget → no deadline.
	if _, ok := wallClockDeadline(nil, []byte(`{}`)); ok {
		t.Fatal("expected no deadline when wall_clock_seconds absent")
	}
	// Zero disables.
	if _, ok := wallClockDeadline(nil, []byte(`{"wall_clock_seconds":0}`)); ok {
		t.Fatal("expected no deadline when wall_clock_seconds=0")
	}
}
