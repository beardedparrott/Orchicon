package ask

// askcard.go — the RECORDED ask card's CURSOR: who owns the keys, and what a key does.
//
// REPORTED: "It works but in the TUI the highlight bar is gone. Looks fine the gui." — and then, asked
// which bar: "This already works with the orchicon adapter. It should mimic that."
//
// The split they were naming is real. An ask served by the NATIVE bridge enters the consent registry, so
// the transcript draws it as a KindConsent card with Sel, ↑/↓, Enter and a ▸ bar. An ask served over MCP
// is RECORDED and its turn is over, so it is drawn by renderAskCardSpans as a KindAsk card — which had no
// cursor at all. Same question, same box, and only one of the two answerable from the keyboard.
//
// So this mirrors ask/screens/consent.go: the ITEM holds Sel (chat.ParsedAsk.Sel, mutated only on the tea
// loop), this screen adopts the pointer the renderer draws, the keys move it, and the claim is released by
// ctrl+g (DropKeyClaim) exactly as the consent card's is.
//
// WHAT DIFFERS IS WHAT AN ANSWER IS, and it is a real difference rather than an inconsistency:
//
//	consent card   the turn is LIVE and blocked on the decision, so a choice is a REPLY to that turn
//	recorded card  the turn is OVER, so a choice is the operator's NEXT MESSAGE (AnswerQuestion → Send)
//
// That is why Enter routes through ConsentSend (the composer funnel) instead of ConsentResolve.
//
// AND ONE DELIBERATE DEPARTURE FROM THE CONSENT CARD: TYPING RELEASES THE CLAIM.
//
// The consent card claims the keyboard absolutely and SWALLOWS every key it does not use, because a live
// turn is blocked on it — the operator has nothing else to do. A recorded card sits in an IDLE
// conversation, where the operator may well want to keep typing, so an absolute claim would hijack the
// composer for as long as the question stayed unanswered. Their instruction was explicit: "card holds the
// keys until I start typing". So a printable character hands the keys back AND is delivered to the
// composer — the keystroke is re-dispatched once the claim is gone, rather than being eaten.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// askReplyHost is the shell surface a RECORDED card needs to answer: the reply RPC, addressed by the
// ask's id. Separate from consentHost because it is a different operation — a consent card replies to a
// PAUSED turn, a recorded card settles a transcript marker and delivers the answer as the next turn.
type askReplyHost interface {
	AskReply(askID, answer string) tea.Cmd
}

// SyncAskCard adopts the RECORDED ask card that currently holds the cursor, from the items the shell
// hands the screen on every wake (app.go onChatWake -> RenderTranscript).
//
// IT IS ALSO THE RELEASE PATH THAT CANNOT BE FORGOTTEN — the same contract SyncTranscriptConsent and
// SyncAskDraft have. A card whose ask left the transcript (answered, the conversation changed, the
// transcript reloaded) must not keep the keyboard, and comparing against the items it actually carries is
// the one check that survives every way a card can disappear.
//
// THE NEWEST PENDING CARD WINS. One transcript can hold several unanswered questions, and the one the
// operator is looking at is the last. Adopting it by KEY (not by "the newest") is what keeps the claim
// from leaking onto a different card when a new one arrives.
func (m *Model) SyncAskCard(items []chat.ChatItem) {
	var (
		want   *chat.ParsedAsk
		wantID string
	)
	for i := range items {
		it := items[i]
		if it.Kind != chat.KindAsk || it.Ask == nil {
			continue
		}
		// A settled or refused ask is a RECORD, not a card: it draws as one line and offers nothing.
		if it.Ask.Answered || it.Ask.Refused {
			continue
		}
		want, wantID = it.Ask, it.Key
	}
	if want != m.askCard {
		// A DIFFERENT card, so clamp the adopted cursor to what it actually offers. The cursor lives on
		// the item, and an item reused for a new question could otherwise start out of range — Enter
		// would then read a row that is not there and do nothing.
		m.clampAskSel(want)
		m.askCardKey = wantID
	}
	m.askCard = want
	if want == nil {
		m.askCardKey = ""
	}
}

// clampAskSel keeps the cursor inside the card's rows: one per option, plus the Other row when the
// question offers free text.
func (m *Model) clampAskSel(a *chat.ParsedAsk) {
	if a == nil {
		a = m.askCard
	}
	if a == nil {
		return
	}
	rows := len(a.Options)
	if a.AllowOther {
		rows++
	}
	if rows == 0 {
		a.Sel = 0
		return
	}
	if a.Sel < 0 {
		a.Sel = 0
	}
	if a.Sel >= rows {
		a.Sel = rows - 1
	}
}

// askCardClaims reports whether the recorded card currently owns the keyboard.
//
// IT YIELDS TO THE FREE-TEXT ROW. While the operator is typing an answer into Other, handleAskDraftKey
// owns every key — a cursor claim on top of it would move the highlight under their typing.
func (m *Model) askCardClaims() bool {
	if m.askCard == nil || m.askCard.Answered || m.askCard.Refused || m.askCard.Drafting {
		return false
	}
	return m.askCardKey != m.askCardDeferred
}

// ReArmAskCardClaim takes the keyboard back for a card that is still pending, after a CLICK on one of its
// rows — the mirror of ReArmConsentClaim, and it exists for the same reason: a click is the operator
// acting ON the card, so the card owns the keys again, including after ctrl+g released them.
func (m *Model) ReArmAskCardClaim() { m.askCardDeferred = "" }

// MoveAskSel moves the cursor by delta, wrapping — the consent card's MoveSel, without its disabled-row
// skip, because no row on this card can be disabled (there is no grant and no deny list on a question).
func (m *Model) MoveAskSel(delta int) {
	a := m.askCard
	if a == nil {
		return
	}
	rows := len(a.Options)
	if a.AllowOther {
		rows++
	}
	if rows <= 0 {
		return
	}
	a.Sel = ((a.Sel+delta)%rows + rows) % rows
}

// handleAskCardKey routes one key to the pending recorded card. handled=false when no card holds the
// keys, so the caller runs the normal path.
//
// THE KEYS ARE THE CONSENT CARD'S, so the operator learns one card rather than two: ↑/↓ (and k/j, tab)
// move, Enter confirms, esc puts the keys down. The one addition is the typing release described above.
func (m *Model) handleAskCardKey(k tea.KeyMsg) (tea.Cmd, bool) {
	if !m.askCardClaims() {
		return nil, false
	}
	switch k.String() {
	case "esc":
		// NOT A DISMISSAL. Esc on a permission card denies, and on a consent QUESTION it records
		// "dismissed — no answer sent". Neither belongs here: this card is inside an idle conversation,
		// nothing is blocked on it, and a question must never be closed by a keystroke the operator used
		// to get their keyboard back. Esc RELEASES the claim and leaves the card answerable (by key
		// again after a click, or by click at once).
		m.askCardDeferred = m.askCardKey
		return m.repaint(), true
	case "up", "k", "shift+tab":
		m.MoveAskSel(-1)
		return m.repaint(), true
	case "down", "j", "tab":
		m.MoveAskSel(1)
		return m.repaint(), true
	case "enter":
		return m.confirmAskCard(), true
	}
	// TYPING HANDS THE KEYS BACK AND IS DELIVERED, not swallowed.
	//
	// The consent card swallows everything it does not use, which is right for a card blocking a live
	// turn and wrong for one sitting in an idle conversation: an absolute claim would read as a composer
	// that stopped working. Clearing the deferral FIRST means the re-dispatched keystroke reaches the
	// composer on the ordinary path — the operator loses no character, which a plain "ignore and release"
	// would have cost them (their first letter of the next message).
	if len(k.Runes) > 0 {
		m.askCardDeferred = m.askCardKey
		return tea.Batch(m.repaint(), redispatchKey(k)), true
	}
	// Every other key (arrows we did not claim, function keys, backspace) is swallowed: the card owns the
	// keyboard, and a stray key must not reach a screen action behind it.
	return nil, true
}

// redispatchKey re-delivers a keystroke to the shell once the card has released its claim.
func redispatchKey(k tea.KeyMsg) tea.Cmd {
	return func() tea.Msg { return k }
}

// confirmAskCard commits the highlighted row.
func (m *Model) confirmAskCard() tea.Cmd {
	a := m.askCard
	if a == nil {
		return nil
	}
	// THE OTHER ROW OPENS THE INPUT RATHER THAN SENDING ITS OWN LABEL — the same rule the click path
	// applies (router.go), and the reason it exists: sending "Other" would put the name of the button in
	// the transcript where the operator's words belong.
	if a.AllowOther && a.Sel == len(a.Options) {
		a.Drafting = true
		a.Draft = ""
		// The free-text row owns the keys now, so the card's cursor claim must not linger on top of it.
		// The next wake hands the adopted pointer to SyncAskDraft, which claims for the row instead.
		return m.repaint()
	}
	if a.Sel < 0 || a.Sel >= len(a.Options) {
		return nil
	}
	label := strings.TrimSpace(a.Options[a.Sel].Label)
	if label == "" {
		return nil
	}
	h, ok := m.consentHost()
	if !ok {
		return nil
	}
	// THE ANSWER IS A REPLY, THROUGH THE ASK'S OWN ID.
	//
	// It used to go out as ConsentSend — an ordinary next message — which LOOKS equivalent from the
	// terminal and is not: the transcript keeps the awaiting marker, so the card is still open to every
	// other client and to a reload, and nothing durable records what was answered. That is the whole of
	// "I answer it and it comes back". AskReply goes through ReplyPermissionAsk, which replaces the
	// marker AND delivers the answer as the conversation's next turn.
	//
	// A card with NO id (a row recorded before the field existed) falls back to the send, so a degraded
	// answer beats a dead card.
	if strings.TrimSpace(a.ID) == "" {
		return tea.Batch(h.ConsentSend(label), m.repaint())
	}
	if h, ok := m.Shell().(askReplyHost); ok {
		return tea.Batch(h.AskReply(a.ID, label), m.repaint())
	}
	return tea.Batch(h.ConsentSend(label), m.repaint())
}
