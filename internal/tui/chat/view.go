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
func RenderItems(items []ChatItem, maxWidth int) string {
	var b strings.Builder
	for _, it := range items {
		switch it.Kind {
		case KindUser:
			b.WriteString(renderBubble("you", it.Text, theme.ListTitle, maxWidth))
		case KindText:
			b.WriteString(renderBubble("orch", it.Text, theme.DetailValue, maxWidth))
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
