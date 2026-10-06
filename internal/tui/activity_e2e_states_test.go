// Package tui — activity_e2e_states_test.go — the two live turns (with tools, and with none) that
// the capstone observes. See activity_e2e_pty_test.go for the harness, the gate, and why ONE
// provider layer is substituted while every layer above it is the production one.
package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/testfixtures/activitye2e"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestActivityLineE2E is the capstone: ONE real turn, driven through the real binary in a real pty,
// observed on the REPLAYED SCREEN GRID at every state the acceptance criteria name.
//
// It is intentionally ONE session driven start-to-finish (no relaunch, no re-entry): AC1 is
// "observe the line appear, show counters, and rotate its verb WITHOUT leaving the surface".
func TestActivityLineE2E(t *testing.T) {
	skipActivityE2E(t)

	const cols, rows = 180, 50
	url, plane, killPlane := activityE2EPlane(t)
	_deadPlane := killPlane
	// PhaseStarted: the turn streams its first content and STOPS there until this test flips it to
	// flight. That makes "just started, nothing counted yet" a steady state rather than a race.
	plane.Reset(activitye2e.PhaseStarted)

	bin := orchBinPath(t)
	if alt := os.Getenv("ORCH_ACTIVITY_E2E_BIN"); alt != "" {
		t.Logf("activity-line E2E: using ORCH_ACTIVITY_E2E_BIN=%s (the pre-change leg)", alt)
		bin = alt
	}
	home := t.TempDir()
	writeOrchConfig(t, home, url)
	s := startOrchPtySized(t, bin, url, home, cols, rows)
	defer s.close()

	scr := openActivityConversation(t, s, cols, rows)
	e2eDumpFrame(t, "tui-01-conversation-open", scr)

	// ------------------------------------------------------------------
	// OBSERVATION 1 — the turn JUST STARTED and has counted nothing.
	// ------------------------------------------------------------------
	t.Run("1_turn_started_bare_line", func(t *testing.T) {
		sendPrompt(s, activitye2e.Prompt)
		// WAIT FOR THE CONTENT, THEN READ THE BARE LINE. The line is up from the first second; the
		// point of this state is that it is up with NO counter while the turn is genuinely running.
		frame := waitForFrame(t, s, cols, rows, 15*time.Second, "E2EWITNESSCONTENT")
		row := footerRow(frame)
		e2eDumpFrame(t, "tui-02-turn-started-bare-line", frame)
		if row == "" {
			t.Fatalf("no activity line appeared in the pane's footer at all:\n%s", screenText(frame))
		}
		if !activityWordInList(row) {
			t.Errorf("the line's word is not one the reviewed rotation names: %q", row)
		}
		if hasCounter(row) {
			t.Errorf("the line shows a TOOL COUNT before any tool call landed — a \"0 modifies\"-class "+
				"false claim of work: %q", row)
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 2 — counters land and GROW.
	// ------------------------------------------------------------------
	t.Run("2_counters_appear_and_grow", func(t *testing.T) {
		// RELEASE THE TURN: the scripted work now lands, one call at a time.
		plane.SetPhase(activitye2e.PhaseFlight)
		early, _ := waitFooter(t, s, cols, rows, 20*time.Second, func(r string) bool {
			return strings.Contains(countHalf(footerCounter(r)), "1 read")
		})
		e2eDumpFrame(t, "tui-03-counter-first-call", replay(t, s, cols, rows))
		if !strings.Contains(footerCounter(early), "1 read") {
			t.Fatalf("the counter never showed the first call. footer = %q", early)
		}
		// GROWN: the read/grep/bash/modify mix the fixture scripts.
		grown, frame := waitFooter(t, s, cols, rows, 20*time.Second, func(r string) bool {
			return strings.Contains(countHalf(footerCounter(r)), "1 modify")
		})
		e2eDumpFrame(t, "tui-04-counter-grown", frame)
		got := countHalf(footerCounter(grown))
		for _, want := range []string{"1 modify", "2 reads", "1 bash"} {
			if !strings.Contains(got, want) {
				t.Fatalf("the counter did not grow to the mixed set the fixture issued: want %q in %q "+
					"(footer = %q)", want, got, grown)
			}
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 3 — AC3, THE REGRESSION: content has arrived AND the line is STILL there, in ONE
	// frame. This is the exact case the feature exists to fix.
	// ------------------------------------------------------------------
	t.Run("3_ac3_line_survives_content", func(t *testing.T) {
		row, frame := waitFooter(t, s, cols, rows, 10*time.Second, func(r string) bool {
			return hasCounter(r) && strings.Contains(screenText(replay(t, s, cols, rows)),
				"E2EWITNESSCONTENT")
		})
		e2eDumpFrame(t, "tui-05-ac3-content-and-line-same-frame", frame)
		text := screenText(frame)
		if !strings.Contains(text, "E2EWITNESSCONTENT") {
			t.Fatalf("AC3 fixture: no streamed content is rendered in this frame, so \"the line "+
				"survived content\" is not what this frame shows:\n%s", text)
		}
		if row == "" {
			t.Fatalf("AC3 FAILED: the reply body has content rendered and the activity line is GONE "+
				"from the footer — this is exactly the regression the feature fixes (the line used to "+
				"require \"before the first token\"):\n%s", text)
		}
		if !hasCounter(row) {
			t.Errorf("AC3: the line survived, but without its counter — the feature's whole point is "+
				"that the WORK report is present mid-reply too. footer = %q", row)
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 4 — the verb ROTATES on the server clock (AC1's third clause).
	// ------------------------------------------------------------------
	t.Run("4_verb_rotates_on_the_server_clock", func(t *testing.T) {
		before, frame := waitFooter(t, s, cols, rows, 20*time.Second, func(r string) bool {
			return hasCounter(r) && verbWord(r) != ""
		})
		e2eDumpFrame(t, "tui-06-rotation-before", frame)
		w0 := verbWord(before)
		// MOVE THE SERVER CLOCK one full period: the word MUST change, and it must be the word the
		// selector names for the NEW stamp — evidence that the rotation reads the server's clock
		// rather than a local timer or a phase literal.
		newStamp := plane.Stamp() + chat.VerbPeriodMS
		plane.SetStamp(newStamp)
		wantWord := chat.VerbAt(newStamp)
		after, frame2 := waitFooter(t, s, cols, rows, 15*time.Second, func(r string) bool {
			return verbWord(r) == wantWord
		})
		e2eDumpFrame(t, "tui-07-rotation-after", frame2)
		if w0 == wantWord {
			t.Fatalf("fixture: the pre-rotation word %q already equals the target %q, so this test "+
				"cannot detect a frozen word", w0, wantWord)
		}
		if verbWord(after) != wantWord {
			t.Errorf("the word did not rotate with the server clock: before %q, after %q, want %q "+
				"(stamp -> %d)\n%s", w0, verbWord(after), wantWord, newStamp, screenText(frame2))
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 5 — AC4/AC7: the counts reconcile with the SERVER's ledger.
	// ------------------------------------------------------------------
	var cc crossClient
	t.Run("5_counters_match_the_server_ledger", func(t *testing.T) {
		row, frame := waitFooter(t, s, cols, rows, 20*time.Second, func(r string) bool {
			return strings.Contains(countHalf(footerCounter(r)), "1 modify")
		})
		e2eDumpFrame(t, "tui-08-ac4-reconcile", frame)
		clientCount := countHalf(footerCounter(row))
		want := countHalf(serverCounter(plane))
		if want == "" {
			t.Fatalf("fixture: the server's own render of the ledger is empty, so this reconciliation " +
				"would compare nothing")
		}
		if clientCount != want {
			t.Errorf("AC4 FAILED: the painted counter and the server's own render of the same ledger "+
				"disagree.\n  client (painted frame): %q\n  server (SummarizeCalls over the plane's "+
				"rows): %q\n  ledger: %v", clientCount, want, plane.Calls())
		}
		cc = crossClient{Stamp: plane.Stamp(), Word: verbWord(row), Counter: clientCount, Line: row}
		writeCrossClient(t, cc)
		e2eFooterRow = footerRowIndex(frame)
	})

	// ------------------------------------------------------------------
	// OBSERVATION 6 — AC6: the line is ONE row, at the pane's fixed footer row.
	// ------------------------------------------------------------------
	t.Run("6_ac6_footer_is_one_row", func(t *testing.T) {
		frame := replay(t, s, cols, rows)
		e2eDumpFrame(t, "tui-09-ac6-row-budget", frame)
		n := 0
		for r := 0; r < rows; r++ {
			if strings.Contains(frame.row(r), "Orchicon is ") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("AC6 FAILED: the activity line occupies %d rows — the footer is ONE row and a "+
				"wrapped line is a second row the pane must never spend:\n%s", n, screenText(frame))
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 7 — AC5, the SAFETY property, observed LIVE: the stream goes SILENT and the line
	// ESCALATES to the watchdog's verdict with the counters GONE.
	// ------------------------------------------------------------------
	t.Run("7_ac5_silence_escalates_and_drops_the_counter", func(t *testing.T) {
		plane.SetPhase(activitye2e.PhaseStalled)
		row, frame := waitFooter(t, s, cols, rows, 40*time.Second, func(r string) bool {
			return strings.Contains(r, "no output for")
		})
		e2eDumpFrame(t, "tui-10-ac5-stalled-no-output", frame)
		if !strings.Contains(row, "no output for") {
			t.Fatalf("AC5 FAILED: the stream went silent but the line never escalated to the "+
				"watchdog's verdict within the 25s warn band. footer = %q\n%s", row, screenText(frame))
		}
		if hasCounter(row) {
			t.Errorf("AC5 FAILED: the line escalated AND kept its tool counter (%q) — a count beside "+
				"\"no output\" claims work is still happening, which is the exact false claim this "+
				"band exists to prevent", row)
		}
		if !strings.Contains(row, "Orchicon is ") {
			t.Errorf("the escalated line lost its verb entirely: %q", row)
		}
	})

	// ------------------------------------------------------------------
	// OBSERVATION 8 — AC5's SAFETY PROPERTY, observed live on the REAL socket: with the counters
	// GENUINELY SHOWING, the connection is killed mid-turn (every socket the plane holds is closed)
	// and the footer must stop painting a live-looking tool counter — the false liveness claim the
	// whole precedence chain exists to prevent.
	//
	// WHY THE HEALTHY PHASE IS RESTORED FIRST. Coming straight out of the escalated leg the counter is
	// already absent, so killing the plane there would "pass" while proving nothing. Re-establishing
	// the healthy turn puts counters back on the row; killing the socket then makes their
	// disappearance an observation rather than an inheritance.
	//
	// THE OBSERVABLE FORM ON THE ASK TAB. `transcriptStatusLine`'s disconnected/`reconnecting` arms key
	// off the SUBSCRIPTION REGISTRY's worst status and the event store's reconnect flag, and the Ask
	// tab registers no live subscription of its own (its conversation list is the shell's rail — see
	// subs.go's own note). So the honest assertion on THIS surface is the one that holds here: the
	// counter cannot survive the dead connection; the line escalates to the watchdog's own verdict
	// ("no output for Ns") or clears, and never keeps claiming work. The GUI leg
	// (frontend/tests/activity-line-e2e.spec.ts) observes its `disconnected` outranking directly,
	// because that state is a client-side guard rather than a registry status.
	t.Run("8_ac5_killed_connection_never_keeps_the_counter", func(t *testing.T) {
		// FRESH WORK LANDS ON THE TURN. The stall leg above ran past the summarizer's 30s rolling
		// window, so the earlier calls have honestly aged out and the row is bare again by design.
		// Re-issuing puts counters back for a reason the feature's own rule permits, so the kill that
		// follows is observed on a row that was genuinely claiming work.
		plane.SetPhase(activitye2e.PhaseFlight)
		plane.Reissue()
		back, _ := waitFooter(t, s, cols, rows, 30*time.Second, func(r string) bool { return hasCounter(r) })
		if !hasCounter(back) {
			t.Fatalf("fixture: fresh calls landed on the turn but no counter reached the row (%q), so "+
				"this leg would assert the absence of something that was never there", back)
		}
		e2eDumpFrame(t, "tui-11-with-counters-before-the-kill", replay(t, s, cols, rows))

		if _deadPlane == nil {
			t.Skip("no plane handle: this leg needs the in-process fixture to tear its own sockets down")
		}
		_deadPlane()

		deadline := time.Now().Add(45 * time.Second)
		for {
			frame := replay(t, s, cols, rows)
			row := footerRow(frame)
			if row == "" || !hasCounter(row) {
				e2eDumpFrame(t, "tui-12-ac5-killed-connection-counter-gone", frame)
				return
			}
			if time.Now().After(deadline) {
				e2eDumpFrame(t, "tui-12-ac5-killed-connection-FAILED", frame)
				t.Fatalf("AC5 FAILED: the connection was killed mid-turn with counters on the row and "+
					"the footer is STILL painting a live-looking tool counter (%q) — a liveness claim "+
					"the dead plane cannot deliver:\n%s", row, screenText(frame))
			}
			time.Sleep(400 * time.Millisecond)
		}
	})

	t.Logf("cross-client handshake written: stamp=%d word=%q counter=%q", cc.Stamp, cc.Word, cc.Counter)
}

// TestActivityLineE2EZeroToolCalls is AC8 (the "0 tool calls in a turn: no counters, exactly as
// before the change") AND the second half of AC6: the transcript body keeps its row.
//
// It runs a SECOND real turn on a reset plane (no ledger rows at all) and compares the body's own
// witness row against the with-tools turn's — so the counter's cost to the transcript is a measured
// delta rather than an assurance.
func TestActivityLineE2EZeroToolCalls(t *testing.T) {
	skipActivityE2E(t)

	const cols, rows = 180, 50
	url, plane, _ := activityE2EPlane(t)
	plane.Reset(activitye2e.PhaseNoTools)

	bin := orchBinPath(t)
	if alt := os.Getenv("ORCH_ACTIVITY_E2E_BIN"); alt != "" {
		bin = alt
	}
	home := t.TempDir()
	writeOrchConfig(t, home, url)
	s := startOrchPtySized(t, bin, url, home, cols, rows)
	defer s.close()

	openActivityConversation(t, s, cols, rows)
	sendPrompt(s, activitye2e.Prompt)

	row, frame := waitFooter(t, s, cols, rows, 15*time.Second, func(r string) bool {
		return strings.Contains(screenText(replay(t, s, cols, rows)), "E2EWITNESSFINAL")
	})
	e2eDumpFrame(t, "tui-13-zero-tool-calls", frame)
	if row == "" {
		t.Fatalf("with a live turn the activity line must still be present even when nothing was "+
			"counted:\n%s", screenText(frame))
	}
	if !activityWordInList(row) {
		t.Errorf("the word is outside the reviewed rotation: %q", row)
	}
	if hasCounter(row) {
		t.Errorf("AC8 FAILED: a turn with ZERO tool calls renders a counter (%q) — the summarizer's "+
			"empty string exists precisely so this cannot happen, and a \"0 modifies\" is a false "+
			"claim that work is happening", row)
	}
	if got := footerCounter(row); got != "" && !strings.HasPrefix(got, "last activity") {
		t.Errorf("AC8: with nothing counted the line must fall back to the plain watchdog age, got %q",
			got)
	}
	// AC6's other half, measured on the live frame: the pane spends the SAME rows on chrome in a
	// turn with counters and a turn without, so the body neither loses nor gains a row.
	if e2eFooterRow >= 0 {
		if got := footerRowIndex(frame); got != e2eFooterRow {
			t.Errorf("AC6 FAILED: the pane's footer row is index %d here and %d in the with-tools "+
				"turn — the counter changed the pane's row budget instead of riding the footer it "+
				"already had", got, e2eFooterRow)
		}
	}
}
