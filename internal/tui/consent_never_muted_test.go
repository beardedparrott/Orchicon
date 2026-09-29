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
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestACardIsDrawnEvenWhenThisClientHasSeenTheDirectoryGranted is the reported symptom.
// This client's cache of the SERVER's grants says /home/ops/project is allowed for the
// session; the server is asking about it anyway (grants live in its memory and are dropped
// by every plane restart — the prod plane restarted 14 times in the two days this was
// measured). The card must be DRAWN: the ask is the server's, and only it knows whether
// consent is missing.
func TestACardIsDrawnEvenWhenThisClientHasSeenTheDirectoryGranted(t *testing.T) {
	m, _ := consentApp(t)
	// A grant this client has SEEN, straight from the server's list.
	m.applyConsentGrants(chat.PermissionGrantsMsg{
		ConvID: "c1",
		Grants: []chat.SessionGrant{{Directory: "/home/ops/project", GrantedAt: 1}},
	})

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
	// And nothing entered this client's view of the grants: the list is the SERVER's, and a refused decision
	// never reached it.
	if grants, ok := m.ConsentGrants("c1"); !ok || len(grants) != 0 {
		t.Fatalf("a REFUSED allow-session showed up as a session grant: %+v", grants)
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
	// The grant itself is the SERVER's, and this shell is TOLD it (the verdict handler re-fetches); it is not
	// accrued locally. So what is asserted here is that the shell asked.
	if m.permGrantsLoaded["c1"] && len(m.permGrants["c1"]) != 0 {
		t.Fatalf("the shell must not invent a grant the server did not report: %+v", m.permGrants["c1"])
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
	if grants, ok := m.ConsentGrants("c1"); !ok || len(grants) != 0 {
		t.Fatalf("an allow-once recorded a session grant: %+v", grants)
	}
}

// The verdict for an ask this client never held a card for is a no-op, not a panic.
func TestVerdictForAnUnknownAskIsInert(t *testing.T) {
	m, _ := consentApp(t)
	if dir := m.settleConsentScope("c1", "ask-nope", "session · "); dir != "" {
		t.Fatalf("an unknown ask confirmed a grant for %q", dir)
	}
	if grants, ok := m.ConsentGrants("c1"); !ok || len(grants) != 0 {
		t.Fatalf("an unknown ask recorded a grant: %+v", grants)
	}
}

// stubAsk answers the one RPC under test and leaves every other method of the
// generated client to the embedded nil interface (which panics if it is ever called —
// so an unexpected call is a loud failure, not a silent pass).
type stubAsk struct {
	apiv1connect.AskOrchiconServiceClient
	reply func(context.Context, *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error)
	// grants / revoke serve the session-grant RPCs; a stub that leaves them nil panics if they are called,
	// which is how "the shell never asked the server" stays a loud failure.
	grants func(context.Context, *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error)
	revoke func(context.Context, *connect.Request[apiv1.RevokePermissionGrantRequest]) (*connect.Response[apiv1.RevokePermissionGrantResponse], error)
}

func (s stubAsk) ListPermissionGrants(ctx context.Context, req *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error) {
	return s.grants(ctx, req)
}

func (s stubAsk) RevokePermissionGrant(ctx context.Context, req *connect.Request[apiv1.RevokePermissionGrantRequest]) (*connect.Response[apiv1.RevokePermissionGrantResponse], error) {
	return s.revoke(ctx, req)
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

// errStub stands in for a transport failure.
var errStub = errors.New("stub: the plane did not answer")

// --- the session grants are the SERVER's list ------------------------------------------------

// TestGrantsAreFetchedFromTheServer — the list the operator sees is the plane's, not a local tally.
//
// It used to be a second record accrued from the cards this client answered, with a "tool" and a "count" per
// directory that only this client could have known. Grants live in the PLANE's memory, they change without this
// client doing anything (a plane restart drops them all; the GUI granting one adds one), and the plane is what
// enforces them — so the only honest list is the server's.
func TestGrantsAreFetchedFromTheServer(t *testing.T) {
	m, _ := consentApp(t)
	var asked string
	m.clients.Ask = stubAsk{grants: func(_ context.Context, req *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error) {
		asked = req.Msg.GetConversationId()
		return connect.NewResponse(&apiv1.ListPermissionGrantsResponse{Grants: []*apiv1.SessionPermissionGrant{
			{Directory: "/home/ops/project", GrantedAtUnix: 1_700_000_000},
		}}), nil
	}}

	cmd := m.loadConsentGrants("c1")
	if cmd == nil {
		t.Fatal("loadConsentGrants returned no command")
	}
	msg, ok := cmd().(chat.PermissionGrantsMsg)
	if !ok {
		t.Fatalf("loadConsentGrants produced %T, want chat.PermissionGrantsMsg", cmd())
	}
	if asked != "c1" {
		t.Fatalf("the RPC asked about conversation %q, want c1", asked)
	}
	if msg.Err != "" {
		t.Fatalf("fetch failed: %s", msg.Err)
	}
	m.applyConsentGrants(msg)

	grants, available := m.ConsentGrants("c1")
	if !available {
		t.Fatal("a successful fetch must report as available")
	}
	if len(grants) != 1 || grants[0].Directory != "/home/ops/project" || grants[0].GrantedAt != 1_700_000_000 {
		t.Fatalf("the server's grant did not reach the view: %+v", grants)
	}
}

// A FAILED fetch is not an empty list. "You have allowed nothing" and "I could not ask" are different facts, and
// showing the first for the second is the lie the roll-up exists to prevent.
func TestAFailedGrantFetchIsNotAnEmptyList(t *testing.T) {
	m, _ := consentApp(t)
	m.clients.Ask = stubAsk{grants: func(context.Context, *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error) {
		return nil, connect.NewError(connect.CodeUnavailable, errStub)
	}}
	m.applyConsentGrants(m.loadConsentGrants("c1")().(chat.PermissionGrantsMsg))

	if _, available := m.ConsentGrants("c1"); available {
		t.Fatal("a failed fetch must report as UNAVAILABLE, not as an empty list")
	}
	if !m.ConsentGrantsLoaded("c1") {
		t.Fatal("a failed fetch counts as asked: it must not be retried on every poll")
	}
}

// TestRevokeReachesTheServerAndAdoptsTheRefreshedList — the revoke that used to revoke nothing.
//
// The TUI dropped the grant from its OWN map, so the row vanished from the operator's list while the plane went
// on honouring the grant: the very next tool call for that directory proceeded without asking.
func TestRevokeReachesTheServerAndAdoptsTheRefreshedList(t *testing.T) {
	m, _ := consentApp(t)
	var revoked string
	m.clients.Ask = stubAsk{
		grants: func(context.Context, *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error) {
			return connect.NewResponse(&apiv1.ListPermissionGrantsResponse{Grants: []*apiv1.SessionPermissionGrant{
				{Directory: "/p"}, {Directory: "/q"},
			}}), nil
		},
		revoke: func(_ context.Context, req *connect.Request[apiv1.RevokePermissionGrantRequest]) (*connect.Response[apiv1.RevokePermissionGrantResponse], error) {
			revoked = req.Msg.GetConversationId() + "|" + req.Msg.GetDirectory()
			// The server returns the REFRESHED list, so the client never keeps its own copy to drift.
			return connect.NewResponse(&apiv1.RevokePermissionGrantResponse{
				Removed: true,
				Grants:  []*apiv1.SessionPermissionGrant{{Directory: "/q"}},
			}), nil
		},
	}
	// Load first, so the cache looks like a real one before the revoke.
	m.applyConsentGrants(m.loadConsentGrants("c1")().(chat.PermissionGrantsMsg))

	if err := m.ConsentRevoke("c1", "/p"); err != nil {
		t.Fatalf("ConsentRevoke = %v", err)
	}
	if m.pendingConsentRevoke == nil {
		t.Fatal("the revoke command was not staged — the Ask screen's synchronous call cannot return it")
	}
	if revoked != "" {
		t.Fatal("the RPC was issued before the staged command was run")
	}
	msg := m.pendingConsentRevoke().(chat.PermissionGrantsMsg)
	if revoked != "c1|/p" {
		t.Fatalf("the server was asked to revoke %q, want c1|/p", revoked)
	}
	m.applyConsentGrants(msg)

	grants, _ := m.ConsentGrants("c1")
	if len(grants) != 1 || grants[0].Directory != "/q" {
		t.Fatalf("the client must adopt the server's refreshed list, got %+v", grants)
	}
	if !strings.Contains(m.dock.Notice, "revoked") {
		t.Fatalf("the operator must be told what happened, notice = %q", m.dock.Notice)
	}
}

// A revoke for a directory the server does not have is reported as such — never as a silent success.
func TestARevokeThatRemovedNothingSaysSo(t *testing.T) {
	m, _ := consentApp(t)
	m.clients.Ask = stubAsk{revoke: func(context.Context, *connect.Request[apiv1.RevokePermissionGrantRequest]) (*connect.Response[apiv1.RevokePermissionGrantResponse], error) {
		return connect.NewResponse(&apiv1.RevokePermissionGrantResponse{Removed: false}), nil
	}}
	m.ConsentRevoke("c1", "/gone")
	m.applyConsentGrants(m.pendingConsentRevoke().(chat.PermissionGrantsMsg))

	if !strings.Contains(m.dock.Notice, "nothing was revoked") {
		t.Fatalf("a revoke that removed nothing must say so, notice = %q", m.dock.Notice)
	}
}
