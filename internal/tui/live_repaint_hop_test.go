package tui

// live_repaint_hop_test.go — THE HOP BETWEEN A LIVE POKE AND THE RENDER.
//
// The operator, mid-live-test: "Executions are NOT updating live. I have to move off the execution and
// back onto it to see the newest updates."
//
// The chain has four hops:
//
//	stream event → subs.EventPokeMsg → the screen's handler → the shell's RefreshExecutionSession
//	             → merged with the durable parts → the screen's RenderSession → the pane's transcript
//
// The execution package's live_update_test.go pins the SCREEN's three hops (its own header says so, and
// says this one is "exercised in the tui package, where the shell lives"). It was not: nothing in this
// package called RefreshExecutionSession, so the hop in the MIDDLE of the chain — the one that decides
// whether a poke ever reaches the pane — was the only hop nothing measured. This file measures it.
//
// WHY THIS HOP IS THE ONE WORTH PINNING. App.refreshExecutionSession refuses unless the pane is showing
// the execution that poked:
//
//	if s.(interface{ DetailID() string }).DetailID() != execID { return nil }
//
// That guard is correct and it is SILENT. A guard that answers "no" for the wrong id — a stale
// DetailID, an id in a different spelling, the pane having moved on — returns without complaint, the
// handler returns without complaint, and the pane simply stops changing. To the operator that reads as
// a hung execution, not as a bug, which is exactly the report this work started from. So both
// directions are asserted here: the poke that MUST repaint, and the poke that must not.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/execution"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// landedExecution builds the shell, opens execID in the detail pane and lands its record, so the pane is
// in the state a live poke finds it in: PAINTED with a known execution, and describable
// (composeExecutionBody refuses until the record has been fetched, so RenderSession would silently no-op
// on an unlanded pane and this test would measure nothing).
func landedExecution(t *testing.T, execID string) (*App, *execution.Model) {
	t.Helper()
	m, scr, _ := newExecListApp(t)
	scr.SetDetailID(execID)
	landDetail(t, scr, "executions", execID)

	title, fields, _ := scr.DetailForTest()
	if len(fields) == 0 {
		t.Fatalf("fixture: the detail pane's record never landed, so a repaint would be dropped by " +
			"RenderSession's empty-fields guard")
	}
	if !strings.Contains(title, "Execution "+execID) {
		t.Fatalf("fixture: the pane is not showing %s (title %q)", execID, title)
	}
	return m, scr
}

// landDetail fetches a row's detail and DELIVERS the result to the screen, the way the bubbletea runtime
// does: cmd → msg → Update. A command whose result is discarded (see execCmdTree) can only tell us which
// RPC ran; the pane is written by the base's detailMsg handler, so anything asserted ABOUT THE PANE has
// to deliver.
func landDetail(t *testing.T, scr *execution.Model, src, id string) {
	t.Helper()
	cmd := scr.RequestDetail(src, id)
	if cmd == nil {
		t.Fatalf("fixture: the screen has no detail loader for %s", src)
	}
	msg := cmd()
	if msg == nil {
		t.Fatalf("fixture: the detail command for %s produced no message", id)
	}
	scr.Update(msg)
}

// THE POKE REPAINTS THE OPEN EXECUTION. A durable transcript item (the follow-up reply the operator was
// waiting for) must reach the pane when the shell is told the execution has news.
func TestALivePokeRepaintsTheOpenExecutionTranscript(t *testing.T) {
	const reply = "the reply the operator was waiting for"
	m, scr := landedExecution(t, "exec-1")

	// Before the poke the pane shows the execution but NOT the reply: an assertion made only after the
	// poke could pass on a pane that was already correct.
	if _, _, body := scr.DetailForTest(); strings.Contains(body, reply) {
		t.Fatalf("fixture: the pane already shows the reply before the poke:\n%s", body)
	}

	// The durable side, which is what a follow-up's answer lands in (refresh.go's header explains why no
	// event is emitted for that path and why the pane therefore has to be told).
	m.execSessions["exec-1"] = []chat.ChatItem{{Kind: chat.KindText, Text: reply}}

	m.RefreshExecutionSession("exec-1", nil)

	if _, _, body := scr.DetailForTest(); !strings.Contains(body, reply) {
		t.Fatalf("a live poke left the open execution's transcript unchanged — the operator's reply is "+
			"invisible until they leave the screen and come back, which is the report.\nbody:\n%s", body)
	}
}

// AND THE GUARD HOLDS: a poke for an execution the pane is NOT showing must not repaint it. This is the
// silent half of the hop, and asserting it is what keeps the guard from being "fixed" into a repaint of
// whatever happens to be open.
func TestAPokeForAnotherExecutionDoesNotRepaintThePane(t *testing.T) {
	const other = "another execution's reply, which must not appear here"
	m, scr := landedExecution(t, "exec-1")

	m.execSessions["exec-2"] = []chat.ChatItem{{Kind: chat.KindText, Text: other}}
	before, _, bodyBefore := scr.DetailForTest()

	m.RefreshExecutionSession("exec-2", nil)

	title, _, bodyAfter := scr.DetailForTest()
	if strings.Contains(bodyAfter, other) {
		t.Fatalf("a poke for exec-2 repainted the pane showing exec-1:\n%s", bodyAfter)
	}
	if title != before || bodyAfter != bodyBefore {
		t.Errorf("the pane changed on a poke meant for another execution (title %q -> %q)", before, title)
	}
}

// THE SOURCE GUARD IS THE SCREEN'S — the other half of "does a poke reach the pane". The execution
// transcript belongs to the Executions SOURCE, so a poke that arrives while the operator is reading
// another source's detail must not be acted on at all.
//
// This is asserted through the REAL CHAIN (poke → the screen's handler), not by calling the shell
// directly, because that is where this guard lives: the shell's own guard is the ID, and it is right for
// it not to know about sources. A test that called RefreshExecutionSession here would be asserting an
// invariant the shell was never given, and would fail for a correct program — which is what its first
// version did.
func TestAPokeIsIgnoredWhileAnotherSourceIsSelected(t *testing.T) {
	const reply = "an execution reply that must not be painted over another source's detail"
	m, scr := landedExecution(t, "exec-1")
	m.execSessions["exec-1"] = []chat.ChatItem{{Kind: chat.KindText, Text: reply}}

	if !scr.SelectSource("runs") {
		t.Skip("fixture: this screen has no runs source to switch to")
	}

	// What the stream's re-armable poke delivers, with the Runs source selected.
	scr.Update(subs.EventPokeMsg{Name: "execution-events"})

	if _, _, body := scr.DetailForTest(); strings.Contains(body, reply) {
		t.Fatalf("a poke for the execution repainted the pane while another source was selected, so the "+
			"transcript would overwrite whatever the operator was reading:\n%s", body)
	}
}
