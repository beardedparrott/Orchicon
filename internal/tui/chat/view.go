package chat

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

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
func RenderItems(items []ChatItem, maxWidth int) string {
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case KindUser:
			b.WriteString(renderChatMessage(it.Text, theme.BubbleUser, maxWidth, true, userBandLabel))
		case KindText:
			b.WriteString(renderChatMessage(it.Text, theme.BubbleModel, maxWidth, false, ""))
		case KindReasoning:
			b.WriteString(renderBubble("thinking", it.Text, theme.HintText, maxWidth))
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
	body := strings.Split(strings.TrimRight(wrapText(text, inner), "\n"), "\n")

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
