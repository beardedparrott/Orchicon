package tui

// orch_pty_ss3_test.go — THE SS3 HOME/END FIX IN A REAL TERMINAL.
//
// The operator: "On my keyboard I have to hold fn+left/right to do a home/end. That
// should matter though. If I click into the conversation so it has focus it doesn't work
// and when I do it inside the composer I get the following OH and OF."
//
// The second sentence is the diagnosis, and it names the layer: "OH" and "OF" are not a
// focus problem and not a missing chord — they are bubbletea reading a byte sequence it
// does not decode as ORDINARY CHARACTERS. The keyboard's Home/End emits the SS3
// encoding (ESC O H / ESC O F); the library decodes only the CSI spellings, so ESC+O
// became alt+O and the letter became a rune, and the composer inserted both.
//
// This gate drives the real bin/orch in a real pty and writes those raw bytes, so it
// observes the REPORTED SYMPTOM itself rather than its cause. Opt-in (ORCH_PTY_SMOKE=1).
//
// THE TRANSCRIPT-JUMP HALF IS ASSERTED ELSEWHERE, deliberately. This fixture's
// conversation is two messages and fits the pane, so home/end have nowhere to travel
// and an assertion about the scroll offset would be vacuous. What IS observable here is
// the thing that was actually broken — whether the key arrives as a Home/End key at all
// — and that is what these assertions pin, using the textarea as the surface that proves
// it (it is the same KeyMsg the transcript's route receives). The jump itself is
// measured in findbox_test.go against a parked 80-line transcript, where it can be.

import (
	"strings"
	"testing"
	"time"
)

func TestPTYSS3HomeEndReachesTheComposerAsRealKeys(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)
	planeURL, _, _ := activityE2EPlane(t)
	home := t.TempDir()
	writeOrchConfig(t, home, planeURL)

	const cols, rows = 120, 40
	s := startOrchPtySized(t, bin, planeURL, home, cols, rows)
	defer s.close()

	// The shell opens COMPOSER-focused, so the bytes below land where the operator's
	// Home/End landed when they reported the litter.
	waitForFrame(t, s, cols, rows, 8*time.Second, "❯")
	time.Sleep(5 * time.Second)

	composer := func() string {
		for _, l := range strings.Split(screenText(replay(t, s, cols, rows)), "\n") {
			if strings.Contains(l, "❯") {
				return strings.TrimSpace(l)
			}
		}
		return ""
	}

	// (1) THE REPORTED SYMPTOM, GONE. Before the fix these two writes left the composer
	//     holding "OH" and "OF" — byte-for-byte what the operator photographed.
	_, _ = s.tty.WriteString("\x1bOH")
	time.Sleep(1200 * time.Millisecond)
	if got := composer(); strings.Contains(got, "OH") || strings.Contains(got, "OF") {
		t.Fatalf("the SS3 Home sequence still lands as literal text: %q\n"+
			"bubbletea splits ESC O H into alt+O and the rune H, and the composer inserts both", got)
	}

	// (2) AND IT IS NOW A REAL KEY. Typing at the END and then pressing Home must put the
	//     next character at the START — which only happens if the bytes became the Home
	//     key. Without the fix the buffer would read "abcOHX"; with it, "Xabc".
	_, _ = s.tty.WriteString("abc")
	time.Sleep(600 * time.Millisecond)
	_, _ = s.tty.WriteString("\x1bOH")
	time.Sleep(600 * time.Millisecond)
	_, _ = s.tty.WriteString("X")
	time.Sleep(800 * time.Millisecond)
	got := composer()
	if !strings.Contains(got, "Xabc") {
		t.Fatalf("after typing \"abc\", pressing SS3 Home and typing \"X\", the composer reads %q — "+
			"want \"Xabc\". The sequence is not arriving as the Home key, so it cannot move the "+
			"caret here NOR jump the transcript.", got)
	}

	// (3) END IS THE SAME KEY, the other way: from the start of the line, End must put the
	//     next character at the END.
	_, _ = s.tty.WriteString("\x1bOF")
	time.Sleep(600 * time.Millisecond)
	_, _ = s.tty.WriteString("Y")
	time.Sleep(800 * time.Millisecond)
	got = composer()
	if !strings.Contains(got, "XabcY") {
		t.Fatalf("after pressing SS3 End and typing \"Y\", the composer reads %q — want \"XabcY\"", got)
	}

	// (4) THE ARROWS STILL WORK, which is the negative control at the terminal level: the
	//     application-cursor arrows are a DIFFERENT member of the same SS3 family and
	//     bubbletea already decodes them, so the fix must have left them alone. Up must
	//     move the textarea's caret to the line start without inserting anything.
	_, _ = s.tty.WriteString("\x1bOA")
	time.Sleep(600 * time.Millisecond)
	_, _ = s.tty.WriteString("Z")
	time.Sleep(800 * time.Millisecond)
	got = composer()
	if strings.Contains(got, "OA") {
		t.Fatalf("the application-cursor Up sequence now lands as literal text: %q — the rewrite must "+
			"only touch Home and End", got)
	}
	if !strings.Contains(got, "Z") {
		t.Fatalf("Up did not reach the composer as a key: %q", got)
	}
}
