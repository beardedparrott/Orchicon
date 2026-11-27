package opencode

import (
	"testing"
	"time"
)

// Parity with internal/orchicon/progress_tool_time_test.go: both engines must agree that tool time is
// not model silence. The measured case is run 01M8FE5CWEZ9WP7QXQREDJDM9D, where the last thing the
// worker said before being killed was "Now booting the disposable plane."

func TestOpencodeNoProgressDoesNotFireWhileToolInFlight(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second})
	m.now = func() time.Time { return now }

	m.observeToolStart("bash")
	now = now.Add(120 * time.Second)

	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q while a tool was in flight, want no stall", reason)
	}
}

func TestOpencodeNoProgressFiresWhenModelGoesQuietWithNoTool(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second})
	m.now = func() time.Time { return now }

	now = now.Add(61 * time.Second)
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("check() = %q, want stalled:no_progress for a silent model with no tool", reason)
	}
}

func TestOpencodeLongToolThenResolveGetsAFreshWindow(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{noProgress: 60 * time.Second})
	m.now = func() time.Time { return now }

	m.observeToolStart("bash")
	now = now.Add(300 * time.Second)
	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q mid-tool, want none", reason)
	}

	// The call resolves (a completed tool part): the model gets a full window from here.
	m.observe("tool_use", map[string]any{
		"tool":  "bash",
		"state": map[string]any{"status": "completed", "input": map[string]any{"command": "go build ./..."}},
	})
	if reason := m.check(); reason != "" {
		t.Fatalf("check() = %q right after the long tool resolved, want none", reason)
	}

	now = now.Add(61 * time.Second)
	if reason := m.check(); reason != "stalled:no_progress" {
		t.Fatalf("check() = %q after the fresh window expired, want stalled:no_progress", reason)
	}
}

// TestOpencodeToolHangStillDetectsAWedge pins that narrowing no_progress did not remove the ceiling on a
// genuinely wedged tool: tool_hang is checked first and still fires.
func TestOpencodeToolHangStillDetectsAWedge(t *testing.T) {
	now := time.Now()
	m := newProgressMonitor("exec", stallWindows{
		noProgress: 60 * time.Second,
		toolHang:   30 * time.Second,
	})
	m.now = func() time.Time { return now }

	m.observeToolStart("bash")
	now = now.Add(31 * time.Second)

	if reason := m.check(); reason != "stalled:tool_hang:bash" {
		t.Fatalf("check() = %q, want stalled:tool_hang:bash", reason)
	}
}
