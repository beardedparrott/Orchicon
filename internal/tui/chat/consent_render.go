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
	st := it.Consent
	if st == nil {
		return ""
	}
	if !st.Pending() {
		return theme.ListMeta.Render(truncateRow(consentRecord(st), width)) + "\n"
	}
	var b strings.Builder
	for _, l := range kit2.CardLines(consentSpec(st), width) {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
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
		return "answer · " + st.Choice
	default:
		return "consent " + ConsentAllowOnce + " · " + subject
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
		spec.Body = strings.TrimSpace(a.Tool + " " + a.Target)
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
