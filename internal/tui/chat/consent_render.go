package chat

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// consent_render.go draws a KindConsent item in the transcript: the CARD while the ask is pending,
// a one-line RECORD once it has been decided.

// consentLines renders one consent item at the given pane width.
func consentLines(it ChatItem, width int) string {
	text, _ := consentLineSpans(it, width)
	return text
}

// consentLineSpans is consentLines plus WHERE EACH OPTION ROW LANDED, so a CLICK on the card
// resolves to the row under it — the same contract the clarifying-question card has had.
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

// ConsentSubject names what an ask is ABOUT, in one line: the tool and its target for a permission ask,
// the QUESTION for a clarifying one.
//
// IT IS EXPORTED SO THE FOOTER AND THE CARD CANNOT DESCRIBE THE SAME ASK DIFFERENTLY. The shell's status
// line names the ask the turn is parked on (App.transcriptStatusLine -> cardWaitingLine) while the card
// itself is rendered from the same PermissionAsk, and two locally-derived subjects would be two answers to
// "what am I being asked about" — the drift this package's one-adapter comment exists to prevent.
func ConsentSubject(a PermissionAsk) string {
	if a.Kind == AskQuestion {
		if q := strings.TrimSpace(a.Question); q != "" {
			return q
		}
	}
	return strings.TrimSpace(a.Tool + " " + a.Target)
}

// consentRecord is the settled form: what was decided, about what.
func consentRecord(st *ConsentState) string {
	a := st.Ask
	subject := ConsentSubject(a)
	switch st.Decision {
	case DecisionAllowSession:
		scope := a.Directory
		if scope == "" {
			scope = a.Target
		}
		out := ConsentSessionRecord + " · " + scope + " (this session)"
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
	case DecisionUnanswered:
		// A QUESTION THE TURN DIED WAITING ON.
		if q := strings.TrimSpace(a.Question); q != "" {
			return "unanswered · " + q + " — the turn ended while it was waiting on you; reply in your own words and it will be sent as your next message"
		}
		return "unanswered · the turn ended while it was waiting on you"
	default:
		// An UNRECOGNISED decision must not silently claim "allow once" — that was the previous default,
		// which would report a permission as granted on the strength of a value it did not understand.
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
		// THE BODY NAMES THE TOOL AND THE TARGET, which is the whole content of the decision: "approve" is
		// meaningless without what is being approved.
		spec.Body = a.Summary
		if spec.Body == "" {
			spec.Body = strings.TrimSpace(a.Tool + " " + a.Target)
		}
		if a.DeniedBy != "" {
			spec.Notice = "denied by the permission list (" + a.DeniedBy + ") — a session grant cannot override it"
		} else if a.Directory == "" {
			// The row cannot be taken because there is nothing to grant. Saying which of the two reasons applies
			// is the whole point: a disabled row with no explanation reads as a broken control.
			spec.Notice = "this ask names no directory a session grant could cover, so there is nothing to allow for the session — allow it once, or deny it"
		}
	}
	labels := a.OptionLabels()
	for i, l := range labels {
		line := kit2.CardLine{Text: l, Selected: i == st.Sel, Disabled: a.RowDisabled(i)}
		if line.Disabled {
			line.Detail = a.sessionRowSuppressed()
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
