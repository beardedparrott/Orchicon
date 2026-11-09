package ask

// askdraft.go — the RECORDED ask card's free-text row: who owns the keys while it is open, and what
// submitting it does.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// IT IS THE ONLY RELEASE PATH THAT CANNOT BE FORGOTTEN — the same reason SyncTranscriptConsent
// compares ids against what the transcript actually carries.
func (m *Model) SyncAskDraft(items []chat.ChatItem) {
	for i := range items {
		it := items[i]
		if it.Kind != chat.KindAsk || it.Ask == nil || !it.Ask.Drafting {
			continue
		}
		if m.draft != it.Ask {
			m.draftKey = it.Key
		}
		m.draft = it.Ask
		return
	}
	m.draft = nil
	m.draftKey = ""
}

// ReArmAskDraftClaim takes the keyboard back for an open free-text row.
func (m *Model) ReArmAskDraftClaim() { m.draftDeferred = "" }

// handleAskDraftKey routes one key to an open free-text row.
func (m *Model) handleAskDraftKey(k tea.KeyMsg) (tea.Cmd, bool) {
	a := m.draft
	if a == nil || !a.Drafting {
		return nil, false
	}
	switch k.String() {
	case "esc":
		// BACK TO THE OPTIONS, NOT A DISMISSAL: a typo must not throw the question away. The card
		// stays answerable — its options still send, and Other opens this row again.
		a.Drafting = false
		a.Draft = ""
		return m.repaint(), true
	case "enter":
		text := strings.TrimSpace(a.Draft)
		if text == "" {
			// An empty submit is not an answer: it would send the operator's silence as a message.
			return nil, true
		}
		a.Drafting = false
		a.Draft = ""
		// THE FREE-TEXT ROW ANSWERS THE SAME WAY THE OPTIONS DO — a REPLY addressed to the ask, not an
		// ordinary message.
		if strings.TrimSpace(a.ID) != "" {
			if h, ok := m.Shell().(askReplyHost); ok {
				return tea.Batch(h.AskReply(a.ID, text), m.repaint()), true
			}
		}
		h, ok := m.consentHost()
		if !ok {
			return m.repaint(), true
		}
		return tea.Batch(h.ConsentSend(text), m.repaint()), true
	case "backspace":
		r := []rune(a.Draft)
		if len(r) > 0 {
			a.Draft = string(r[:len(r)-1])
		}
		return m.repaint(), true
	}
	if len(k.Runes) > 0 {
		a.Draft += string(k.Runes)
		return m.repaint(), true
	}
	// Every other key (arrows and the rest) is swallowed by the input row.
	return nil, true
}
