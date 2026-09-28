package tui

// consent_never_muted_test.go — a card the SERVER raised is always drawn, and a session
// grant is only recorded once the server APPLIED it.
//
// The operator, watching both clients at once: "the permission ask card pops up in the
// GUI but it doesn't pop up in the TUI. It just sits at 'orchicon is thinking'."
//
// The shell had a second, unreliable authority beside the server's grant store: a local
// directory map it consulted before drawing a card. Because the server only raises an ask
// it has no consent for, muting that card never removed a question — it removed the ANSWER,
// and the turn parked on the server with nothing on screen to act on.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestACardIsDrawnEvenWhenThisClientBelievesTheDirectoryIsGranted is the reported symptom.
// The local mirror says /home/ops/project was granted for the session; the server is asking
// about it anyway (its grants are in memory and every plane restart drops them — the prod
// plane restarted 14 times in the two days this was measured). The card must be DRAWN: the
// ask is the server's, and only it knows whether consent is missing.
func TestACardIsDrawnEvenWhenThisClientBelievesTheDirectoryIsGranted(t *testing.T) {
	m, _ := consentApp(t)
	// This client answered an earlier ask for the same directory with "allow for the session".
	m.sessionGrants.grant("c1", "/home/ops/project", "write")

	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-2", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go test ./...", Directory: "/home/ops/project",
	})

	items := m.chatStore.snapshot("c1")
	if len(items) != 1 || items[0].AskID != "ask-2" {
		t.Fatalf("the TUI swallowed a card the server raised — the operator would see the GUI ask and this client sit at 'orchicon is thinking': %+v", items)
	}
	if st := m.chatStore.consentState("c1", "ask-2"); st == nil || !st.Pending() {
		t.Fatal("the drawn card must be pending, so it can be answered from here")
	}
}

// The same ask must not be drawn twice. This is not hypothetical: a dropped socket re-dials
// through WatchTurnStream, whose replay re-emits every still-open ask, so the card the
// operator already has would otherwise appear a second time.
func TestTheSameAskIsDrawnOnce(t *testing.T) {
	m, _ := consentApp(t)
	ask := chat.PermissionAsk{
		ID: "ask-3", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go build ./...", Directory: "/home/ops/project",
	}
	m.ShowConsentAsk(ask)
	m.ShowConsentAsk(ask) // the re-attach replay

	items := m.chatStore.snapshot("c1")
	if len(items) != 1 {
		t.Fatalf("a replayed ask was drawn again: %d card(s)", len(items))
	}
}

// A REFUSED allow-session must not be recorded as a grant. The server answers an ask that
// expired (or was answered elsewhere) with applied=false / expired=true and stores nothing —
// so a client that recorded the grant anyway believed in consent the server never gave, and
// the mirror then muted every later card for that directory while the GUI kept asking.
func TestARefusedAllowSessionIsNotRecordedAsAGrant(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-4", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go test ./...", Directory: "/home/ops/project",
	})
	m.ConsentResolve("ask-4", chat.DecisionAllowSession, "")

	// The verdict: the ask was no longer open, so nothing was applied.
	if dir := m.settleConsentScope("c1", "ask-4", ""); dir != "" {
		t.Fatalf("a refused decision reported a confirmed grant for %q", dir)
	}
	if grants := m.sessionGrants.list("c1"); len(grants) != 0 {
		t.Fatalf("a REFUSED allow-session was recorded as a session grant: %+v", grants)
	}
	if st := m.chatStore.consentState("c1", "ask-4"); st == nil || st.Note != "session · not applied" {
		t.Fatalf("the card must stop claiming a scope it never got, got note %q", noteOf(m, "ask-4"))
	}
}

// An APPLIED allow-session is the one case that records it, and it scopes the card record.
func TestAnAppliedAllowSessionRecordsTheGrant(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-5", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go test ./...", Directory: "/home/ops/project",
	})
	m.ConsentResolve("ask-5", chat.DecisionAllowSession, "")

	if dir := m.settleConsentScope("c1", "ask-5", "session · "); dir != "/home/ops/project" {
		t.Fatalf("the applied decision did not confirm a grant for the ask's directory, got %q", dir)
	}
	grants := m.sessionGrants.list("c1")
	if len(grants) != 1 || grants[0].Directory != "/home/ops/project" {
		t.Fatalf("an APPLIED allow-session must be recorded: %+v", grants)
	}
	if st := m.chatStore.consentState("c1", "ask-5"); st == nil || st.Note != "session · /home/ops/project" {
		t.Fatalf("the record must name the scope the server granted, got %q", noteOf(m, "ask-5"))
	}
}

// The verdict must reach the ONE card that asked it, and must not invent a grant for a
// decision that was never an allow-session (an allow-once or a denial grants nothing).
func TestOnlyAnAllowSessionDecisionCanConfirmAGrant(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-6", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go test ./...", Directory: "/home/ops/project",
	})
	m.ConsentResolve("ask-6", chat.DecisionAllowOnce, "")
	if dir := m.settleConsentScope("c1", "ask-6", "session · "); dir != "" {
		t.Fatalf("an allow-once confirmed a session grant for %q", dir)
	}
	if grants := m.sessionGrants.list("c1"); len(grants) != 0 {
		t.Fatalf("an allow-once recorded a session grant: %+v", grants)
	}
}

// The verdict for an ask this client never held a card for is a no-op, not a panic.
func TestVerdictForAnUnknownAskIsInert(t *testing.T) {
	m, _ := consentApp(t)
	if dir := m.settleConsentScope("c1", "ask-nope", "session · "); dir != "" {
		t.Fatalf("an unknown ask confirmed a grant for %q", dir)
	}
	if grants := m.sessionGrants.list("c1"); len(grants) != 0 {
		t.Fatalf("an unknown ask recorded a grant: %+v", grants)
	}
}

// stubAsk answers the one RPC under test and leaves every other method of the
// generated client to the embedded nil interface (which panics if it is ever called —
// so an unexpected call is a loud failure, not a silent pass).
type stubAsk struct {
	apiv1connect.AskOrchiconServiceClient
	reply func(context.Context, *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error)
}

func (s stubAsk) ReplyPermissionAsk(ctx context.Context, req *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error) {
	return s.reply(ctx, req)
}

// The reply RPC must name its ask — in the REQUEST, so the server answers the right one,
// and in the VERDICT, so the shell can settle the right card and record a grant only for a
// decision the server actually applied.
func TestReplyPermissionAskCarriesItsAskID(t *testing.T) {
	m, _ := consentApp(t)
	var sent string
	var gotChoice apiv1.PermissionChoice
	m.clients.Ask = stubAsk{reply: func(_ context.Context, req *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error) {
		sent = req.Msg.GetAskId()
		gotChoice = req.Msg.GetChoice()
		return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{Applied: true, Detail: "ok"}), nil
	}}

	cmd := m.chat.ReplyPermissionAsk("c1", "ask-7", apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION, "")
	if cmd == nil {
		t.Fatal("ReplyPermissionAsk returned no command")
	}
	msg, ok := cmd().(chat.ConsentRepliedMsg)
	if !ok {
		t.Fatalf("ReplyPermissionAsk produced %T, want chat.ConsentRepliedMsg", cmd())
	}
	if sent != "ask-7" || gotChoice != apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION {
		t.Fatalf("the request carried ask %q / choice %v, want ask-7 / ALLOW_SESSION", sent, gotChoice)
	}
	if msg.AskID != "ask-7" {
		t.Fatalf("the verdict carries ask id %q, want ask-7 — without it the grant cannot be attributed", msg.AskID)
	}
	if !msg.Applied {
		t.Fatal("the server applied the decision, so the verdict must say so")
	}

	// The verdict for THIS ask must not touch a grant belonging to another card.
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-7", ConvID: "c1", Kind: chat.AskTool, Tool: "bash",
		Target: "go test ./...", Directory: "/home/ops/project",
	})
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-8", ConvID: "c1", Kind: chat.AskTool, Tool: "write",
		Target: "/elsewhere/x.go", Directory: "/elsewhere",
	})
	m.ConsentResolve("ask-7", chat.DecisionAllowSession, "")
	if dir := m.settleConsentScope("c1", msg.AskID, "session · "); dir != "/home/ops/project" {
		t.Fatalf("the verdict settled the wrong card: %q", dir)
	}
	if st := m.chatStore.consentState("c1", "ask-8"); st == nil || st.Note != "" {
		t.Fatalf("an unrelated card was scoped by someone else's verdict: %q", noteOf(m, "ask-8"))
	}
	if st := m.chatStore.consentState("c1", "ask-8"); st != nil && !st.Pending() {
		t.Fatal("an unrelated card was settled by someone else's verdict")
	}
}

func noteOf(m *App, askID string) string {
	st := m.chatStore.consentState("c1", askID)
	if st == nil {
		return "<no card>"
	}
	return st.Note
}
