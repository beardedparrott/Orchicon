package execution

// execution_blocks.go — the execution transcript as COLLAPSIBLE BLOCKS with a movable cursor, and
// the inline composer at the bottom.
//
// The operator, two reports that are one mechanism:
//
//	1. "I would rather a chat box be at the bottom of the execution and you can click into there or
//	   gain focus with the down arrow key or tab and then type in your response and hit enter to
//	   send it. (Interjects should work the same way on live executions)"
//	2. "The executions are incredibly long. In the GUI, all blocks are auto collapsed. We should be
//	   collapsing all conversation blocks, tool calls, etc. and a user can move down the line using
//	   tab or down arrow and hitting enter on a block should collapse or expand it."
//
// BOTH are the same shape: the transcript is a LIST OF BLOCKS with a cursor, and the composer is
// the LAST POSITION on that list. Walking down past the last block lands on the composer; walking
// up from the composer returns to the blocks. That is why they are built together — a cursor that
// stops at the last block would need a separate gesture to reach the input, and a separate gesture
// is what made the old modal (`f`) unintuitive.
//
// WHAT THE GUI DOES, which this mirrors (frontend/src/components/executions/SessionChatPane.tsx):
//   - "tool calls are compact collapsible cards, reasoning is collapsed" — the two kinds that are
//     long and rarely wanted in full start CLOSED;
//   - every collapsible card is `useState(false)` — i.e. collapsed by default, not just the tools;
//   - the composer is a single always-available control whose PLACEHOLDER changes with the
//     execution's state: "Message the worker mid-run (no new work item is created)…" while running,
//     "Ask a follow-up — it continues this conversation…" when terminal. ONE control, because
//     SendExecutionMessage and ContinueExecutionSession are the same act from the operator's side —
//     which is also why the TUI must stop making them two chords (`i` vs `f`).
//
// WHY THE OLD CHORDS GO: `f` was bound to BOTH the follow-up box (this screen) and the base's
// "more pages" pager (`kit2.Base.loadMore`, advertised in the pane title as "more pages: press f").
// One key, two meanings, and the one the operator was reading was not the one that fired. The
// composer removes the collision by removing the chord: sending is `enter`, in the composer.

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/md"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// blockKind classifies a transcript item for collapsing.
type blockKind int

const (
	blockText      blockKind = iota // the model's prose, or the operator's own message
	blockTool                       // a tool call — long, collapsed by default (the GUI's rule)
	blockReasoning                  // thinking — long, collapsed by default (the GUI's rule)
	blockArtifact                   // a produced file
	blockError                      // an error — NEVER collapsed: it is why the operator is here
	blockSession                    // a session/serve marker row
)

// collapsible reports whether a kind starts collapsed and can be toggled. Errors are deliberately
// exempt: collapsing the one row that explains a failure would hide the thing being looked for.
func (k blockKind) collapsible() bool {
	switch k {
	case blockTool, blockReasoning, blockArtifact:
		return true
	}
	return false
}

// textBlock is one rendered block: its summary (always shown) plus, when expanded, its body.
//
// The SUMMARY is what makes collapsing useful rather than merely shorter: a collapsed tool call
// still says which tool ran and what it produced, so the line is scannable — which is the point of
// collapsing a 200-line JSON dump in the first place.
type textBlock struct {
	kind blockKind
	// key is the block's identity within the transcript, used to REMEMBER what the operator
	// expanded. It is the chat item's Key when it has one (the tool id / message id the session
	// parser assigns) and a positional fallback otherwise, so a live transcript that grows does not
	// rename the blocks above it.
	key string
	// summary is the row shown when collapsed (and the header when expanded).
	summary string
	// body is the full content, shown only when expanded.
	body string
	// live marks a block that is still streaming: it is drawn expanded, because watching it arrive
	// is the reason to look at a running execution at all. Its own collapse state is not consulted
	// until it settles.
	live bool
	// user marks the operator's own message, which is labelled on its first line.
	user bool
	// markdown marks a body the GUI renders through react-markdown, so the TUI renders it the same
	// way instead of printing the SOURCE. This mirrors the GUI exactly rather than generously: prose
	// and reasoning are markdown, a TOOL's output is not (ToolCard draws a <pre>), and an artifact is
	// markdown only when its type says so or its name ends in .md (ArtifactCard's own isMarkdown).
	markdown bool
}

// defaultExpanded reports whether a block is shown expanded with NO operator decision recorded.
//
// A settled block follows its kind (tools and thinking closed, prose open — the GUI's behaviour).
// A LIVE block is always open: an operator watching a run sees the answer being written, which is
// the entire value of the live view, and a collapsed live bubble would look like a stall.
func (b textBlock) defaultExpanded() bool {
	if b.live {
		return true
	}
	return !b.kind.collapsible()
}

// label names the block on its summary row.
func (b textBlock) label() string {
	switch b.kind {
	case blockTool:
		return "tool"
	case blockReasoning:
		return "thinking"
	case blockArtifact:
		return "artifact"
	case blockError:
		return "error"
	case blockSession:
		return "session"
	}
	return ""
}

// blocksFromItems converts a transcript into blocks.
//
// The rendering is done ONCE, here, into summary + body — so the painter only ever joins strings,
// and the collapse decision cannot accidentally change what a block CONTAINS.
func blocksFromItems(items []chat.ChatItem, width int) []textBlock {
	out := make([]textBlock, 0, len(items))
	for i, it := range items {
		b := textBlock{key: it.Key}
		if b.key == "" {
			// No id from the parser: fall back to the position. Stable for a given transcript, which
			// is all the collapse memory needs (it is cleared when the execution changes).
			b.key = fmt.Sprintf("#%d", i)
		}
		b.live = it.Live
		switch it.Kind {
		case chat.KindUser:
			b.kind = blockText
			b.body = it.Text
			b.markdown = true
			// The operator's own words are LABELLED on their first line rather than given a separate
			// header, so the band reads as one block.
			b.user = true
		case chat.KindText:
			b.kind = blockText
			b.body = it.Text
			b.markdown = true
		case chat.KindReasoning:
			b.kind = blockReasoning
			// The summary is the first RENDERED line, not the first source line: a collapsed block
			// whose preview read "## Findings" or an opening fence would be the raw source again,
			// which is the whole defect. Reasoning is markdown in the GUI (ReasoningBubble defaults
			// to the rendered view, with a Raw toggle beside it).
			b.summary = mdFirstLine(it.Text, width)
			b.body = it.Text
			b.markdown = true
		case chat.KindError:
			// ERRORS STAY RAW. An error is diagnostic text the operator reads verbatim, and
			// emphasis markers inside a stack trace are evidence, not formatting.
			b.kind = blockError
			b.summary = firstLine(it.Text)
			b.body = it.Text
		case chat.KindTool:
			b.kind = blockTool
			b.summary, b.body = toolSummaryBody(it.Tool)
		case chat.KindArtifact:
			b.kind = blockArtifact
			b.summary = it.Name + " (" + it.Type + ")"
			b.body = it.Content
			b.markdown = strings.EqualFold(it.Type, "markdown") ||
				strings.HasSuffix(strings.ToLower(it.Name), ".md")
		case chat.KindSession:
			b.kind = blockSession
			b.summary = "session " + it.SessionID
			if it.ServeURL != "" {
				b.summary += " · " + it.ServeURL
			}
		default:
			b.kind = blockText
			b.body = it.Text
			b.summary = firstLine(it.Text)
		}
		out = append(out, b)
	}
	return out
}

// toolSummaryBody splits a tool call into the one-line summary shown collapsed and the full
// input/output shown expanded.
//
// The collapsed line keeps BOTH the tool name and the first line of its output, because "which tool
// ran" and "what came back" are the two things an operator scans a collapsed transcript for.
func toolSummaryBody(t *chat.ParsedTool) (string, string) {
	if t == nil {
		return "(tool)", ""
	}
	sum := "⚙ " + t.ToolName
	if in := firstLine(t.Input); in != "" {
		sum += " " + in
	}
	if outLine := firstLine(t.Output); outLine != "" {
		sum += " → " + outLine
	}
	var body strings.Builder
	if strings.TrimSpace(t.Input) != "" {
		body.WriteString("input:\n" + strings.TrimRight(t.Input, "\n") + "\n")
	}
	if strings.TrimSpace(t.Output) != "" {
		body.WriteString("output:\n" + strings.TrimRight(t.Output, "\n"))
	}
	return sum, strings.TrimRight(body.String(), "\n")
}

// firstLine is the block's one-line summary: the first non-empty line, bounded.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return truncateForField(t, 120)
		}
	}
	return ""
}

// --- the cursor over the transcript -------------------------------------------------------------

// transcriptCursor is where the keyboard is in the execution detail: a block, or the composer.
//
// `atComposer` is a POSITION rather than a mode, which is what makes the operator's "move down the
// line" work: walking down past the last block lands in the composer, and walking up from the
// composer returns to the blocks. There is no separate key to learn.
type transcriptCursor struct {
	idx        int
	atComposer bool
}

// blockState remembers which blocks the operator has expanded or collapsed, and WHERE the cursor is
// in the transcript.
//
// Keyed by block key, NOT by index: a live transcript appends blocks and a re-fetch can change the
// count, and an index-keyed map would apply the operator's expansion to whatever block later
// occupied that slot. Cleared when the execution changes, because the keys are only unique within
// one transcript.
type blockState struct {
	execID string
	// overrides maps a block key to the operator's explicit decision (true = expanded).
	overrides map[string]bool
	// cursor is where the keyboard is: a block, or the composer at the bottom of the ladder.
	cursor transcriptCursor
}

// reset clears the memory when the pane shows a DIFFERENT execution.
func (s *blockState) reset(execID string) {
	if s.execID == execID {
		return
	}
	s.execID = execID
	s.overrides = nil
}

// expanded reports whether a block should be drawn expanded.
func (s *blockState) expanded(b textBlock) bool {
	if s.overrides != nil {
		if v, ok := s.overrides[b.key]; ok {
			return v
		}
	}
	return b.defaultExpanded()
}

// toggle flips a block's expansion and records the decision.
func (s *blockState) toggle(b textBlock) {
	if !b.kind.collapsible() {
		return // errors and prose are not collapsible (see blockKind.collapsible)
	}
	if s.overrides == nil {
		s.overrides = map[string]bool{}
	}
	s.overrides[b.key] = !s.expanded(b)
}

// renderBlocks draws the transcript with the cursor, collapsing what has not been expanded.
//
// It returns the body and a block-index → line-offset map, so the pane can scroll to FOLLOW the
// cursor: a real transcript is far taller than the pane, and a cursor you cannot see is one you
// cannot use (the same reason the run's step flow follows its cursor).
func renderBlocks(blocks []textBlock, state *blockState, width int, cur transcriptCursor) (string, []int) {
	var b strings.Builder
	offsets := make([]int, len(blocks))
	line := 0
	write := func(s string) {
		if s == "" {
			return
		}
		b.WriteString(s + "\n")
		line += strings.Count(s, "\n") + 1
	}

	for i, blk := range blocks {
		offsets[i] = line
		onCursor := !cur.atComposer && i == cur.idx
		marker := "  "
		if onCursor {
			marker = "▸ "
		}
		expanded := state.expanded(blk)

		// The header row: marker, a disclosure glyph for anything collapsible, the kind label when
		// it has one, then the summary.
		//
		// A PROSE block has no header of its own to draw: its summary IS its first line, so printing
		// both printed the opening sentence twice (the content, then the body that contains it).
		// Prose is shown whole, preceded only by the cursor marker.
		glyph := ""
		if blk.kind.collapsible() {
			if expanded {
				glyph = "▾ "
			} else {
				glyph = "▸ "
			}
		}
		prose := blk.kind == blockText
		if prose {
			// The whole block is the content; the marker (plus "You" for the operator's own words)
			// carries the cursor.
			header := marker
			if blk.user {
				header += theme.BubbleUser.Render("You") + " "
			}
			if onCursor {
				header = theme.ListTitle.Render(header)
			}
			if strings.TrimSpace(header) != "" {
				write(" " + header)
			}
		} else {
			header := marker + glyph
			if lbl := blk.label(); lbl != "" {
				header += theme.HintText.Render(lbl) + " "
			}
			header += blk.summary
			if onCursor {
				header = theme.ListTitle.Render(header)
			}
			write(" " + header)
		}

		// The body, indented under its header. A collapsed block emits NOTHING here — not an empty
		// row — so a collapsed transcript really is one line per block.
		if expanded && strings.TrimSpace(blk.body) != "" {
			for _, l := range blk.bodyLines(width - len(bodyIndent)) {
				write(bodyIndent + l)
			}
		}
		write("")
	}
	return strings.TrimRight(b.String(), "\n"), offsets
}

// bodyIndent is the columns a block's body is indented by, under its header row.
const bodyIndent = "   "

// bodyLines renders a block's body to display lines at the available width.
//
// A markdown body goes through the renderer at the width LEFT AFTER the indent, which is what keeps
// every emitted line inside the pane — there is no horizontal scroll here, and the pane truncates, so
// an over-wide line is content lost with no error. Tool output and errors stay raw (see textBlock).
func (b textBlock) bodyLines(width int) []string {
	if b.markdown {
		// RenderOn with the surface this body is drawn on, so inline code uses the themed CHIP rather
		// than reverse video. Reverse video is theme-blind: on a light terminal it inverts to a dark
		// block with white text, which is the operator's "black text background you can't see
		// anything" across the execution transcript. The surface is the detail pane's, since that is
		// what hosts these blocks.
		if lines := md.RenderOn(b.body, width, md.SurfaceTokens(theme.Text, theme.Bg)); len(lines) > 0 {
			return lines
		}
	}
	return strings.Split(strings.TrimRight(b.body, "\n"), "\n")
}

// mdFirstLine is the first VISIBLE line of a markdown body, for a collapsed block's summary.
func mdFirstLine(text string, width int) string {
	for _, l := range md.Render(text, width) {
		if s := strings.TrimSpace(ansi.Strip(l)); s != "" {
			return s
		}
	}
	return firstLine(text)
}

// --- the inline composer ------------------------------------------------------------------------

// composer holds the execution detail's inline message box.
//
// It is the TUI's answer to the GUI's composer: ONE control that nudges a live session or asks a
// follow-up on a finished one, with the placeholder saying which. That is the operator's model —
// "Interjects should work the same way on live executions" — and it removes the two-chord split
// (`i` / `f`) that was neither intuitive nor discoverable.
type composer struct {
	value string
	// cursor is the caret's position in RUNES (not bytes — the draft is arbitrary text).
	cursor int
	err    string
	// busy is set while a send is in flight, so a double-enter cannot post twice.
	busy bool
}

// focused reports whether the composer holds the keyboard.
func (m *Model) composerFocused() bool { return m.blocks.cursor.atComposer }

// composerPlaceholder states what sending will DO, which is the GUI's own rule: the same box means
// "message the worker" mid-run and "ask a follow-up" afterwards.
func composerPlaceholder(meta string) string {
	if isLiveExecution(meta) {
		return "Message the worker mid-run (no new work item is created) — enter sends"
	}
	return "Ask a follow-up — it continues this conversation — enter sends"
}

// insertComposerText inserts at the caret.
func (c *composer) insertText(s string) {
	r := []rune(c.value)
	i := c.cursor
	if i > len(r) {
		i = len(r)
	}
	c.value = string(append(append(append([]rune{}, r[:i]...), []rune(s)...), r[i:]...))
	c.cursor = i + len([]rune(s))
}

// key handles a key while the composer has the keyboard. handled is false for keys the composer
// does not own, so the caller can route them onwards (esc leaves the box, up goes back to the
// blocks).
func (c *composer) key(kstr string, runes []rune) (handled bool) {
	switch kstr {
	case "left":
		if c.cursor > 0 {
			c.cursor--
		}
		return true
	case "right":
		if r := []rune(c.value); c.cursor < len(r) {
			c.cursor++
		}
		return true
	case "home", "ctrl+a":
		c.cursor = 0
		return true
	case "end", "ctrl+e":
		c.cursor = len([]rune(c.value))
		return true
	case "backspace":
		r := []rune(c.value)
		if c.cursor > 0 && c.cursor <= len(r) {
			c.value = string(append(append([]rune{}, r[:c.cursor-1]...), r[c.cursor:]...))
			c.cursor--
		}
		return true
	case "delete", "ctrl+d":
		r := []rune(c.value)
		if c.cursor >= 0 && c.cursor < len(r) {
			c.value = string(append(append([]rune{}, r[:c.cursor]...), r[c.cursor+1:]...))
		}
		return true
	case "ctrl+u":
		// Clear the line, which is the standard readline gesture and the only way to abandon a
		// half-typed question without leaving the box.
		c.value = ""
		c.cursor = 0
		return true
	}
	// Printable input. Multi-rune messages arrive as Runes.
	if len(runes) > 0 {
		for _, r := range runes {
			if r < 0x20 || r == 0x7f {
				return false // a control character we do not own (enter/tab/esc handled above)
			}
		}
		c.insertText(string(runes))
		return true
	}
	return false
}

// composerLine renders the box as ONE line (the input is a single line; a long draft scrolls).
//
// A one-line box is the right shape here: the operator is asking a question, not composing a
// document, and a growing multi-line box inside a fixed-height detail pane would push the
// transcript out of view — the surface they need in order to ask a sensible question.
func (c *composer) render(width int, meta string, focused bool) string {
	label := "message"
	if !isLiveExecution(meta) {
		label = "follow-up"
	}
	border := theme.Border
	if focused {
		border = theme.AccentCyan
	}
	_ = border

	prompt := theme.HintText.Render(label + "› ")
	if strings.TrimSpace(c.value) == "" {
		ph := composerPlaceholder(meta)
		if focused {
			// While focused, the CARET is drawn, so "click here and type" is unambiguous.
			return prompt + theme.HintText.Render(caretBar()+ph)
		}
		return prompt + theme.HintText.Render(ph)
	}
	// The caret splits the value, so the operator can see where they are typing.
	r := []rune(c.value)
	i := c.cursor
	if i > len(r) {
		i = len(r)
	}
	out := prompt + string(r[:i]) + caretBar() + string(r[i:])
	if c.err != "" {
		out += "\n" + theme.ErrorText.Render(c.err)
	}
	return out
}

// caretBar is the insertion point. A reverse-video space reads as a caret on every terminal, which
// a bare "|" does not when the text around it is already styled.
func caretBar() string { return "\x1b[7m \x1b[27m" }

// sendComposer posts the draft: SendExecutionMessage for a LIVE execution, ContinueExecutionSession
// for a terminal one — the GUI's own branch, on the GUI's own control.
func (m *Model) sendComposer() tea.Cmd {
	draft := strings.TrimSpace(m.composer.value)
	if draft == "" {
		return nil
	}
	if m.Base.ActiveSourceName() != srcExecutions {
		return nil
	}
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	if isLiveExecution(it.Meta) {
		m.composer.value = ""
		m.composer.cursor = 0
		m.composer.err = ""
		return m.Mutate(mutate.Request{
			Name:   "message worker " + id,
			Source: srcExecutions,
			Do: func(ctx context.Context) error {
				return m.rpcSendMessage(ctx, id, draft)
			},
		})
	}
	// Terminal: a follow-up, whose reply lands in the transcript (the reply is collected
	// asynchronously and appended to the session, which the pane re-reads).
	m.composer.value = ""
	m.composer.cursor = 0
	m.composer.err = ""
	return m.Mutate(mutate.Request{
		Name:   "follow-up " + id,
		Source: srcExecutions,
		Do: func(ctx context.Context) error {
			_, err := m.cl.Executions.ContinueExecutionSession(ctx, connect.NewRequest(&apiv1.ContinueExecutionSessionRequest{
				ExecutionId: id, Message: draft,
			}))
			if err != nil {
				return err
			}
			m.notice = "follow-up sent — its answer joins the transcript when it arrives"
			return nil
		},
	})
}

// --- the key routing ----------------------------------------------------------------------------

// handleTranscriptKeys drives the block cursor and the composer for the Executions detail.
//
// handled is false when the key belongs to the navigation layer, so this only ever claims what it
// acts on — the same contract as every other chord on this screen.
//
// The two states are ONE ladder rather than two modes:
//
//	blocks focused:  up/down (tab/shift+tab) walk · enter toggles · `i`/`esc`… fall through
//	composer focused: everything types · enter SENDS · esc leaves · up returns to the blocks
//
// `up` from the FIRST block stays on the first block (clamping, not wrapping): wrapping from the
// top to the bottom of a long transcript is disorienting, and the composer is at the bottom where a
// wrap would land.
func (m *Model) handleTranscriptKeys(kstr string) (tea.Cmd, bool) {
	// NOT gated on DetailID. The first version was, and it made the box unreachable in the one
	// state an operator hits constantly: DetailID is set by the DETAIL LANDING, so between
	// selecting a row and the detail resolving there is no id — and the composer was dead for that
	// whole window, which reads as "the chat box does not work". The composer's own send path
	// resolves the execution from the SELECTED ROW, so it needs no id to be usable.
	if m.blocks.cursor.atComposer {
		return m.composerKeys(kstr)
	}
	return m.blockKeys(kstr)
}

// composerKeys handles the keyboard while the composer holds it.
func (m *Model) composerKeys(kstr string) (tea.Cmd, bool) {
	switch kstr {
	case "esc":
		// Leaving the box keeps the DRAFT: an accidental esc must not destroy a typed question.
		m.blocks.cursor.atComposer = false
		return nil, true
	case "up":
		m.blocks.cursor.atComposer = false
		return m.repaintTranscript(), true
	case "tab", "shift+tab":
		// TAB LEAVES THE CHAT — the operator's "we should allow tab to tab between the Execution
		// details and the chat prompt. Currently once you go into a chat in an execution you are
		// locked in it and can't get out."
		//
		// It used to be claimed and then DROPPED ("nothing below the composer"), so the key vanished;
		// and where the shell won the race instead it moved the top menu while the caret stayed here,
		// which reads as stuck. This is the same toggle as blockKeys' — the pane has two positions,
		// so forward and reverse both land on the other one — and OwnsTab is what stops the shell
		// acting on it first.
		m.blocks.cursor.atComposer = false
		return m.repaintTranscript(), true
	case "enter":
		// ENTER SENDS — the operator's gesture ("type in your response and hit enter to send it").
		// The GUI uses the same rule (enter sends, shift+enter newlines), and this box is one line.
		return m.sendComposer(), true
	case "down":
		// Already at the bottom of the ladder: nothing below the composer.
		return nil, true
	}
	runes := []rune(nil)
	if r := []rune(kstr); len(r) == 1 && r[0] >= 0x20 && r[0] != 0x7f {
		runes = r
	}
	if m.composer.key(kstr, runes) {
		return m.repaintTranscript(), true
	}
	return nil, false
}

// blockKeys handles the keyboard while the block cursor holds it.
func (m *Model) blockKeys(kstr string) (tea.Cmd, bool) {
	n := len(m.blockItems())
	switch kstr {
	case "down", "j":
		if n == 0 {
			// No transcript (an execution with no session): the composer is still reachable, which
			// matters because asking a follow-up is exactly what an operator wants on a silent run.
			m.blocks.cursor.atComposer = true
			return m.repaintTranscript(), true
		}
		if m.blocks.cursor.idx >= n-1 {
			// Past the last block: into the composer. This IS the operator's "gain focus with the
			// down arrow key" — no separate key to learn.
			m.blocks.cursor.atComposer = true
			return m.repaintTranscript(), true
		}
		m.blocks.cursor.idx++
		return m.repaintTranscript(), true
	case "tab", "shift+tab":
		// TAB MOVES TO THE MESSAGE BOX — the other half of the pane's two-region toggle (see
		// composerKeys). Tab used to WALK THE BLOCKS here, which made it a second Down; the operator
		// asked for it to move between the two regions instead, and up/down still walk the transcript
		// (down past the last block lands in the box, exactly as before).
		m.blocks.cursor.atComposer = true
		return m.repaintTranscript(), true
	case "up", "k":
		if m.blocks.cursor.idx > 0 {
			m.blocks.cursor.idx--
		}
		return m.repaintTranscript(), true
	case "enter":
		// Toggle the block under the cursor. A non-collapsible block (prose, an error) has nothing
		// to toggle, so enter is left unclaimed there rather than swallowed.
		items := m.blockItems()
		if m.blocks.cursor.idx < 0 || m.blocks.cursor.idx >= len(items) {
			return nil, false
		}
		blocks := blocksFromItems(items, m.w)
		blk := blocks[m.blocks.cursor.idx]
		if !blk.kind.collapsible() {
			return nil, false
		}
		m.blocks.toggle(blk)
		return m.repaintTranscript(), true
	}
	return nil, false
}

// repaintTranscript redraws the pane from its CACHED parts, so moving the cursor or toggling a block
// costs no round trip — the same rule the runs step flow follows.
func (m *Model) repaintTranscript() tea.Cmd {
	id := m.Base.DetailID()
	if id == "" || m.Base.ActiveSourceName() != srcExecutions {
		return nil
	}
	body, fields := m.composeExecutionBody(id)
	if len(fields) == 0 {
		return nil // the record has not landed; there is nothing to draw yet
	}
	m.paintExecution(id, fields, body)
	return m.scrollToBlockCursor()
}

// scrollToBlockCursor pins the pane so the cursor is VISIBLE.
//
// A real transcript is far taller than the detail pane, so without this the cursor would walk off
// the bottom of the screen and the operator would be toggling blocks they cannot see — the same
// defect the run's step flow had to solve.
func (m *Model) scrollToBlockCursor() tea.Cmd {
	if m.blocks.cursor.atComposer {
		// The composer is the LAST row of the body, so pinning to the bottom is exact.
		m.Base.SetDetailScrollBottom()
		return nil
	}
	items := m.blockItems()
	if len(items) == 0 {
		return nil
	}
	blocks := blocksFromItems(items, m.w)
	_, offsets := renderBlocks(blocks, &m.blocks, m.w, m.blocks.cursor)
	if m.blocks.cursor.idx >= len(offsets) {
		return nil
	}
	// The body has sections ABOVE the transcript (context, todos, the record), so the block's
	// offset within the transcript is not its offset in the pane. The prefix is measured by
	// composing the body WITHOUT the transcript and counting its rows — exact, and cheap (it is
	// strings, not I/O).
	prefix := m.transcriptPrefixRows()
	line := prefix + offsets[m.blocks.cursor.idx]
	scroll := line - 2
	if scroll < 0 {
		scroll = 0
	}
	m.Base.SetDetailScrollTop(scroll)
	return nil
}

// transcriptPrefixRows counts the body rows that precede the transcript, so a block's offset can be
// translated into a pane offset.
func (m *Model) transcriptPrefixRows() int {
	id := m.Base.DetailID()
	rows := 0
	if u := m.execUsage.get(id); u != nil {
		if s := renderUsage(u, m.w); s != "" {
			rows += strings.Count(s, "\n") + 2 // + the section separator
		}
	}
	if s := renderTodos(m.todos.get(id), m.w); s != "" {
		rows += strings.Count(s, "\n") + 2
	}
	if p := m.execDetail.parts(id); p != nil && p.record != "" {
		rows += strings.Count(p.record, "\n") + 2
	}
	return rows
}

// paintExecution is the ONE place an execution detail is written to the pane, so the fixed message
// box cannot be forgotten by any of the three callers (the detail landing, a cache repaint, and a
// cursor move).
//
// THE COMPOSER IS A PANE FOOTER, NOT A BODY SECTION. It used to be appended to the body — which
// reads correctly and is unusable: the transcript is taller than the pane, the viewport opens at the
// TOP, and the input therefore sat ~65 lines below the fold. The operator reported exactly that
// ("I STILL don't see a chat prompt inside an execution in the TUI") and the prompt had been there
// the whole time, just never on screen. A footer is the shape an input needs: always visible,
// never scrolled away (screenkit.Detail.SetFooter).
//
// It is installed even when there is NO transcript, because an execution that produced nothing is
// precisely when the operator wants to ask why.
func (m *Model) paintExecution(id string, fields []screenkit.Field, body string) {
	m.installComposerFooter(id)
	m.Base.SetDetailContentLaidOut("Execution "+id, fields, body)
}

// installComposerFooter puts the message box in the pane's FIXED footer band.
//
// It is idempotent and state-driven (the band's placeholder depends on whether the execution is live
// and whether the box has the keyboard), so every caller can call it unconditionally: the detail
// landing, a cache repaint, and a cursor move all end with the band in the right state.
func (m *Model) installComposerFooter(id string) {
	if m.Base.ActiveSourceName() != srcExecutions {
		// Another source's detail is showing: the band must not linger over it.
		m.Base.SetDetailFooter("")
		return
	}
	m.Base.SetDetailFooter(m.composer.render(m.w, m.execMeta(id), m.blocks.cursor.atComposer))
}
