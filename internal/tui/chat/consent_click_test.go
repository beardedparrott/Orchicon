package chat

import (
	"strings"
	"testing"
)

// TestConsentCardReportsItsOptionRowsForClicking is the operator's report as a
// test: "I saw the card and actually selected accept but I also noticed I couldn't
// click on it in the TUI. I had to click into the card then use the keyboard to
// select it."
//
// The permission card now reports WHERE each option row landed, exactly as the
// clarifying-question card does — so a click can resolve to the row under it
// instead of the card being keyboard-only.
func TestConsentCardReportsItsOptionRowsForClicking(t *testing.T) {
	ask := PermissionAsk{
		ID: "a1", Kind: AskTool, Tool: "write",
		Target: "/tmp/x.txt", Directory: "/tmp",
	}
	it := ConsentItem(ask, 1000)

	text, opts := consentLineSpans(it, 70)
	if len(opts) != 3 {
		t.Fatalf("want a span per option (allow once / allow for this session / deny), got %d: %+v", len(opts), opts)
	}
	lines := strings.Split(text, "\n")
	for _, o := range opts {
		if o.Line < 0 || o.Line+o.Lines > len(lines) {
			t.Fatalf("option %q spans %d..%d, outside the %d rendered lines", o.Label, o.Line, o.Line+o.Lines, len(lines))
		}
		if !strings.Contains(lines[o.Line], o.Label) {
			t.Errorf("the span for %q points at a row that does not show it:\n  row %d = %q\nfull card:\n%s",
				o.Label, o.Line, lines[o.Line], text)
		}
	}
	// The three rows must be the DECISIONS, in the card's own order — a click resolves
	// the row by GEOMETRY and the row's INDEX decides what it means (DecisionForRow), so a
	// row in the wrong slot approves the wrong thing.
	for i, want := range []string{ConsentAllowOnce, ask.SessionLabel(), ConsentDeny} {
		if opts[i].Label != want {
			t.Errorf("row %d = %q, want %q", i, opts[i].Label, want)
		}
	}
}

// TestSettledConsentCardReportsNoRows: a decided card is a one-line record, not a
// choice. Reporting rows for it would let a click re-decide something settled.
func TestSettledConsentCardReportsNoRows(t *testing.T) {
	it := ConsentItem(PermissionAsk{
		ID: "a2", Kind: AskTool, Tool: "write", Target: "/tmp/x.txt", Directory: "/tmp",
	}, 1000)
	it.Consent.Decision = DecisionAllowOnce

	text, opts := consentLineSpans(it, 70)
	if len(opts) != 0 {
		t.Fatalf("a settled card reported %d clickable rows", len(opts))
	}
	if strings.Contains(text, "┌") {
		t.Errorf("a settled card should be a record line, not a box:\n%s", text)
	}
}

// TestQuestionConsentCardReportsItsAnswersAsRows: the question card rides the same
// KindConsent item, so its rows must be reported too — the click path handles both.
// The free-text row ("Other") is a real choice and gets a span like any other.
func TestQuestionConsentCardReportsItsAnswersAsRows(t *testing.T) {
	it := ConsentItem(PermissionAsk{
		ID: "q1", Kind: AskQuestion, Question: "Which file did you mean?",
		Options: []string{"main.go", "util.go"}, AllowOther: true,
	}, 1000)

	_, opts := consentLineSpans(it, 70)
	if len(opts) != 3 {
		t.Fatalf("want a span per answer, including the free-text row, got %d: %+v", len(opts), opts)
	}
	for i, want := range []string{"main.go", "util.go", ConsentOther} {
		if opts[i].Label != want {
			t.Errorf("row %d = %q, want %q", i, opts[i].Label, want)
		}
	}
}
