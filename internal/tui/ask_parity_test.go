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

// Commands that need an open conversation refuse loudly instead of guessing.
func TestConversationCommandsRefuseWithoutOpenConversation(t *testing.T) {
	m, _ := newAskApp(t)
	for _, c := range []string{"/rename x", "/delete"} {
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
