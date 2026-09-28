package chat

// askcard_other_test.go — the RECORDED ask card's free-text row.
//
// The operator: "In the GUI, it lets you type in your own response. In the TUI clicking on it does
// nothing. You should be able to click on other and type in a response there."
//
// They were looking at the CONSENT card (whose Other row was a separate bug, fixed in
// App.consentDecideFromRow), but the divergence they described was one row wide on BOTH cards: this
// one had no Other row AT ALL, and its footer said "or reply in your own words" — a different
// promise, pointing at the composer, where the GUI opens an input on the card itself.
//
// These tests read the RENDERED card back, because the row's whole job is to be on it.

import (
	"strings"
	"testing"
)

// recordedOtherCard is an unanswered recorded question that allows free text.
func recordedOtherCard(drafting bool, draft string) ChatItem {
	return ChatItem{
		Kind: KindAsk, Key: "ask-1", At: 1,
		Ask: &ParsedAsk{
			Question:   "Which branch should the run clone off?",
			Options:    []AskOption{{Label: "develop"}, {Label: "main"}},
			AllowOther: true,
			Drafting:   drafting,
			Draft:      draft,
		},
	}
}

// askSpanOf renders one item and returns its lines and its span.
func askSpanOf(t *testing.T, items []ChatItem, width int) (string, ItemSpan) {
	t.Helper()
	out, spans := RenderItemsSpans(items, width)
	for _, sp := range spans {
		if sp.Kind == KindAsk {
			return out, sp
		}
	}
	t.Fatalf("no ask span was emitted:\n%s", out)
	return "", ItemSpan{}
}

// TestTheRecordedCardOffersAFreeTextRow: the row exists, and it is a CHOICE — its span resolves to
// the label the click path matches on, so a click on it can be routed somewhere.
func TestTheRecordedCardOffersAFreeTextRow(t *testing.T) {
	out, sp := askSpanOf(t, []ChatItem{recordedOtherCard(false, "")}, 70)
	if !strings.Contains(out, ConsentOther) {
		t.Fatalf("the card must offer the free-text row the GUI offers, got:\n%s", out)
	}
	last := sp.Options[len(sp.Options)-1]
	if last.Label != ConsentOther {
		t.Fatalf("the last option span is %q, want %q", last.Label, ConsentOther)
	}
	if got, ok := sp.OptionAt(sp.Line + last.Line); !ok || got != ConsentOther {
		t.Fatalf("OptionAt(the free-text row) = (%q, %v), want %q", got, ok, ConsentOther)
	}
	// THE FOOTER NAMES IT AS A ROW, since it is one now: it used to send the operator to the
	// composer to type what the card can take itself.
	if !strings.Contains(out, "click Other") {
		t.Errorf("the card's hint must say the row exists, got:\n%s", out)
	}
}

// TestTheRecordedCardsFreeTextRowOpensAnInput: with the row open, the card DRAWS the input and what
// has been typed into it — and the input is not itself a choice, so a click on it answers nothing.
func TestTheRecordedCardsFreeTextRowOpensAnInput(t *testing.T) {
	out, sp := askSpanOf(t, []ChatItem{recordedOtherCard(true, "release-candidate")}, 70)
	lines := strings.Split(out, "\n")

	// The span for the free-text row still points at its own row, ABOVE the input.
	last := sp.Options[len(sp.Options)-1]
	if row := lines[sp.Line+last.Line]; !strings.Contains(row, ConsentOther) {
		t.Fatalf("the free-text span points at %q, want the Other row it belongs to", row)
	}

	found := false
	for i := 0; i < sp.Lines; i++ {
		row := lines[sp.Line+i]
		if !strings.Contains(row, "release-candidate") {
			continue
		}
		found = true
		if got, ok := sp.OptionAt(sp.Line + i); ok {
			t.Errorf("a click on the input row resolved to the choice %q — typing is not answering", got)
		}
	}
	if !found {
		t.Fatalf("an open row must draw what has been typed, got:\n%s", out)
	}
}

// AND THE ROW IS OFFERED ONLY WHEN THE ASK ALLOWED FREE TEXT — otherwise the card answers a question
// the model did not ask. This is the control for both tests above.
func TestACardThatDidNotAllowFreeTextHasNoOtherRow(t *testing.T) {
	item := recordedOtherCard(false, "")
	item.Ask.AllowOther = false
	out, sp := askSpanOf(t, []ChatItem{item}, 70)
	if strings.Contains(out, ConsentOther) {
		t.Errorf("a card whose ask did not allow free text must not offer it:\n%s", out)
	}
	if len(sp.Options) != 2 {
		t.Errorf("option spans = %d, want the 2 the ask carried", len(sp.Options))
	}
}
