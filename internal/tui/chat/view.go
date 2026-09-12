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
// Chat layout matches the GUI: the operator's messages are RIGHT-aligned in a
// shaded bubble, the model's are LEFT-aligned in a slightly different shade,
// so a turn reads as a conversation (operator request: "put the users
// messages and the models messages inside a shaded bubble", user on the
// right).
func RenderItems(items []ChatItem, maxWidth int) string {
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case KindUser:
			b.WriteString(renderChatBubble(it.Text, theme.BubbleUser, maxWidth, true))
		case KindText:
			b.WriteString(renderChatBubble(it.Text, theme.BubbleModel, maxWidth, false))
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

// renderChatBubble renders one message as a shaded bubble with padding, at
// bubbleWidth (a fraction of the pane so the alignment reads), nudged to the
// right or left edge of the pane.
//
// The fill is a theme.Bubble* style (see theme/bubble.go): the user's bubble is
// markedly different from the model's on every palette, which is the operator's
// ask. The alignment filler is plain spaces so the pane's own background shows
// either side — no band across the pane.
func renderChatBubble(text string, style lipgloss.Style, maxWidth int, right bool) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	pane := maxWidth
	if pane <= 0 {
		pane = 80
	}
	// Bubbles take at most ~72% of the pane, so the two sides stay visually
	// distinct even on a long message.
	bubbleW := pane * 3 / 4
	if bubbleW > pane-4 {
		bubbleW = pane - 4
	}
	if bubbleW < 12 {
		bubbleW = pane
	}

	body := strings.Split(strings.TrimRight(wrapText(text, bubbleW-2), "\n"), "\n")
	padded := style.Padding(0, 1)

	rows := make([]string, 0, len(body)+1)
	for _, l := range body {
		rows = append(rows, padded.Render(l))
	}
	// A one-row blank gutter after each bubble separates turns.
	var out strings.Builder
	for _, r := range rows {
		pad := pane - lipgloss.Width(r)
		if pad < 0 {
			pad = 0
		}
		if right {
			out.WriteString(strings.Repeat(" ", pad) + r)
		} else {
			out.WriteString(r)
		}
		out.WriteString("\n")
	}
	out.WriteString("\n")
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
