package chat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/md"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// view.go renders grouped ChatItems as themed terminal rows — the TUI
// ToolCard/ArtifactCard equivalents: one row per tool call / artifact,
// wrapped bubble rows for user/assistant/reasoning/error text.

// RenderItems renders the items into lines clamped to maxWidth (0 =
// unlimited). Grouping is the caller's concern (GroupByPhase /
// MergeSessionItems); this renders what it is given.
//
// The two speakers are separated by a FULL-WIDTH background band: the
// operator's messages are right-aligned on the lighter fill, the model's
// left-aligned on the darker one. The fills are derived per palette and gated
// by TestBubbleContrast (separation + legibility on every palette).
func RenderItems(items []ChatItem, maxWidth int, collapse ...func(key string) bool) string {
	folded := func(string) bool { return false }
	if len(collapse) > 0 && collapse[0] != nil {
		folded = collapse[0]
	}
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case KindUser:
			b.WriteString(renderChatMessage(userTextWithMarkers(it), theme.BubbleUser, maxWidth, true, userBandLabel))
		case KindText:
			b.WriteString(renderChatMessage(it.Text, theme.BubbleModel, maxWidth, false, ""))
		case KindReasoning:
			// A REASONING BLOCK, not a dim paragraph — the GUI's own shape, and COLLAPSIBLE.
			//
			// The GUI renders reasoning as a card whose header reads "reasoning · thinking…" while the
			// model is still reasoning and "reasoning · 60,909 chars" once it has stopped, with an arrow
			// that hides the body. The TUI drew the same content as
			// `renderMarkdownBubble("thinking", …, theme.HintText, …)` — one dim word, in the same style as
			// every hint line in the app — so a 60,909-character stream was indistinguishable from a
			// footnote, and the operator's report was exactly that: "No reasoning block."
			//
			// THE ARROW IS THE OPERATOR'S OWN ASK: "reasoning should look similar but have a arrow on the
			// left to expand and collapse". The transcript KEEPS ITS BUBBLES — the user and model bands and
			// this block are all the bubble shape they have always been — rather than adopting the
			// execution pane's card layout.
			//
			// collapse is a PARAMETER rather than package state because this is a pure function of its
			// input and two panes render the same items at different widths; a package-level map would make
			// one pane's folds apply to the other.
			b.WriteString(renderReasoningBlock(it, maxWidth, folded(it.Key)))
		case KindError:
			b.WriteString(renderBubble("error", it.Text, theme.ErrorText, maxWidth))
		case KindTool:
			b.WriteString(renderToolRow(it.Tool, maxWidth))
		case KindArtifact:
			b.WriteString(renderArtifactRow(it, maxWidth))
		case KindSession:
			meta := "session " + it.SessionID
			if it.ServeURL != "" {
				meta += " · " + it.ServeURL
			}
			b.WriteString(theme.HintText.Render(truncateRow(meta, maxWidth)) + "\n")
		}
	}
	return b.String()
}

// chatBandGap is the number of BLANK rows emitted after each message band, so
// the two speakers read as separate blocks of text rather than one continuous
// fill (the operator's "there should be a visible gap between user messages and
// model messages ... just enough to really show a gap"). Tune here.
const chatBandGap = 3

// userBandLabel names the operator's own messages at the band's left edge.
//
// It is not decoration. The operator's bands are right-aligned on a full-width
// fill, and when one appeared to be missing there was no way to tell "the
// message was never rendered" from "it rendered and was overlooked" — which is
// exactly the ambiguity that kept this open. Naming the speaker makes the band
// unmistakable, and the label's presence or absence now answers the question
// directly.
const userBandLabel = "You"

// renderChatMessage renders one message as a FULL-WIDTH background band: every
// line is padded to the pane's width and painted with the speaker's fill, so
// the band runs from the left edge of the conversation pane to its right edge
// (the operator's "a whole background color change behind the entire block of
// text ... from beginning to end of the width of the conversation for each
// section ... a lighter or darker color"). The operator's messages sit at the
// RIGHT of their band, the model's at the LEFT.
//
// The padding is rendered INSIDE the style, which is the whole point: filling
// only the text and leaving the margin unstyled is what made an earlier
// attempt read as "just different colored text" rather than a block.
//
// userTextWithMarkers appends the attachment markers to the operator's own text for DISPLAY.
//
// The operator said an attachment "shows up as [image] in the chat prompt", so the marker rides with the
// message it belongs to and the operator can see, on scrolling back, that a turn carried a screenshot. It is
// applied at RENDER time rather than stored into Text, so the durable copy of the message — which is what the
// server keeps and what the optimistic-echo dedupe matches on — stays exactly the text that was sent.
func userTextWithMarkers(it ChatItem) string {
	if len(it.Attachments) == 0 {
		return it.Text
	}
	markers := strings.Join(it.Attachments, " ")
	if strings.TrimSpace(it.Text) == "" {
		return markers
	}
	return it.Text + "\n" + markers
}

func renderChatMessage(text string, style lipgloss.Style, maxWidth int, right bool, label string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	pane := maxWidth
	if pane <= 0 {
		pane = 80
	}
	// One cell of padding inside the band on each side.
	inner := pane - 2
	if inner < 8 {
		inner = pane
	}
	// THE BODY IS MARKDOWN — the same text the GUI renders through react-markdown in
	// AssistantBubble and in the operator's own bubble. The operator saw the raw source instead:
	// "The thing that concerned me was those greater than signs. What are those representing? It
	// seemed kind of ugly." Those were BLOCKQUOTE markers doing real work (marking quoted wording),
	// and printed literally they read as punctuation soup.
	//
	// This is safe INSIDE a band because the renderer emits attributes only, each closed with a
	// TARGETED off-code (`\x1b[22m`) rather than a full reset (`\x1b[0m`) — so the band's own
	// foreground and background survive every styled span. A markdown renderer that coloured its
	// text would punch a hole in this fill at the first bold word.
	//
	// The width is the band's INNER width, so a markdown line can never exceed the pane: there is no
	// horizontal scroll in the band to fall back on.
	body := md.RenderOn(text, inner, md.SurfaceOf(style))
	if len(body) == 0 {
		body = []string{text}
	}

	var out strings.Builder
	for i, l := range body {
		pad := inner - lipgloss.Width(l)
		if pad < 0 {
			pad = 0
		}
		var row string
		switch {
		case i == 0 && label != "":
			// Label at the band's LEFT edge, text at the RIGHT: the speaker is
			// named and the alignment still reads as the operator's side. A
			// right-aligned band with no label is genuinely easy to miss — and
			// when a message looked absent the only way to tell "not rendered"
			// from "rendered and overlooked" was to be told which it was.
			gap := pad - lipgloss.Width(label)
			if gap < 1 {
				gap = 1
			}
			row = " " + label + strings.Repeat(" ", gap) + l + " "
		case right:
			row = " " + strings.Repeat(" ", pad) + l + " "
		default:
			row = " " + l + strings.Repeat(" ", pad) + " "
		}
		out.WriteString(style.Render(row))
		out.WriteString("\n")
	}
	// Separate this band from the next with BLANK rows (the pane's own
	// background), so consecutive messages read as distinct blocks.
	for i := 0; i < chatBandGap; i++ {
		out.WriteString("\n")
	}
	return out.String()
}

// renderMarkdownBubble is renderBubble for content the GUI renders as MARKDOWN (its ReasoningBubble
// defaults to the rendered view, with a Raw toggle beside it). The label keeps its own styling; the
// body goes through the markdown renderer at the width left after the label.
//
// The markdown output carries ONLY targeted attribute codes, so it nests inside the theme style
// without clearing it — which is what makes this safe over a labelled, styled row.
func renderMarkdownBubble(label, text string, style lipgloss.Style, maxWidth int) string {
	if text == "" {
		return ""
	}
	avail := maxWidth
	if avail > 0 {
		avail -= lipgloss.Width(label) + 3
	}
	lines := md.RenderOn(text, avail, md.SurfaceOf(style))
	if len(lines) == 0 {
		lines = []string{text}
	}
	var b strings.Builder
	b.WriteString(theme.ListMeta.Render(label) + " ")
	for i, l := range lines {
		if i > 0 {
			// Continuation rows are indented under the body, past the label.
			l = strings.Repeat(" ", lipgloss.Width(label)+1) + l
		}
		b.WriteString(style.Render(l))
		b.WriteString("\n")
	}
	return b.String()
}

// reasoningBodyMaxRows bounds how much of a reasoning block is drawn.
//
// MEASURED, and the reason this constant exists: a 62,000-character reasoning stream renders to ~860
// transcript LINES on its own. The operator's conversation showed "reasoning · 60,909 chars", so their
// transcript was carrying several hundred lines of reasoning per turn — and that is what makes the pane
// pathological: the newest window is almost entirely reasoning, so the visible rows are a dense wall of
// text that repaints constantly, and the conversation's real content (the messages) is pushed far up out
// of view.
//
// THE OTHER LONG RENDERERS ALREADY DO THIS, which is what makes the omission a bug rather than a choice:
// a tool call with 62,000 characters of output renders to ONE line, and an artifact to TWO — both draw a
// one-line summary and leave the body to a place built for it. Reasoning was the one long body drawn in
// full, because before the block existed it was a dim single-line footnote and the question could not
// arise. Giving it a visible body made it visible at full length, which is the regression being seen.
//
// The cap is a RENDERING limit, not a data limit: the full text is still on the ChatItem, the header's
// char count reports the true total, and a collapsible expansion (when the block model lands) reveals the
// rest.
const reasoningBodyMaxRows = 12

// renderReasoningBlock renders a reasoning ("thinking") item the way the GUI does: a labelled header
// carrying its own state, then the body beneath it.
//
// THE HEADER IS THE WHOLE POINT OF THE CHANGE. The GUI's header says "reasoning · thinking…" while the
// model is still reasoning and "reasoning · 60,909 chars" when it has finished, so the operator can tell
// at a glance (a) that this is reasoning rather than the reply, (b) that it is STILL COMING, and (c) how
// much of it there is. The TUI had none of the three: the body was drawn under a dim "thinking" word in
// the hint style.
//
// The char count is formatted with thousands separators, matching the GUI's `toLocaleString` — 60,909
// rather than 60909 — because that is what the operator is comparing against when they glance between
// the two clients.
func renderReasoningBlock(it ChatItem, maxWidth int, folded bool) string {
	if strings.TrimSpace(it.Text) == "" {
		return ""
	}
	// THE ARROW IS TEXT, not colour, so it reads on a monochrome terminal — and it is on the LEFT, where
	// the operator asked for it. It points DOWN when the body is showing and RIGHT when it is hidden,
	// which is the convention every tree in this TUI already uses.
	arrow := "▾"
	if folded {
		arrow = "▸"
	}
	header := theme.ReasoningLabel.Render(arrow + " reasoning")
	if it.Live {
		header += theme.ReasoningLabel.Render(" · thinking…")
	} else {
		header += theme.ListMeta.Render(" · " + groupDigits(len([]rune(it.Text))) + " chars")
	}
	if folded {
		// A collapsed block is ONE row. The header carries the state and the size, so nothing is lost by
		// hiding the body — which is the whole point of folding it.
		return header + "\n"
	}

	avail := maxWidth
	if avail > 0 {
		avail -= 4 // the body is indented under the arrow and the label
	}
	body := md.RenderOn(it.Text, avail, md.SurfaceOf(theme.ReasoningBody))
	if len(body) == 0 {
		body = []string{it.Text}
	}

	// BOUNDED — see reasoningBodyMaxRows. A transcript that draws a reasoning block in full is how a long
	// conversation's pane became an unreadable wall, and the header already carries the true size.

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for i, l := range body {
		if i >= reasoningBodyMaxRows {
			// The elision is EXPLICIT: a silent cut would read as the end of the model's reasoning rather
			// than as a display limit, and the operator would have no way to know there was more.
			b.WriteString(theme.ReasoningBody.Render(fmt.Sprintf("  … %d more lines", len(body)-reasoningBodyMaxRows)))
			b.WriteString("\n")
			break
		}
		b.WriteString(theme.ReasoningBody.Render("  " + l))
		b.WriteString("\n")
	}
	return b.String()
}

// groupDigits inserts thousands separators: 60909 -> "60,909".
//
// Hand-rolled rather than localised, because the count must not change its meaning with the machine's
// locale — the GUI prints a plain comma-separated number and these two must read the same.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// renderBubble renders `label · text` with wrap, skipping empty bodies.
func renderBubble(label, text string, style lipgloss.Style, maxWidth int) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ListMeta.Render(label) + " ")
	body := text
	if maxWidth > 0 && maxWidth > len(label)+4 {
		// soft-wrap at the width boundary (word-greedy)
		body = wrapText(text, maxWidth-len(label)-3)
	}
	b.WriteString(style.Render(body))
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func renderToolRow(t *ParsedTool, maxWidth int) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.StatusBusy.Render("⚙ " + t.ToolName))
	if t.Input != "" {
		b.WriteString(theme.HintText.Render(" " + firstLine(t.Input)))
	}
	if t.Output != "" {
		b.WriteString(theme.ListMeta.Render(" → " + firstLine(t.Output)))
	}
	b.WriteString("\n")
	return truncateLine(b.String(), maxWidth)
}

func renderArtifactRow(it ChatItem, maxWidth int) string {
	line := "⬒ " + it.Name + " (" + it.Type + ")"
	out := theme.ListTitle.Render(line)
	if it.Content != "" {
		out += "\n" + theme.HintText.Render(firstLine(it.Content))
	}
	return truncateLine(out+"\n", maxWidth)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

func truncateRow(s string, w int) string {
	if w <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func truncateLine(s string, w int) string {
	if w <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = truncateRow(l, w)
	}
	return strings.Join(lines, "\n") + "\n"
}

// wrapText soft-wraps text at width w (simple greedy word wrap).
func wrapText(s string, w int) string {
	if w < 8 {
		w = 8
	}
	var out strings.Builder
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out.WriteString("\n")
			continue
		}
		line := ""
		for _, word := range words {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= w:
				line += " " + word
			default:
				out.WriteString(line + "\n")
				line = word
			}
			for len(line) > w { // single long word: hard-split
				out.WriteString(line[:w] + "\n")
				line = line[w:]
			}
		}
		out.WriteString(line + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}
