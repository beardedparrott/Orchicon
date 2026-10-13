package tui

// orch_pty_find_test.go — THE FIND BOX IN A REAL TERMINAL.
//
// WHY THIS EXISTS ON TOP OF THE UNIT TESTS. findbox_test.go drives the real Update with real key
// messages and asserts the search state, which pins the BEHAVIOUR. It cannot see the surface: whether
// the box PAINTS, whether its rows survive the frame's compositing, whether the query actually holds
// the caret, or whether the composer row the operator reads says the right thing while the box is up.
// Those live in the renderer and the frame composition, which is one layer further out than a unit
// test reaches — and the operator's report was about the surface ("I would prefer a modal pop up that
// you can type in your search").
//
// So this drives the REAL bin/orch in a REAL pty against the activity-E2E fixture plane, opens a real
// conversation, and reads the box off the replayed screen grid — the layer the operator looks at.
// Opt-in (ORCH_PTY_SMOKE=1) and skipped otherwise, like every other pty gate here.
//
// THE NAVIGATION KEYS ARE NOT ASSERTED HERE, deliberately: their effect is the transcript's scroll
// offset, and this fixture serves a TWO-message conversation that fits the pane — so home/end have
// nowhere to travel and any assertion would be vacuous. That half is pinned by
// findbox_test.go against a parked 80-line transcript, which is where it can actually be measured.

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

	// (1) THE COMPOSER ADVERTISES THE KEYS — the operator's second ask, on the row they look at:
	// "no note in the composer about search or jumping to beginning to end is there so no one would
	// know they exist."
	shell := screenText(scr)
	for _, want := range []string{"ctrl+f: find in the transcript", "home/end: first·latest message"} {
		if !strings.Contains(shell, want) {
			t.Fatalf("the composer's affordance row never advertised %q — the keys exist but nothing "+
				"says so, which is the report.\n---\n%s\n---", want, tailOfPlain(shell, 2500))
		}
	}

	// (2) CTRL+F PAINTS THE BOX, with a live count of the conversation that is behind it. The
	// fixture serves exactly two messages, so a real count is the assertion that the box is reading
	// the conversation rather than rendering a placeholder.
	_, _ = s.tty.WriteString("\x06")
	box := waitForFrame(t, s, cols, rows, 8*time.Second, "find in transcript")
	boxText := screenText(box)
	for _, want := range []string{"find in transcript", "find > ", "type a term to filter"} {
		if !strings.Contains(boxText, want) {
			t.Fatalf("the box never painted %q.\n---\n%s\n---", want, tailOfPlain(boxText, 2500))
		}
	}
	t.Logf("box on screen: %s", strings.Join(boxRows(boxText), " | "))

	// AND IT SAYS THE BOX'S KEYS, not the transcript's — while the box holds the keyboard those
	// chords are inert, and a row that kept advertising them would be describing a state the
	// operator is not in.
	if !strings.Contains(boxText, "enter: keep searching") {
		t.Errorf("while the box is up the composer row must describe the BOX's keys.\n%s",
			tailOfPlain(boxText, 2000))
	}

	// (3) TYPING FILTERS THE REAL TRANSCRIPT, live. A term that cannot match proves the filter RAN
	// (an unfiltered box would still say "type a term to filter") without depending on the fixture's
	// wording.
	_, _ = s.tty.WriteString("zzqqxx-not-in-this-conversation")
	typed := waitForFrame(t, s, cols, rows, 6*time.Second, "no matches in")
	if !strings.Contains(screenText(typed), "no matches in") {
		t.Fatalf("typing into the box did not filter the transcript.\n---\n%s\n---",
			tailOfPlain(screenText(typed), 2500))
	}
	t.Logf("after typing: %s", strings.Join(boxRows(screenText(typed)), " | "))

	// (4) ESC CANCELS: the box closes and the transcript's own advertisement comes back.
	_, _ = s.tty.WriteString("\x1b")
	after := waitForFrame(t, s, cols, rows, 6*time.Second, "ctrl+f: find in the transcript")
	if strings.Contains(screenText(after), "find in transcript") {
		t.Errorf("esc must close the box.\n%s", tailOfPlain(screenText(after), 2000))
	}

	// (5) ENTER KEEPS THE SEARCH AND HANDS THE KEYBOARD TO THE TRANSCRIPT. The footer is the one
	// place that says which surface holds the keyboard, so it is what proves the handover happened in
	// the real shell: with the composer focused it reads "— esc/ctrl+g content"; once focus is in the
	// content it reads "ctrl+g composer".
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
