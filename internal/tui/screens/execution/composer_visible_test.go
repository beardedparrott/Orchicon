package execution

// composer_visible_test.go — THE MESSAGE BOX MUST BE ON SCREEN.
//
// The operator reported this twice:
//
//	"I would rather a chat box be at the bottom of the execution and you can click into there or
//	 gain focus with the down arrow key or tab and then type in your response and hit enter to send
//	 it."
//	"I STILL don't see a chat prompt inside an execution in the TUI"
//
// Both times the box EXISTED and both times it was invisible, for two different reasons — which is
// why these tests assert VISIBILITY rather than existence:
//
//  1. It was appended to the detail BODY, at the end of the transcript. A real transcript is taller
//     than the pane, the viewport opens at the TOP, so the box sat ~65 lines below the fold. The
//     assertion that would have caught it is "the rendered pane contains the box", not "the body
//     contains the box" — the second was true the whole time.
//  2. Even once it became a fixed FOOTER, the host Panel renders exactly Height-2 content rows and
//     CLIPS the overflow from the TAIL — so the band was installed, correct, and discarded. The
//     height budget had to account for every row Detail.View writes.
//
// So: render the PANE, and look for the prompt in it. Anything less does not test what the operator
// reported.

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// longTranscript builds a transcript far taller than the pane, which is the condition under which
// the box went missing both times. 60 tool blocks comfortably exceeds a 40-row pane.
func longTranscript() []chat.ChatItem {
	items := make([]chat.ChatItem, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, chat.ChatItem{
			Kind: chat.KindTool,
			Key:  "t" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Tool: &chat.ParsedTool{ID: "t", ToolName: "bash", Input: "run something", Output: "output line"},
		})
	}
	items = append(items, chat.ChatItem{Kind: chat.KindText, Text: "I just finished a piece of work.", Key: "m1"})
	return items
}

// execWithTranscript loads an execution whose detail the BASE installs — the real path, through
// detailMsg and the onDetail hook — because that is the path the composer's install depends on.
func execWithTranscript(t *testing.T, meta string, items []chat.ChatItem) *Model {
	t.Helper()
	p := execPlane()
	m := newModel(t, p)
	m.Base.SetSize(200, 40)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: meta}}, "")
	m.execDetail.putTranscript("exec-1", items)

	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	// DeliverDetailForTest routes a detailMsg through the base exactly as a real fetch landing does,
	// INCLUDING the onDetail hook that installs the box.
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)
	return m
}

// THE REPORT ITSELF: with a transcript far taller than the pane, the rendered pane contains the
// message box.
func TestMessageBoxIsVisibleWithATallTranscript(t *testing.T) {
	m := execWithTranscript(t, "succeeded", longTranscript())

	view := m.Base.View()
	if !strings.Contains(view, "›") {
		t.Fatalf("the message box is NOT in the rendered pane — this is the operator's report "+
			"(\"I STILL don't see a chat prompt inside an execution in the TUI\").\n"+
			"footer installed=%q", m.Base.DetailFooter())
	}
	// And it is at the BOTTOM of the pane, where a prompt belongs — not somewhere in the middle of
	// the transcript where it would read as one more line of output.
	lines := strings.Split(view, "\n")
	bandLine := -1
	for i, l := range lines {
		if strings.Contains(l, "›") {
			bandLine = i
		}
	}
	if bandLine < 0 {
		t.Fatal("the band was found but not located — impossible")
	}
	// The pane's last few rows: the band must be within them. (The base's View includes the panel
	// border, so allow a small margin.)
	if bandLine < len(lines)-5 {
		t.Errorf("the message box is on line %d of %d — it must be at the BOTTOM of the pane, not "+
			"buried in the transcript", bandLine, len(lines))
	}
}

// The box is visible for a TERMINAL execution (a follow-up) and for a LIVE one (a mid-run message) —
// the two states the placeholder distinguishes.
func TestMessageBoxIsVisibleForBothStates(t *testing.T) {
	for _, meta := range []string{"succeeded", "running"} {
		t.Run(meta, func(t *testing.T) {
			m := execWithTranscript(t, meta, longTranscript())
			view := m.Base.View()
			if !strings.Contains(view, "›") {
				t.Fatalf("no message box in the pane for a %s execution (footer=%q)", meta, m.Base.DetailFooter())
			}
			want := "follow-up"
			if meta == "running" {
				want = "message"
			}
			if !strings.Contains(view, want) {
				t.Errorf("the band does not read %q for a %s execution — the placeholder must name the "+
					"act enter will perform", want, meta)
			}
		})
	}
}

// An execution that produced NOTHING still gets the box: asking why is exactly what an operator
// wants on a silent run, so the band must not depend on there being a transcript.
func TestMessageBoxIsVisibleWithNoTranscript(t *testing.T) {
	m := execWithTranscript(t, "succeeded", nil)
	if view := m.Base.View(); !strings.Contains(view, "›") {
		t.Fatalf("no message box for an execution with no transcript (footer=%q) — that is when it "+
			"matters most", m.Base.DetailFooter())
	}
}

// The band is a FOOTER, not a body section: it must not appear in the scrolling body, or it would
// scroll away exactly as it did when it was appended to the transcript.
func TestMessageBoxIsNotPartOfTheBody(t *testing.T) {
	m := execWithTranscript(t, "succeeded", longTranscript())
	if strings.Contains(m.composeExecutionBodyFor("exec-1"), "›") {
		t.Error("the message box is in the BODY — it would scroll out of view, which is the defect")
	}
	if m.Base.DetailFooter() == "" {
		t.Error("the message box is not in the footer either — it is nowhere")
	}
}

// The band does not linger over ANOTHER source's detail. Selecting a non-execution pane clears it,
// or the prompt would sit under a workflow's flow view and offer to message a worker.
func TestMessageBoxClearsForAnotherSource(t *testing.T) {
	m := execWithTranscript(t, "succeeded", longTranscript())
	if m.Base.DetailFooter() == "" {
		t.Fatal("setup: expected the band on the Executions pane")
	}
	m.Base.SelectSource(srcRuns)
	m.installComposerFooter("run-1") // the hook runs for the new source
	if got := m.Base.DetailFooter(); got != "" {
		t.Errorf("the band survived a source switch (%q) — it must not appear over another pane's detail", got)
	}
}

// The cursor's position is REFLECTED in the band: with the box focused the caret is drawn, and with
// the blocks focused it is not. Otherwise "walk down into the box" gives no visible feedback.
func TestBandShowsTheCaretOnlyWhenTheBoxIsFocused(t *testing.T) {
	m := execWithTranscript(t, "succeeded", longTranscript())

	m.blocks.cursor.atComposer = false
	m.installComposerFooter("exec-1")
	unfocused := m.Base.DetailFooter()

	m.blocks.cursor.atComposer = true
	m.installComposerFooter("exec-1")
	focused := m.Base.DetailFooter()

	if focused == unfocused {
		t.Error("focusing the box changed nothing in the band — the operator gets no feedback that " +
			"typing will now go into it")
	}
	if !strings.Contains(focused, "\x1b[7m") {
		t.Errorf("the focused band draws no caret: %q", focused)
	}
}
