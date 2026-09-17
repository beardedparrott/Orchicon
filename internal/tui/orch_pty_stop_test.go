// Package tui — orch_pty_stop_test.go — the STOP chord (ctrl+y) MEASURED on a real pty.
//
// WHY THIS EXISTS AT ALL. Every chord in this client is measured rather than assumed, and the reason is
// that the failure mode is INVISIBLE to unit tests: a key the terminal never delivers, and a key the
// composer's textarea eats before any route sees it, both look exactly like a passing dispatch test
// (the test builds the KeyMsg by hand; a real terminal has to produce it). This client has already been
// bitten by control bytes that do not exist as keys — ctrl+3 arrives as ESC and ctrl+8 as backspace,
// ctrl+1 arrives as ctrl+q (see the tab-chord note in app.go) — so "ctrl+y is a control byte, therefore
// it will arrive" is precisely the kind of assumption that note refuses to make.
//
// The probe drives the REAL bin/orch inside a real pty and writes the raw 0x19 byte a terminal
// produces, then asserts the shell's own answer is painted. That proves delivery, the bubbletea parse,
// the key-route dispatch, the textarea bypass (composerBypassKeys), and the visible acknowledgement —
// end to end, at the only layer where all five are true or false at once.
package tui

import (
	"strings"
	"testing"
	"time"
)

// TestPTYStopChordArrivesAndSaysWhyItDidNothing presses ctrl+y with no reply in flight and expects the
// shell to name its own reason. The idle path is the right probe: it needs no server-side turn, and a
// silent result is exactly the "bound but dead" failure this test is for.
//
// The painted stream is STRIPPED with the harness's own stripANSI (orch_pty_mouse_test.go) before
// matching, the same way every other pty assertion in this package reads the screen: a row the operator
// reads as one sentence is bytes interleaved with per-cell styling, and matching raw would be matching
// the renderer's escape layout rather than what is on screen.
func TestPTYStopChordArrivesAndSaysWhyItDidNothing(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty stop-chord probe: skipped in -short")
	}
	bin := orchBinPath(t)
	plane := connectPlaneFixture(t)
	home := t.TempDir()
	writeOrchConfig(t, home, plane.URL)
	s := startOrchPtyAt(t, bin, plane.URL, home)
	defer s.close()

	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "❯") {
		t.Fatalf("shell never painted the composer (%d bytes)", len(out))
	}

	// ctrl+y as the terminal sends it: the single control byte 0x19.
	//
	// The frame is POLLED rather than sampled once: the notice is painted on the repaint that follows
	// the keypress, and a single fixed window measures the app's frame timing (which the startup RPC
	// fan-out can delay) instead of whether the chord is reachable at all. Measured while building
	// this: the same probe passed and failed run-to-run against a 2s window, and instrumenting the
	// dispatch proved the byte arrives (DBGKEY[ctrl+y]) and the route fires, so the flake was the
	// probe's timing, not the chord.
	_, _ = s.tty.WriteString("\x19")
	deadline := time.Now().Add(8 * time.Second)
	var painted string
	for time.Now().Before(deadline) {
		painted = stripANSI(s.readFor(500 * time.Millisecond))
		if strings.Contains(painted, "nothing to stop") {
			return // the chord arrived, the route fired, and the shell's answer is on screen
		}
	}
	// DIAGNOSTIC: locate the fragments, so a failure distinguishes "never painted" from "painted but
	// split by a wrap" — the two look identical to a single substring assertion.
	t.Logf("DIAG len=%d prompt=%v nothing=%v stop=%v reply_in_flight=%v",
		len(painted), strings.Contains(painted, "❯"), strings.Contains(painted, "nothing"),
		strings.Contains(painted, "stop"), strings.Contains(painted, "reply in flight"))
	for _, probe := range []string{"stop", "conversation open", "nothing to"} {
		if i := strings.Index(painted, probe); i >= 0 {
			lo := i - 120
			if lo < 0 {
				lo = 0
			}
			hi := i + 200
			if hi > len(painted) {
				hi = len(painted)
			}
			t.Logf("DIAG around %q: %q", probe, painted[lo:hi])
		}
	}
	t.Fatalf("the stop chord never reached the shell's stop control: the byte was delivered but no key "+
		"press was recognised (or the composer ate it before the route could see it), so ctrl+y is "+
		"bound-but-dead in a real terminal\n--- painted tail (stripped) ---\n%s", tailOfPlain(painted, 3000))
}

// tailOfPlain returns the last n characters of a STRIPPED frame — the rows as the operator reads them.
func tailOfPlain(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
