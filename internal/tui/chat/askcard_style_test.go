package chat

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// askCardFor renders a recorded ask_user call and returns the rendered text plus
// the click spans — exactly what the transcript draws and what a click resolves
// against.
func askCardFor(t *testing.T, width int, args string) (string, []ItemSpan) {
	t.Helper()
	items := conversationItems([]*apiv1.ChatMessage{{
		Id:   "m1",
		Role: "assistant",
		ToolCalls: []*apiv1.ToolCall{{
			Id:           "tc-1",
			Type:         "function",
			FunctionName: "ask_user",
			Arguments:    args,
		}},
	}})
	out, spans := RenderItemsSpans(items, width)
	return out, spans
}

// TestAskCardRendersBelowTheReplyText: the operator's report — "I see the Orchicon
// asks box but … it is ON TOP of a bunch of other text you sent. That is not
// intuitive. It should be at the bottom (newest/recent)."
//
// The card is the thing to ACT ON, so it is the last line of the turn, not a
// record buried above the prose that followed the call.
func TestAskCardRendersBelowTheReplyText(t *testing.T) {
	items := conversationItems([]*apiv1.ChatMessage{{
		Id:      "m1",
		Role:    "assistant",
		Content: "I need to know which branch before I carry on.",
		ToolCalls: []*apiv1.ToolCall{{
			Id: "tc-1", Type: "function", FunctionName: "ask_user",
			Arguments: `{"question":"Which branch?","options":[{"label":"develop"},{"label":"main"}]}`,
		}},
	}})

	textIdx, askIdx := -1, -1
	for i, it := range items {
		if it.Kind == KindAsk {
			askIdx = i
		}
		if it.Kind == KindText && strings.Contains(it.Text, "which branch") {
			textIdx = i
		}
	}
	if textIdx < 0 {
		t.Fatalf("the reply text item is missing: %+v", items)
	}
	if askIdx < 0 {
		t.Fatalf("the ask card item is missing: %+v", items)
	}
	if askIdx < textIdx {
		t.Fatalf("the card is emitted at %d, above the reply text at %d — it must be the LAST item", askIdx, textIdx)
	}
	if askIdx != len(items)-1 {
		t.Errorf("the card is at %d of %d items; it must be the bottom line of the turn", askIdx, len(items))
	}
}

// TestAskCardLooksLikeACard is the operator's report as a test: the clarifying
// question "didn't really do a good job at making it seem like it was a clickable
// card … it just looked like a regular normal list".
//
// It is drawn by the shared kit2 card widget now, so it carries the same chrome
// the permission card does — and this asserts that rather than leaving it to a
// screenshot nobody can run.
func TestAskCardLooksLikeACard(t *testing.T) {
	out, _ := askCardFor(t, 70, `{"question":"Which branch?","options":[{"label":"develop"},{"label":"main"}]}`)

	// The rounded border, top and bottom: the single strongest visual signal that
	// this is a box you INTERACT with rather than prose you read.
	if !strings.Contains(out, "┌") || !strings.Contains(out, "└") {
		t.Errorf("the question card draws no border — it reads as a list:\n%s", out)
	}
	if !strings.Contains(out, "│") {
		t.Errorf("the question card has no side borders:\n%s", out)
	}
	// The title, so it announces itself as a question.
	if !strings.Contains(out, "Orchicon asks") {
		t.Errorf("the card does not name itself:\n%s", out)
	}
	// The affordance: a card that does not say it can be acted on is a card the
	// operator will not act on.
	//
	// IT NAMES BOTH GESTURES because both work now (see screens/ask/askcard.go): the cursor the
	// operator sees is a real one, and the click still answers. The operator's report was the gap:
	// "It works but in the TUI the highlight bar is gone" — a bar over rows that nothing could reach.
	if !strings.Contains(out, "click also answers") {
		t.Errorf("the card offers no click affordance:\n%s", out)
	}
	if !strings.Contains(out, "↑/↓") {
		t.Errorf("the card does not name its cursor keys:\n%s", out)
	}
	// The question and its options are still there.
	for _, want := range []string{"Which branch?", "1. develop", "2. main"} {
		if !strings.Contains(out, want) {
			t.Errorf("card is missing %q:\n%s", want, out)
		}
	}
}

// TestAskCardAffordanceMatchesWhatWorks: the footer must not advertise a gesture
// that does nothing.
//
// THIS TEST USED TO ASSERT THE OPPOSITE, and it was right to at the time: the card had no cursor, so a
// footer promising ↑/↓ would have been a lie, and the test pinned that. The cursor exists now
// (screens/ask/askcard.go: handleAskCardKey), so the guard inverts — the footer must NAME the keys,
// and the behaviour side is pinned by the screen's own key tests. A footer is only allowed to promise
// what another test proves.
func TestAskCardAffordanceMatchesWhatWorks(t *testing.T) {
	out, _ := askCardFor(t, 70, `{"question":"Which?","options":[{"label":"a"},{"label":"b"}],"allow_other":true}`)
	// THE FOOTER WRAPS, so the phrases below are matched against the card with its line breaks and
	// border glyphs flattened: a hint is read as one sentence by the operator even when the box breaks
	// it across two rows, and asserting on the unwrapped form made this test fail on the width rather
	// than on the promise.
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "┌", " ", "└", " ", "─", " ").Replace(out)), " ")
	if !strings.Contains(flat, "reply in your own words") {
		t.Errorf("allow_other must tell the operator they can answer freely:\n%s", out)
	}
	// THE KEYS ARE PROMISED BECAUSE THEY WORK. Both are asserted here so the footer and the handler
	// cannot drift apart in the direction that matters: an implemented key the operator is never told
	// about, or a promised key nothing handles.
	for _, want := range []string{"↑/↓ select", "enter answer"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the card implements %q but does not say so:\n%s", want, out)
		}
	}
	// ESC RELEASES THE KEYBOARD, it does not dismiss a question. Promising a dismissal would invite
	// the operator to close a question they were still considering.
	if !strings.Contains(flat, "esc hands back the keyboard") {
		t.Errorf("the card does not explain what esc does:\n%s", out)
	}
	if strings.Contains(flat, "esc dismisses") {
		t.Errorf("the card promises that esc DISMISSES a question, which it must not do:\n%s", out)
	}
}

// TestAskCardClickSpansPointAtTheirOwnOptions is THE regression guard for the
// restyle: a click resolves through these line offsets, so if the card's chrome
// shifts them the operator's click answers with a different option than the one
// under the cursor.
//
// It reads the RENDERED text back at each span's line range and requires the
// option to actually be there, which is what a click ultimately does.
func TestAskCardClickSpansPointAtTheirOwnOptions(t *testing.T) {
	out, spans := askCardFor(t, 70, `{"question":"Which branch should the run clone off?",`+
		`"options":[{"label":"develop","description":"the integration branch"},`+
		`{"label":"main"},{"label":"a-branch-name-long-enough-to-wrap-in-a-narrow-pane-x"}]}`)

	var ask *ItemSpan
	for i := range spans {
		if spans[i].Kind == KindAsk {
			ask = &spans[i]
		}
	}
	if ask == nil {
		t.Fatalf("no ask span was emitted")
	}
	if len(ask.Options) != 3 {
		t.Fatalf("want 3 option spans, got %d", len(ask.Options))
	}

	// The item's own lines, so a span's Line can be read as an index into them.
	lines := strings.Split(out, "\n")
	if ask.Line < 0 || ask.Line+ask.Lines > len(lines) {
		t.Fatalf("the ask span (%d..%d) does not fit the %d rendered lines", ask.Line, ask.Line+ask.Lines, len(lines))
	}

	for _, o := range ask.Options {
		// Every row of the option's range must exist...
		if o.Line < 0 || o.Line+o.Lines > ask.Lines {
			t.Fatalf("option %q spans %d..%d, outside the item's %d lines", o.Label, o.Line, o.Line+o.Lines, ask.Lines)
		}
		// ...and the FIRST row must actually carry that option, read back from
		// the rendered transcript exactly as a click would land on it.
		row := lines[ask.Line+o.Line]
		if !strings.Contains(row, o.Label) {
			t.Errorf("the span for %q points at a row that does not show it:\n  row %d = %q\nfull card:\n%s",
				o.Label, ask.Line+o.Line, row, out)
		}
		// Resolving by line must return the same option — the path a click takes.
		if got, ok := ask.OptionAt(ask.Line + o.Line); !ok || got != o.Label {
			t.Errorf("OptionAt(%d) = (%q, %v), want %q", ask.Line+o.Line, got, ok, o.Label)
		}
		// The LAST row of a wrapped option must still resolve to it, or clicking
		// the tail of a long option would do nothing.
		if got, ok := ask.OptionAt(ask.Line + o.Line + o.Lines - 1); !ok || got != o.Label {
			t.Errorf("OptionAt(last row of %q) = (%q, %v), want the same option", o.Label, got, ok)
		}
	}
}

// TestAskCardSpanLinesCoverTheCardNotTheChrome: the report the operator gets must
// be about the OPTIONS. A span line pointing at the border, the title or the
// footer would make a click on the box itself answer a question nobody chose.
func TestAskCardSpanLinesCoverTheCardNotTheChrome(t *testing.T) {
	out, spans := askCardFor(t, 70, `{"question":"Which?","options":[{"label":"a"},{"label":"b"}]}`)
	var ask *ItemSpan
	for i := range spans {
		if spans[i].Kind == KindAsk {
			ask = &spans[i]
		}
	}
	if ask == nil {
		t.Fatal("no ask span")
	}
	lines := strings.Split(out, "\n")
	for _, o := range ask.Options {
		for row := o.Line; row < o.Line+o.Lines; row++ {
			text := lines[ask.Line+row]
			if strings.Contains(text, "┌") || strings.Contains(text, "└") {
				t.Errorf("option %q claims the border row %q", o.Label, text)
			}
			if strings.Contains(text, "click an option") {
				t.Errorf("option %q claims the footer row %q", o.Label, text)
			}
		}
	}
}

// TestAskCardRendersInANarrowPane: a pane narrower than the option labels must
// still draw every option (they wrap inside the card), and the spans must follow
// them onto their extra rows. A card that drops an option is a question the
// operator cannot answer.
func TestAskCardRendersInANarrowPane(t *testing.T) {
	out, spans := askCardFor(t, 34, `{"question":"Which of these two branches should the run clone off?",`+
		`"options":[{"label":"develop"},{"label":"main"}]}`)
	var ask *ItemSpan
	for i := range spans {
		if spans[i].Kind == KindAsk {
			ask = &spans[i]
		}
	}
	if ask == nil {
		t.Fatal("no ask span in a narrow pane — the card vanished")
	}
	if len(ask.Options) != 2 {
		t.Fatalf("narrow pane lost an option: %d spans", len(ask.Options))
	}
	if !strings.Contains(out, "┌") || !strings.Contains(out, "└") {
		t.Errorf("the card lost its chrome when narrow:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	for _, o := range ask.Options {
		row := lines[ask.Line+o.Line]
		if !strings.Contains(row, o.Label) {
			t.Errorf("narrow card: %q span points at %q", o.Label, row)
		}
	}
}

// TestTheCardDrawsACursorBarAndItFollowsTheSelection is the OPERATOR'S REPORT as a test.
//
// "It works but in the TUI the highlight bar is gone. Looks fine the gui." — then, asked which bar,
// "This already works with the orchicon adapter. It should mimic that." The recorded card drew its options
// as PLAIN ROWS: no ▸, no highlight, nothing that said which row Enter would answer. The permission card
// (the one the native bridge produces) has had the bar all along, which is why the two cards looked like
// they came from different products.
//
// The assertions are structural rather than snapshot-based so they survive the card's styling: the bar is
// drawn ONCE, on the row the item's Sel names, and it MOVES when Sel moves.
func TestTheCardDrawsACursorBarAndItFollowsTheSelection(t *testing.T) {
	card := func(sel int) string {
		item := ChatItem{
			Kind: KindAsk, Key: "ask-1", At: 1000,
			Ask: &ParsedAsk{
				Question: "Which branch should the run clone off?",
				Options:  []AskOption{{Label: "develop"}, {Label: "main"}},
				Sel:      sel,
			},
		}
		var out strings.Builder
		rendered, _ := RenderItemsSpansWithCopy([]ChatItem{item}, 70, "›", nil)
		out.WriteString(rendered)
		return out.String()
	}

	// The bar exists at all.
	if !strings.Contains(card(0), "▸") {
		t.Fatalf("the card draws no cursor bar — the rows are inert text:\n%s", card(0))
	}
	// It is on the SELECTED row, and only there: two bars would make "what does Enter answer"
	// ambiguous, which is worse than none.
	for sel, want := range map[int]string{0: "develop", 1: "main"} {
		rendered := card(sel)
		if n := strings.Count(rendered, "▸"); n != 1 {
			t.Errorf("Sel=%d draws %d cursor bars, want exactly 1:\n%s", sel, n, rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if !strings.Contains(line, "▸") {
				continue
			}
			if !strings.Contains(line, want) {
				t.Errorf("Sel=%d puts the bar on the wrong row — it should mark %q:\n%s", sel, want, rendered)
			}
		}
	}
}
