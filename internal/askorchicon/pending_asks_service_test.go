package askorchicon

// pending_asks_service_test.go — the DISCOVERY surface for open asks.
//
// The operator, after losing cards in the TUI while the GUI showed them:
//
//	"you sent numerous permission card requests and user ask card requests. ALL of them
//	 reached the GUI conversation just fine, however, they did not all reach the TUI… it
//	 appeared you were stalled. So I went into the GUI and lo and behold, a permissions
//	 card was waiting for me to click on it. This is unacceptable and we need to get this
//	 fix."
//
// The state was never missing — pendingAskRegistry has always held it — but it could only
// be DELIVERED (as a live stream event, or replayed to a late watcher of that stream).
// These tests pin that it can now be ASKED FOR, which is what makes a card discoverable
// from durable state rather than dependent on having caught an event.

import (
	"context"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func TestListPendingAsksReturnsOpenAsks(t *testing.T) {
	svc := testConsentService()
	svc.pending.put("conv-1", &pendingAsk{
		AskID:          "ask-1",
		ConversationID: "conv-1",
		Tool:           "bash",
		Command:        "rm -rf build",
		state:          askOpen,
	})
	svc.pending.put("conv-1", &pendingAsk{
		AskID:          "ask-2",
		ConversationID: "conv-1",
		Question:       "which branch?",
		Options:        []string{"develop", "main"},
		state:          askOpen,
	})

	res, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{
		ConversationId: "conv-1",
	}))
	if err != nil {
		t.Fatalf("ListPendingAsks: %v", err)
	}
	asks := res.Msg.GetAsks()
	if len(asks) != 2 {
		t.Fatalf("asks = %d, want the two open asks", len(asks))
	}
	byID := map[string]*apiv1.PermissionAsk{}
	for _, a := range asks {
		byID[a.GetAskId()] = a
	}
	// A permission ask and a QUESTION ask both come back — the discovery must cover both
	// kinds, because a clarifying question parks the turn exactly the same way.
	if got := byID["ask-1"]; got == nil || got.GetTool() != "bash" || got.GetCommand() != "rm -rf build" {
		t.Fatalf("permission ask = %+v", got)
	}
	if got := byID["ask-2"]; got == nil || got.GetQuestion() != "which branch?" || len(got.GetOptions()) != 2 {
		t.Fatalf("question ask = %+v", got)
	}
}

// A DECIDED ASK IS NOT RETURNED. Returning one would resurrect a card the operator has
// already answered — the same rule the re-attach replay follows (isOpen), so discovery and
// replay cannot disagree about what counts as pending.
func TestListPendingAsksOmitsDecidedAndFinalized(t *testing.T) {
	svc := testConsentService()
	svc.pending.put("conv-1", &pendingAsk{AskID: "open", ConversationID: "conv-1", state: askOpen})
	svc.pending.put("conv-1", &pendingAsk{AskID: "decided", ConversationID: "conv-1", state: askClientReplied})
	svc.pending.put("conv-1", &pendingAsk{AskID: "final", ConversationID: "conv-1", state: askFinalized})

	res, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{
		ConversationId: "conv-1",
	}))
	if err != nil {
		t.Fatalf("ListPendingAsks: %v", err)
	}
	asks := res.Msg.GetAsks()
	if len(asks) != 1 || asks[0].GetAskId() != "open" {
		t.Fatalf("asks = %+v, want ONLY the still-open one", asks)
	}
}

// ASKS ARE SCOPED TO THEIR CONVERSATION. A card discovered for one conversation must never
// be raised in another — the shell appends a card to the conversation that asked.
func TestListPendingAsksIsScopedToTheConversation(t *testing.T) {
	svc := testConsentService()
	svc.pending.put("conv-1", &pendingAsk{AskID: "a1", ConversationID: "conv-1", state: askOpen})
	svc.pending.put("conv-2", &pendingAsk{AskID: "a2", ConversationID: "conv-2", state: askOpen})

	for _, tc := range []struct{ conv, want string }{
		{"conv-1", "a1"},
		{"conv-2", "a2"},
	} {
		res, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{
			ConversationId: tc.conv,
		}))
		if err != nil {
			t.Fatalf("ListPendingAsks(%s): %v", tc.conv, err)
		}
		asks := res.Msg.GetAsks()
		if len(asks) != 1 || asks[0].GetAskId() != tc.want {
			t.Fatalf("%s returned %+v, want only %s", tc.conv, asks, tc.want)
		}
	}

	// A conversation with nothing pending is an empty list, NOT an error — the client calls
	// this on every attach, so the common case must be cheap and quiet.
	res, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{
		ConversationId: "conv-empty",
	}))
	if err != nil {
		t.Fatalf("an empty conversation must not error: %v", err)
	}
	if len(res.Msg.GetAsks()) != 0 {
		t.Fatalf("empty conversation returned %+v", res.Msg.GetAsks())
	}
}

// AN EMPTY CONVERSATION ID IS REFUSED, never silently answered with an empty list — a
// client that forgot the id would otherwise conclude "nothing is pending" and appear to
// work while losing every card.
func TestListPendingAsksRefusesAnEmptyConversationID(t *testing.T) {
	svc := testConsentService()
	if _, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{})); err == nil {
		t.Fatal("an empty conversation_id must be refused, not answered")
	}
}

// NO TENANT IN CONTEXT IS REFUSED (the shared contract of this service's handlers).
func TestListPendingAsksRequiresATenant(t *testing.T) {
	svc := testConsentService()
	// A plain background context carries no tenant — WithID(nil, …) would panic on the
	// nil parent, which is not the contract under test.
	_, err := svc.ListPendingAsks(context.Background(), connectReq(&apiv1.ListPendingAsksRequest{ConversationId: "conv-1"}))
	if err == nil {
		t.Fatal("a missing tenant must be refused")
	}
}

// A DISCOVERED ASK AND A STREAMED ASK ARE THE SAME MESSAGE.
//
// This is the property that lets a client feed both through one path and dedupe by ask id.
// If the two ever differed by a field, a card discovered on attach would render differently
// from the same card delivered live — which is the exact class of defect this file's sibling
// has been chasing.
func TestADiscoveredAskMatchesTheStreamedAsk(t *testing.T) {
	svc := testConsentService()
	a := &pendingAsk{
		AskID:          "ask-1",
		ConversationID: "conv-1",
		SessionID:      "sess-1",
		Tool:           "bash",
		Command:        "rm -rf /tmp/x",
		Targets:        []string{"/tmp/x"},
		Directory:      "/tmp",
		InsideProject:  true,
		Summary:        "delete a directory",
		DenyBelow:      []string{"/tmp/x/keep"},
		Question:       "proceed?",
		Options:        []string{"yes", "no"},
		AllowOther:     true,
		state:          askOpen,
	}
	svc.pending.put("conv-1", a)

	res, err := svc.ListPendingAsks(tenantCtx(), connectReq(&apiv1.ListPendingAsksRequest{
		ConversationId: "conv-1",
	}))
	if err != nil {
		t.Fatalf("ListPendingAsks: %v", err)
	}
	if len(res.Msg.GetAsks()) != 1 {
		t.Fatalf("asks = %+v", res.Msg.GetAsks())
	}
	discovered := res.Msg.GetAsks()[0]

	// The stream arm's builder, over the same ask.
	streamed := permissionAskProto(a)
	if discovered.String() != streamed.String() {
		t.Fatalf("a discovered ask differs from the streamed one:\n  discovered: %s\n  streamed:   %s",
			discovered.String(), streamed.String())
	}
}
