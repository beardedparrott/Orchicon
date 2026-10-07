package askorchicon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
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

// The operator: "The model is constantly losing its brain. It doesn't know it's already done things and then
// tries to do them again. So holding onto sessions seems broken somewhere."
//
// A tool held by a HUMAN was counted as a wedged tool. The wedge clock is stamped when the tool is ISSUED and
// awaitingConsent only suppressed the VERDICT while the ask was open — so the moment a decision landed, an
// already-expired clock was re-read on the next tick, and the collector aborted the session and re-seeded a
// fresh one from the last 50 messages. The wait, not the tool, destroyed the context.
func TestChatStallToolWedgeDoesNotCountConsentTime(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour // isolate the wedge signal
	m.toolWedgeWindow = 30 * time.Second
	base := time.Now()
	m.now = func() time.Time { return base }

	// The model issues a bash call, and the ask goes out. The operator reads it for two minutes — well past
	// the window, because a human is not a stalled tool.
	m.observeToolStart("bash")
	m.setAwaitingConsent(true)
	m.now = func() time.Time { return base.Add(2 * time.Minute) }
	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("a tool held by the operator was reported wedged: %q", tool)
	}

	// The decision lands. The tool has NOT been running — it starts running NOW, so its clock does too.
	m.setAwaitingConsent(false)
	m.now = func() time.Time { return base.Add(2*time.Minute + 20*time.Second) }
	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("the expiry of the CONSENT WAIT recycled the session %q after 20s of actual tool time", tool)
	}

	// A genuinely hung tool is still caught, on a clock that now measures the tool.
	m.now = func() time.Time { return base.Add(2*time.Minute + 31*time.Second) }
	tool, wedged := m.toolWedge()
	if !wedged || tool != "bash" {
		t.Fatalf("toolWedge = (%q, %v), want a wedge on bash once the TOOL has been silent past the window", tool, wedged)
	}
}

// Disarming the gate with NO tool in flight must not arm one: the next tick would then find a wedge that does
// not exist.
func TestChatStallConsentDisarmWithoutAnOpenToolIsInert(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = 30 * time.Second
	base := time.Now()
	m.now = func() time.Time { return base }

	m.setAwaitingConsent(true)
	m.now = func() time.Time { return base.Add(10 * time.Minute) }
	m.setAwaitingConsent(false)

	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("toolWedge = (%q, true) with no tool ever issued, want no wedge", tool)
	}
}

// The native adapter resolves a tool with a typed "tool_result" and emits no "tool_use" part, so the wedge
// slot has to be closed by that event. Without it a bash call stayed open from its START, and a silent
// command longer than the window — a build, a test suite — was recycled as an MCP wedge with no ask involved.
func TestChatStallCloseToolEndsTheWedgeSlot(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = 30 * time.Second
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(5 * time.Minute) }
	m.closeTool()

	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("a RESOLVED tool was reported wedged: %q", tool)
	}
}

// The operator's prod plane carried 13 "session wedged on a tool — recycling to a fresh session" entries in two
// days, every one of them tool="bash" on the native (in-process) transport — each destroying the session's
// context over a shell command that was still running. The host suite's bash bounds itself (bashTimeoutDefault
// 120s / bashTimeoutMax 600s), so a slow call is not a wedged call.
func TestChatStallLocallyBoundedToolIsNeverAWedge(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = 30 * time.Second
	m.setLocallyBoundedTools(hostSuiteToolNames)
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("bash")
	// Ten minutes of silence from a build or a test suite — past bash's own default deadline, and past the
	// wedge window several times over.
	m.now = func() time.Time { return base.Add(10 * time.Minute) }
	if tool, wedged := m.toolWedge(); wedged {
		t.Fatalf("a shell command the transport is RUNNING was reported wedged as %q, and the recycle would have erased the session", tool)
	}
}

// THE CONTROL, and the reason the exemption is per-tool and not per-adapter: the same transport also runs MCP
// tools, which leave this process and CAN wedge with nothing to report. Recovery for the case the signal exists
// for must survive.
func TestChatStallUnboundedToolStillWedgesOnAnInProcessTransport(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = 30 * time.Second
	m.setLocallyBoundedTools(hostSuiteToolNames)
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("mcp__sentry__list_issues")
	m.now = func() time.Time { return base.Add(time.Minute) }
	tool, wedged := m.toolWedge()
	if !wedged || tool != "mcp__sentry__list_issues" {
		t.Fatalf("toolWedge = (%q, %v), want the MCP call reported wedged so the session can be healed", tool, wedged)
	}
}

// AND THE SECOND CONTROL: with no locally-bounded set — i.e. a SERVE-side transport, where the same "bash" is
// executed out of process and silence really is the only evidence — the wedge must stay armed.
func TestChatStallServeSideTransportKeepsTheWedgeForEveryTool(t *testing.T) {
	m := newChatStallMonitor("opencode/deepseek-v4-flash-free", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = 30 * time.Second
	base := time.Now()
	m.now = func() time.Time { return base }

	m.observeToolStart("bash")
	m.now = func() time.Time { return base.Add(time.Minute) }
	if _, wedged := m.toolWedge(); !wedged {
		t.Fatal("a serve-side transport must keep the wedge: a call it dispatched is invisible to us, so silence is all we have")
	}
}

// The capability gate itself: only a transport that DECLARES in-process tool execution is exempted, and a
// transport that declares nothing gets the default (armed).
func TestInProcessToolRunnerOnlyForDeclaringTransports(t *testing.T) {
	if inProcessToolRunner(nil) {
		t.Fatal("a nil transport must not be treated as an in-process tool runner")
	}
	if !inProcessToolRunner(inProcTransport{}) {
		t.Fatal("a transport declaring ToolsRunInProcess must be recognized")
	}
	if inProcessToolRunner(serveTransport{}) {
		t.Fatal("a transport that does not declare the capability must keep the wedge armed")
	}
	if !inProcessToolRunner(&orchicon.NativeBridge{}) {
		t.Fatal("the native bridge runs the host suite in-process and must declare it")
	}
}

// inProcTransport / serveTransport are minimal declarations of the OPTIONAL capability: the embedded nil
// interface supplies the rest of ChatTurnClient and panics if it is ever called (it never is — the capability
// is read, not exercised).
type inProcTransport struct {
	scheduler.ChatTurnClient
}

func (inProcTransport) ToolsRunInProcess() bool { return true }

type serveTransport struct{ scheduler.ChatTurnClient }

// THE OPERATOR'S REPORT, pinned: "No card ever came to me. That is why you may have been waiting for
// approval."
//
// awaitingConsent gated ONLY the tool-wedge signal. The no-progress clock measures from lastActivity, which
// an ASK does not advance — so a card sitting unanswered read as "no activity from the model", the turn was
// aborted at the window, and the abort resolved the outstanding ask as consentCancelled. The model was then
// told its APPROVAL had been cancelled when the STALL MONITOR had killed the turn: a conversation that died
// waiting for a card surfaces as a consent error, and from the transcript the two are indistinguishable.
func TestChatStallNoProgressDoesNotCountConsentTime(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = 2 * time.Minute
	m.toolWedgeWindow = time.Hour // isolate the no-progress signal
	base := time.Now()
	m.now = func() time.Time { return base }

	// A bash call goes out and the ask is raised. The operator takes five minutes — far past the window,
	// because a person reading a card is not a stalled model.
	m.observeToolStart("bash")
	m.setAwaitingConsent(true)
	m.now = func() time.Time { return base.Add(5 * time.Minute) }
	if r := m.stallReason(); r != "" {
		t.Fatalf("a turn waiting on the operator was declared stalled: %q — the abort then reports an APPROVAL error, which is how a missing card reads as a consent failure", r)
	}

	// The decision lands. The clock restarts, so the turn is not killed one tick later on a clock that had
	// been running the whole time the human was reading.
	m.setAwaitingConsent(false)
	m.now = func() time.Time { return base.Add(5*time.Minute + 30*time.Second) }
	if r := m.stallReason(); r != "" {
		t.Fatalf("the turn was declared stalled 30s after the operator answered, on a clock that ran while they read: %q", r)
	}

	// A model that THEN genuinely goes quiet is still caught — a full window after the decision, not never.
	m.now = func() time.Time { return base.Add(5*time.Minute + 2*time.Minute + time.Second) }
	if r := m.stallReason(); !strings.Contains(r, "no_progress") {
		t.Fatalf("stallReason = %q, want a no_progress trip once the MODEL has been silent past its window", r)
	}
}

// Repetition is about the MODEL looping, which a pending ask neither causes nor excuses — so it stays armed
// while a consent is outstanding. Gating it too would have been the easy over-correction.
func TestChatStallRepetitionStillFiresWhileAwaitingConsent(t *testing.T) {
	m := newChatStallMonitor("orchicon/deepseek/deepseek-flash", nil)
	m.noProgressWindow = time.Hour
	m.toolWedgeWindow = time.Hour
	m.repetitionCount = 2
	m.repetitionWindow = time.Hour
	base := time.Now()
	m.now = func() time.Time { return base }

	m.setAwaitingConsent(true)
	tool := map[string]any{"type": "tool", "tool": "bash", "input": map[string]any{"command": "ls"}}
	for i := 0; i < 4; i++ {
		m.observe("tool_use", tool)
	}
	if r := m.stallReason(); !strings.Contains(r, "repetition") {
		t.Fatalf("stallReason = %q, want a repetition trip — a looping model is a loop whether or not an ask is open", r)
	}
}
