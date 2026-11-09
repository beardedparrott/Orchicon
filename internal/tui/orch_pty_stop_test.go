// Package tui — orch_pty_stop_test.go — the STOP chord (ctrl+y) MEASURED on a real pty.
package tui

import (
	"strings"
	"testing"
	"time"
)

// TestPTYStopChordArrivesAndSaysWhyItDidNothing presses ctrl+y with no reply in flight and expects
// the shell to name its own reason.
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
