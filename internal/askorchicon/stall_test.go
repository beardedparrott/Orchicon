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
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
	m.noProgressWindow = 100 * time.Millisecond
	base := time.Now()
	m.now = func() time.Time { return base }

	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason before window = %q, want empty", reason)
	}
	// Advance past the window with no activity. The reason must name the
	// model so the user sees WHICH model wedged.
	m.now = func() time.Time { return base.Add(200 * time.Millisecond) }
	reason := m.stallReason()
	if !strings.HasPrefix(reason, "stalled:no_progress") {
		t.Fatalf("stallReason = %q, want stalled:no_progress", reason)
	}
	if !strings.Contains(reason, "deepseek-v4-flash-free") {
		t.Fatalf("stallReason = %q, want model ref named", reason)
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
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
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
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
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
	m2 := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
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

// TestChatStallMonitorRepetitionOpenCodeShape verifies the REGRESSION that
// killed legitimate turns: opencode v1.x tool parts nest the tool input
// under `state.input` (see LegacyEventFromBus), not `input`/`args`. Reading
// the top-level fields collapsed every bash call to `bash|null`, so a model
// running 8 DIFFERENT git commands looked like "8 repeats of bash|null" and
// tripped the repetition detector. Distinct state.input values must produce
// distinct signatures.
func TestChatStallMonitorRepetitionOpenCodeShape(t *testing.T) {
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
	m.noProgressWindow = time.Hour
	m.repetitionWindow = time.Hour
	m.repetitionCount = 3
	base := time.Now()
	m.now = func() time.Time { return base }

	// 8 DIFFERENT bash commands with the real opencode part shape.
	for i := 0; i < 8; i++ {
		m.observe("tool_use", map[string]any{
			"type": "tool",
			"tool": "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"command": fmt.Sprintf("git -C /repo log --oneline -%d", i)},
			},
		})
	}
	if reason := m.stallReason(); reason != "" {
		t.Fatalf("stallReason = %q, want empty (8 distinct bash commands are not a loop)", reason)
	}

	// The SAME command repeated is still caught.
	for i := 0; i < 5; i++ {
		m.observe("tool_use", map[string]any{
			"type": "tool",
			"tool": "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]any{"command": "git pull"},
			},
		})
	}
	reason := m.stallReason()
	if !strings.HasPrefix(reason, "stalled:repetition") {
		t.Fatalf("stallReason = %q, want stalled:repetition (same command loop)", reason)
	}
	if !strings.Contains(reason, "deepseek-v4-flash-free") {
		t.Fatalf("stallReason = %q, want model ref named", reason)
	}
}

// TestFoldReasoningTail pins the one piece of the exit path that can be silently wrong in
// two opposite directions.
//
// The live reasoning tail and the segmenter's flush describe the SAME bytes from two
// directions: the tail is everything that went into the live buffer, while `flushed` is the
// single folded body the flush just committed to the durable slice. Appending the tail
// unchanged duplicates that body (a doubled thinking bubble); dropping the tail loses the
// native reasoning deltas, which nothing else ever commits (a missing one). Neither shows up
// in a happy-path turn, so the rule is pinned here rather than inferred from one.
func TestFoldReasoningTail(t *testing.T) {
	cases := []struct {
		name    string
		tail    string
		flushed string
		want    string
	}{
		{
			name: "nothing flushed leaves the native tail intact",
			// The ordinary native case: reasoning deltas, no folded run open at all.
			tail:    "weighing the options",
			flushed: "",
			want:    "weighing the options",
		},
		{
			name:    "the flushed body is stripped so it is not recorded twice",
			tail:    "a folded body",
			flushed: "a folded body",
			want:    "",
		},
		{
			name: "native reasoning BEFORE a folded run keeps only the native part",
			// The realistic mixed case, and the reason a SUFFIX strip is the right rule:
			// a terminated folded body RESETS the live buffer and is committed at that
			// moment (the segmenter's terminal callback), so a committed body can never sit
			// in the MIDDLE of the tail — only an UNTERMINATED one is still in it, and that
			// one is always its suffix. Native deltas that accumulated before the run opened
			// are the part that must survive.
			tail:    "native firstfolded body",
			flushed: "folded body",
			want:    "native first",
		},
		{
			name: "an unrelated flush does not eat the tail",
			// The conservative direction: the worst case must be a duplicate, never a loss.
			tail:    "native tail",
			flushed: "something else entirely",
			want:    "native tail",
		},
		{
			name:    "an empty tail stays empty",
			tail:    "",
			flushed: "a folded body",
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldReasoningTail(tc.tail, tc.flushed); got != tc.want {
				t.Fatalf("foldReasoningTail(%q, %q) = %q, want %q", tc.tail, tc.flushed, got, tc.want)
			}
		})
	}
}
