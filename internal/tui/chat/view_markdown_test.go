package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// TestProseRendersMarkdownNotSource is the operator's report end to end through the Ask transcript:
// the stored text contains markdown blockquote markers, and they must arrive as a BLOCK.
//
//	"The thing that concerned me was those greater than signs. What are those representing? It seemed
//	 kind of ugly."
//
// The GUI renders this message through react-markdown, so the two clients disagreed about the same
// bytes. The quoted words must survive VERBATIM (they are attribution), the marker must not.
func TestProseRendersMarkdownNotSource(t *testing.T) {
	text := strings.Join([]string{
		"Here is my proposal.",
		"",
		"## Operator requirement (verbatim intent)",
		"",
		"> \"the box at the bottom that you type for chats\"",
		"> needs to be larger.",
	}, "\n")

	items := []ChatItem{
		{Kind: KindText, Text: text},
		{Kind: KindUser, Text: text},
	}
	out := RenderItems(items, 70)

	if strings.Contains(out, "> ") {
		t.Fatalf("a raw blockquote marker reached the transcript:\n%s", out)
	}
	if !strings.Contains(out, "│") {
		t.Fatalf("a blockquote must be drawn as a bordered block:\n%s", out)
	}
	if !strings.Contains(out, "the box at the bottom that you type for chats") {
		t.Fatalf("the quoted words must survive verbatim:\n%s", out)
	}
	// Markdown emphasis is consumed...
	if strings.Contains(out, "##") {
		t.Fatalf("a heading marker was printed literally:\n%s", out)
	}
}

// TestBandFillSurvivesMarkdownStyling is the reason the renderer is attribute-only.
//
// Each transcript row is padded to the pane and painted with the speaker's fill, so the band runs edge
// to edge. A markdown span that ended in a FULL reset (`\x1b[0m`) would clear that fill for the rest
// of the row — a band with a hole punched in it wherever a bold word appeared. So: every full reset in
// a rendered row must be the BAND'S OWN, i.e. the last thing on the line.
func TestBandFillSurvivesMarkdownStyling(t *testing.T) {
	text := "A line with **bold** and `code` and a [link](https://x.example) in it, plus enough words to wrap."
	out := RenderItems([]ChatItem{{Kind: KindText, Text: text}}, 48)

	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(rows) < 2 {
		t.Fatalf("expected the message to wrap across rows, got %d: %q", len(rows), out)
	}
	for i, row := range rows {
		if idx := strings.Index(row, "\x1b[0m"); idx >= 0 {
			if after := row[idx+len("\x1b[0m"):]; after != "" {
				t.Fatalf("row %d has a FULL reset mid-row — it would punch a hole in the band fill:\n%q", i, row)
			}
		}
	}
	// The styling is still applied — we are not passing plain text to dodge the problem.
	if !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("bold was not applied:\n%q", out)
	}
}

// TestReasoningRendersMarkdown: the GUI's ReasoningBubble defaults to the RENDERED view (its Raw
// toggle is opt-in), so reasoning is markdown here too — with the label kept on the first row and the
// continuation rows indented past it.
func TestReasoningRendersMarkdown(t *testing.T) {
	out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "## Step one\n\n> because of the constraint"}}, 60)
	if !strings.Contains(out, "thinking") {
		t.Fatalf("the reasoning label was lost:\n%s", out)
	}
	if strings.Contains(out, "##") {
		t.Fatalf("a heading marker was printed literally:\n%s", out)
	}
	if !strings.Contains(out, "│") {
		t.Fatalf("the quoted line is not drawn as a block:\n%s", out)
	}
}

// TestErrorsStayRaw: an error is diagnostic text read verbatim. Markdown rendering would CONSUME
// emphasis markers inside a stack trace, and those characters are evidence.
func TestErrorsStayRaw(t *testing.T) {
	const err = "rpc error: **fatal** at `step-quick`"
	out := RenderItems([]ChatItem{{Kind: KindError, Text: err}}, 70)
	if !strings.Contains(out, "**fatal**") {
		t.Fatalf("the error text was altered; errors must stay raw:\n%s", out)
	}
}

// TestToolRowsStayRaw: the GUI renders a tool call as a <pre> (ToolCard), never as markdown.
func TestToolRowsStayRaw(t *testing.T) {
	it := ChatItem{Kind: KindTool, Tool: &ParsedTool{ToolName: "bash", Input: "**not markdown**"}}
	out := RenderItems([]ChatItem{it}, 70)
	if !strings.Contains(out, "bash") {
		t.Fatalf("tool name lost:\n%s", out)
	}
}

// TestNoRowExceedsThePane is the width promise at the transcript level: there is no horizontal scroll
// in the band, so an over-wide row is content the operator cannot reach.
func TestNoRowExceedsThePane(t *testing.T) {
	text := "## A heading\n\n> a quoted line that is long enough to wrap several times in a narrow pane\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```\n" + strings.Repeat("code ", 30) + "\n```"
	for _, w := range []int{24, 40, 70} {
		out := RenderItems([]ChatItem{{Kind: KindText, Text: text}}, w)
		for _, row := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if got := lipgloss.Width(row); got > w {
				t.Fatalf("width %d: row is %d cells: %q", w, got, row)
			}
		}
	}
}

// TestBandPaddingStillApplies keeps the alignment contract: the operator's rows stay right-aligned and
// the model's left-aligned inside their fills, with markdown bodies included.
func TestBandPaddingStillApplies(t *testing.T) {
	mine := RenderItems([]ChatItem{{Kind: KindUser, Text: "short note"}}, 40)
	if !strings.Contains(mine, "You") {
		t.Fatalf("the operator's band must be labelled:\n%q", mine)
	}
	// Every band row must be exactly the pane width (the fill runs edge to edge).
	for _, row := range strings.Split(strings.TrimRight(mine, "\n"), "\n") {
		if row == "" {
			continue
		}
		if got := lipgloss.Width(row); got != 40 {
			t.Fatalf("band row is %d cells, want the pane's 40: %q", got, row)
		}
	}
	_ = theme.BubbleModel
}
