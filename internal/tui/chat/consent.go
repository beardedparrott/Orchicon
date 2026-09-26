package chat

import (
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// consent.go is the TUI's OWN model of a pending permission ask (the consent
// card) and of a clarifying question, plus the three action LABELS the card
// offers.
//
// THE TUI NEVER STORES A PROTO TYPE HERE. The ask rides the stream as a wire
// message the sibling policy task owns; the TUI maps it onto PermissionAsk and
// renders that. A change to the wire shape is then one adapter in
// internal/tui/chat's stream switch, not a change in every surface that shows a
// card.

// The three action labels. THEY ARE THE GUI'S WORDS, VERBATIM — the repo's
// parity rule: the two clients must describe the same decision identically, so
// the operator recognises the choice whichever client they are in.
const (
	ConsentAllowOnce    = "Allow once"
	ConsentAllowSession = "Allow for this session"
	ConsentDeny         = "Deny"

	// ConsentOther is the free-text escape hatch's row label on a clarifying
	// question card. The operator picking it types their own answer rather than
	// being forced into a canned option.
	ConsentOther = "Other"
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
//
// Target is the PATH when the tool is a file write and the COMMAND when it is
// an execution — the card must name whichever it is, because "approve" means
// different things for the two and the operator cannot consent to a target
// they were not shown.
type PermissionAsk struct {
	ID        string
	Tool      string
	Target    string
	Directory string // the scope a session grant is given for

	Kind       AskKind
	Question   string   // AskQuestion
	Options    []string // AskQuestion — the selectable answers
	AllowOther bool     // AskQuestion — offer the free-text row

	// DeniedBy names the persistent rule that denies this target ("" when
	// none). The card states it and DISABLES the session row instead of
	// offering a grant the policy will refuse — silent escalation is exactly
	// what a permission system must not do.
	DeniedBy string
}

// ConsentAskMsg carries a pending permission ask from the turn stream to the
// shell, which renders it as a transcript card (App.ShowConsentAsk).
//
// WHY IT IS A MESSAGE AND NOT A STORE CALL. The controller reaches the UI only
// through EventStore and this command channel, so a card that arrives mid-turn
// has to travel the same way every other stream signal does. The shell is the
// only place that knows the conversation's session grants, and it must consult
// them before drawing a card — a directory already granted for this session must
// not ask again.
//
// ConvID rides the message because the card is appended to the conversation's
// slot, not to whatever conversation happens to be on screen when the ask
// lands.
type ConsentAskMsg struct {
	ConvID string
	Ask    PermissionAsk
}

// PermissionAskFromProto maps the wire ask onto the TUI's own model.
//
// THIS IS THE ONE ADAPTER the package comment promises: the wire shape can change
// without touching any surface that draws a card. It is deliberately pure so the
// mapping is testable without a stream, a store or a terminal.
//
// Target is the COMMAND for an execution and the PATH for a file write — the card
// has to name whichever it is, because "allow" means different things for each.
// The wire puts them in different fields (command; repeated targets), so the
// choice is made here rather than left to the renderer.
//
// DeniedBy takes the first deny entry at or below the ask's directory: the card
// STATES the rule and disables the session-grant row rather than offering a grant
// the policy will refuse. Offering a choice that cannot take effect is the silent
// escalation a permission system must never perform.
func PermissionAskFromProto(p *apiv1.PermissionAsk) PermissionAsk {
	if p == nil {
		return PermissionAsk{}
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
		Tool:      p.GetTool(),
		Target:    target,
		Directory: p.GetDirectory(),
		Kind:      AskTool,
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
	return []string{ConsentAllowOnce, ConsentAllowSession, ConsentDeny}
}

// RowDisabled reports whether a row cannot be selected. The session grant is
// the one case: a target the persistent list denies cannot be widened for the
// session, so the row is shown DISABLED with the pattern named rather than
// offered and then refused.
func (a PermissionAsk) RowDisabled(i int) bool {
	return a.Kind == AskTool && a.DeniedBy != "" && i == 1
}

// ConsentState is the card's live state, carried on the transcript item so ONE
// object is both what the renderer draws and what the key handler moves.
//
// IT IS MUTATED ONLY ON THE TEA UPDATE LOOP (the screen's key handler and the
// screen's reconcile are both there), so the pointer shared with the
// transcript store needs no lock of its own.
type ConsentState struct {
	Ask PermissionAsk

	// Sel is the highlighted row.
	Sel int

	// Decision is "" while the card is PENDING; once set the item renders as a
	// one-line record instead of a card. That is what makes a refresh
	// idempotent: a resolved item is never re-asked.
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
func ConsentItem(a PermissionAsk) ChatItem {
	return ChatItem{
		Kind:    KindConsent,
		AskID:   a.ID,
		Key:     "consent-" + a.ID,
		Consent: &ConsentState{Ask: a},
	}
}

// SessionGrant is one directory the operator allowed for the current session.
type SessionGrant struct {
	Directory string
	Tool      string
	Count     int
}

// PermissionStore is the TUI's view of the PERSISTENT allow/deny list. The
// FILE is the source of truth (the sibling policy task owns storage), so this
// is deliberately a narrow read/write handle: a change made from the TUI is the
// same change the GUI and a hand-edit see.
type PermissionStore interface {
	Rules() ([]PolicyRule, error)
	UpsertRule(r PolicyRule) error
	DeleteRule(effect, tool, pattern string) error
	// SessionGrants lists the directory grants the plane holds for a
	// conversation; RevokeGrant drops one.
	SessionGrants(convID string) ([]SessionGrant, error)
	RevokeGrant(convID, directory string) error
}

// PolicyRule is one persistent allow/deny entry.
type PolicyRule struct {
	Effect  string // "allow" | "deny"
	Tool    string
	Pattern string
}

// DenyingRule returns the pattern that denies a target, or "" when none does.
// Only DENY entries matter here: an allow entry never has to be reported on the
// card, and the deny is the one that makes a session grant impossible.
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

// MatchPattern reports whether a path/command matches a policy glob. A trailing
// "/**" (or a bare "*") matches everything beneath the prefix; a pattern with no
// wildcard is an exact match.
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
