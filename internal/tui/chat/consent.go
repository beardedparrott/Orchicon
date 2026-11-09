package chat

import (
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// THE TUI NEVER STORES A PROTO TYPE HERE.

// The card's action labels.
const (
	ConsentAllowOnce = "Allow once"
	ConsentDeny      = "Deny"

	// ConsentOther is the free-text escape hatch's row label on a clarifying question card.
	ConsentOther = "Other"

	// The SESSION row is not a fixed string, because what it does depends on WHICH directory it covers
	// and the operator is entitled to see that before they choose it.
	ConsentSessionPrefix = "Never ask again in "
	ConsentSessionSuffix = " this session"
	// ConsentSessionNoDir is the session row's label when the ask names NO directory a grant could
	// cover.
	ConsentSessionNoDir = "Not available for a session grant"

	// ConsentSessionRecord is the settled RECORD's name for the session decision.
	ConsentSessionRecord = "never ask again"
)

// AskKind is what the card is asking for.
type AskKind string

const (
	// AskTool is the permission decision: may this write/execution proceed?
	AskTool AskKind = "tool"
	// AskQuestion is the clarifying question: which of these did you mean?
	AskQuestion AskKind = "question"
)

// PermissionAsk is one pending decision, as the TUI models it.
type PermissionAsk struct {
	ID string
	// ConvID is the conversation the ask was raised ON, which is NOT necessarily the conversation the
	// operator is looking at when it arrives.
	ConvID string
	Tool   string
	Target string
	// Directory is the scope a session grant would be given for.
	Directory string

	Kind       AskKind
	Question   string   // AskQuestion
	Options    []string // AskQuestion — the selectable answers
	AllowOther bool     // AskQuestion — offer the free-text row

	// Summary is the server's one-line description of the action (the wire's `summary`), which names
	// EVERY target rather than only the first.
	Summary string
	// DeniedBy names the persistent rule that denies this target ("" when none).
	DeniedBy string
}

// PermissionAskFromProto maps the wire ask onto the TUI's own model.
func PermissionAskFromProto(p *apiv1.PermissionAsk) PermissionAsk {
	if p == nil {
		return PermissionAsk{}
	}
	// A NON-EMPTY QUESTION MAKES THIS A QUESTION ASK, not a permission: no allow/deny, no grant, and
	// the answer is CONTENT the model reads.
	if q := strings.TrimSpace(p.GetQuestion()); q != "" {
		return PermissionAsk{
			ID:         p.GetAskId(),
			ConvID:     p.GetConversationId(),
			Kind:       AskQuestion,
			Question:   q,
			Options:    p.GetOptions(),
			AllowOther: p.GetAllowOther(),
		}
	}
	target := strings.TrimSpace(p.GetCommand())
	if target == "" {
		if targets := p.GetTargets(); len(targets) > 0 {
			target = targets[0]
		}
	}
	deniedBy := ""
	if below := p.GetDenyEntriesBelow(); len(below) > 0 {
		deniedBy = below[0]
	}
	return PermissionAsk{
		ID:        p.GetAskId(),
		ConvID:    p.GetConversationId(),
		Tool:      p.GetTool(),
		Target:    target,
		Directory: p.GetDirectory(),
		Kind:      AskTool,
		Summary:   strings.TrimSpace(p.GetSummary()),
		DeniedBy:  deniedBy,
	}
}

// ConsentDecision is what the operator chose.
type ConsentDecision string

const (
	DecisionAllowOnce    ConsentDecision = "allow_once"
	DecisionAllowSession ConsentDecision = "allow_session"
	DecisionDeny         ConsentDecision = "deny"
	// DecisionAnswer is a clarifying-question choice; the chosen text rides in
	// ConsentState.Choice and is sent as the next user message.
	DecisionAnswer ConsentDecision = "answer"
	// DecisionSettled is NOT a decision the operator made — it is this client admitting it no longer
	// knows the outcome. It is set ONLY on server evidence, never on an inferred turn state.
	DecisionSettled ConsentDecision = "settled"
	// DecisionUnanswered is a QUESTION the turn ended while it was still waiting on.
	DecisionUnanswered ConsentDecision = "unanswered"
)

// OptionLabels are the card's selectable rows, in order.
func (a PermissionAsk) OptionLabels() []string {
	if a.Kind == AskQuestion {
		out := append([]string{}, a.Options...)
		if a.AllowOther {
			out = append(out, ConsentOther)
		}
		return out
	}
	return []string{ConsentAllowOnce, a.SessionLabel(), ConsentDeny}
}

// SessionLabel is the session row's text, naming the DIRECTORY the grant would
// cover. See consentSessionPrefix for why the row names its scope at all.
func (a PermissionAsk) SessionLabel() string {
	dir := strings.TrimSpace(a.Directory)
	if dir == "" {
		return ConsentSessionNoDir
	}
	return ConsentSessionPrefix + dir + ConsentSessionSuffix
}

// DecisionForRow maps a card ROW index onto the decision that row means.
func (a PermissionAsk) DecisionForRow(i int) (ConsentDecision, bool) {
	if a.Kind != AskTool || a.RowDisabled(i) {
		return "", false
	}
	switch i {
	case 0:
		return DecisionAllowOnce, true
	case 1:
		return DecisionAllowSession, true
	case 2:
		return DecisionDeny, true
	default:
		return "", false
	}
}

// RowDisabled reports whether a row cannot be selected.
//
// IT IS ONE QUESTION WITH TWO ANSWERS, and both of them are "offering this row would be a lie":
//
//   - a persistent DENY entry covers the target, so a session grant cannot widen it (the card names the
//     pattern instead of offering a grant the policy will refuse — silent escalation is exactly what a
//     permission system must not do);
//   - the ask names NO directory a grant could cover, so a grant would silence nothing at all.
//
// See sessionRowSuppressed for the reason the card STATES, which is what the operator needs in order to
// understand the row they cannot take.
func (a PermissionAsk) RowDisabled(i int) bool {
	return a.Kind == AskTool && i == 1 && a.sessionRowSuppressed() != ""
}

// sessionRowSuppressed returns WHY the session-grant row cannot be taken, or "" when it can.
func (a PermissionAsk) sessionRowSuppressed() string {
	if a.DeniedBy != "" {
		return "denied by " + a.DeniedBy
	}
	if a.Directory == "" {
		return "no directory here for a session grant to cover"
	}
	return ""
}

// IT IS MUTATED ONLY ON THE TEA UPDATE LOOP (the screen's key handler and the screen's reconcile
// are both there), so the pointer shared with the transcript store needs no lock of its own.
type ConsentState struct {
	Ask PermissionAsk

	// Sel is the highlighted row.
	Sel int

	// Decision is "" while the card is PENDING; once set the item renders as a one-line record instead
	// of a card.
	Decision ConsentDecision
	// Choice is the chosen label: an option's text for a question card, or the
	// free text the operator typed under "Other".
	Choice string
	// Note is the resolution's scope, shown in the record row ("session · /dir").
	Note string

	// OtherMode is true while the free-text row is being typed into.
	OtherMode  bool
	OtherInput string
}

// Pending reports whether the card still needs a decision.
func (c ConsentState) Pending() bool { return c.Decision == "" }

// MoveSel moves the highlight by delta, SKIPPING disabled rows so a disabled
// grant can never be reached by the arrow keys.
func (c *ConsentState) MoveSel(delta int) {
	n := len(c.Ask.OptionLabels())
	if n == 0 {
		return
	}
	i := c.Sel
	for step := 0; step < n; step++ {
		i = (i + delta + n) % n
		if !c.Ask.RowDisabled(i) {
			c.Sel = i
			return
		}
	}
}

// SelectedDisabled reports whether the highlighted row is one the operator may
// not choose (Enter is then a no-op rather than a decision).
func (c ConsentState) SelectedDisabled() bool { return c.Ask.RowDisabled(c.Sel) }

// ConsentItem builds the transcript item for a pending ask.
func ConsentItem(a PermissionAsk, at int64) ChatItem {
	return ChatItem{
		Kind:    KindConsent,
		AskID:   a.ID,
		Key:     "consent-" + a.ID,
		At:      at,
		Consent: &ConsentState{Ask: a},
	}
}

// SessionGrant is one directory the operator allowed for the current session, AS THE SERVER HOLDS
// IT.
type SessionGrant struct {
	Directory string
	// GrantedAt is Unix SECONDS (0 when the server does not know), the wire's own precision.
	GrantedAt int64
}

// PermissionStore is the TUI's view of the PERSISTENT allow/deny list.
type PermissionStore interface {
	Rules() ([]PolicyRule, error)
	UpsertRule(r PolicyRule) error
	DeleteRule(effect, tool, pattern string) error
}

// PolicyRule is one persistent allow/deny entry.
type PolicyRule struct {
	Effect  string // "allow" | "deny"
	Tool    string
	Pattern string
}

// DenyingRule returns the pattern that denies a target, or "" when none does.
func DenyingRule(rules []PolicyRule, tool, target string) string {
	for _, r := range rules {
		if r.Effect != "deny" {
			continue
		}
		if r.Tool != "" && r.Tool != tool {
			continue
		}
		if MatchPattern(r.Pattern, target) {
			return r.Pattern
		}
	}
	return ""
}

// MatchPattern reports whether a path/command matches a policy glob.
func MatchPattern(pattern, target string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(target, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == target
}
