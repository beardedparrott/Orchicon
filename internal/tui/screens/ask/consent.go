package ask

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// consent.go is the Ask screen's half of the consent card: WHO OWNS THE KEYS while a card is
// pending, and what a decision does.

// consentHost is the shell surface the card needs. It is a narrow interface so
// the screen depends on exactly two things: settle an ask, and send a message.
type consentHost interface {
	// ConsentResolve settles the ask, records a session grant when one was granted, sends a clarifying
	// answer through the composer funnel, and repaints the transcript.
	ConsentResolve(askID string, dec chat.ConsentDecision, choice string) tea.Cmd
	// ConsentSend sends text as the next user message.
	ConsentSend(text string) tea.Cmd
	// ConsentGrants lists the session grants for the open conversation, AS THE SERVER HOLDS THEM.
	ConsentGrants(convID string) ([]chat.SessionGrant, bool)
	// ConsentGrantsLoaded reports whether the grants have been FETCHED for this conversation yet. IT
	// EXISTS SO A SURFACE NEVER GUESSES.
	ConsentGrantsLoaded(convID string) bool
	// ConsentStore exposes the persistent allow/deny list, when the plane has one.
	ConsentStore() (chat.PermissionStore, bool)
	// ConsentRevoke drops a session grant for a directory.
	ConsentRevoke(convID, directory string) error
}

func (m *Model) consentHost() (consentHost, bool) {
	h, ok := m.Shell().(consentHost)
	return h, ok
}

// IT IS THE ONLY RELEASE PATH THAT CANNOT BE FORGOTTEN.
func (m *Model) SyncTranscriptConsent(items []chat.ChatItem) {
	for i := range items {
		it := items[i]
		if it.Kind != chat.KindConsent || it.Consent == nil || !it.Consent.Pending() {
			continue
		}
		if m.consent != it.Consent {
			m.consent = it.Consent
			m.consentID = it.AskID
		}
		return
	}
	// No pending ask in the transcript: release the claim, always.
	m.consent = nil
	m.consentID = ""
}

// ClaimsKeys reports that the screen owns every key right now: a pending card (a decision being
// made) or an open list overlay.
func (m *Model) ClaimsKeys() bool {
	if m.ov != nil {
		return true
	}
	// A RECORDED CARD'S FREE-TEXT ROW CLAIMS TOO: the shell hands a claiming screen every key verbatim
	// (router.go), and an input row with nothing typing into it is not an input.
	if m.draft != nil && m.draft.Drafting && m.draftKey != m.draftDeferred {
		return true
	}
	// A PENDING RECORDED CARD CLAIMS ITS CURSOR KEYS TOO, so ↑/↓ and Enter answer it — the same claim
	// the permission card makes, for the card drawn by the other renderer (see askcard.go).
	if m.askCardClaims() {
		return true
	}
	if m.consent == nil {
		return false
	}
	// A DEFERRED CARD DOES NOT CLAIM.
	return m.consent.Ask.ID != m.consentDeferred
}

// FormOpen is deliberately FALSE while a card is up.
func (m *Model) FormOpen() bool { return m.ov != nil && m.ov.kind == ovPolicyAdd }

// OwnsTab reports that TAB belongs to THIS screen while it claims the keyboard.
func (m *Model) OwnsTab() bool { return m.ClaimsKeys() }

// DropKeyClaim is the focus chord's release (ctrl+g). IT MUST NOT DECIDE ANYTHING, and it used to
// deny.
func (m *Model) DropKeyClaim() {
	if m.consent != nil && m.consent.Pending() {
		m.consentDeferred = m.consent.Ask.ID
	}
	// AN OPEN FREE-TEXT ROW ON A RECORDED CARD YIELDS THE SAME WAY, and for the same reason: the chord
	// is the advertised way to reach the composer and must never be blocked by a card.
	if m.draft != nil && m.draft.Drafting {
		m.draftDeferred = m.draftKey
	}
	// THE RECORDED CARD'S CURSOR YIELDS THE SAME WAY, and for the same reason: ctrl+g is the advertised
	// route to the composer and must never be blocked by a card.
	if m.askCard != nil && !m.askCard.Answered && !m.askCard.Refused {
		m.askCardDeferred = m.askCardKey
	}
	// The overlay is a genuine dismissal: it is a local picker, and leaving it open on an
	// unfocused screen is what the release exists to prevent.
	m.ov = nil
	m.Base.DropKeyClaim()
}

// ReArmConsentClaim takes the keyboard back for a card that is still pending.
func (m *Model) ReArmConsentClaim() {
	if m.consent != nil && m.consent.Pending() {
		m.consentDeferred = ""
	}
}

// handleConsentKey routes one key to the pending card. It returns handled=false
// when there is no card (the caller then runs the normal path).
func (m *Model) handleConsentKey(k tea.KeyMsg) (tea.Cmd, bool) {
	st := m.consent
	if st == nil || !st.Pending() {
		return nil, false
	}
	// THE FREE-TEXT ROW OWNS EVERY KEY while it is up: what is typed there is
	// the operator's answer, not a screen action.
	if st.OtherMode {
		switch k.String() {
		case "esc":
			// Back to the options, NOT a dismissal: a typo must not throw the
			// question away (decision 13).
			st.OtherMode = false
			st.OtherInput = ""
			return m.repaint(), true
		case "enter":
			if strings.TrimSpace(st.OtherInput) == "" {
				return nil, true
			}
			return m.resolveConsent(chat.DecisionAnswer), true
		case "backspace":
			r := []rune(st.OtherInput)
			if len(r) > 0 {
				st.OtherInput = string(r[:len(r)-1])
			}
			return m.repaint(), true
		}
		if len(k.Runes) > 0 {
			st.OtherInput += string(k.Runes)
			return m.repaint(), true
		}
		// Every other key (arrows and the rest) is swallowed by the input row.
		return nil, true
	}

	switch k.String() {
	case "esc":
		if st.Ask.Kind == chat.AskQuestion {
			return m.resolveConsent(chat.DecisionAnswer), true
		}
		return m.resolveConsent(chat.DecisionDeny), true
	case "up", "k", "shift+tab":
		st.MoveSel(-1)
		return m.repaint(), true
	case "down", "j", "tab":
		st.MoveSel(1)
		return m.repaint(), true
	case "enter":
		return m.confirmConsent(), true
	}
	// ANY OTHER KEY IS SWALLOWED. That is the point of claiming: a bare letter
	// must not reach a screen action or the composer.
	return nil, true
}

// confirmConsent commits the highlighted row.
func (m *Model) confirmConsent() tea.Cmd {
	st := m.consent
	if st == nil {
		return nil
	}
	if st.SelectedDisabled() {
		// A disabled row cannot be chosen — Enter is a no-op rather than a
		// decision the policy would refuse.
		return nil
	}
	labels := st.Ask.OptionLabels()
	if len(labels) == 0 || st.Sel >= len(labels) {
		return nil
	}
	choice := labels[st.Sel]
	if st.Ask.Kind == chat.AskQuestion {
		if choice == chat.ConsentOther {
			st.OtherMode = true
			st.OtherInput = ""
			return m.repaint()
		}
		st.Choice = choice
		return m.resolveConsent(chat.DecisionAnswer)
	}
	// THE ROW INDEX IS THE DECISION, and it comes from the shared mapping so the KEYBOARD and the CLICK
	// cannot disagree about what row 1 means.
	dec, ok := st.Ask.DecisionForRow(st.Sel)
	if !ok {
		// A row that is not a permission choice — the session row when the deny list disables it.
		return nil
	}
	return m.resolveConsent(dec)
}

// resolveConsent settles the card and releases the claim.
func (m *Model) resolveConsent(dec chat.ConsentDecision) tea.Cmd {
	st := m.consent
	if st == nil {
		return nil
	}
	choice := st.Choice
	if st.Ask.Kind == chat.AskQuestion && st.OtherMode {
		choice = strings.TrimSpace(st.OtherInput)
	}
	if st.Ask.Kind == chat.AskQuestion {
		dec = chat.DecisionAnswer
		if choice == "" {
			// Esc on a question card with nothing chosen DISMISSES: no message is
			// sent, but the transcript still records that the question passed.
			dec = chat.DecisionAnswer
		}
	}
	st.Decision = dec
	st.Choice = choice
	st.Note = ""
	st.OtherMode = false
	askID := m.consentID
	// THE CLAIM IS RELEASED HERE, before the shell repaints: the next keystroke
	// must belong to the composer again.
	m.consent = nil
	m.consentID = ""
	if h, ok := m.consentHost(); ok {
		return h.ConsentResolve(askID, dec, choice)
	}
	return nil
}

// repaint asks the shell to redraw the transcript, which is where the card is
// drawn (the item's state was mutated in place above).
func (m *Model) repaint() tea.Cmd {
	if h, ok := m.Shell().(interface{ RepaintTranscript() tea.Cmd }); ok {
		return h.RepaintTranscript()
	}
	return nil
}

// ConsentPendingForTest reports the pending ask id (used by the card tests).
func (m *Model) ConsentPendingForTest() string {
	if m.consent == nil || !m.consent.Pending() {
		return ""
	}
	return m.consentID
}
