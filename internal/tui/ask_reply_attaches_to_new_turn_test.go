package tui

// ask_reply_attaches_to_new_turn_test.go — THE MODEL'S REPLY TO AN ANSWERED ASK STREAMS IN.
//
// The operator, on a `claude/…` conversation:
//
//	"When the model's reply to that answer is short, it never appears; it only shows up after clicking
//	 away from the conversation and back. When the reply is long, it does appear."
//
// That is a MISSING ATTACH, not a missing repaint. An MCP-served ask (claude/opencode/antigravity) is
// answered by CONTINUING the conversation: `ReplyPermissionAsk` starts a brand-new server-side turn and
// the client that asked never gets told which turn it is. So the reply arrives on a stream nobody is
// consuming:
//
//	LONG reply  — still running at the next 5s rail reload, whose turn_in_flight re-attaches. It appears,
//	              up to five seconds late, BY ACCIDENT.
//	SHORT reply — finished inside the gap between rail reloads. Nothing re-attaches, and the only other
//	              thing that re-reads the transcript is opening the conversation. It appears never.
//
// The fix is the server naming the turn (`assistant_message_id`) and the shell attaching to it at once —
// Reattach is exactly what opening a conversation does, so the reply streams in on the click instead of
// through the rail. These tests drive the real shell, the real controller and a real (stub) plane, and
// assert on the PAINTED FRAME, because that is what the operator could not see.
//
// NOTHING HERE OPENS THE CONVERSATION, CLICKS THE RAIL OR RUNS A REFRESH TICK — that is the whole claim.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// replyAfterAnswerTokens name the two halves of the operator's report.
const (
	longReplyToken  = "REPLY-AFTER-ANSWER"
	shortReplyToken = "SHORT-REPLY"
)

// answerAttachPlane is the plane of a conversation whose ask has just been answered: ReplyPermissionAsk
// names the new turn, WatchTurnStream either streams that turn or reports it already finished, and
// ListMessages is the durable transcript — which, exactly like the real server, is INCOMPLETE while the
// turn runs: the reply row only appears once it has been produced.
type answerAttachPlane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	// assistantID is the new turn's assistant message id the reply RPC names ("" = the live-ask shape,
	// which starts no turn and is the pre-fix wire contract).
	assistantID string
	// chunks are the text parts WatchTurnStream emits for that turn before closing.
	chunks []string
	// watchNotFound makes WatchTurnStream report the turn as over — the SHORT-reply case, where the
	// model finished before this client could dial.
	watchNotFound bool
	// replyVisibleAfter is the number of ListMessages calls that must elapse before the finished reply
	// row is in the durable page. It models WHEN the reply became durable: the immediate poll after the
	// verdict can land before the turn has produced anything, while any later poll sees it.
	replyVisibleAfter int

	mu           sync.Mutex
	listMessages int
	watchFor     string
	watches      int
	replies      []string
}

func (p *answerAttachPlane) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	// THE RAIL SAYS IDLE, which is the operator's short-reply case: the turn is over (or finishing) and
	// nothing in the rail is going to re-attach this client.
	return connect.NewResponse(&apiv1.ListConversationsResponse{
		Conversations: []*apiv1.Conversation{{Id: "c1", Title: "Alpha", MessageCount: 3}},
	}), nil
}

func (p *answerAttachPlane) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	p.mu.Lock()
	p.listMessages++
	call := p.listMessages
	p.mu.Unlock()

	base := time.Unix(1_700_000_000, 0)
	// NEWEST-FIRST, as the real server orders them.
	msgs := []*apiv1.ChatMessage{
		{Id: "m-user-answer", Role: "user", Content: "develop", CreatedAt: timestamppb.New(base.Add(2 * time.Second))},
	}
	if call > p.replyVisibleAfter {
		msgs = append([]*apiv1.ChatMessage{{
			Id:        "m-reply",
			Role:      "assistant",
			Content:   p.replyToken(),
			CreatedAt: timestamppb.New(base.Add(3 * time.Second)),
		}}, msgs...)
	}
	return connect.NewResponse(&apiv1.ListMessagesResponse{Messages: msgs}), nil
}

func (p *answerAttachPlane) GetConversation(_ context.Context, req *connect.Request[apiv1.GetConversationRequest]) (*connect.Response[apiv1.GetConversationResponse], error) {
	return connect.NewResponse(&apiv1.GetConversationResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId(), Title: "Alpha"},
	}), nil
}

func (p *answerAttachPlane) ReplyPermissionAsk(_ context.Context, req *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error) {
	p.mu.Lock()
	p.replies = append(p.replies, req.Msg.GetAnswer())
	p.mu.Unlock()
	return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
		Applied:            true,
		Detail:             "answer sent as your next message",
		AssistantMessageId: p.assistantID,
	}), nil
}

func (p *answerAttachPlane) WatchTurnStream(_ context.Context, req *connect.Request[apiv1.WatchTurnStreamRequest], st *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	p.mu.Lock()
	p.watches++
	p.watchFor = req.Msg.GetAssistantMessageId()
	p.mu.Unlock()
	if p.watchNotFound {
		// The turn finished before this client dialled. The real RPC's own wording.
		return connect.NewError(connect.CodeNotFound,
			errNoRunningTurn())
	}
	for _, c := range p.chunks {
		if err := st.Send(&apiv1.ChatStreamResponse{
			Event: &apiv1.ChatStreamResponse_TextChunk{TextChunk: &apiv1.TextChunk{Content: c}},
		}); err != nil {
			return err
		}
	}
	return nil
}

// errNoRunningTurn is the server's NotFound for a watch on a finished turn.
func errNoRunningTurn() error {
	return &turnOverError{}
}

type turnOverError struct{}

func (*turnOverError) Error() string {
	return "no running turn for this message — the turn finished or was superseded"
}

func (p *answerAttachPlane) replyToken() string {
	if p.watchNotFound {
		return shortReplyToken
	}
	return longReplyToken
}

func (p *answerAttachPlane) watchCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watches
}

func (p *answerAttachPlane) watchedID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watchFor
}

func (p *answerAttachPlane) answered() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.replies...)
}

// answerAttachApp opens c1 on the Ask tab with a RECORDED question card on screen, wired to the plane.
// Its ParsedAsk.ID is the recorded call's id — the thing ReplyPermissionAsk addresses — because without
// it a click can only fall back to an ordinary send.
func answerAttachApp(t *testing.T, p *answerAttachPlane) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(p))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	healthyPlane(m)
	m.conversations = []chat.Conversation{{ID: "c1", Title: "Alpha", MessageN: 2}}
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "cut me a branch", Key: "m-u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindAsk,
		Key:  "m-m1-ask",
		At:   2,
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

// clickAskOption answers the recorded card the way the operator does, and returns the shell's verdict
// message (already run against the plane, so the reply RPC really happened).
func clickAskOption(t *testing.T, m *App, option string) (*App, tea.Cmd, chat.ConsentRepliedMsg) {
	t.Helper()
	row := askRowAt(t, m, option)
	next, clickCmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: row,
	})
	m = next.(*App)
	if clickCmd == nil {
		t.Fatalf("fixture: clicking %q produced no reply command, so no verdict could ever arrive", option)
	}
	// THE CLICK'S COMMAND IS A BATCH (the reply plus the repaint it schedules), so the verdict is found
	// inside it rather than assumed to be the batch's own value.
	var verdict chat.ConsentRepliedMsg
	if !findConsentReply(runCmdBounded(clickCmd, runCtxCmdBudget), 0, &verdict) {
		t.Fatalf("fixture: clicking %q produced no chat.ConsentRepliedMsg, so no verdict could arrive", option)
	}
	if verdict.Answer != option {
		t.Fatalf("fixture: the click answered %q, want %q", verdict.Answer, option)
	}
	return m, nil, verdict
}

// findConsentReply walks a command's result (unwrapping batches) looking for the reply verdict.
func findConsentReply(msg tea.Msg, depth int, out *chat.ConsentRepliedMsg) bool {
	if msg == nil || depth > 6 {
		return false
	}
	switch m := msg.(type) {
	case chat.ConsentRepliedMsg:
		*out = m
		return true
	case tea.BatchMsg:
		for _, c := range m {
			if c == nil {
				continue
			}
			if findConsentReply(runCmdBounded(c, runCtxCmdBudget), depth+1, out) {
				return true
			}
		}
	}
	return false
}

// drainAttach replays the shell's command graph — plus the controller's OWN follow-up channel, which is
// where a re-attached stream and its polls arrive — for at most budget, stopping early when until holds.
// perStep observes the shell after every delivered message, which is how a test can catch a state that
// only exists mid-flight (a live chunk before the durable page catches up).
func drainAttach(t *testing.T, m *App, cmd tea.Cmd, until func(*App) bool, budget time.Duration, perStep func(*App)) *App {
	t.Helper()
	queue := []tea.Cmd{cmd}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if until(m) {
			return m
		}
		if len(queue) == 0 {
			select {
			case c := <-m.chatCmds:
				if c != nil {
					queue = append(queue, c)
				}
			case <-time.After(15 * time.Millisecond):
			}
			if perStep != nil {
				perStep(m)
			}
			continue
		}
		c := queue[0]
		queue = queue[1:]
		msg := runCmdBounded(c, runCtxCmdBudget)
		if msg == nil {
			continue
		}
		if bm, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range bm {
				if sub != nil {
					queue = append(queue, sub)
				}
			}
			continue
		}
		next, follow := m.Update(msg)
		if next != nil {
			m = next.(*App)
		}
		if follow != nil {
			queue = append(queue, follow)
		}
		if perStep != nil {
			perStep(m)
		}
	}
	return m
}

// storeHasLiveToken reports whether a LIVE (streamed) chunk carrying token is in the transcript store —
// the difference between "the reply arrived on the turn's stream" and "a poll later found it".
func storeHasLiveToken(m *App, token string) bool {
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Live && strings.Contains(it.Text, token) {
			return true
		}
	}
	return false
}

// TestTheReplyToAnAnsweredAskStreamsInWithoutReEntry is AC2, the outermost proof of the operator's
// fixed case: click an option, the server names the new turn, and the model's reply is PAINTED on the
// same screen — with no conversation open, no rail click and no refresh tick.
func TestTheReplyToAnAnsweredAskStreamsInWithoutReEntry(t *testing.T) {
	plane := &answerAttachPlane{
		assistantID: "a-2",
		chunks:      []string{longReplyToken},
		// The reply is DURABLE only once the watch has served it: before that the turn has produced
		// nothing, so no poll could legitimately find it.
		replyVisibleAfter: 1 << 20,
	}
	m := answerAttachApp(t, plane)
	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.View(), longReplyToken) {
		t.Fatal("fixture: the reply token is already on screen, so this test would prove nothing")
	}

	m, _, verdict := clickAskOption(t, m, "develop")
	if verdict.AssistantMessageID != "a-2" {
		t.Fatalf("the verdict carries AssistantMessageID=%q, want the server's a-2", verdict.AssistantMessageID)
	}

	next, cmds := m.Update(verdict)
	m = next.(*App)

	var sawLive bool
	m = drainAttach(t, m, cmds, func(a *App) bool {
		return strings.Contains(a.viewFrame(), longReplyToken)
	}, 5*time.Second, func(a *App) {
		if storeHasLiveToken(a, longReplyToken) {
			sawLive = true
		}
	})

	// THE REPLY IS ON THE PAINTED FRAME — no OpenAskConversation, no rail click, no refresh tick anywhere
	// above (the fixture opened the conversation, and that is the last time it was opened).
	if frame := m.viewFrame(); !strings.Contains(frame, longReplyToken) {
		t.Fatalf("the model's reply to the answer is NOT on screen:\n%s\n"+
			"  This is the operator's report: the reply only appears after leaving the conversation and "+
			"coming back, because nothing attached to the turn the answer started.", frame)
	}
	// AND IT ARRIVED ON THE NEW TURN'S STREAM, not through a poll that happened to catch it: the shell
	// dialled the id the server named, and a live chunk with the reply's text reached the store.
	if plane.watchCalls() == 0 {
		t.Fatal("WatchTurnStream was never dialled — the shell did not attach to the turn the answer " +
			"started, so the reply could only be found by a later poll by accident")
	}
	if got := plane.watchedID(); got != "a-2" {
		t.Fatalf("the shell dialled a watch for %q, want the new turn a-2", got)
	}
	if !sawLive {
		t.Error("no LIVE chunk carrying the reply ever reached the store — the text on screen came from " +
			"a poll, not from the turn's stream, so a slow reply would not appear incrementally")
	}
	// THE ANSWER REALLY WENT, so this is the operator's flow and not a synthetic attach.
	if got := plane.answered(); len(got) != 1 || got[0] != "develop" {
		t.Fatalf("the plane received answers %q, want exactly one (develop)", got)
	}
}

// TestTheAnsweredTurnsIndicatorsAppear is AC3: the shell shows the NEW turn as running — stop
// affordance and the activity line — the instant the answer lands, rather than claiming an idle
// conversation while a reply is on its way.
func TestTheAnsweredTurnsIndicatorsAppear(t *testing.T) {
	plane := &answerAttachPlane{
		assistantID:       "a-2",
		chunks:            []string{longReplyToken},
		replyVisibleAfter: 1 << 20,
	}
	m := answerAttachApp(t, plane)
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: the conversation must be idle before the answer")
	}
	if line := m.askStatusLine(); strings.Contains(line, "Orchicon is") {
		t.Fatalf("fixture: the activity line is already up (%q)", line)
	}

	m, _, verdict := clickAskOption(t, m, "develop")
	// THE VERDICT ALONE, delivered but not yet drained: this is the moment "after the reply verdict and
	// before the stream closes".
	next, _ := m.Update(verdict)
	m = next.(*App)
	m.onChatWake() // the repaint the handler batches

	if !m.chat.IsStreaming("c1") {
		t.Fatal("the conversation's stream slot is IDLE after answering an ask whose answer started a " +
			"turn — nothing would consume that turn's reply, which is the reported bug")
	}
	if line := m.askStatusLine(); !strings.Contains(line, "Orchicon is") {
		t.Errorf("the activity line does not report the new turn (%q) — the operator is shown an idle "+
			"conversation while a reply is on its way", line)
	}
	// AND THE STOP AFFORDANCE follows the same slot: the composer advertises ctrl+y only while a reply
	// is in flight, so its presence IS the affordance.
	if hint := m.dock.Hint(); !strings.Contains(hint, "ctrl+y stop") {
		t.Errorf("the composer offers no Stop (%q) for the turn the answer started", hint)
	}
}

// TestAPermissionDecisionDoesNotAttach is AC4: a verdict that names no turn — a live-ask answer, a
// permission decision, an expired ask — must dial nothing. Attaching there would abandon the stream the
// client is already reading (the orchicon adapter's working path).
func TestAPermissionDecisionDoesNotAttach(t *testing.T) {
	plane := &answerAttachPlane{assistantID: "a-2", chunks: []string{longReplyToken}}
	m := answerAttachApp(t, plane)
	// A PERMISSION card the shell drew, so the verdict takes the permission arm rather than the question
	// arm that owns a recorded ask.
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "perm-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	m.onChatWake()

	next, cmds := m.Update(chat.ConsentRepliedMsg{
		ConvID: "c1", AskID: "perm-1", Applied: true,
	})
	m = next.(*App)
	m = drainAttach(t, m, cmds, func(*App) bool { return false }, 600*time.Millisecond, nil)

	if got := plane.watchCalls(); got != 0 {
		t.Fatalf("WatchTurnStream was dialled %d time(s) for a verdict that named no turn — a permission "+
			"decision starts no turn, and attaching would abandon the stream the client is reading", got)
	}
	if m.chat.IsStreaming("c1") {
		t.Fatal("a permission decision armed a stream slot — nothing is streaming")
	}

	// THE SAME FOR A LIVE-ASK QUESTION ANSWER: applied, but no turn named (the server returns empty on the
	// in-turn path), so still nothing to attach to.
	plane2 := &answerAttachPlane{assistantID: "", chunks: []string{longReplyToken}}
	m2 := answerAttachApp(t, plane2)
	m2, _, verdict := clickAskOption(t, m2, "main")
	if verdict.AssistantMessageID != "" {
		t.Fatalf("fixture: the live-ask shape must carry no id, got %q", verdict.AssistantMessageID)
	}
	next2, cmds2 := m2.Update(verdict)
	m2 = next2.(*App)
	m2 = drainAttach(t, m2, cmds2, func(*App) bool { return false }, 600*time.Millisecond, nil)
	if got := plane2.watchCalls(); got != 0 {
		t.Fatalf("WatchTurnStream was dialled %d time(s) for a live-ask answer — the orchicon adapter's "+
			"in-turn answer starts no turn and must not be re-attached", got)
	}
}

// TestAShortReplyAlreadyFinishedStillAppears is AC8 — the operator's EXACT failing case.
//
// The model finished before this client could dial: the watch reports the turn over, the rail says the
// conversation is idle (so no rail reload will ever re-attach it), and the durable page already holds the
// finished reply. It must appear, the slot must resolve, and the activity line must clear — with no
// refresh tick and no re-entry.
func TestAShortReplyAlreadyFinishedStillAppears(t *testing.T) {
	plane := &answerAttachPlane{
		assistantID:   "a-3",
		watchNotFound: true,
		// The immediate poll after the verdict lands BEFORE the reply is durable (the turn had not
		// produced it yet); every later poll sees it. That is the five-second gap the operator fell into.
		replyVisibleAfter: 1,
	}
	m := answerAttachApp(t, plane)

	m, _, verdict := clickAskOption(t, m, "develop")
	next, cmds := m.Update(verdict)
	m = next.(*App)

	m = drainAttach(t, m, cmds, func(a *App) bool {
		return strings.Contains(a.viewFrame(), shortReplyToken) && !a.chat.IsStreaming("c1")
	}, 5*time.Second, nil)

	if frame := m.viewFrame(); !strings.Contains(frame, shortReplyToken) {
		t.Fatalf("the finished reply (%q) is NOT on screen:\n%s\n"+
			"  The turn ended before this client dialled and the rail reports the conversation idle, so "+
			"nothing re-attaches and no tick re-reads the transcript — the operator's short-reply case, "+
			"which only showed up after leaving the conversation and coming back.", shortReplyToken, frame)
	}
	if m.chat.IsStreaming("c1") {
		t.Fatal("the slot is still streaming a turn the server has already finished — the reply would sit " +
			"under an \"Orchicon is …\" line that never clears")
	}
	if line := m.askStatusLine(); strings.Contains(line, "Orchicon is") {
		t.Errorf("the activity line still claims the turn is running (%q) after the reply landed", line)
	}
	if plane.watchCalls() == 0 {
		t.Fatal("the shell never dialled the finished turn, so it never learned the turn was over")
	}
}

// TestWithoutTheTurnIDTheShortReplyStaysInvisible is AC9, and it is the RACE, REPRODUCED.
//
// It runs the SAME idle-rail / short-reply scenario with the PRE-FIX wire contract — a verdict that names
// no turn, which is exactly what the server returned before `assistant_message_id` existed. Nothing
// attaches, the rail never reports a turn, and the transcript is never re-read: the reply stays invisible
// even across a refresh tick. That is the five-second gap the operator hit, and it is why "the short
// reply never appears" was not a repaint bug.
func TestWithoutTheTurnIDTheShortReplyStaysInvisible(t *testing.T) {
	plane := &answerAttachPlane{
		assistantID: "", // the PRE-FIX contract: the reply names no turn.
		// ... and the reply was not yet durable when the only poll that would ever run happened.
		replyVisibleAfter: 1,
	}
	m := answerAttachApp(t, plane)

	m, _, verdict := clickAskOption(t, m, "develop")
	if verdict.AssistantMessageID != "" {
		t.Fatalf("fixture: this must model the pre-fix wire contract (no turn id), got %q",
			verdict.AssistantMessageID)
	}
	next, cmds := m.Update(verdict)
	m = next.(*App)
	m = drainAttach(t, m, cmds, func(*App) bool { return false }, 600*time.Millisecond, nil)

	// AND A REFRESH TICK — the rolling window's own tick, the thing that was supposed to save it. It is a
	// cache merge plus a rail reload: it re-reads NO transcript.
	if cmd := m.refreshActiveView(); cmd != nil {
		m = drainAttach(t, m, cmd, func(*App) bool { return false }, 600*time.Millisecond, nil)
	}
	m.onChatWake()

	if frame := m.viewFrame(); strings.Contains(frame, shortReplyToken) {
		t.Fatalf("the short reply appeared WITHOUT the turn id — this scenario no longer reproduces the "+
			"operator's race, so it proves nothing about the fix:\n%s", frame)
	}
	if plane.watchCalls() != 0 {
		t.Fatalf("WatchTurnStream was dialled %d time(s) with no turn id — the pre-fix client could not",
			plane.watchCalls())
	}
	if m.chat.IsStreaming("c1") {
		t.Fatal("a slot was armed with no turn to watch")
	}
}
