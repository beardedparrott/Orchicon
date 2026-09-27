package chat

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// consent_render.go draws a KindConsent item in the transcript: the CARD while
// the ask is pending, a one-line RECORD once it has been decided.
//
// The box itself is drawn by kit2 (kit2.CardLines), so the border, the padding
// and the selection highlight come from the shared primitives rather than from
// a second renderer that would drift from them.

// consentLines renders one consent item at the given pane width.
func consentLines(it ChatItem, width int) string {
	text, _ := consentLineSpans(it, width)
	return text
}

// consentLineSpans is consentLines plus WHERE EACH OPTION ROW LANDED, so a CLICK on
// the card resolves to the row under it — the same contract the clarifying-question
// card has had.
//
// The operator: "I saw the card and actually selected accept but I also noticed I
// couldn't click on it in the TUI. I had to click into the card then use the
// keyboard to select it." Two cards for the same kind of decision, one clickable
// and one keyboard-only, is a coin-flip for the operator rather than a design.
//
// The offsets come from the widget that DREW the card (kit2.CardLinesSpans), never
// from a counter here: a drifted offset means a click answers with the row the
// operator did not choose.
func consentLineSpans(it ChatItem, width int) (string, []AskOptionSpan) {
	st := it.Consent
	if st == nil {
		return "", nil
	}
	if !st.Pending() {
		return theme.ListMeta.Render(truncateRow(consentRecord(st), width)) + "\n", nil
	}
	spec := consentSpec(st)
	lines, rows := kit2.CardLinesSpans(spec, width)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	labels := st.Ask.OptionLabels()
	opts := make([]AskOptionSpan, 0, len(rows))
	for i, r := range rows {
		if i >= len(labels) {
			break
		}
		opts = append(opts, AskOptionSpan{Line: r.Line, Lines: r.Lines, Label: labels[i]})
	}
	return b.String(), opts
}

// consentRecord is the settled form: what was decided, about what.
func consentRecord(st *ConsentState) string {
	a := st.Ask
	subject := strings.TrimSpace(a.Tool + " " + a.Target)
	switch st.Decision {
	case DecisionAllowSession:
		scope := a.Directory
		if scope == "" {
			scope = a.Target
		}
		out := ConsentAllowSession + " · " + scope + " (session)"
		return "consent " + out
	case DecisionDeny:
		return "consent " + ConsentDeny + " · " + subject
	case DecisionAnswer:
		if strings.TrimSpace(st.Choice) == "" {
			// Esc on a question card picks nothing: record THAT, rather than the
			// dangling "answer · " a bare separator left behind.
			return "question dismissed — no answer sent"
		}
		return "answer · " + st.Choice
	case DecisionSettled:
		// NOT "expired unanswered" and NOT a denial: this client simply stopped being
		// able to see the card. The outcome was decided somewhere it cannot observe.
		return "no longer pending · " + subject
	default:
		// An UNRECOGNISED decision must not silently claim "allow once" — that was the
		// previous default, which would report a permission as granted on the strength
		// of a value it did not understand.
		return "consent resolved (" + string(st.Decision) + ") · " + subject
	}
}

// consentSpec maps the ask's state onto the shared card spec.
func consentSpec(st *ConsentState) kit2.CardSpec {
	a := st.Ask
	spec := kit2.CardSpec{Footer: kit2.CardFooter(a.Kind == AskQuestion)}
	switch a.Kind {
	case AskQuestion:
		spec.Title = "Question"
		spec.Body = a.Question
	default:
		spec.Title = "Permission"
		// THE BODY NAMES THE TOOL AND THE TARGET, which is the whole content of
		// the decision: "approve" is meaningless without what is being approved.
		//
		// The SERVER's summary is preferred when present because it names EVERY
		// target — Target is a single path, so a batch_write touching two files
		// showed only the first. The operator: "the GUI showed what file/directory
		// batch_write was modifying but the TUI did not." The GUI renders the
		// summary, so the TUI does too and the two describe the same action.
		spec.Body = a.Summary
		if spec.Body == "" {
			spec.Body = strings.TrimSpace(a.Tool + " " + a.Target)
		}
		if a.DeniedBy != "" {
			spec.Notice = "denied by the permission list (" + a.DeniedBy + ") — a session grant cannot override it"
		}
	}
	labels := a.OptionLabels()
	for i, l := range labels {
		line := kit2.CardLine{Text: l, Selected: i == st.Sel, Disabled: a.RowDisabled(i)}
		if line.Disabled {
			line.Detail = "denied by " + a.DeniedBy
		}
		spec.Lines = append(spec.Lines, line)
	}
	spec.ShowInput = st.OtherMode
	spec.Input = st.OtherInput
	return spec
}

// ConsentCardText is the card's plain-text form (no styling), for tests and for
// any surface that needs the content without the box.
func ConsentCardText(it ChatItem, width int) string {
	return strings.TrimRight(consentLines(it, width), "\n")
}
