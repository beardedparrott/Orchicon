package chat

// reasoning_block_test.go — REASONING RENDERS AS A BLOCK, NOT A FOOTNOTE.
//
// The operator, looking at the TUI beside the GUI: "No reasoning block."
//
// The GUI renders reasoning as its own card: violet, labelled "reasoning", with a dot that pulses while
// the model is still thinking and a char count once it has stopped ("reasoning · thinking…" /
// "reasoning · 60,909 chars"). The TUI drew the same 60,909 characters under a single dim word —
// `renderMarkdownBubble("thinking", …, theme.HintText, …)` — in the SAME style as every hint line in the
// application, so the reasoning stream was indistinguishable from a footnote and the operator could not
// tell it was reasoning, could not tell it was still arriving, and could not tell how much of it there
// was.
//
// These pin the three things the header has to say, because they are the three the operator was looking
// for. The tests read the RENDERED OUTPUT rather than the item's fields: the fields were always correct,
// and it is the rendering that was wrong.

import (
	"strings"
	"testing"
)

// THE HEADER NAMES THE BLOCK. Without this the body is unlabelled prose that could be mistaken for the
// model's reply — the failure the GUI's violet card exists to prevent.
func TestReasoningBlockIsLabelled(t *testing.T) {
	out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "weighing it up"}}, 80)
	if !strings.Contains(out, "reasoning") {
		t.Errorf("the reasoning block is not labelled:\n%s", out)
	}
	// And NOT under the old vocabulary, which named the ACTIVITY rather than the CONTENT and read as a
	// status line rather than as a block.
	if strings.Contains(out, "thinking\n") && !strings.Contains(out, "reasoning") {
		t.Errorf("the block is still labelled only as activity:\n%s", out)
	}
}

// A LIVE BLOCK SAYS IT IS STILL COMING — the GUI's "· thinking…". This is the state the operator watches
// during a slow turn, and it is the reason the indicator and the block are different things: one says the
// turn started, this says the model is still working.
func TestLiveReasoningSaysItIsThinking(t *testing.T) {
	live := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "partial thou", Live: true}}, 80)
	if !strings.Contains(live, "thinking") {
		t.Errorf("a live reasoning block does not say it is still arriving:\n%s", live)
	}
	if strings.Contains(live, "chars") {
		t.Errorf("a live block shows a char count, which reads as FINISHED:\n%s", live)
	}
}

// A FINISHED BLOCK CARRIES ITS SIZE, with thousands separators — the GUI's own format. The count is what
// makes a collapsed-looking wall of reasoning legible at a glance, and the separator is what keeps the two
// clients reading the same way.
func TestFinishedReasoningCarriesACount(t *testing.T) {
	out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: strings.Repeat("x", 60909)}}, 80)
	if !strings.Contains(out, "60,909 chars") {
		t.Errorf("a finished reasoning block does not report its size in the GUI's own format "+
			"(want \"60,909 chars\"):\n%.200s", out)
	}
}

// A REASONING BODY IS CAPPED, and says so.
//
// MEASURED BEFORE THE FIX: a 62,000-character reasoning stream rendered to ~860 transcript LINES. The
// operator's conversation reported "reasoning · 60,909 chars", so their transcript carried several hundred
// lines of reasoning per turn — the newest window was almost entirely reasoning, and the conversation's
// real content was pushed far out of view.
//
// The cap is what EVERY OTHER long renderer on this transcript already had: a tool call with 62,000
// characters of output renders to ONE line and an artifact to TWO, both leaving the body to a place built
// for it. Reasoning was the one long body drawn in full, because before the block existed it was a dim
// single-line footnote and the question could not arise.
func TestReasoningBodyIsCapped(t *testing.T) {
	big := strings.Repeat("Weighing the options carefully. ", 2000) // ~62k chars
	out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: big}}, 80)
	rows := len(strings.Split(strings.TrimRight(out, "\n"), "\n"))

	// Header + at most the cap + the elision row + trailing newline handling.
	maxRows := 1 + reasoningBodyMaxRows + 1
	if rows > maxRows {
		t.Fatalf("a %d-character reasoning block drew %d transcript rows, want <= %d — an uncapped block "+
			"is how a long conversation becomes an unreadable wall of scrolling text", len(big), rows, maxRows)
	}
	// AND THE ELISION IS VISIBLE. A silent cut would read as the end of the model's reasoning rather than
	// as a display limit, leaving the operator no way to know there was more.
	if !strings.Contains(out, "more lines") {
		t.Errorf("the block was cut without saying so:\n%s", out)
	}
	// The header still reports the TRUE size, so the operator knows how much was elided.
	if !strings.Contains(out, "chars") {
		t.Errorf("the header lost its char count:\n%s", out)
	}
}

// THE CAP IS A RENDERING LIMIT, NOT A DATA LIMIT. The item still carries every character — so the count in
// the header is honest and a future expansion has something to reveal.
func TestReasoningCapDoesNotTruncateTheItem(t *testing.T) {
	big := strings.Repeat("x", 5000)
	it := ChatItem{Kind: KindReasoning, Text: big}
	RenderItems([]ChatItem{it}, 80)
	if len(it.Text) != 5000 {
		t.Errorf("the item was mutated by rendering: %d chars, want 5000", len(it.Text))
	}
	// A SHORT block is untouched by the cap — it must not decorate an ordinary one.
	short := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "brief thought"}}, 80)
	if strings.Contains(short, "more lines") {
		t.Errorf("a short block was elided:\n%s", short)
	}
}

// groupDigits is the formatter behind that, including the boundaries where a naive implementation breaks.
func TestGroupDigits(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{0, "0"}, {7, "7"}, {99, "99"}, {999, "999"},
		{1000, "1,000"}, {1234, "1,234"}, {60909, "60,909"},
		{999999, "999,999"}, {1000000, "1,000,000"}, {-1, "-1"},
	} {
		if got := groupDigits(c.n); got != c.want {
			t.Errorf("groupDigits(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// THE BODY IS PRESENT AND INDENTED UNDER THE HEADER, so the block reads as one unit rather than as a label
// with loose text after it.
func TestReasoningBodySitsUnderItsHeader(t *testing.T) {
	out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "first line\nsecond line"}}, 80)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("the block rendered one line:\n%s", out)
	}
	if !strings.Contains(lines[0], "reasoning") {
		t.Errorf("line 0 is not the header: %q", lines[0])
	}
	// The body rows are indented, so they cannot be confused with the header or with the next block.
	foundIndented := false
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "  ") && strings.Contains(l, "line") {
			foundIndented = true
		}
	}
	if !foundIndented {
		t.Errorf("the body is not indented under its header:\n%s", out)
	}
}

// AN EMPTY REASONING CHUNK RENDERS NOTHING — not a dangling header. The stream can emit empty fragments,
// and a header with no body would be a permanent "reasoning · 0 chars" row in every transcript.
func TestEmptyReasoningRendersNothing(t *testing.T) {
	if out := RenderItems([]ChatItem{{Kind: KindReasoning, Text: "   "}}, 80); out != "" {
		t.Errorf("an empty reasoning chunk rendered %q, want nothing", out)
	}
}
