package orchicon

import (
	"testing"
	"time"
)

// The regression these lock down: `stalled:no_progress` fired FATAL while a legitimate long tool was
// executing, because the window is measured from the last model text and NO event can arrive while the
// loop is blocked inside executeTools. Measured on run 01M8FE5CWEZ9WP7QXQREDJDM9D, three executions in
// a row, each mid-build/test-sweep.

func TestNoProgressDoesNotFireWhileToolInFlight(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second, toolHang: 0})
	m.now = func() time.Time { return now }

	// A tool starts, then the session goes quiet for twice the no-progress window: that is a tool
	// WORKING, not a model that stopped.
	m.observeToolStart("bash")
	now = now.Add(120 * time.Second)

	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q while a tool was in flight, want no stall — tool time is not model silence", reason)
	}
}

func TestNoProgressFiresWhenModelGoesQuietWithNoTool(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second})
	m.now = func() time.Time { return now }

	// No tool in flight and no model output past the window: a genuinely silent model must still be
	// caught, or the suppression would have removed the detector instead of narrowing it.
	now = now.Add(61 * time.Second)
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("check() = %q, want stalled:no_progress for a silent model with no tool running", reason)
	}
}

// TestLongToolThenResolveGetsAFreshWindow is the follow-on trap: a tool that outlasts the window must
// not hand the model a deadline that has ALREADY passed the moment the tool returns, or the abort just
// moves one tick later.
func TestLongToolThenResolveGetsAFreshWindow(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second})
	m.now = func() time.Time { return now }

	m.observeToolStart("bash")
	now = now.Add(300 * time.Second) // a five-minute build
	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q mid-tool, want none", reason)
	}

	// The tool resolves. The model is about to be sent a fresh request, so it is owed a full window.
	m.observeToolCall("bash", `{"command":"go build ./..."}`, false)
	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q immediately after a long tool resolved, want none — the model was never given a window", reason)
	}

	// And the window still works afterwards.
	now = now.Add(61 * time.Second)
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("check() = %q after the fresh window expired, want stalled:no_progress", reason)
	}
}

// TestNoProgressStillFatalWhenToolHangDisabled pins the interaction with the tool_hang watchdog: even
// with the hang detector switched OFF, a silent model with a tool nominally in flight is not reported
// by no_progress (the suppression is unconditional), which is why the hang watchdog's own escalation
// path must stay the thing that bounds a wedged tool.
func TestToolHangStillDetectsAWedgeIndependentOfNoProgress(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{
		noProgress: 60 * time.Second,
		toolHang:   30 * time.Second,
	})
	m.now = func() time.Time { return now }

	m.observeToolStart("bash")
	now = now.Add(31 * time.Second)

	// tool_hang is checked BEFORE no_progress and fires on a call with zero events, so a wedged tool is
	// still caught while a working one is not.
	if reason := m.check(); reason != "stalled:tool_hang:bash" {
		t.Fatalf("check() = %q, want stalled:tool_hang:bash — a wedged tool must still be detected", reason)
	}
}
