package tui

// orch_pty_find_test.go — THE FIND BOX IN A REAL TERMINAL.

import (
	"strings"
	"testing"
	"time"
)

func TestPTYFindBoxOverARealConversation(t *testing.T) {
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

	scr := openActivityConversation(t, s, cols, rows)

	// (1) THE COMPOSER ADVERTISES THE KEYS — the operator's second ask, on the row they look at: "no
	// note in the composer about search or jumping to beginning to end is there so no one would know.
	shell := screenText(scr)
	for _, want := range []string{"ctrl+f: find", "home/end: top·bottom"} {
		if !strings.Contains(shell, want) {
			t.Fatalf("the composer's affordance row never advertised %q — the keys exist but nothing "+
				"says so, which is the report.\n---\n%s\n---", want, tailOfPlain(shell, 2500))
		}
	}

	// (2) CTRL+F PAINTS THE BOX, with a live count of the conversation that is behind it.
	_, _ = s.tty.WriteString("\x06")
	box := waitForFrame(t, s, cols, rows, 8*time.Second, "find in transcript")
	boxText := screenText(box)
	for _, want := range []string{"find in transcript", "find > ", "type a term to filter"} {
		if !strings.Contains(boxText, want) {
			t.Fatalf("the box never painted %q.\n---\n%s\n---", want, tailOfPlain(boxText, 2500))
		}
	}
	t.Logf("box on screen: %s", strings.Join(boxRows(boxText), " | "))

	// AND IT SAYS THE BOX'S KEYS, not the transcript's — while the box holds the keyboard those chords
	// are inert, and a row that kept advertising them would be describing a state the operator is not.
	if !strings.Contains(boxText, "enter: keep searching") {
		t.Errorf("while the box is up the composer row must describe the BOX's keys.\n%s",
			tailOfPlain(boxText, 2000))
	}

	// (3) TYPING FILTERS THE REAL TRANSCRIPT, live.
	_, _ = s.tty.WriteString("zzqqxx-not-in-this-conversation")
	typed := waitForFrame(t, s, cols, rows, 6*time.Second, "no matches in")
	if !strings.Contains(screenText(typed), "no matches in") {
		t.Fatalf("typing into the box did not filter the transcript.\n---\n%s\n---",
			tailOfPlain(screenText(typed), 2500))
	}
	t.Logf("after typing: %s", strings.Join(boxRows(screenText(typed)), " | "))

	// (4) ESC CANCELS: the box closes and the transcript's own advertisement comes back.
	_, _ = s.tty.WriteString("\x1b")
	after := waitForFrame(t, s, cols, rows, 6*time.Second, "home/end: top·bottom")
	if strings.Contains(screenText(after), "find in transcript") {
		t.Errorf("esc must close the box.\n%s", tailOfPlain(screenText(after), 2000))
	}

	// (5) ENTER KEEPS THE SEARCH AND HANDS THE KEYBOARD TO THE TRANSCRIPT.
	_, _ = s.tty.WriteString("\x06")
	waitForFrame(t, s, cols, rows, 6*time.Second, "find in transcript")
	_, _ = s.tty.WriteString("\r")
	committed := waitForFrame(t, s, cols, rows, 8*time.Second, "ctrl+g composer")
	if strings.Contains(screenText(committed), "find in transcript") {
		t.Errorf("enter must close the box.\n%s", tailOfPlain(screenText(committed), 2000))
	}
	if !strings.Contains(screenText(committed), "ctrl+g composer") {
		t.Errorf("enter must hand the keyboard to the TRANSCRIPT (the footer must say the composer is "+
			"no longer focused) — otherwise n/N and home/end are unreachable, which is the whole reason "+
			"the handover exists.\n---\n%s\n---", tailOfPlain(screenText(committed), 2500))
	}
}

// boxRows returns the box's own rows from a replayed grid, so a run can print exactly the cells the
// assertions rest on.
func boxRows(grid string) []string {
	var out []string
	for _, l := range strings.Split(grid, "\n") {
		if strings.Contains(l, "find in transcript") || strings.Contains(l, "find > ") ||
			strings.Contains(l, "matches") || strings.Contains(l, "enter: keep searching") ||
			strings.Contains(l, "type a term") {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}
