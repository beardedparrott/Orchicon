package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// ask_parity_test.go — assertions for the Ask Orchicon read-write parity
// work item: new chat, rename/delete + rail reconcile, the ask-model picker
// (model_ref on the created conversation), mode, the kit2 Stream transcript
// (append preserves the scroll offset; tail followed only at the bottom),
// and the explicit attachment refusal.

type stubAskParity struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	convs []*apiv1.Conversation

	createdModel string
	createdMode  apiv1.ConversationMode

	renamedID    string
	renamedTitle string

	deletedID string

	modeID  string
	modeSet apiv1.ConversationMode

	compactedID     string
	compactedReason string
	compactResp     *apiv1.CompactConversationResponse

	// abortedID records the conversation a Stop was issued for (the GUI's Stop
	// button calls the same RPC).
	abortedID string

	// projectMoveFor records the conversation whose project was changed, and the target — the pair a test needs
	// to tell "the write happened" from "the rail merely reloaded".
	//
	// THIS RPC HAD NO STUB AT ALL, and that is why the move shipped broken: with no implementation, the embedded
	// Unimplemented handler answers every call, so NO test could move a conversation and the reload path could
	// not be exercised end to end. The missing fixture was the missing coverage.
	projectMoveFor  string
	projectMoveDest string
}

func (s *stubAskParity) SetConversationProject(_ context.Context, req *connect.Request[apiv1.SetConversationProjectRequest]) (*connect.Response[apiv1.SetConversationProjectResponse], error) {
	s.projectMoveFor = req.Msg.GetId()
	s.projectMoveDest = req.Msg.GetProjectId()
	// THE STUB PERSISTS THE MOVE, because ListConversations reads this same slice. A stub that only recorded the
	// call would reload the OLD project ids and every "did it leave the rail?" assertion would fail for a reason
	// that has nothing to do with the shell.
	for _, c := range s.convs {
		if c.GetId() == req.Msg.GetId() {
			c.ProjectId = req.Msg.GetProjectId()
			break
		}
	}
	return connect.NewResponse(&apiv1.SetConversationProjectResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId(), ProjectId: req.Msg.GetProjectId()},
	}), nil
}

// AbortConversationTurn is the Stop path. The server treats it as idempotent, so
// recording the id (rather than erroring on a repeat) matches real behaviour.
func (s *stubAskParity) AbortConversationTurn(_ context.Context, req *connect.Request[apiv1.AbortConversationTurnRequest]) (*connect.Response[apiv1.AbortConversationTurnResponse], error) {
	s.abortedID = req.Msg.GetConversationId()
	return connect.NewResponse(&apiv1.AbortConversationTurnResponse{}), nil
}

func (s *stubAskParity) CompactConversation(_ context.Context, req *connect.Request[apiv1.CompactConversationRequest]) (*connect.Response[apiv1.CompactConversationResponse], error) {
	s.compactedID = req.Msg.GetConversationId()
	s.compactedReason = req.Msg.GetReason()
	resp := s.compactResp
	if resp == nil {
		resp = &apiv1.CompactConversationResponse{Compacted: true, Detail: "compacted 40 messages into 1 summary + 6 recent messages"}
	}
	return connect.NewResponse(resp), nil
}

func (s *stubAskParity) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	return connect.NewResponse(&apiv1.ListConversationsResponse{Conversations: s.convs}), nil
}

func (s *stubAskParity) CreateConversation(_ context.Context, req *connect.Request[apiv1.CreateConversationRequest]) (*connect.Response[apiv1.CreateConversationResponse], error) {
	s.createdModel = req.Msg.GetModelRef()
	s.createdMode = req.Msg.GetMode()
	return connect.NewResponse(&apiv1.CreateConversationResponse{
		Conversation: &apiv1.Conversation{Id: "new1", Title: "New chat", ModelRef: s.createdModel},
	}), nil
}

func (s *stubAskParity) UpdateConversationTitle(_ context.Context, req *connect.Request[apiv1.UpdateConversationTitleRequest]) (*connect.Response[apiv1.UpdateConversationTitleResponse], error) {
	s.renamedID, s.renamedTitle = req.Msg.GetId(), req.Msg.GetTitle()
	return connect.NewResponse(&apiv1.UpdateConversationTitleResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId(), Title: req.Msg.GetTitle()},
	}), nil
}

func (s *stubAskParity) DeleteConversation(_ context.Context, req *connect.Request[apiv1.DeleteConversationRequest]) (*connect.Response[apiv1.DeleteConversationResponse], error) {
	s.deletedID = req.Msg.GetId()
	return connect.NewResponse(&apiv1.DeleteConversationResponse{}), nil
}

func (s *stubAskParity) SetConversationMode(_ context.Context, req *connect.Request[apiv1.SetConversationModeRequest]) (*connect.Response[apiv1.SetConversationModeResponse], error) {
	s.modeID, s.modeSet = req.Msg.GetId(), req.Msg.GetMode()
	return connect.NewResponse(&apiv1.SetConversationModeResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId()},
	}), nil
}

func (s *stubAskParity) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	return connect.NewResponse(&apiv1.ListMessagesResponse{}), nil
}

// newAskApp builds an App wired to a stub Ask service on a real Connect
// HTTP server (the same harness the chat controller tests use).
func newAskApp(t *testing.T) (*App, *stubAskParity) {
	t.Helper()
	stub := &stubAskParity{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.SwitchTo(TabAsk)
	return m, stub
}

// New chat: the active conversation is dropped (transcript starts empty) and
// the first send creates the conversation with the picked model + mode.
func TestNewChatClearsAndFirstSendCreatesConversation(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "old"
	m.newChat()
	if m.chatConvID != "" {
		t.Fatalf("new chat must clear the active conversation, got %q", m.chatConvID)
	}
	if m.chat.Active() != "" {
		t.Fatalf("controller must have no active conversation, got %q", m.chat.Active())
	}

	// New chat / new conversation needs no screen change: the composer's
	// first send creates it (the GUI's lazy CreateConversation).
	if handled, _ := m.dispatchSlash("/model orchicon/some-free-model"); !handled {
		t.Fatal("/model must be handled")
	}
	if handled, _ := m.dispatchSlash("/mode brainstorm"); !handled {
		t.Fatal("/mode must be handled")
	}

	msg := m.createConversationAndSend("hello", "")()
	cm, ok := msg.(chatConvCreatedMsg)
	if !ok {
		t.Fatalf("want chatConvCreatedMsg, got %T", msg)
	}
	if cm.convID != "new1" {
		t.Fatalf("created conversation id = %q", cm.convID)
	}
	// The picked model is what persists on the conversation row's model_ref.
	if stub.createdModel != "orchicon/some-free-model" {
		t.Fatalf("CreateConversation model_ref = %q (the picker must persist)", stub.createdModel)
	}
	if stub.createdMode != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Fatalf("CreateConversation mode = %v", stub.createdMode)
	}
}

// Rename + delete issue the RPCs and reconcile the rail.
func TestRenameAndDeleteRPCAndRailReconcile(t *testing.T) {
	m, stub := newAskApp(t)
	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "one"}, {ID: "c2", Title: "two"}}})
	m.chatConvID = "c1"

	handled, cmd := m.dispatchSlash("/rename renamed title")
	if !handled || cmd == nil {
		t.Fatal("/rename must dispatch a write")
	}
	rm, ok := cmd().(chat.ConversationMutatedMsg)
	if !ok || rm.Err != "" {
		t.Fatalf("rename result = %#v", rm)
	}
	if stub.renamedID != "c1" || stub.renamedTitle != "renamed title" {
		t.Fatalf("UpdateConversationTitle got (%q, %q)", stub.renamedID, stub.renamedTitle)
	}
	if rec := m.onConversationMutated(rm); rec == nil {
		t.Fatal("rename must reconcile the conversations rail")
	}

	handled, cmd = m.dispatchSlash("/delete")
	if !handled || cmd == nil {
		t.Fatal("/delete must dispatch a write")
	}
	dm, ok := cmd().(chat.ConversationMutatedMsg)
	if !ok || dm.Err != "" {
		t.Fatalf("delete result = %#v", dm)
	}
	if stub.deletedID != "c1" {
		t.Fatalf("DeleteConversation got %q", stub.deletedID)
	}
	// Rail reconcile: the deleted row drops locally and the rail refetches.
	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1"}, {ID: "c2"}}})
	m.chatConvID = "c1"
	if rec := m.onConversationMutated(dm); rec == nil {
		t.Fatal("delete must reconcile the conversations rail")
	}
	if len(m.conversations) != 1 || m.conversations[0].ID != "c2" {
		t.Fatalf("rail not reconciled: %+v", m.conversations)
	}
	if m.chatConvID != "" {
		t.Fatalf("the deleted conversation must stop being active, got %q", m.chatConvID)
	}
}

// /compact issues CompactConversation with reason "manual" for the open
// conversation, and surfaces the server's own detail line (including WHY it
// declined) rather than inventing one.
func TestCompactCommandIssuesManualRPC(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	stub.compactResp = &apiv1.CompactConversationResponse{
		Compacted:           true,
		Detail:              "compacted 40 messages into 1 summary + 6 recent messages",
		ContextTokensBefore: 1_016_584,
		ContextTokensAfter:  61_200,
	}

	handled, cmd := m.dispatchSlash("/compact")
	if !handled {
		t.Fatal("/compact must be handled, not sent as a chat message")
	}
	if cmd == nil {
		t.Fatal("/compact must issue a write when a conversation is open")
	}
	msg, ok := cmd().(chat.ConversationMutatedMsg)
	if !ok {
		t.Fatalf("cmd produced %T, want chat.ConversationMutatedMsg", cmd())
	}
	if msg.Op != "compact" {
		t.Errorf("op = %q, want compact", msg.Op)
	}
	if msg.Err != "" {
		t.Fatalf("unexpected error: %s", msg.Err)
	}
	if stub.compactedID != "c1" {
		t.Errorf("compacted conversation = %q, want c1", stub.compactedID)
	}
	// A hand-issued compaction is "manual" — the server records the reason in
	// the audit trail, so a wrong reason would misreport who compacted what.
	if stub.compactedReason != "manual" {
		t.Errorf("reason = %q, want manual", stub.compactedReason)
	}
	// The MEASURED sizes are surfaced (they are never estimates).
	if !strings.Contains(msg.Detail, "1,016,584") && !strings.Contains(msg.Detail, "1016584") {
		t.Errorf("detail must carry the measured before/after sizes, got %q", msg.Detail)
	}
	if !strings.Contains(msg.Detail, "compacted 40 messages") {
		t.Errorf("detail must carry the server's own explanation, got %q", msg.Detail)
	}
}

// A DECLINED compaction is not an error: the server's reason must reach the
// operator verbatim, because "nothing to compact" and "I compacted it" look
// identical otherwise.
func TestCompactCommandReportsDecline(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	stub.compactResp = &apiv1.CompactConversationResponse{
		Compacted: false,
		Detail:    "nothing to compact — only 4 messages so far",
	}
	handled, cmd := m.dispatchSlash("/compact")
	if !handled || cmd == nil {
		t.Fatal("/compact must dispatch")
	}
	msg := cmd().(chat.ConversationMutatedMsg)
	if msg.Err != "" {
		t.Fatalf("a decline is not an error: %s", msg.Err)
	}
	if !strings.Contains(msg.Detail, "only 4 messages") {
		t.Errorf("the decline reason must be surfaced verbatim, got %q", msg.Detail)
	}
}

// NOTE: the mid-turn refusal is asserted in the chat package
// (TestCanCompactRefusesWhileStreaming), where a live turn slot can be created
// through the controller's own send path. From this package the slot's state is
// not settable, and testing it here would mean asserting the guard's absence
// rather than its behaviour.

// Commands that need an open conversation refuse loudly instead of guessing.
func TestConversationCommandsRefuseWithoutOpenConversation(t *testing.T) {
	m, _ := newAskApp(t)
	for _, c := range []string{"/rename x", "/delete", "/compact"} {
		handled, cmd := m.dispatchSlash(c)
		if !handled || cmd != nil {
			t.Fatalf("%s without an open conversation must refuse (got handled=%v cmd=%v)", c, handled, cmd)
		}
		if !strings.Contains(m.dock.View(), "no conversation open") {
			t.Fatalf("%s refusal not surfaced: %q", c, m.dock.View())
		}
	}
}

// Mode: SetConversationMode is issued for the open conversation.
func TestModeSetRPC(t *testing.T) {
	m, stub := newAskApp(t)
	m.chatConvID = "c1"
	handled, cmd := m.dispatchSlash("/mode brainstorm")
	if !handled || cmd == nil {
		t.Fatal("/mode must dispatch a write when a conversation is open")
	}
	mm, ok := cmd().(chat.ConversationMutatedMsg)
	if !ok || mm.Err != "" {
		t.Fatalf("mode result = %#v", mm)
	}
	if stub.modeID != "c1" || stub.modeSet != apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM {
		t.Fatalf("SetConversationMode got (%q, %v)", stub.modeID, stub.modeSet)
	}
	if handled, _ := m.dispatchSlash("/mode warp"); !handled {
		t.Fatal("unknown mode must be handled (refused, not sent)")
	}
	if !strings.Contains(m.dock.View(), "unknown mode") {
		t.Fatalf("unknown mode refusal not surfaced: %q", m.dock.View())
	}
}

// Attachments: an attachment input is explicitly refused (no silent drop).
func TestAttachmentInputIsExplicitlyRefused(t *testing.T) {
	m, _ := newAskApp(t)
	handled, cmd := m.dispatchSlash("/attach /tmp/report.pdf")
	if !handled {
		t.Fatal("/attach must be handled — an attachment path is never silently dropped")
	}
	if cmd != nil {
		t.Fatal("the refusal must not produce a send")
	}
	if !strings.Contains(m.dock.View(), "not supported") {
		t.Fatalf("refusal not user-visible: %q", m.dock.View())
	}
}

// The live transcript renders through the kit2 Stream widget: streaming
// chunks append, the operator's scroll offset survives the append, and the
// tail is followed only when the view was already at the bottom.
func TestTranscriptStreamAppendPreservesOffsetAndFollowsAtBottom(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	str := m.transcriptStream("c1", 60, 6)
	if m.TranscriptStream("c1") == nil {
		t.Fatal("the transcript stream must be registered for the conversation")
	}

	items := make([]chat.ChatItem, 0, 12)
	for i := 0; i < 10; i++ {
		items = append(items, chat.ChatItem{Kind: chat.KindUser, Text: fmt.Sprintf("m%d", i), Key: fmt.Sprintf("u%d", i)})
	}
	m.syncTranscript("c1", str, items, 60)
	if len(str.Lines) < 7 {
		t.Fatalf("expected scrollback, got %d lines", len(str.Lines))
	}
	if !str.AtBottom() {
		t.Fatal("a fresh render must be pinned to the tail")
	}

	// Operator scrolls back: a following live chunk must NOT yank the offset.
	str.Wheel(-3)
	off := str.Offset
	if str.AtBottom() {
		t.Fatal("expected the view to be scrolled off the bottom")
	}
	m.syncTranscript("c1", str, append(append([]chat.ChatItem{}, items...), chat.ChatItem{Kind: chat.KindText, Text: "live", Key: "l1"}), 60)
	if str.Offset != off {
		t.Fatalf("append yanked the operator's scroll offset %d -> %d", off, str.Offset)
	}

	// At the bottom: a new chunk IS followed.
	str.ScrollToBottom()
	m.syncTranscript("c1", str, append(append([]chat.ChatItem{}, items...),
		chat.ChatItem{Kind: chat.KindText, Text: "live", Key: "l1"},
		chat.ChatItem{Kind: chat.KindText, Text: "tail", Key: "l2"}), 60)
	if !str.AtBottom() {
		t.Fatal("the tail must be followed when the view was already at the bottom")
	}
	if got := str.Lines[len(str.Lines)-1]; !strings.Contains(got, "tail") {
		t.Fatalf("tail line = %q", got)
	}
}

// Vertical scroll keys on the Ask tab drive the transcript stream.
func TestScrollActiveDetailScrollsTranscript(t *testing.T) {
	m, _ := newAskApp(t)
	m.chatConvID = "c1"
	str := m.transcriptStream("c1", 60, 4)
	var items []chat.ChatItem
	for i := 0; i < 10; i++ {
		items = append(items, chat.ChatItem{Kind: chat.KindUser, Text: fmt.Sprintf("m%d", i), Key: fmt.Sprintf("u%d", i)})
	}
	m.syncTranscript("c1", str, items, 60)
	str.ScrollToBottom()
	m.scrollActiveDetail(-3)
	if str.AtBottom() {
		t.Fatal("scroll keys must move the transcript viewport")
	}
}

// A USER MESSAGE APPEARS THE MOMENT IT IS SENT — driven through the REAL path.
//
// The operator: "User messages don't appear until AFTER the model responds."
//
// THE CAUSE was a click-vs-keyboard asymmetry, the same shape as the Enter bug on this rail. The rail's
// CLICK path calls RequestDetail, which writes the screen's detail id as a side effect; the KEYBOARD
// path (OpenAskConversation) did not. The shell's chat repaint is guarded on that id, so with it empty
// the repaint was a NO-OP — the transcript stream was never even created, and the operator's text stayed
// invisible until something incidental painted the pane.
//
// This test drives OpenAskConversation and then the composer's send, and asserts the transcript exists
// and carries the operator's text. It deliberately does NOT call syncTranscript directly: the two
// existing tests in this file do, which is why neither could see a bug in the guard that decides whether
// to call it at all.
func TestUserMessageIsVisibleImmediatelyOnOpenConversation(t *testing.T) {
	m, _ := newAskApp(t)
	// The REAL Ask screen: the repaint needs RenderTranscript, and the guard needs DetailID.
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true

	m.OpenAskConversation("c1")

	if dr, ok := m.screens[TabAsk].(interface{ DetailID() string }); ok {
		if dr.DetailID() != "c1" {
			t.Fatalf("opening a conversation must declare the pane's content: DetailID = %q, want c1 — "+
				"the shell's chat repaint is guarded on it, so an empty id means NO repaint at all",
				dr.DetailID())
		}
	} else {
		t.Fatal("the Ask screen must expose DetailID")
	}

	m.sendFromComposer("hello there")

	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("the transcript stream was never created — the send produced no repaint, so the " +
			"operator's own message cannot render until something else paints the pane")
	}
	if joined := strings.Join(str.Lines, "\n"); !strings.Contains(joined, "hello there") {
		t.Errorf("the operator's message is not in the transcript: %q", joined)
	}
	// And nothing has replied yet, so the GUI's thinking indicator should be showing.
	if str.Notice != "Orchicon is thinking…" {
		t.Errorf("transcript notice = %q, want %q — the GUI shows it until the first content arrives",
			str.Notice, "Orchicon is thinking…")
	}
}

// THE THINKING INDICATOR CLEARS when the model's prose arrives, and does not appear when there is
// nothing to wait for. The decision is a pure function so both halves are testable without driving the
// controller's stream.
func TestAwaitingReplyTracksTheModelFirstContent(t *testing.T) {
	user := chat.ChatItem{Kind: chat.KindUser, Text: "hi", Key: "draft-1"}
	reply := chat.ChatItem{Kind: chat.KindText, Text: "hello"}
	tool := chat.ChatItem{Kind: chat.KindTool, Text: "read_file"}

	if awaitingReply(nil) {
		t.Error("nothing was sent, so there is no reply to wait for")
	}
	if !awaitingReply([]chat.ChatItem{user}) {
		t.Error("a sent message with no content yet IS a pending reply")
	}
	if waiting := awaitingReply([]chat.ChatItem{user, tool}); !waiting {
		t.Error("a tool row is not the model's answer — the indicator must stay up")
	}
	if awaitingReply([]chat.ChatItem{user, reply}) {
		t.Error("the model has spoken, so the indicator must clear")
	}
	// A FOLLOW-UP in a long conversation still waits: the question is the LAST user item, not the
	// first, so an earlier reply must not suppress the indicator.
	followUp := chat.ChatItem{Kind: chat.KindUser, Text: "and again", Key: "draft-2"}
	if !awaitingReply([]chat.ChatItem{user, reply, followUp}) {
		t.Error("a follow-up after an earlier reply IS a pending reply")
	}
}
