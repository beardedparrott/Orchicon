package tui

// ask_card_answer_settles_test.go — ANSWERING A RECORDED CARD SETTLES IT THERE AND THEN.
//
// The reported symptom: a recorded clarifying-question card ("Orchicon asks") stayed drawn open and
// clickable after the operator answered it, and only showed as answered after leaving the conversation
// and coming back. The card's state is DERIVED from the durable transcript and the transcript is only
// re-read on open and while a turn STREAMS — but a recorded ask's turn has ended by the time it is
// answered, so nothing ever re-read it. The reply verdict now settles the card itself and batches a
// transcript poll; these tests pin that end to end through the shell.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// askAnswerPlane serves just enough of the Ask service to run the reply and the transcript poll, and
// counts the poll so a test can tell "the transcript was re-read" from "the card was only repainted".
type askAnswerPlane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	mu           sync.Mutex
	listMessages int
	replies      []askReplyRecord
}

type askReplyRecord struct {
	AskID  string
	Answer string
}

func (p *askAnswerPlane) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	p.mu.Lock()
	p.listMessages++
	p.mu.Unlock()
	return connect.NewResponse(&apiv1.ListMessagesResponse{}), nil
}

func (p *askAnswerPlane) ReplyPermissionAsk(_ context.Context, req *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error) {
	p.mu.Lock()
	p.replies = append(p.replies, askReplyRecord{AskID: req.Msg.GetAskId(), Answer: req.Msg.GetAnswer()})
	p.mu.Unlock()
	return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{Applied: true}), nil
}

func (p *askAnswerPlane) pollCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listMessages
}

func (p *askAnswerPlane) replyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.replies)
}

// askAnswerApp opens c1 on the Ask tab with a RECORDED question on screen. Its ParsedAsk.ID is the
// recorded call's id — the thing ReplyPermissionAsk addresses — because without it the click path can
// only fall back to an ordinary send.
func askAnswerApp(t *testing.T, h apiv1connect.AskOrchiconServiceHandler) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindAsk,
		Key:  "m-m1-ask",
		At:   1,
		Ask: &chat.ParsedAsk{
			ID:         "ask-1",
			Question:   "Which branch should the run clone off?",
			Options:    []chat.AskOption{{Label: "develop"}, {Label: "main"}},
			AllowOther: true,
		},
	})
	m.onChatWake()
	return m
}

// askItem returns the conversation's recorded card, so an assertion reads the store the renderer reads.
func askItem(t *testing.T, m *App) *chat.ParsedAsk {
	t.Helper()
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindAsk && it.Ask != nil && it.Ask.ID == "ask-1" {
			return it.Ask
		}
	}
	t.Fatal("fixture: the recorded card is not in the store")
	return nil
}

// pressKey sends one key through the shell, the way the runtime does.
func pressKey(t *testing.T, m *App, k tea.KeyMsg) (*App, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(k)
	app, ok := next.(*App)
	if !ok {
		t.Fatalf("Update returned %T, want *App", next)
	}
	return app, cmd
}

// TestAnAnsweredRecordedCardSettlesWithoutLeavingTheConversation is the reported bug, through the
// CLICK path: click an option, the server's verdict comes back, and — with NO open/transcript re-read —
// the card is settled on screen and in the store.
func TestAnAnsweredRecordedCardSettlesWithoutLeavingTheConversation(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)

	// The option is genuinely clickable first, so the assertion below cannot pass vacuously.
	row := askRowAt(t, m, "1. develop")
	if got, ok := m.transcriptAskOptionAtFrameRow(row); !ok || got != "develop" {
		t.Fatalf("fixture: the develop row resolved to (%q, %v), want (develop, true)", got, ok)
	}

	// CLICK IT.
	next, clickCmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: row,
	})
	m = next.(*App)
	if clickCmd == nil {
		t.Fatal("the click produced no reply command, so no verdict could ever arrive")
	}

	// THE SERVER'S VERDICT, exactly what ReplyPermissionAsk returns.
	next, _ = m.Update(chat.ConsentRepliedMsg{
		ConvID: "c1", AskID: "ask-1", Applied: true, Answer: "develop",
	})
	m = next.(*App)
	// THE REPAINT THE HANDLER BATCHES (no OpenAskConversation / onTranscript anywhere above).
	m.onChatWake()

	item := askItem(t, m)
	if !item.Answered {
		t.Fatal("the answered card is still OPEN in the store — this is the operator's card that only " +
			"settles after leaving and re-entering the conversation")
	}
	if item.AnswerText != "develop" {
		t.Fatalf("the settled card records %q, want the answer %q", item.AnswerText, "develop")
	}
	if got, ok := m.transcriptAskOptionAtFrameRow(askRowAt(t, m, "answered · develop")); ok {
		t.Errorf("the settled record's row still resolves to the option %q — it is drawn as a clickable card", got)
	}
	frame := m.viewFrame()
	if !strings.Contains(frame, "answered · develop") {
		t.Errorf("the painted frame does not show the settled record:\n%s", frame)
	}
	for _, gone := range []string{"Orchicon asks", "1. develop", "2. main"} {
		if strings.Contains(frame, gone) {
			t.Errorf("the painted frame still shows %q after the answer:\n%s", gone, frame)
		}
	}
	if notice := m.dock.Notice; !strings.Contains(notice, "answer sent") {
		t.Errorf("notice = %q, want a question-appropriate confirmation", notice)
	}
	if notice := m.dock.Notice; strings.Contains(notice, "permission decision applied") {
		t.Errorf("notice = %q — a question must not be reported as a permission decision", notice)
	}
}

// TestKeyboardAnswerSettlesTheRecordedCard: select + Enter on the card takes the SAME reply path and
// the SAME verdict settles it. The reply is executed against the stub, so this is the keyboard's real
// AnswerQuestion hop rather than a hand-built message.
func TestKeyboardAnswerSettlesTheRecordedCard(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)

	m, _ = pressKey(t, m, tea.KeyMsg{Type: tea.KeyDown}) // develop -> main
	m, cmd := pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on the recorded card produced no reply command")
	}
	msg := runCmdBounded(cmd, runCtxCmdBudget)
	verdict, ok := msg.(chat.ConsentRepliedMsg)
	if !ok {
		t.Fatalf("Enter produced %T, want chat.ConsentRepliedMsg", msg)
	}
	if verdict.AskID != "ask-1" || verdict.Answer != "main" {
		t.Fatalf("the keyboard replied with (ask=%q, answer=%q), want (ask-1, main)", verdict.AskID, verdict.Answer)
	}
	if plane.replyCount() != 1 {
		t.Fatalf("ReplyPermissionAsk was called %d times, want 1 — the keyboard did not use the reply RPC", plane.replyCount())
	}

	next, _ := m.Update(verdict)
	m = next.(*App)
	m.onChatWake()

	item := askItem(t, m)
	if !item.Answered || item.AnswerText != "main" {
		t.Fatalf("keyboard answer left the card answered=%v text=%q, want true/main", item.Answered, item.AnswerText)
	}
	if got, ok := m.transcriptAskOptionAtFrameRow(askRowAt(t, m, "answered · main")); ok {
		t.Errorf("the keyboard-answered card still resolves to the option %q", got)
	}
	if frame := m.viewFrame(); strings.Contains(frame, "Orchicon asks") {
		t.Errorf("the keyboard-answered card is still drawn as an open card:\n%s", frame)
	}
}

// TestOtherFreeTextAnswerSettlesTheRecordedCard: the Other row's typed answer settles the card with the
// text the operator typed — not the word "Other".
func TestOtherFreeTextAnswerSettlesTheRecordedCard(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)

	// Cursor to the Other row (options: develop, main, Other).
	m, _ = pressKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = pressKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	// Enter on the Other row OPENS the input; it repaints synchronously rather than sending, so the
	// command may legitimately be nil.
	m, _ = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !askItem(t, m).Drafting {
		t.Fatal("Enter on the Other row did not open the free-text row")
	}
	for _, r := range "release-candidate" {
		m, _ = pressKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := askItem(t, m).Draft; got != "release-candidate" {
		t.Fatalf("the free-text row collected %q, want what was typed", got)
	}
	m, cmd := pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("submitting the typed answer produced no reply command")
	}
	msg := runCmdBounded(cmd, runCtxCmdBudget)
	verdict, ok := msg.(chat.ConsentRepliedMsg)
	if !ok {
		t.Fatalf("the Other submit produced %T, want chat.ConsentRepliedMsg", msg)
	}
	if verdict.Answer != "release-candidate" {
		t.Fatalf("the Other submit replied with %q, want the typed text", verdict.Answer)
	}

	next, _ := m.Update(verdict)
	m = next.(*App)
	m.onChatWake()

	item := askItem(t, m)
	if !item.Answered || item.AnswerText != "release-candidate" {
		t.Fatalf("Other answer left the card answered=%v text=%q, want true/release-candidate",
			item.Answered, item.AnswerText)
	}
	if frame := m.viewFrame(); !strings.Contains(frame, "answered · release-candidate") {
		t.Errorf("the painted frame does not show the typed answer:\n%s", frame)
	}
	if got, ok := m.transcriptAskOptionAtFrameRow(askRowAt(t, m, "answered · release-candidate")); ok {
		t.Errorf("the Other-answered card still resolves to the option %q", got)
	}
}

// TestASuccessfulAskAnswerQueuesATranscriptPoll: settling the card locally is only half — the durable
// transcript (the answer and the model's continuation) must be re-read in the same batch.
func TestASuccessfulAskAnswerQueuesATranscriptPoll(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)
	before := plane.pollCount()

	next, cmd := m.Update(chat.ConsentRepliedMsg{
		ConvID: "c1", AskID: "ask-1", Applied: true, Answer: "develop",
	})
	m = next.(*App)
	if cmd == nil {
		t.Fatal("the verdict produced no command, so no transcript poll was queued")
	}
	m = runCmdDelivering(t, m, cmd)

	if got := plane.pollCount(); got <= before {
		t.Fatalf("ListMessages calls = %d after the reply, was %d — the durable transcript was never "+
			"re-read, so the answer and the model's continuation stay invisible without a re-entry", got, before)
	}
}

// TestAnExpiredAskAnswerSettlesAsNoLongerOpen: the server says the question is gone. The card must stop
// offering rows, and the notice must name the question rather than a permission decision.
func TestAnExpiredAskAnswerSettlesAsNoLongerOpen(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)

	next, _ := m.Update(chat.ConsentRepliedMsg{ConvID: "c1", AskID: "ask-1", Expired: true})
	m = next.(*App)
	m.onChatWake()

	if item := askItem(t, m); !item.Answered {
		t.Fatal("an expired question is still drawn as an open, answerable card")
	}
	row := askRowAt(t, m, "answered ·")
	if got, ok := m.transcriptAskOptionAtFrameRow(row); ok {
		t.Errorf("the expired card's row still resolves to the option %q", got)
	}
	notice := m.dock.Notice
	if !strings.Contains(notice, "no longer open") {
		t.Errorf("notice = %q, want it to say the question is no longer open", notice)
	}
	if strings.Contains(notice, "permission decision applied") {
		t.Errorf("notice = %q — an expired question was reported as a permission decision", notice)
	}
}

// TestAFailedAskAnswerKeepsTheCardAnswerable: a transport/refusal error answered nothing, so the card
// must stay clickable and the notice must name the failure.
func TestAFailedAskAnswerKeepsTheCardAnswerable(t *testing.T) {
	plane := &askAnswerPlane{}
	m := askAnswerApp(t, plane)

	next, _ := m.Update(chat.ConsentRepliedMsg{ConvID: "c1", AskID: "ask-1", Err: "boom"})
	m = next.(*App)
	m.onChatWake()

	if item := askItem(t, m); item.Answered {
		t.Fatal("a failed reply settled the card — the operator's question was closed by an error")
	}
	row := askRowAt(t, m, "1. develop")
	if got, ok := m.transcriptAskOptionAtFrameRow(row); !ok || got != "develop" {
		t.Fatalf("after a failed answer the develop row resolved to (%q, %v) — the card is no longer "+
			"answerable", got, ok)
	}
	notice := m.dock.Notice
	if !strings.Contains(notice, "answer failed: boom") {
		t.Errorf("notice = %q, want an answer-failed notice", notice)
	}
	if strings.Contains(notice, "permission decision applied") {
		t.Errorf("notice = %q — a failed question reply was reported as a permission decision", notice)
	}
}
