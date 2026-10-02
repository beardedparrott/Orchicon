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
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
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
	// One row per block PLUS the gap row between them: the gap is blockGap rows per BOUNDARY, so n
	// blocks occupy n + blockGap*(n-1) rows before slack. (Before the gap existed this was "roughly
	// one line per block"; the gap is what makes the blocks readable, so the bound moves with it
	// rather than the gap being dropped to keep a stale number.)
	budget := len(blocks) + blockGap*(len(blocks)-1) + 3
	if strings.Count(body, "\n")+1 > budget {
		t.Errorf("a collapsed transcript should be roughly one line per block plus the gap; got %d lines for %d blocks (budget %d):\n%s",
			strings.Count(body, "\n")+1, len(blocks), budget, body)
	}
	if !strings.Contains(body, "bash") {
		t.Error("the collapsed tool SUMMARY is missing — a collapsed tool call must say which tool ran")
	}
}

// BLOCKS ARE SEPARATED BY A BLANK ROW — the operator's "the text in executions in the TUI are hard to
// read. They are very scrunched up."
//
// The separator was written through the same closure that renders a block's rows, and that closure
// SKIPS an empty string (which is how a collapsed block contributes nothing) — so the separator was
// silently dropped and every block's last row ran straight into the next block's header. Measured
// before the fix, the six-block fixture rendered as six consecutive non-blank rows.
//
// The assertion uses the offsets renderBlocks returns, because those are what the pane SCROLLS by:
// pinning the gap through them covers the layout and the scroll math in one go.
func TestBlocksAreSeparatedByABlankRow(t *testing.T) {
	blocks := blocksFromItems(transcriptItems(), 100)
	body, offsets := renderBlocks(blocks, &blockState{}, 100, transcriptCursor{})
	rows := strings.Split(body, "\n")
	if len(offsets) != len(blocks) {
		t.Fatalf("offsets cover %d blocks, want %d", len(offsets), len(blocks))
	}
	for i := 1; i < len(blocks); i++ {
		at := offsets[i]
		if at <= 0 || at > len(rows) {
			t.Fatalf("block %d starts at row %d, outside the %d rendered rows", i, at, len(rows))
		}
		// The row ABOVE a block (other than the first) is the gap.
		if gap := rows[at-1]; strings.TrimSpace(ansi.Strip(gap)) != "" {
			t.Errorf("block %d starts on row %d with no blank row before it — the block above runs into it:\n%s",
				i, at, body)
		}
	}
	// And the gap is exactly blockGap rows: a run of blank rows would be dead space (a stray extra
	// newline is how the gap would silently double if the writer were changed). A run of k blank rows
	// is k+1 consecutive newlines, so ONE blank row — the expected gap — shows up as "\n\n".
	if strings.Contains(body, strings.Repeat("\n", blockGap+2)) {
		t.Errorf("the transcript contains more than %d blank row(s) in a row; the gap is %d", blockGap, blockGap)
	}
}

// AN ERROR'S TEXT IS PRINTED ONCE. An error block is never collapsible, so its summary is not a
// preview — it is the header itself. Keeping the whole text in the body as well printed the same
// sentence on two consecutive rows ("error boom: index out of range" / "boom: index out of range").
func TestErrorBlockPrintsItsTextOnce(t *testing.T) {
	items := []chat.ChatItem{{Kind: chat.KindError, Text: "boom: index out of range", Key: "e1"}}
	body, _ := renderBlocks(blocksFromItems(items, 100), &blockState{}, 100, transcriptCursor{})
	if got := strings.Count(body, "boom: index out of range"); got != 1 {
		t.Errorf("the error text appears %d times, want ONCE:\n%s", got, body)
	}
	if !strings.Contains(body, "error") {
		t.Errorf("the error block lost its label:\n%s", body)
	}

	// A MULTI-LINE error keeps everything below its first line — the split removes the duplication,
	// not the diagnostic text (which is what an operator reads an execution for).
	multi := chat.ChatItem{Kind: chat.KindError, Key: "e2", Text: "boom\n  at parser.go:12\n  at main.go:3"}
	body, _ = renderBlocks(blocksFromItems([]chat.ChatItem{multi}, 100), &blockState{}, 100, transcriptCursor{})
	for _, want := range []string{"boom", "at parser.go:12", "at main.go:3"} {
		if !strings.Contains(body, want) {
			t.Errorf("a multi-line error dropped %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, "boom"); got != 1 {
		t.Errorf("the first line of a multi-line error appears %d times, want ONCE:\n%s", got, body)
	}
}

// splitFirstLine is the error-block split as a unit: first non-empty line, then the remainder.
func TestSplitFirstLine(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		summary string
		body    string
	}{
		{"one line", "boom", "boom", ""},
		{"two lines", "boom\n  at parser.go:12", "boom", "  at parser.go:12"},
		{"leading blanks are skipped", "\n\nboom\nrest", "boom", "rest"},
		{"empty", "", "", ""},
		{"only blanks", "  \n\t\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSummary, gotBody := splitFirstLine(c.in)
			if gotSummary != c.summary || gotBody != c.body {
				t.Errorf("splitFirstLine(%q) = (%q, %q), want (%q, %q)", c.in, gotSummary, gotBody, c.summary, c.body)
			}
		})
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

// A LIVE BLOCK'S BODY IS BOUNDED, and says so. A live block is drawn expanded without the operator
// having asked (defaultExpanded), so a long one — streamed reasoning is routinely tens of thousands of
// characters — buried the rest of the transcript in text nobody chose to open. That is the execution half
// of the report "tools and thought are not being collapsed": the TOOLS were collapsed and the streamed
// THINKING was drawn in full, because it was live.
//
// The bound is the chat pane's own (reasoningBodyMaxRows), so the two panes show the same amount of the
// model's thinking, and the elision is EXPLICIT: a silent cut reads as the end of the reasoning.
//
// The fixture is a TOOL block on purpose: a tool body is RAW text (ToolCard draws a <pre>), so one source
// line is one rendered row and the count is exact. A MARKDOWN body is re-flowed — consecutive lines join
// into a paragraph — so counting occurrences there measures the renderer's wrapping, not the bound.
func TestLiveBodiesAreBoundedAndSaySo(t *testing.T) {
	const bodyLines = liveBodyMaxRows * 3
	var out []string
	for i := 0; i < bodyLines; i++ {
		out = append(out, fmt.Sprintf("out-%02d", i))
	}
	live := []chat.ChatItem{{Kind: chat.KindTool, Key: "t1", Live: true,
		Tool: &chat.ParsedTool{ID: "t1", ToolName: "bash", Output: strings.Join(out, "\n")}}}
	blocks := blocksFromItems(live, 100)
	if !blocks[0].live || !blocks[0].defaultExpanded() {
		t.Fatal("fixture: a live block is drawn expanded")
	}
	body, _ := renderBlocks(blocks, &blockState{}, 100, transcriptCursor{})

	drawn := 0
	for i := 0; i < bodyLines; i++ {
		if strings.Contains(body, fmt.Sprintf("out-%02d", i)) {
			drawn++
		}
	}
	if drawn != liveBodyMaxRows-1 {
		t.Errorf("a live block drew %d output lines, want %d — the bound is %d ROWS of the body, and "+
			"the body's own %q label occupies one of them:\n%s",
			drawn, liveBodyMaxRows-1, liveBodyMaxRows, "output:", body)
	}
	if !strings.Contains(body, "more lines") {
		t.Errorf("the bound is SILENT — an operator would read it as the end of the thinking:\n%s", body)
	}
	if !strings.Contains(body, "expand") {
		t.Errorf("the elision does not say the rest is reachable:\n%s", body)
	}

	// A SETTLED block is NOT bounded once the operator expands it: they asked for all of it, and
	// truncating that would be the same defect in reverse.
	settled := blocksFromItems([]chat.ChatItem{{Kind: chat.KindTool, Key: "t1",
		Tool: &chat.ParsedTool{ID: "t1", ToolName: "bash", Output: strings.Join(out, "\n")}}}, 100)
	state := &blockState{}
	state.toggle(settled[0]) // tools start collapsed; the operator opens this one
	if !state.expanded(settled[0]) {
		t.Fatal("fixture: the operator's expansion was not recorded")
	}
	got, _ := renderBlocks(settled, state, 100, transcriptCursor{})
	all := 0
	for i := 0; i < bodyLines; i++ {
		if strings.Contains(got, fmt.Sprintf("out-%02d", i)) {
			all++
		}
	}
	if all != bodyLines {
		t.Errorf("an OPERATOR-EXPANDED block drew %d of %d lines — a block they opened must not be cut",
			all, bodyLines)
	}
	if strings.Contains(got, "more lines") {
		t.Error("an operator-expanded block carries an elision marker")
	}

	// The MARKDOWN path is bounded too (streamed reasoning is markdown in the GUI), which is the case
	// the operator actually hit.
	var think []string
	for i := 0; i < bodyLines; i++ {
		think = append(think, fmt.Sprintf("thought %02d about the problem", i))
	}
	liveThink := blocksFromItems([]chat.ChatItem{{Kind: chat.KindReasoning, Key: "r1", Live: true,
		Text: strings.Join(think, "\n\n")}}, 100)
	got, _ = renderBlocks(liveThink, &blockState{}, 100, transcriptCursor{})
	if !strings.Contains(got, "more lines") {
		t.Errorf("a live MARKDOWN body is unbounded — the bound must cover the markdown path, which is "+
			"where streamed reasoning lands:\n%s", got)
	}
}

// THE COLLAPSE DECISION AND THE LAYOUT ARE THEME-INDEPENDENT. The operator narrowed the report to the
// transparent themes ("I believe it may only be happening in the transparent themes. It looks like tools
// and thought are not being collapsed"), and that is worth pinning either way: a transparent theme paints
// no fills, so the ONLY thing separating one block from the next is the layout this renderer emits. If a
// future change ever made collapsing theme-dependent, the transcript would read as one wall of text on a
// transparent theme and this test would catch it.
func TestTranscriptLayoutIsThemeIndependent(t *testing.T) {
	items := transcriptItems()
	var first string
	for _, name := range []string{"obsidian", "obsidian-transparent", "lumen-transparent", "light"} {
		if !theme.Use(name) {
			t.Fatalf("theme %q not found — the named transparent variants must exist", name)
		}
		blocks := blocksFromItems(items, 100)
		for _, b := range blocks {
			// The COLLAPSE decision, which is the operator's actual complaint.
			if b.kind.collapsible() && b.defaultExpanded() {
				t.Errorf("theme %s: a %s block is drawn EXPANDED — collapsing must not depend on the theme",
					name, b.label())
			}
		}
		body, offsets := renderBlocks(blocks, &blockState{}, 100, transcriptCursor{})
		got := fmt.Sprintf("%v\n%s", offsets, ansi.Strip(body))
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("theme %s renders a DIFFERENT transcript from the first theme — the layout (and so the "+
				"only separator a transparent theme has) must not vary by palette", name)
		}
	}
	theme.Use("obsidian") // leave the process on the default
}
