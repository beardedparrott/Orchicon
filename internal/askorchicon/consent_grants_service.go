package askorchicon

// consent_grants_service.go — the session-grant RPCs: list a conversation's
// active grants, and revoke one by directory.
//
// The grant store is in-memory and conversation-scoped (see consent.go); these
// handlers are a thin, validated view onto it. They deliberately keep NO copy:
// the store is the single source of truth, so a revoke here is visible to the
// very next tool call (the guard shim reads the same store on every decision,
// ask_guard.go) and a grant recorded by ReplyPermissionAsk shows up in a list
// immediately.

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ListPermissionGrants returns the conversation's active session grants.
func (s *Service) ListPermissionGrants(ctx context.Context, req *connect.Request[apiv1.ListPermissionGrantsRequest]) (*connect.Response[apiv1.ListPermissionGrantsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	convID := strings.TrimSpace(req.Msg.ConversationId)
	if convID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("conversation_id must not be empty"))
	}
	// Tenant ownership, skipped when the service has no pool (a unit test drives
	// the store directly) — mirroring ReplyPermissionAsk.
	if s.pool != nil {
		if _, err := s.loadConversationRow(ctx, tenantID, convID); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&apiv1.ListPermissionGrantsResponse{
		Grants: wireGrants(s.grants.List(convID)),
	}), nil
}

// ListPendingAsks returns the conversation's still-OPEN asks: the cards a turn is
// parked on, awaiting a human decision.
//
// WHY IT EXISTS. An ask is delivered on the turn stream, so a client only learns of
// one if it is WATCHING that turn at that instant — and the server then parks the
// turn waiting for an answer whose card never arrived. That is the failure the
// operator reported: "they did not all reach the TUI… it appeared you were stalled.
// So I went into the GUI and lo and behold, a permissions card was waiting."
//
// The state was never missing (pendingAskRegistry has always held it, and the
// re-attach path already replayed it to a LATE watcher); what was missing was a way
// to ASK. With this, a card is discoverable from durable state, so a client that
// calls it on attach, re-attach and its turn poll cannot miss one — and calling it
// twice is free, because both clients dedupe by ask id (the TUI's drawConsentAsk is
// idempotent by design).
//
// OPEN ONLY. A decided or finalized ask is omitted: its outcome is already in the
// transcript, and returning it would resurrect a card the operator has answered.
// That filter is the same isOpen() the replay path uses, so discovery and replay
// cannot disagree about what counts as pending.
func (s *Service) ListPendingAsks(ctx context.Context, req *connect.Request[apiv1.ListPendingAsksRequest]) (*connect.Response[apiv1.ListPendingAsksResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	convID := strings.TrimSpace(req.Msg.ConversationId)
	if convID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("conversation_id must not be empty"))
	}
	// Tenant ownership, skipped when the service has no pool (a unit test drives
	// the store directly) — mirroring ListPermissionGrants and ReplyPermissionAsk.
	if s.pool != nil {
		if _, err := s.loadConversationRow(ctx, tenantID, convID); err != nil {
			return nil, err
		}
	}
	open := make([]*apiv1.PermissionAsk, 0)
	for _, a := range s.pending.list(convID) {
		if !a.isOpen() {
			continue
		}
		if p := permissionAskProto(a); p != nil {
			open = append(open, p)
		}
	}
	return connect.NewResponse(&apiv1.ListPendingAsksResponse{Asks: open}), nil
}

// RevokePermissionGrant drops one session grant for a directory and returns the
// refreshed list. An unknown directory reports removed=false — never a silent
// success.
func (s *Service) RevokePermissionGrant(ctx context.Context, req *connect.Request[apiv1.RevokePermissionGrantRequest]) (*connect.Response[apiv1.RevokePermissionGrantResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	convID := strings.TrimSpace(req.Msg.ConversationId)
	dir := strings.TrimSpace(req.Msg.Directory)
	if convID == "" || dir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("conversation_id and directory must not be empty"))
	}
	if s.pool != nil {
		if _, err := s.loadConversationRow(ctx, tenantID, convID); err != nil {
			return nil, err
		}
	}
	removed := s.grants.Revoke(convID, dir)
	// ALWAYS the refreshed list: the client never keeps its own copy, so a
	// revoke that raced another revoke still leaves the client correct.
	return connect.NewResponse(&apiv1.RevokePermissionGrantResponse{
		Removed: removed,
		Grants:  wireGrants(s.grants.List(convID)),
	}), nil
}

// wireGrants maps the store's grants onto the wire message. A zero granted-at
// (unknown) is sent as 0 rather than "now".
func wireGrants(grants []sessionGrant) []*apiv1.SessionPermissionGrant {
	if len(grants) == 0 {
		return nil
	}
	out := make([]*apiv1.SessionPermissionGrant, 0, len(grants))
	for _, g := range grants {
		var at int64
		if !g.GrantedAt.IsZero() {
			at = g.GrantedAt.Unix()
		}
		out = append(out, &apiv1.SessionPermissionGrant{
			Directory:     g.Directory,
			GrantedAtUnix: at,
		})
	}
	return out
}
