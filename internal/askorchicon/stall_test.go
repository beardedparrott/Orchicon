package askorchicon

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestChatStallMonitorNoProgress verifies the no_progress signal: a monitor
// fed no activity for longer than the window trips once with a stall reason.
func TestChatStallMonitorNoProgress(t *testing.T) {
	m := newChatStallMonitor()
	m.noProgressWindow = 100 * time.Millisecond
	base := time.Now()
	m.now = func() time.Time { return base }

	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason before window = %q, want empty", reason)
	}
	// Advance past the window with no activity.
	m.now = func() time.Time { return base.Add(200 * time.Millisecond) }
	if reason := m.stallReason(); !strings.HasPrefix(reason, "stalled:no_progress") {
		t.Fatalf("stallReason = %q, want stalled:no_progress", reason)
	}
	// Latched: subsequent checks return empty.
	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason after latch = %q, want empty (fires once)", reason)
	}
}

// TestChatStallMonitorActivityResetsClock verifies ANY activity (text,
// reasoning, step_finish, tool_use) resets the no-progress clock — a model
// producing output, even a long reasoning streak, is never reaped.
func TestChatStallMonitorActivityResetsClock(t *testing.T) {
	m := newChatStallMonitor()
	m.noProgressWindow = 100 * time.Millisecond
	base := time.Now()
	m.now = func() time.Time { return base }

	// Activity at t=0 keeps advancing.
	for i := 0; i < 100; i++ {
		tick := base.Add(time.Duration(i) * 90 * time.Millisecond)
		m.now = func() time.Time { return tick }
		m.observe("text", map[string]any{"text": "still writing"})
		if reason := m.stallReason(); reason != "" {
			t.Fatalf("stallReason = %q at tick %d, want empty (activity resets)", reason, i)
		}
	}
	// Reasoning counts as activity too.
	m.now = func() time.Time { return base.Add(50 * time.Millisecond) }
	m.observe("reasoning", map[string]any{"text": "thinking"})
	m.now = func() time.Time { return base.Add(140 * time.Millisecond) }
	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason = %q, want empty (reasoning is activity)", reason)
	}
}

// TestChatStallMonitorRepetition verifies the repetition signal: the same
// tool_use signature repeated more than the count within the window trips,
// while distinct signatures (or a single call) never do.
func TestChatStallMonitorRepetition(t *testing.T) {
	m := newChatStallMonitor()
	m.noProgressWindow = time.Hour
	m.repetitionWindow = time.Hour
	m.repetitionCount = 3
	base := time.Now()
	m.now = func() time.Time { return base }

	tool := map[string]any{"type": "tool", "tool": "orchicon_list_projects", "input": map[string]any{"dir": "src"}}

	// Three repeats is at the threshold (count=3 → more than 3 trips).
	m.observe("tool_use", tool)
	m.observe("tool_use", tool)
	m.observe("tool_use", tool)
	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason at count 3 = %q, want empty (needs > count)", reason)
	}
	m.observe("tool_use", tool)
	if reason := m.stallReason(); !strings.HasPrefix(reason, "stalled:repetition") {
		t.Fatalf("stallReason at count 4 = %q, want stalled:repetition", reason)
	}

	// Distinct signatures must not trip: reset and feed 10 different calls.
	m2 := newChatStallMonitor()
	m2.noProgressWindow = time.Hour
	m2.repetitionWindow = time.Hour
	m2.repetitionCount = 3
	m2.now = func() time.Time { return base }
	for i := 0; i < 10; i++ {
		m2.observe("tool_use", map[string]any{"tool": "orchicon_list_projects", "input": map[string]any{"dir": fmt.Sprintf("src-%d", i)}})
	}
	if reason := m2.stallReason(); reason != "" {
		t.Fatalf("stallReason = %q, want empty (distinct signatures are not a loop)", reason)
	}
}
