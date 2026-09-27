package askorchicon

// fullsend_test.go — FULLSEND at the CONSENT layer.
//
// The mode has two enforcement points (this decision path and the host-suite guard shim
// a bash subprocess runs under; the shim's half is pinned in guard_fullsend_test.go).
// What is pinned HERE is the boundary of the waiver at the point the operator experiences
// it: which actions stop raising a card, and which keep refusing.
//
// The three properties these tests exist for:
//
//  1. A write that would have raised a card proceeds instead, and does so SILENTLY — the
//     point of the mode is not to ask, so an implementation that proceeded but still
//     emitted a card would be the worst of both.
//  2. A DENY entry still refuses, for the reason the whole mode is safe: a deny is a
//     policy DECISION, not a permission request, so there is no prompt to waive.
//  3. The never-allow binary class still refuses, because it is refused before any
//     permission decision is reached.

import (
	"context"
	"testing"
)

// TestFullsendStoreIsPerConversationAndFailsClosed pins the store's contract, including
// the two directions that must never be permissive.
func TestFullsendStoreIsPerConversationAndFailsClosed(t *testing.T) {
	// A NIL STORE IS OFF. Service is constructed without one in several tests and in any
	// pre-wiring path, and the failure mode of getting this wrong is that an
	// uninitialised store approves everything.
	var nilStore *fullsendStore
	if nilStore.Enabled("c1") {
		t.Fatal("a nil store reported fullsend ON")
	}
	nilStore.Set("c1", true) // must not panic

	st := newFullsendStore()
	if st.Enabled("c1") {
		t.Fatal("a fresh store reported fullsend ON — fullsend must never be the default")
	}
	st.Set("c1", true)
	if !st.Enabled("c1") {
		t.Fatal("Set(true) did not take")
	}
	// PER CONVERSATION, not per plane: arming one chat must not arm the next.
	if st.Enabled("c2") {
		t.Fatal("fullsend leaked to another conversation")
	}
	st.Set("c1", false)
	if st.Enabled("c1") {
		t.Fatal("Set(false) did not turn the mode off — turning it off must be immediate")
	}
	st.Set("c1", true)
	st.ClearConversation("c1")
	if st.Enabled("c1") {
		t.Fatal("ClearConversation left the mode on")
	}
	// An EMPTY id is a no-op rather than a wildcard: a caller that lost the conversation
	// id must not arm every conversation.
	st.Set("", true)
	if st.Enabled("") || st.Enabled("c9") {
		t.Fatal("an empty conversation id armed something")
	}
}

// TestFullsendProceedsWithoutACard is the mode working: the action an operator would have
// been asked about runs, and NO card is raised for it.
func TestFullsendProceedsWithoutACard(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()

	// The CONTROL, first: with fullsend off this exact action asks. Without this the test
	// would pass on a fixture that never asked in the first place.
	off := newTestConsentTurn(svc, "/p/proj", true, nil)
	if _, ask, _ := off.decide(context.Background(), "ses_1", fileAskEvent("per_off", "write", "/p/sibling/x.md")); ask == nil {
		t.Fatal("control: the action did not ask with fullsend off, so this test proves nothing")
	}

	svc.fullsend.Set("conv-1", true)
	on := newTestConsentTurn(svc, "/p/proj", true, nil)
	resp, ask, refusal := on.decide(context.Background(), "ses_1", fileAskEvent("per_on", "write", "/p/sibling/x.md"))
	if ask != nil {
		t.Fatalf("fullsend raised a card (%+v) — the point of the mode is not to ask", ask)
	}
	if resp != "once" || refusal != "" {
		t.Fatalf("fullsend: resp=%q refusal=%q — want a silent proceed", resp, refusal)
	}
	// And it is PER CONVERSATION at the DECISION too, not only in the store: another
	// conversation of the same service still asks.
	other := newConsentTurn(svc, "conv-2", "tenant-1", nil, nil)
	other.scope = AskFileScope{Dir: "/p/proj", FromConversation: true}
	other.scopeOnce.Do(func() {})
	if _, ask2, _ := other.decide(context.Background(), "ses_2", fileAskEvent("per_other", "write", "/p/sibling/y.md")); ask2 == nil {
		t.Fatal("fullsend leaked to another conversation's decision")
	}
}

// TestFullsendCoversAnUnresolvedAction — an ask whose detail could not be extracted is
// still an ASK, so it is still waived. The operator said "stop asking me", not "ask me
// only about the things you can describe".
func TestFullsendCoversAnUnresolvedAction(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	svc.fullsend.Set("conv-1", true)
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	// No target and no command: an unresolvable action, which the card path raises
	// BECAUSE it cannot vouch for it.
	evt := permissionEvent("per_unresolved", map[string]any{"patterns": []any{"*"}, "metadata": map[string]any{}})
	resp, ask, refusal := ct.decide(context.Background(), "ses_1", evt)
	if ask != nil {
		t.Fatalf("fullsend raised a card for an unresolved action: %+v", ask)
	}
	if resp != "once" || refusal != "" {
		t.Fatalf("fullsend on an unresolved action: resp=%q refusal=%q", resp, refusal)
	}
}

// TestFullsendStillRefusesADenyEntry is THE safety property at this layer.
//
// If this fails, FULLSEND opens the operator's own exclusions — the presets included —
// and the mode means something other than what it is called. It must fail with a REASON,
// because that reason is the model's only route to a workaround.
func TestFullsendStillRefusesADenyEntry(t *testing.T) {
	isolatedPolicy(t, "deny:\n  - \"/p/denied/**\"\naccept: []\n")
	svc := testConsentService()
	svc.fullsend.Set("conv-1", true)
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	resp, ask, refusal := ct.decide(context.Background(), "ses_1", fileAskEvent("per_1", "write", "/p/denied/x.md"))
	if resp != "reject" {
		t.Fatalf("FULLSEND OVERRODE A DENY ENTRY: resp=%q — a deny is a policy decision, not a prompt", resp)
	}
	if ask != nil {
		t.Fatalf("a denied target must not raise a card even under fullsend: %+v", ask)
	}
	if refusal == "" {
		t.Fatal("a denied refusal must carry the reason — it is the model's only route to a workaround")
	}
}

// TestFullsendCannotReachTheNeverAllowClass — sudo / dd / mkfs* are refused at the TOP of
// decide, before the policy is consulted and long before the fullsend check, so there is
// no permission for the mode to waive.
func TestFullsendCannotReachTheNeverAllowClass(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	svc.fullsend.Set("conv-1", true)
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	resp, ask, refusal := ct.decide(context.Background(), "ses_1", bashAskEvent("per_1", "sudo rm -rf /"))
	if resp != "reject" || ask != nil || refusal == "" {
		t.Fatalf("FULLSEND REACHED THE NEVER-ALLOW CLASS: resp=%q ask=%v refusal=%q", resp, ask, refusal)
	}
}
