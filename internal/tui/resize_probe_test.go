package tui

// resize_probe_test.go — the two rules that keep the Windows size probe invisible.
//
// The probe exists because the input path that gives Windows bracketed paste and Shift+Enter COSTS bubbletea
// its console resize event (reader_windows.go, resize_probe.go). It is a quarter-second poll, so the thing
// that must be proved is that polling cannot be FELT: an unreadable size is ignored, and an unchanged size
// never reaches the renderer — the standard renderer repaints on every WindowSizeMsg, so forwarding an
// identical size four times a second would repaint an idle chat continuously.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSizeProbeIgnoresAnUnreadableSize pins that "no information" is not "size zero": a terminal that will
// not answer must not resize the shell to nothing.
func TestSizeProbeIgnoresAnUnreadableSize(t *testing.T) {
	for _, probe := range []terminalSizeProbeMsg{
		{},
		{width: 0, height: 40},
		{width: 120, height: 0},
		{width: -1, height: -1},
	} {
		if got := windowSizeFromProbe(probe, 120, 40); got != nil {
			t.Errorf("probe %+v must be ignored, got %#v", probe, got)
		}
	}
}

// TestSizeProbeDropsAnUnchangedSize is the anti-repaint rule. Without it the probe would force a full
// repaint on every tick of an otherwise idle screen.
func TestSizeProbeDropsAnUnchangedSize(t *testing.T) {
	if got := windowSizeFromProbe(terminalSizeProbeMsg{width: 120, height: 40}, 120, 40); got != nil {
		t.Fatalf("an unchanged size must not be forwarded (it would repaint an idle screen every tick), got %#v", got)
	}
}

// TestSizeProbeForwardsARealChange pins the case the probe exists for: a window drag.
func TestSizeProbeForwardsARealChange(t *testing.T) {
	cases := []struct {
		name               string
		probe              terminalSizeProbeMsg
		currentW, currentH int
		wantW, wantH       int
	}{
		{"wider", terminalSizeProbeMsg{width: 140, height: 40}, 120, 40, 140, 40},
		{"taller", terminalSizeProbeMsg{width: 120, height: 50}, 120, 40, 120, 50},
		{"both", terminalSizeProbeMsg{width: 140, height: 50}, 120, 40, 140, 50},
		// FIRST SIGHT is a change even against a zero shell: the shell may never have been sized if the
		// initial one-shot read failed, and refusing this would leave it at zero columns forever.
		{"first sight", terminalSizeProbeMsg{width: 100, height: 30}, 0, 0, 100, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := windowSizeFromProbe(c.probe, c.currentW, c.currentH).(tea.WindowSizeMsg)
			if !ok {
				t.Fatalf("a real change must be forwarded as a tea.WindowSizeMsg")
			}
			if got.Width != c.wantW || got.Height != c.wantH {
				t.Errorf("forwarded %dx%d, want %dx%d", got.Width, got.Height, c.wantW, c.wantH)
			}
		})
	}
}

// TestSizeProbeChainStartsOnlyWhereItIsNeeded pins the platform gate: on a platform that delivers its own
// resize events the probe is nil, so the chain must never start, and on one that needs the poll it must.
func TestSizeProbeChainStartsOnlyWhereItIsNeeded(t *testing.T) {
	armed := sizeProbeTickCmd() != nil
	needed := resizeProbeCmd() != nil
	if armed != needed {
		t.Fatalf("size probe chain armed=%v but a poll is needed=%v — the chain must follow the platform", armed, needed)
	}
}
