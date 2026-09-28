package askorchicon

// consent_grants_service_test.go — the session-grant wire surface: the store's
// revoke/list behaviour (with timestamps) and the two RPCs the GUI drives.
//
// The AC these cover: "active session grants are listed with a revoke action
// that takes effect on the next tool call", and "the file is the source of
// truth" for the persistent list (that half is permpolicy's own suite).

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestGrantStore_ListCarriesTheGrantTimeAndRevokeRemovesIt: the store records
// WHEN a directory was granted (the client lists it), lists in a stable order,
// and a revoke drops exactly that directory.
func TestGrantStore_ListCarriesTheGrantTimeAndRevokeRemovesIt(t *testing.T) {
	g := newGrantStore()
	g.Grant("conv-1", "/p/zeta")
	g.Grant("conv-1", "/p/alpha")

	list := g.List("conv-1")
	if len(list) != 2 {
		t.Fatalf("grants = %v, want two", list)
	}
	if list[0].Directory != "/p/alpha" || list[1].Directory != "/p/zeta" {
		t.Fatalf("grants = %v, want a directory-sorted list", list)
	}
	for _, sg := range list {
		if sg.GrantedAt.IsZero() || time.Since(sg.GrantedAt) > time.Minute {
			t.Fatalf("grant %q granted_at = %v, want the moment it was granted", sg.Directory, sg.GrantedAt)
		}
	}

	if !g.Revoke("conv-1", "/p/alpha") {
		t.Fatal("Revoke reported false for a directory that was granted")
	}
	if g.Has("conv-1", "/p/alpha") {
		t.Fatal("the revoked grant must no longer be held")
	}
	// The guard shim's read (ask_guard.go Roots) is what the next tool call
	// sees: the revoked directory must be gone from it.
	for _, root := range g.Roots("conv-1") {
		if root == "/p/alpha" {
			t.Fatalf("Roots still carries the revoked directory: %v", g.Roots("conv-1"))
		}
	}
	if got := g.List("conv-1"); len(got) != 1 || got[0].Directory != "/p/zeta" {
		t.Fatalf("grants = %v, want only /p/zeta", got)
	}
	// Unknown directory (and an unknown conversation): removed=false, never a
	// silent success.
	if g.Revoke("conv-1", "/p/never-granted") {
		t.Fatal("Revoke reported true for a directory that was never granted")
	}
	if g.Revoke("conv-other", "/p/zeta") {
		t.Fatal("Revoke must not cross conversations")
	}
	if len(g.List("conv-other")) != 0 {
		t.Fatal("another conversation must hold nothing")
	}
}

// TestListPermissionGrantsRPC: the RPC returns the store's grants with their
// timestamps, and validates its argument.
func TestListPermissionGrantsRPC(t *testing.T) {
	svc := testConsentService()
	svc.grants.Grant("conv-1", "/p/sibling")

	res, err := svc.ListPermissionGrants(tenantCtx(), connectReq(&apiv1.ListPermissionGrantsRequest{
		ConversationId: "conv-1",
	}))
	if err != nil {
		t.Fatalf("ListPermissionGrants: %v", err)
	}
	grants := res.Msg.GetGrants()
	if len(grants) != 1 || grants[0].GetDirectory() != "/p/sibling" {
		t.Fatalf("grants = %v, want the granted directory", grants)
	}
	if grants[0].GetGrantedAtUnix() == 0 {
		t.Fatal("granted_at_unix = 0 — the client cannot say when the grant was given")
	}

	// A conversation with nothing granted is an empty list, not an error.
	res, err = svc.ListPermissionGrants(tenantCtx(), connectReq(&apiv1.ListPermissionGrantsRequest{
		ConversationId: "conv-empty",
	}))
	if err != nil {
		t.Fatalf("ListPermissionGrants on an empty conversation: %v", err)
	}
	if len(res.Msg.GetGrants()) != 0 {
		t.Fatalf("grants = %v, want none", res.Msg.GetGrants())
	}

	if _, err := svc.ListPermissionGrants(tenantCtx(), connectReq(&apiv1.ListPermissionGrantsRequest{})); err == nil {
		t.Fatal("an empty conversation_id must be rejected")
	} else if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestRevokePermissionGrantRPCTakesEffectOnTheNextToolCall: the AC's "revoke
// takes effect on the next tool call" — after the revoke, the guard shim's read
// no longer carries the directory AND the consent path asks again for it.
func TestRevokePermissionGrantRPCTakesEffectOnTheNextToolCall(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()
	svc.grants.Grant("conv-1", "/p/sibling")

	res, err := svc.RevokePermissionGrant(tenantCtx(), connectReq(&apiv1.RevokePermissionGrantRequest{
		ConversationId: "conv-1",
		Directory:      "/p/sibling",
	}))
	if err != nil {
		t.Fatalf("RevokePermissionGrant: %v", err)
	}
	if !res.Msg.GetRemoved() {
		t.Fatal("removed = false for a directory that was granted")
	}
	if len(res.Msg.GetGrants()) != 0 {
		t.Fatalf("the response must carry the refreshed list, got %v", res.Msg.GetGrants())
	}

	// The next tool call for that directory asks again: the grant is gone from
	// the guard shim's read, so Decide no longer sees SessionGranted.
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	_, ask, _ := ct.decide(context.Background(), "ses_1", fileAskEvent("per_1", "write", "/p/sibling/x.md"))
	if ask == nil {
		t.Fatal("after the revoke, the directory must ask again")
	}

	// An unknown directory reports removed=false — never a silent success.
	res, err = svc.RevokePermissionGrant(tenantCtx(), connectReq(&apiv1.RevokePermissionGrantRequest{
		ConversationId: "conv-1",
		Directory:      "/p/never-granted",
	}))
	if err != nil {
		t.Fatalf("RevokePermissionGrant: %v", err)
	}
	if res.Msg.GetRemoved() {
		t.Fatal("removed = true for a directory that was never granted")
	}

	// Both fields are validated.
	if _, err := svc.RevokePermissionGrant(tenantCtx(), connectReq(&apiv1.RevokePermissionGrantRequest{
		ConversationId: "conv-1",
	})); err == nil {
		t.Fatal("an empty directory must be rejected")
	} else if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestDecideCarriesTheDenyEntriesBelowTheGrantKey is the precedence the card
// must state: a deny entry under the directory a session grant would cover is
// still carried on the ask (Decide refuses a DENIED TARGET outright and raises
// no ask at all, so this is the only reachable case).
func TestDecideCarriesTheDenyEntriesBelowTheGrantKey(t *testing.T) {
	isolatedPolicy(t, "deny:\n  - \"/p/sibling/private/**\"\n")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	_, ask, _ := ct.decide(context.Background(), "ses_1", fileAskEvent("per_1", "write", "/p/sibling/x.md"))
	if ask == nil {
		t.Fatal("a write outside the project with nothing covering it must ask")
	}
	if ask.Key != "/p/sibling" {
		t.Fatalf("ask key = %q, want the target's directory", ask.Key)
	}
	if len(ask.DenyBelow) != 1 || ask.DenyBelow[0] != "/p/sibling/private/**" {
		t.Fatalf("ask.DenyBelow = %v, want the deny entry under the grant directory", ask.DenyBelow)
	}
	// And it reaches the client.
	if got := permissionAskEvent(ask).GetPermissionAsk().GetDenyEntriesBelow(); len(got) != 1 {
		t.Fatalf("the emitted card carries deny_entries_below = %v", got)
	}

	// A directory with no deny entry under it carries none.
	_, ask2, _ := ct.decide(context.Background(), "ses_1", fileAskEvent("per_2", "write", "/p/other/x.md"))
	if ask2 == nil {
		t.Fatal("a second uncovered directory must ask")
	}
	if len(ask2.DenyBelow) != 0 {
		t.Fatalf("ask.DenyBelow = %v, want none for a directory with no deny entry below it", ask2.DenyBelow)
	}
}
