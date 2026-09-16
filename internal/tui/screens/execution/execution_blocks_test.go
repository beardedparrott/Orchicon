package execution

// execution_blocks_test.go — the transcript as COLLAPSIBLE BLOCKS with a cursor, and the inline
// composer as the last position on that cursor's ladder.
//
// The operator:
//
//	"The executions are incredibly long. In the GUI, all blocks are auto collapsed. We should be
//	 collapsing all conversation blocks, tool calls, etc. and a user can move down the line using
//	 tab or down arrow and hitting enter on a block should collapse or expand it."
//	"I would rather a chat box be at the bottom of the execution and you can click into there or
//	 gain focus with the down arrow key or tab and then type in your response and hit enter to send
//	 it."
//
// The two are ONE mechanism, so they are tested together: the cursor walks the blocks and then lands
// in the composer.

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// transcriptItems is a realistic transcript: prose, a tool call with a LONG body, thinking, an
// error, and a streaming reply.
func transcriptItems() []chat.ChatItem {
	return []chat.ChatItem{
		{Kind: chat.KindUser, Text: "make the parser handle nested blocks"},
		{Kind: chat.KindText, Text: "I'll start by reading the parser.", Key: "m1"},
		{Kind: chat.KindTool, Key: "t1", Tool: &chat.ParsedTool{
			ID: "t1", ToolName: "bash",
			Input:  "grep -rn \"nested\" internal/",
			Output: strings.Repeat("internal/parser.go:12: nested block handling\n", 60),
		}},
		{Kind: chat.KindReasoning, Text: "The recursive case is missing a base.", Key: "r1"},
		{Kind: chat.KindError, Text: "boom: index out of range", Key: "e1"},
		{Kind: chat.KindText, Text: "Fixed the recursion.", Key: "m2", Live: true},
	}
}

// --- collapsing ---------------------------------------------------------------------------------

// TOOL CALLS AND THINKING START COLLAPSED — the GUI's rule ("tool calls are compact collapsible
// cards, reasoning is collapsed") and the operator's ask ("all blocks are auto collapsed").
//
// This is the whole point of the item: a transcript with a 60-line tool dump in it was unreadable,
// and collapsing is what makes the line scannable.
func TestToolAndReasoningBlocksStartCollapsed(t *testing.T) {
	items := transcriptItems()
	blocks := blocksFromItems(items, 100)
	for _, b := range blocks {
		switch b.kind {
		case blockTool, blockReasoning:
			if b.defaultExpanded() {
				t.Errorf("a %s block starts EXPANDED — the operator asked for them collapsed", b.label())
			}
		case blockText:
			if !b.defaultExpanded() {
				t.Error("a prose block starts collapsed — prose is what the operator reads")
			}
		case blockError:
			if !b.defaultExpanded() {
				t.Error("an ERROR block starts collapsed — collapsing the reason a run failed hides the thing being looked for")
			}
		}
	}
	// And the collapsed block is ONE line: the summary states the tool and its first output, but the
	// 60-line body is NOT drawn. (The summary deliberately includes the first line of output — that
	// is what makes a collapsed tool call scannable — so the assertion is on the FULL dump, not on
	// any occurrence of the text.)
	state := &blockState{}
	body, _ := renderBlocks(blocks, state, 100, transcriptCursor{})
	if got := strings.Count(body, "nested block handling"); got != 1 {
		t.Errorf("the collapsed tool's output appears %d times, want ONCE (the summary line only — the body must not be drawn)", got)
	}
	if strings.Count(body, "\n")+1 > len(blocks)+3 {
		t.Errorf("a collapsed transcript should be roughly one line per block; got %d lines for %d blocks:\n%s",
			strings.Count(body, "\n")+1, len(blocks), body)
	}
	if !strings.Contains(body, "bash") {
		t.Error("the collapsed tool SUMMARY is missing — a collapsed tool call must say which tool ran")
	}
}

// A LIVE block is drawn expanded whatever the rules, because watching it arrive is the reason to
// look at a running execution.
func TestLiveBlocksStayExpanded(t *testing.T) {
	items := []chat.ChatItem{{Kind: chat.KindTool, Key: "t1", Live: true, Tool: &chat.ParsedTool{
		ID: "t1", ToolName: "bash", Output: "streaming output line",
	}}}
	blocks := blocksFromItems(items, 100)
	if !blocks[0].defaultExpanded() {
		t.Error("a LIVE block is collapsed — a collapsed live bubble looks like a stall")
	}
	body, _ := renderBlocks(blocks, &blockState{}, 100, transcriptCursor{})
	if !strings.Contains(body, "streaming output line") {
		t.Errorf("the live block's body is not drawn:\n%s", body)
	}
}

// Enter on the cursor's block TOGGLES it, and the decision is remembered.
func TestEnterTogglesTheCursorBlock(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	// Put the cursor on the TOOL block (index 2 in the fixture).
	m.blocks.cursor.idx = 2
	if got := blocks[2].kind; got != blockTool {
		t.Fatalf("fixture: block 2 is %v, want a tool block", got)
	}

	// Collapsed by default → enter EXPANDS it.
	if _, handled := m.handleActionKey("enter"); !handled {
		t.Fatal("enter was not handled on a collapsible block")
	}
	if !m.blocks.expanded(blocks[2]) {
		t.Error("enter did not expand the collapsed tool block")
	}
	// Enter AGAIN collapses it.
	m.handleActionKey("enter")
	if m.blocks.expanded(blocks[2]) {
		t.Error("a second enter did not collapse the block again")
	}
}

// An error block has nothing to toggle, so enter is NOT claimed — it falls through rather than
// being swallowed and appearing to do nothing.
func TestEnterIsNotClaimedOnANonCollapsibleBlock(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	for i, b := range blocks {
		if b.kind == blockError {
			m.blocks.cursor.idx = i
			if _, handled := m.handleActionKey("enter"); handled {
				t.Error("enter was claimed on an error block — it has nothing to toggle")
			}
			return
		}
	}
	t.Fatal("the fixture has no error block")
}

// The operator's expansion is remembered BY BLOCK, so a live repaint (which redraws everything) does
// not undo it.
func TestExpansionSurvivesARepaint(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	m.blocks.cursor.idx = 2
	m.handleActionKey("enter") // expand the tool
	if !m.blocks.expanded(blocks[2]) {
		t.Fatal("setup: the block did not expand")
	}
	// A live repaint arrives (a new chat item appended).
	m.RenderSession(append(transcriptItems(), chat.ChatItem{Kind: chat.KindText, Text: "more", Key: "m3"}))
	if !m.blocks.expanded(blocks[2]) {
		t.Error("a repaint collapsed the block the operator opened")
	}
}

// The per-block state is CLEARED when the pane shows a different execution: the keys are only
// unique within one transcript, so carrying them over would apply one execution's expansion to
// another's blocks.
func TestBlockStateResetsForADifferentExecution(t *testing.T) {
	state := &blockState{}
	state.reset("exec-1")
	state.toggle(textBlock{kind: blockTool, key: "t1"})
	if len(state.overrides) != 1 {
		t.Fatal("setup: the toggle was not recorded")
	}
	state.reset("exec-2")
	if state.overrides != nil {
		t.Error("the expansion state carried over to a different execution")
	}
}

// --- the cursor ladder --------------------------------------------------------------------------

// Walking DOWN past the last block lands in the COMPOSER — the operator's "gain focus with the down
// arrow key or tab". Walking up returns to the blocks.
func TestDownWalksIntoTheComposerAndUpComesBack(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	n := len(blocks)

	// Walk down from the top: the cursor stops on the last block first.
	for i := 0; i < n-1; i++ {
		m.handleActionKey("down")
	}
	if m.blocks.cursor.atComposer {
		t.Fatal("the cursor skipped the last block on its way to the composer")
	}
	if m.blocks.cursor.idx != n-1 {
		t.Fatalf("cursor = %d, want the last block (%d)", m.blocks.cursor.idx, n-1)
	}
	// One more down → the composer.
	m.handleActionKey("down")
	if !m.blocks.cursor.atComposer {
		t.Fatal("walking down past the last block did not reach the composer")
	}
	// Up → back to the blocks.
	m.handleActionKey("up")
	if m.blocks.cursor.atComposer {
		t.Fatal("up from the composer did not return to the blocks")
	}
	if m.blocks.cursor.idx != n-1 {
		t.Errorf("up landed on %d, want the last block (%d)", m.blocks.cursor.idx, n-1)
	}
}

// TAB toggles between the TRANSCRIPT and the MESSAGE BOX — and works BOTH ways.
//
// The operator: "Once we have focus on an execution detail pane, we should allow tab to tab between
// the Execution details and the chat prompt. Currently once you go into a chat in an execution you
// are locked in it and can't get out."
//
// This REPLACES a test that pinned Tab as a second Down (walking the blocks). The operator named both
// keys then; he has since asked for Tab to be the pane toggle, and up/down still walk the transcript
// (down past the last block lands in the box, exactly as before) — so the old behaviour is not lost,
// only Tab's meaning is.
func TestTabTogglesBetweenTranscriptAndMessageBox(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	if len(blocks) == 0 {
		t.Fatal("fixture has no transcript")
	}
	if m.blocks.cursor.atComposer {
		t.Fatal("the fixture starts in the composer")
	}

	// Transcript → the box.
	if _, handled := m.handleActionKey("tab"); !handled {
		t.Fatal("tab was not handled in the transcript — it must move to the message box")
	}
	if !m.blocks.cursor.atComposer {
		t.Fatal("tab did not move focus into the message box")
	}

	// The box → back to the transcript. THIS is the half that was missing: the key used to be claimed
	// and dropped, so the operator could not tab out of the chat at all.
	if _, handled := m.handleActionKey("tab"); !handled {
		t.Fatal("tab was not handled in the message box — this is the operator's \"locked in a chat\"")
	}
	if m.blocks.cursor.atComposer {
		t.Fatal("tab did not move focus OUT of the message box — the operator stays locked in the chat")
	}

	// shift+tab is the same toggle: the pane has two positions, so the reverse lands on the other one.
	m.handleActionKey("shift+tab")
	if !m.blocks.cursor.atComposer {
		t.Error("shift+tab did not move into the message box")
	}
	m.handleActionKey("shift+tab")
	if m.blocks.cursor.atComposer {
		t.Error("shift+tab did not move back out of the message box")
	}
}

// UP/DOWN still walk the transcript, and down past the last block still lands in the box — Tab's new
// meaning did not take the arrows' behaviour with it.
func TestArrowsStillWalkTheTranscript(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	m.handleActionKey("down")
	if m.blocks.cursor.atComposer || m.blocks.cursor.idx != 1 {
		t.Fatalf("down moved to (%d, composer=%v), want block 1", m.blocks.cursor.idx, m.blocks.cursor.atComposer)
	}
	m.handleActionKey("up")
	if m.blocks.cursor.idx != 0 {
		t.Errorf("up left the cursor on %d, want 0", m.blocks.cursor.idx)
	}
	// Down past the last block reaches the box.
	for i := 0; i < len(blocks)+2; i++ {
		m.handleActionKey("down")
	}
	if !m.blocks.cursor.atComposer {
		t.Error("walking down past the last block no longer reaches the message box")
	}
}

// The screen TELLS THE SHELL it owns Tab while the execution detail is focused, so the shell does not
// move the top menu instead — the other half of the lock-in. Scoped: the list keeps the shell's Tab.
func TestOwnsTabOnlyOnTheFocusedExecutionDetail(t *testing.T) {
	m, _ := modelWithTranscript(t)
	if !m.OwnsTab() {
		t.Error("the focused execution detail must own Tab — otherwise the shell moves the top menu while the caret stays in the chat")
	}
	// The LIST does not: Tab there still walks the tab ring.
	m.Base.SetFocusForTest("list")
	if m.OwnsTab() {
		t.Error("the list must NOT own Tab — walking the ring from a list is the navigation model")
	}
	// Another source's detail does not either.
	m.Base.SetFocusForTest("detail")
	m.Base.SelectSource(srcRuns)
	if m.OwnsTab() {
		t.Error("a non-execution source must not own Tab")
	}
}

// The cursor CLAMPS at the top rather than wrapping: wrapping from the first block to the composer
// at the bottom of a long transcript is disorienting.
func TestCursorClampsAtTheTop(t *testing.T) {
	m, _ := modelWithTranscript(t)
	m.handleActionKey("up")
	if m.blocks.cursor.idx != 0 || m.blocks.cursor.atComposer {
		t.Errorf("up from the first block went to (%d, composer=%v) — it must clamp", m.blocks.cursor.idx, m.blocks.cursor.atComposer)
	}
}

// An execution with NO transcript still reaches the composer, which matters because asking about a
// silent run is exactly what an operator wants to do.
func TestComposerIsReachableWithNoTranscript(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")
	m.Base.SetFocusForTest("detail")
	if len(m.blockItems()) != 0 {
		t.Fatal("fixture: expected no transcript")
	}
	m.handleActionKey("down")
	if !m.blocks.cursor.atComposer {
		t.Error("down did not reach the composer with an empty transcript")
	}
}

// The cursor is DRAWN on the block it is on, and only there — a second marker would make "which
// block will enter toggle" ambiguous.
func TestCursorMarkerIsDrawnOnce(t *testing.T) {
	m, blocks := modelWithTranscript(t)
	m.blocks.cursor.idx = 2
	body, _ := renderBlocks(blocks, &m.blocks, 100, m.blocks.cursor)
	if got := strings.Count(body, "▸ ▸"); got != 1 {
		// The header is "marker + glyph": a collapsible block on the cursor reads "▸ ▸".
		t.Errorf("the cursor+disclosure pair appears %d times, want exactly once on the cursor row:\n%s", got, body)
	}
}

// --- helpers ------------------------------------------------------------------------------------

// modelWithTranscript builds a model whose detail pane holds the fixture transcript, and returns it
// with its blocks.
func modelWithTranscript(t *testing.T) (*Model, []textBlock) {
	t.Helper()
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")
	m.Base.SetFocusForTest("detail")
	m.execDetail.putTranscript("exec-1", transcriptItems())
	// Install the record too, so the pane HAS fields (the composer's send path reads the selected
	// row, and composeExecutionBody refuses to paint without a record).
	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)
	blocks := blocksFromItems(transcriptItems(), 100)
	m.clampBlockCursor(len(blocks))
	return m, blocks
}
