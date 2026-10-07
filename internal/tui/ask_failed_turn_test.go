package tui

// ask_failed_turn_test.go — A FAILED TURN MUST SAY WHY, ON THE TRANSCRIPT.
//
// The operator: "The GUI has an actual error message in the conversation that tells you why you couldn't
// connect or if there was a problem. The TUI just drops with no indication as to why." The screenshot is the
// whole report: the operator's message TWICE with NOTHING between them — two failed sends that left no row
// at all.
//
// THE MESSAGE-TO-ITEM DECISION (conversationItems: role first, then metadata.error, then the assistant
// default) is asserted in package chat, where that function lives — see
// internal/tui/chat/failed_turn_items_test.go. THIS file asserts the layer the operator actually reads: the
// PAINTED FRAME, after the real repaint path. ask_render_stop_test.go already records why: the widget's own
// fields can be right while the pane clips them away, and m.View() is the only layer at which "The TUI just
// drops with no indication as to why" is true or false.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// The operator's OWN failure text, verbatim from the report's screenshot (AC 10: verify against the real
// shape, not only a synthetic string).
const operatorFailureText = "conversation session send: orchicon bridge: start Ask turn: provider status 401 Unauthorized"

const operatorFailedModel = "orchicon/ollama/deepseek-v4.1-flash"

// failedTurnRows is the pair the durable transcript carries for a failed turn: the operator's message, then
// the error row conversationItems now builds for it (assistant role + metadata.error → KindError).
func failedTurnRows() []chat.ChatItem {
	return []chat.ChatItem{
		{Kind: chat.KindUser, Text: "why can't you connect?", Key: "m-m1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText(operatorFailureText, operatorFailedModel), Key: "m-m2", At: 2},
	}
}

// AC 1 + AC 3: the failure reason AND the model are in the PAINTED frame, through the real repaint path.
func TestAFailedTurnIsVisibleInThePaintedFrame(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	for _, it := range failedTurnRows() {
		m.chatStore.append("c1", it)
	}
	m.onChatWake()

	frame := m.View()
	if !strings.Contains(frame, "401 Unauthorized") {
		t.Errorf("the failure reason is not in the painted frame — the operator's \"The TUI just drops "+
			"with no indication as to why\"\n--- frame ---\n%s", tailOf(frame, 2000))
	}
	if !strings.Contains(frame, operatorFailedModel) {
		t.Errorf("the failed turn does not name the model in the painted frame (AC 3)\n--- frame ---\n%s",
			tailOf(frame, 2000))
	}
}

// AC 7: the error text is RAW, not markdown-mangled — the transcript's existing rule for KindError. A `*` in
// a provider error must survive literally, and the row must be drawn by the ERROR renderer.
func TestTheErrorTextIsNotMarkdownMangled(t *testing.T) {
	raw := "start Ask turn: provider status 401 Unauthorized — see *Settings → Default models*"
	m, _ := askWithTranscript(t, "c1")
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindError, Text: chat.FailedTurnText(raw, operatorFailedModel), Key: "m-m2", At: 2,
	})
	m.onChatWake()

	frame := ansi.Strip(m.View())
	if !strings.Contains(frame, "*Settings → Default models*") {
		t.Errorf("the raw error text was mangled in the frame (AC 7 — errors stay raw)\n--- frame ---\n%s",
			tailOf(frame, 1600))
	}
	if !strings.Contains(frame, "error") {
		t.Errorf("the KindError row is not drawn with the error label (AC 7)\n--- frame ---\n%s", tailOf(frame, 1600))
	}
}

// AC 8: a LONG provider error WRAPS within the transcript rather than clipping the footer. The row budget is
// unchanged (the error is a normal transcript row, not new chrome), and the footer must still be present.
func TestALongProviderErrorWrapsWithoutClippingTheFooter(t *testing.T) {
	long := operatorFailureText + " " + strings.Repeat("provider refused the request and named the model ", 6)
	m, _ := askWithTranscript(t, "c1")
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindError, Text: chat.WithRetryAffordance(chat.FailedTurnText(long, operatorFailedModel)), Key: "m-m2", At: 2,
	})
	m.onChatWake()

	frame := ansi.Strip(m.View())
	// The tail of the error (which only appears if the text wrapped rather than being clipped) must be
	// present, and the transcript must still have its activity/status slot.
	if !strings.Contains(frame, "press enter to send it again") {
		t.Errorf("a long provider error was clipped rather than wrapped — its retry line is gone (AC 8)\n"+
			"--- frame ---\n%s", tailOf(frame, 2000))
	}
	lines := strings.Split(frame, "\n")
	if len(lines) != m.height {
		t.Errorf("the frame row budget changed: %d rows, want %d (AC 8)", len(lines), m.height)
	}
}

// AC 4 (the operator's OWN case, POST-ack): a durable failed turn puts the draft back in the composer, so the
// error row's "your message is back" line is TRUE — not just on the pre-ack path. This is the case the report
// was: `… start Ask turn: provider status 401 …` is the post-ack collector's wrapped error, so no ErrMsg was
// ever emitted and the draft had no restore until this.
func TestADurableFailedTurnPutsTheDraftBackInTheComposer(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", ModelRef: operatorFailedModel}}
	// The operator typed a message and pressed Enter: requestSend records lastSent (the draft source) and
	// clears the buffer, exactly like the real send path.
	m.dock.SetValue("why can't you connect?")
	m.dock.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.dock.Value() != "" {
		t.Fatalf("fixture: the composer still holds %q after Enter", m.dock.Value())
	}

	// The durable transcript lands WITH an error row (the POST-ack failure), via the real onTranscript path.
	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "why can't you connect?", Key: "m-m1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText(operatorFailureText, operatorFailedModel), Key: "m-m2", At: 2},
	}})

	if got := m.dock.Value(); got != "why can't you connect?" {
		t.Errorf("a durable failed turn did not put the draft back in the composer (AC 4): composer = %q, "+
			"so the error row's retry line is a false claim on the operator's own post-ack case", got)
	}
}

// surfaceTurnFailure puts a PRE-ACK send/turn failure on the TRANSCRIPT, not only on the composer strip.

// AC 5: THE PRE-ACK CASE. A send that fails BEFORE the turn is acked writes NO durable row, so the
// row-rendering fix has nothing to find — the failure must be surfaced LOCALLY. This drives the real path:
// the controller's ErrMsg case in the router, then the painted frame.
func TestAPreAckFailureIsSurfacedOnTheTranscript(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", ModelRef: operatorFailedModel}}
	// The operator's own message is on the transcript (the optimistic echo the send path appends).
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "why can't you connect?", Key: "draft-1", Live: true, At: 1})
	m.onChatWake()

	if strings.Contains(m.View(), "401") {
		t.Fatal("fixture: the failure is already on screen, so this test would measure nothing")
	}

	// The controller reports the pre-ack failure exactly as startStream does, carrying the conversation id.
	m.dispatch(chat.ErrMsg{Where: "send", Err: errStringer(operatorFailureText), ConvID: "c1"})

	frame := ansi.Strip(m.View())
	if !strings.Contains(frame, "401 Unauthorized") {
		t.Errorf("a PRE-ACK send failure (no durable row) is not on the transcript — the operator's "+
			"\"The TUI just drops with no indication as to why\"\n--- frame ---\n%s", tailOf(frame, 2000))
	}
	if !strings.Contains(frame, operatorFailedModel) {
		t.Errorf("the pre-ack failure does not name the model (AC 3/AC 5)\n--- frame ---\n%s", tailOf(frame, 2000))
	}
}

// AC 5, the other half: a failure for a conversation that is NOT open must not be painted on the OPEN one's
// transcript. (The composer strip is global and DOES carry it — that is the existing failed-send surface;
// what must not happen is the open conversation's TRANSCRIPT claiming a failure that belongs to another.)
func TestAPreAckFailureForAnotherConversationDoesNotPaintThisOne(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c2", Title: "other", ModelRef: "m"}}
	m.onChatWake()

	m.dispatch(chat.ErrMsg{Where: "send", Err: errStringer("some other chat's failure"), ConvID: "c2"})

	// The OPEN conversation's transcript stream must not carry the other chat's failure.
	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(ansi.Strip(str.View()), "some other chat's failure") {
		t.Errorf("a failure for another conversation was painted on the open transcript:\n%s",
			ansi.Strip(str.View()))
	}
}

// REGRESSION (reviewer): the durable-restore must be SCOPED to the turn THIS client sent.
//
// An unscoped restore is not a theoretical miss: a durable KindError row is HISTORICAL, so opening any
// conversation whose transcript ends in a turn that failed — a previous session, the OTHER client, hours
// ago — delivers exactly that row through onTranscript, and the composer came back holding the message
// this session last sent into a DIFFERENT chat. Measured against the pre-fix code:
//
//	composer after opening a conversation with an OLD failed turn = "hello from another chat"
//
// The legitimate restore (the failed turn IS this client's own send — the operator's POST-ack case) must
// still happen, and both halves are asserted here so the guard cannot be "fixed" by removing it.
func TestAnOldFailedTurnDoesNotInjectThisSessionsDraftIntoTheComposer(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// This session sent in ANOTHER chat: the dock's lastSent is that message.
	m.dock.SetValue("hello from another chat")
	m.dock.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.dock.Value() != "" {
		t.Fatalf("fixture: the composer still holds %q after Enter", m.dock.Value())
	}

	// Opening c1 delivers its durable transcript, which ends in an OLD failed turn that has nothing to do
	// with what this session sent.
	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "an old question", Key: "m-x1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText("old failure", "old-model"), Key: "m-x2", At: 2},
	}})

	if got := m.dock.Value(); got != "" {
		t.Errorf("a HISTORICAL failed turn injected this session's last send into the composer: %q — the "+
			"restore must be scoped to the turn this client actually sent (the dock's lastSent), not to any "+
			"error row a transcript happens to carry", got)
	}
}

// The scoping uses matchesAny (the same suffix rule the transcript merge uses), so the prepended context
// preamble does not defeat it: the durable user row carries the preamble, lastSent is the bare text.
func TestTheScopedRestoreSurvivesAPrependedContextPreamble(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.dock.SetValue("why can't you connect?")
	m.dock.Update(tea.KeyMsg{Type: tea.KeyEnter})

	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "[context: some worker]\nwhy can't you connect?", Key: "m-u1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText("401", "m"), Key: "m-e1", At: 2},
	}})

	if got := m.dock.Value(); got != "why can't you connect?" {
		t.Errorf("the scoped restore lost the operator's own POST-ack case: composer = %q", got)
	}
}

// AC 3 (reviewer): a PRE-ACK failure is named for the FAILING conversation's model, not the open one's — the
// store keeps a failure for a chat that is not on screen, and naming the open chat's ref there is a claim
// about the wrong model.
func TestAPreAckFailureNamesTheFailingConversationsModel(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// c1 is open; the failure belongs to c2, whose row names a DIFFERENT model.
	m.conversations = []chat.Conversation{
		{ID: "c1", Title: "open", ModelRef: "orchicon/ollama/model-for-the-open-chat"},
		{ID: "c2", Title: "other", ModelRef: "orchicon/ollama/model-that-actually-refused"},
	}

	m.surfaceTurnFailure("c2", errStringer(operatorFailureText))

	items := m.chatStore.snapshot("c2")
	if len(items) == 0 {
		t.Fatalf("no failure row was recorded for c2")
	}
	body := items[len(items)-1].Text
	if !strings.Contains(body, "orchicon/ollama/model-that-actually-refused") {
		t.Errorf("the failure for c2 did not name c2's model (AC 3): %q", body)
	}
	if strings.Contains(body, "model-for-the-open-chat") {
		t.Errorf("the failure for c2 named the OPEN conversation's model: %q", body)
	}
}

// REGRESSION: the retry claim is TRUE only where the draft was actually restored.
//
// The claim is about the SHELL's composer, and the shell restores the draft ONLY for a turn THIS client sent
// (restoreDraftForFailedTurn is scoped to the dock's lastSent). A durable failed turn carried over from
// another session or the other client is DRAWN (its reason and model) but must NOT tell the operator their
// message is back, because it is not. Measured against the pre-fix code: a historical row's frame contained
// "back in the composer" while the composer was empty — the same class of misreport this change exists to fix.
func TestAHistoricalFailedTurnMakesNoComposerClaim(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// This session sent elsewhere; the transcript ends in an OLD failed turn that is not ours.
	m.dock.SetValue("hello from another chat")
	m.dock.Update(tea.KeyMsg{Type: tea.KeyEnter})

	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "an old question", Key: "m-x1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText("old failure 401", "old-model"), Key: "m-x2", At: 2},
	}})
	m.onChatWake()

	if m.dock.Value() != "" {
		t.Fatalf("fixture: the draft was restored (%q), so this test would measure the wrong case", m.dock.Value())
	}
	frame := ansi.Strip(m.View())
	if strings.Contains(frame, "back in the composer") {
		t.Errorf("a HISTORICAL failed turn claims the message is back in the composer while it is empty — a "+
			"false claim about the composer, and the same misreport this change fixes:\n%s", tailOf(frame, 1400))
	}
	if !strings.Contains(frame, "old failure 401") {
		t.Errorf("the historical failure's reason is not drawn at all:\n%s", tailOf(frame, 1400))
	}
}

// AND THE OTHER HALF: the operator's OWN failed turn DOES claim it, because that is the one the draft was
// restored for — so the guard above cannot be satisfied by removing the affordance entirely.
func TestOwnFailedTurnStillShowsTheRetryAffordance(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.dock.SetValue("why can't you connect?")
	m.dock.Update(tea.KeyMsg{Type: tea.KeyEnter})

	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "why can't you connect?", Key: "m-m1", At: 1},
		{Kind: chat.KindError, Text: chat.FailedTurnText(operatorFailureText, operatorFailedModel), Key: "m-m2", At: 2},
	}})
	m.onChatWake()

	if m.dock.Value() != "why can't you connect?" {
		t.Fatalf("fixture: the draft was not restored (%q)", m.dock.Value())
	}
	if frame := ansi.Strip(m.View()); !strings.Contains(frame, "back in the composer") {
		t.Errorf("the operator's OWN failed turn does not show the retry affordance:\n%s", tailOf(frame, 1400))
	}
}
