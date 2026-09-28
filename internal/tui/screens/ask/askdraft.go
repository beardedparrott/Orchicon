package ask

// askdraft.go — the RECORDED ask card's free-text row: who owns the keys while it is open, and what
// submitting it does.
//
// The operator: "In the GUI, it lets you type in your own response. In the TUI clicking on it does
// nothing. You should be able to click on other and type in a response there."
//
// IT IS THE SAME SHAPE AS THE CONSENT CARD'S FREE TEXT, deliberately. The live state lives on the
// transcript ITEM (chat.ParsedAsk.Drafting/Draft), this screen ADOPTS the item's pointer when it
// reconciles, mutates it on the tea loop, and asks the shell to repaint — one object is both what
// the renderer draws and what the key handler moves, so the two cannot disagree.
//
// WHAT DIFFERS IS WHERE THE ANSWER GOES. A consent card blocks a live turn, so its answer is a
// reply to that turn. A RECORDED card's turn is already OVER: answering it is the operator's next
// MESSAGE (Controller.AnswerQuestion → Send), which is why submitting here sends text rather than
// replying to an ask.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// SyncAskDraft adopts the recorded card whose free-text row is open, from the items the shell hands
// the screen on every wake (app.go onChatWake -> RenderTranscript).
//
// IT IS THE ONLY RELEASE PATH THAT CANNOT BE FORGOTTEN — the same reason SyncTranscriptConsent
// compares ids against what the transcript actually carries. A draft whose card has left the
// transcript (the conversation changed, the transcript reloaded and the ask was answered) must not
// keep holding the keyboard, and comparing against the items is the one check that survives every
// way a card can disappear.
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
//
// IT IS THE MIRROR OF ReArmConsentClaim, and it exists for the same gesture: a CLICK on the card's
// own Other row is the operator acting ON the card, so the card owns the keys again — including
// after ctrl+g released them (see DropKeyClaim). It clears the deferral outright rather than
// comparing keys: only ONE draft is adopted at a time, and the caller has just opened it.
func (m *Model) ReArmAskDraftClaim() { m.draftDeferred = "" }

// handleAskDraftKey routes one key to an open free-text row. handled=false when none is open, so
// the caller runs the normal path.
//
// THE KEYS ARE THE CONSENT CARD'S, key for key, so the operator learns one input rather than two:
// Esc puts the row away WITHOUT discarding the question, Enter submits, and everything else that is
// not text is swallowed by the row.
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
