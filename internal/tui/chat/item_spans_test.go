package chat

// item_spans_test.go — THE CLICK GEOMETRY TILES THE BODY.
//
// RenderItemsSpans is what makes a click on the transcript resolvable to a message (internal/tui's
// transcriptUserMessageAtFrameRow). It reports, for each item, the body lines it produced — so the
// contract that matters is that the spans TILE the body exactly: no gaps, no overlaps, and a total
// equal to the number of lines actually drawn. A gap would make a click on part of a message do nothing;
// an overlap would make it copy the neighbouring message.

import (
	"strings"
	"testing"
)

func spanItems() []ChatItem {
	return []ChatItem{
		{Kind: KindUser, Text: "first question", Key: "u1"},
		{Kind: KindReasoning, Text: "thinking about it", Key: "r1"},
		{Kind: KindText, Text: "the reply", Key: "t1"},
		{Kind: KindUser, Text: "second question", Key: "u2"},
		{Kind: KindText, Text: "another reply", Key: "t2"},
	}
}

// THE SPANS ARE CONTIGUOUS AND COVER EVERY LINE OF THE BODY RETURNED.
//
// The body is compared UNTRIMMED (by newline count), which is what the spans are counted against. That
// distinction is not pedantry: callers trim trailing newlines before splitting into lines
// (syncTranscript does), so their line count can be a couple SHORTER than the spans' total — the last
// item's band gap. The indices are unaffected, because trimming only removes lines from the END, so every
// line a caller can address still maps to exactly one span. The next test pins that consequence, which is
// the property a click depends on.
func TestItemSpansTileTheBodyExactly(t *testing.T) {
	body, spans := RenderItemsSpans(spanItems(), 80)
	total := strings.Count(body, "\n")

	if len(spans) != len(spanItems()) {
		t.Fatalf("spans = %d, want one per item (%d)", len(spans), len(spanItems()))
	}
	next := 0
	for i, sp := range spans {
		if sp.Line != next {
			t.Errorf("span %d (%q) starts at line %d, want %d — the spans do not tile the body, so a click "+
				"falls into a gap or lands on the wrong item", i, sp.Key, sp.Line, next)
		}
		if sp.Lines <= 0 {
			t.Errorf("span %d (%q) covers %d lines", i, sp.Key, sp.Lines)
		}
		next = sp.Line + sp.Lines
	}
	if next != total {
		t.Errorf("the spans cover %d lines but the body has %d — the geometry and the render disagree, so "+
			"a click near the end resolves to nothing", next, total)
	}
}

// EVERY LINE A CALLER CAN ADDRESS MAPS TO EXACTLY ONE SPAN — the property a click depends on, asserted
// against the TRIMMED body the caller actually indexes.
func TestEveryAddressableLineMapsToExactlyOneSpan(t *testing.T) {
	body, spans := RenderItemsSpans(spanItems(), 80)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")

	for line := range lines {
		hits := 0
		for _, sp := range spans {
			if sp.Contains(line) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("body line %d is covered by %d spans, want exactly 1 — 0 means a click on it does nothing, "+
				"and 2+ means a click copies whichever span is found first", line, hits)
		}
	}
}

// AND RenderItems IS THE SAME RENDER, so the painted transcript and the clickable geometry can never
// come from two different layouts.
func TestRenderItemsIsTheSpannedRender(t *testing.T) {
	items := spanItems()
	body, _ := RenderItemsSpans(items, 80)
	if got := RenderItems(items, 80); got != body {
		t.Error("RenderItems and RenderItemsSpans produced different bodies — the painted transcript and " +
			"the geometry a click uses have drifted, which would copy the wrong message")
	}
}

// ONLY ITEMS WITH TEXT ARE COPYABLE, and a click on one reports the message rather than its rendering.
func TestOnlyTextBearingItemsAreCopyable(t *testing.T) {
	_, spans := RenderItemsSpans(spanItems(), 80)
	byKey := map[string]ItemSpan{}
	for _, sp := range spans {
		byKey[sp.Key] = sp
	}
	// A user message copies its own words.
	if got := byKey["u2"].Text; got != "second question" {
		t.Errorf("the user span carries %q, want the message text", got)
	}
	// A tool row has nothing to paste, which the empty string signals.
	_, toolSpans := RenderItemsSpans([]ChatItem{
		{Kind: KindTool, Tool: &ParsedTool{ToolName: "read", ID: "x"}, Key: "tool1"},
	}, 80)
	if len(toolSpans) != 1 {
		t.Fatalf("tool spans = %d", len(toolSpans))
	}
	if toolSpans[0].Text != "" {
		t.Errorf("a tool row reports copyable text %q — pasting a tool call is not a useful gesture, and "+
			"offering it would make every click somewhere on the transcript copy something", toolSpans[0].Text)
	}
}

// Contains is the lookup a click performs, so its edges are asserted rather than assumed.
func TestSpanContainsCoversExactlyItsLines(t *testing.T) {
	sp := ItemSpan{Line: 10, Lines: 3}
	for _, line := range []int{10, 11, 12} {
		if !sp.Contains(line) {
			t.Errorf("Contains(%d) = false for a span covering 10..12", line)
		}
	}
	for _, line := range []int{9, 13, 0, -1} {
		if sp.Contains(line) {
			t.Errorf("Contains(%d) = true for a span covering 10..12", line)
		}
	}
}
