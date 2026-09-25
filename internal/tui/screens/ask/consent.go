package ask

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// consent.go is the Ask screen's half of the consent card: WHO OWNS THE KEYS
// while a card is pending, and what a decision does.
//
// THE ITEM HOLDS THE STATE AND THE SCREEN HOLDS THE KEYS. The card's live state
// lives on the transcript item's *chat.ConsentState (chat.ConsentItem), so the
// renderer draws exactly what the key handler moved — one object, no second
// latch to keep in step. This screen ADOPTS that pointer when it reconciles,
// mutates it on the tea loop, and asks the shell to repaint.

// consentHost is the shell surface the card needs. It is a narrow interface so
// the screen depends on exactly two things: settle an ask, and send a message.
type consentHost interface {
	// ConsentResolve settles the ask, records a session grant when one was
	// granted, sends a clarifying answer through the composer funnel, and
	// repaints the transcript.
	ConsentResolve(askID string, dec chat.ConsentDecision, choice string) tea.Cmd
	// ConsentSend sends text as the next user message.
	ConsentSend(text string) tea.Cmd
	// ConsentGrants lists the session grants for the open conversation.
	ConsentGrants(convID string) ([]chat.SessionGrant, bool)
	// ConsentStore exposes the persistent allow/deny list, when the plane has one.
	ConsentStore() (chat.PermissionStore, bool)
	// ConsentRevoke drops a session grant for a directory.
	ConsentRevoke(convID, directory string) error
}

func (m *Model) consentHost() (consentHost, bool) {
	h, ok := m.Shell().(consentHost)
	return h, ok
}

// SyncTranscriptConsent reconciles the card from the item list the shell hands
// the screen on EVERY wake (app.go onChatWake -> RenderTranscript).
//
// IT IS THE ONLY RELEASE PATH THAT CANNOT BE FORGOTTEN. A claim is a latch, and
// a card whose ask is gone from the transcript — the turn ended, the ask was
// superseded, the conversation was aborted — must not keep holding the
// keyboard. Comparing ids against what the transcript actually carries is the
// one check that survives every way an ask can disappear.
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

// ClaimsKeys reports that the screen owns every key right now: a pending card
// (a decision being made) or an open list overlay. The shell's input-modal gate
// (internal/tui/router.go) hands every key to the screen verbatim while this is
// true, so a printable key cannot reach the composer and a bare letter cannot
// fire a shell route.
func (m *Model) ClaimsKeys() bool { return m.consent != nil || m.ov != nil }

// FormOpen is deliberately FALSE while a card is up. A card is not a form: the
// shell advertises a form's keys (ctrl+s / esc) when this is true, and Tab must
// not be captured as a field move — the card uses Tab as row movement. Only the
// add-rule form is a form.
func (m *Model) FormOpen() bool { return m.ov != nil && m.ov.kind == ovPolicyAdd }

// DropKeyClaim is the focus chord's release (ctrl+g).
//
// WHILE A CARD IS PENDING IT IS A DENY, NOT A DISMISSAL. The claim is a latch
// and ctrl+g is the chord that must always be able to leave it, so it cannot be
// a no-op; and leaving focus in a composer the card still claims would be worse
// than either. Denying is already Esc's outcome and it is RECORDED, so the
// operator's decision is never silently dropped.
func (m *Model) DropKeyClaim() {
	if m.consent != nil && m.consent.Pending() {
		m.resolveConsent(chat.DecisionDeny)
	}
	m.ov = nil
	m.Base.DropKeyClaim()
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
	switch st.Sel {
	case 0:
		return m.resolveConsent(chat.DecisionAllowOnce)
	case 1:
		return m.resolveConsent(chat.DecisionAllowSession)
	default:
		return m.resolveConsent(chat.DecisionDeny)
	}
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
