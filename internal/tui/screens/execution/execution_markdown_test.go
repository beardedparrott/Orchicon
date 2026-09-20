package execution

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// blocks renders a transcript the way the pane does, with everything expanded, and returns the
// VISIBLE text (escapes stripped) so a test can assert on what the operator reads.
func blocksVisible(t *testing.T, items []chat.ChatItem, width int) string {
	t.Helper()
	blks := blocksFromItems(items, width)
	st := &blockState{}
	for _, b := range blks {
		if b.kind.collapsible() {
			st.overrides = map[string]bool{b.key: true} // expand everything
		}
	}
	body, _ := renderBlocks(blks, st, width, transcriptCursor{})
	return ansi.Strip(body)
}

// TestProseBlockRendersMarkdown: the execution transcript is where the volume is (1,786 executions
// carry bold markers, 85 carry tables), and its prose is the model's own markdown. The operator saw
// the SOURCE — "> " markers included — which is what this pins.
func TestProseBlockRendersMarkdown(t *testing.T) {
	items := []chat.ChatItem{{
		Kind: chat.KindText,
		Key:  "m1",
		Text: "## Findings\n\n> the operator said this\n\n- one\n- two",
	}}
	got := blocksVisible(t, items, 60)

	if strings.Contains(got, "> ") {
		t.Fatalf("a raw blockquote marker reached the transcript:\n%s", got)
	}
	if !strings.Contains(got, "│") {
		t.Fatalf("the quote is not drawn as a block:\n%s", got)
	}
	if !strings.Contains(got, "the operator said this") {
		t.Fatalf("the quoted words were lost:\n%s", got)
	}
	if strings.Contains(got, "##") {
		t.Fatalf("a heading marker was printed literally:\n%s", got)
	}
	if !strings.Contains(got, "• one") {
		t.Fatalf("the list was not rendered:\n%s", got)
	}
}

// TestToolBlocksStayRaw: tool output is not markdown in the GUI (ToolCard draws a <pre>), so it must
// not be consumed here either — a tool's raw output is frequently exact evidence.
func TestToolBlocksStayRaw(t *testing.T) {
	items := []chat.ChatItem{{
		Kind: chat.KindTool,
		Key:  "t1",
		Tool: &chat.ParsedTool{ToolName: "bash", Output: "**not markdown** and `not code`"},
	}}
	got := blocksVisible(t, items, 70)
	if !strings.Contains(got, "**not markdown**") {
		t.Fatalf("tool output was consumed by the markdown renderer:\n%s", got)
	}
}

// TestErrorBlocksStayRaw: an error is read verbatim.
func TestErrorBlocksStayRaw(t *testing.T) {
	items := []chat.ChatItem{{Kind: chat.KindError, Key: "e1", Text: "failed at **step-quick**"}}
	got := blocksVisible(t, items, 70)
	if !strings.Contains(got, "**step-quick**") {
		t.Fatalf("an error was rendered as markdown:\n%s", got)
	}
}

// TestArtifactMarkdownFollowsTheGUIRule: the GUI's ArtifactCard renders markdown ONLY when the type is
// "markdown" or the file name ends in .md (its isMarkdown). Mirroring the rule exactly is the point —
// rendering every artifact would reformat code, JSON and diffs.
func TestArtifactMarkdownFollowsTheGUIRule(t *testing.T) {
	mdArtifact := chat.ChatItem{Kind: chat.KindArtifact, Key: "a1", Name: "report.md", Type: "text",
		Content: "> quoted in an artifact"}
	got := blocksVisible(t, []chat.ChatItem{mdArtifact}, 60)
	if strings.Contains(got, "> ") || !strings.Contains(got, "│") {
		t.Fatalf("a .md artifact must render as markdown:\n%s", got)
	}

	codeArtifact := chat.ChatItem{Kind: chat.KindArtifact, Key: "a2", Name: "main.go", Type: "code",
		Content: "> not a quote, this is code"}
	got = blocksVisible(t, []chat.ChatItem{codeArtifact}, 60)
	if !strings.Contains(got, "> not a quote") {
		t.Fatalf("a non-markdown artifact must stay raw:\n%s", got)
	}
}

// TestReasoningSummaryIsTheRenderedFirstLine: a collapsed block's preview must show what the operator
// would READ, not the source. A preview reading "## Findings" or an opening fence is the same defect
// one level up.
func TestReasoningSummaryIsTheRenderedFirstLine(t *testing.T) {
	blks := blocksFromItems([]chat.ChatItem{{
		Kind: chat.KindReasoning, Key: "r1",
		Text: "## Findings\n\nThe rest is long enough to be collapsed.",
	}}, 60)
	if len(blks) != 1 {
		t.Fatalf("expected one block, got %d", len(blks))
	}
	if strings.Contains(blks[0].summary, "##") {
		t.Fatalf("a collapsed summary showed the raw heading marker: %q", blks[0].summary)
	}
	if !strings.Contains(blks[0].summary, "Findings") {
		t.Fatalf("the summary lost the heading text: %q", blks[0].summary)
	}
}

// TestNoTranscriptLineExceedsThePane: the pane truncates, so an over-wide line is content lost with no
// error. This is the width promise applied to the transcript's own indent.
func TestNoTranscriptLineExceedsThePane(t *testing.T) {
	items := []chat.ChatItem{{
		Kind: chat.KindText, Key: "m1",
		Text: "| a | b | c |\n|---|---|---|\n| a long value here | another long value | third |\n\n```\n" + strings.Repeat("code ", 25) + "\n```\n\n> " + strings.Repeat("quote ", 20),
	}}
	for _, w := range []int{28, 44, 80} {
		blks := blocksFromItems(items, w)
		body, _ := renderBlocks(blks, &blockState{}, w, transcriptCursor{})
		for _, l := range strings.Split(body, "\n") {
			if got := lipgloss.Width(l); got > w {
				t.Fatalf("width %d: transcript line is %d cells: %q", w, got, ansi.Strip(l))
			}
		}
	}
}
