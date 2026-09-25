package askorchicon

// consent_service.go — the ReplyPermissionAsk RPC: the client's answer to a
// PermissionAsk the turn is waiting on.
//
// The RPC does NOT talk to the serve. It records the decision where the
// decision path reads it (the in-memory grant store for ALLOW_SESSION) and
// hands the ask's owner — the turn's drain loop — a wake-up. The drain loop
// then answers the serve with `once` or `reject`. That split is the settled
// decision: the SESSION decision lives in OUR store, the value on the wire is
// never a session-scoped serve value.

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ReplyPermissionAsk answers a permission.asked the turn is blocked on.
func (s *Service) ReplyPermissionAsk(ctx context.Context, req *connect.Request[apiv1.ReplyPermissionAskRequest]) (*connect.Response[apiv1.ReplyPermissionAskResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	convID := strings.TrimSpace(req.Msg.ConversationId)
	askID := strings.TrimSpace(req.Msg.AskId)
	if convID == "" || askID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("conversation_id and ask_id must not be empty"))
	}
	switch req.Msg.Choice {
	case apiv1.PermissionChoice_ALLOW_ONCE, apiv1.PermissionChoice_ALLOW_SESSION, apiv1.PermissionChoice_DENY:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("choice must be ALLOW_ONCE, ALLOW_SESSION or DENY"))
	}
	// Tenant ownership: the conversation must exist in the caller's tenant.
	// Skipped when the service has no pool (a unit test drives the registry
	// directly) — the registry itself is keyed by conversation id, which is
	// tenant-scoped, so a cross-tenant ask can never be addressed.
	if s.pool != nil {
		if _, err := s.loadConversationRow(ctx, tenantID, convID); err != nil {
			return nil, err
		}
	}
	ask, ok := s.pending.get(convID, askID)
	if !ok {
		return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
			Applied: false,
			Expired: true,
			Detail:  "this ask is no longer open — the turn ended or was superseded",
		}), nil
	}
	// The transitions are ordered so that nothing is applied unless the reply
	// actually WINS the ask. ALLOW_SESSION records the directory grant only
	// after clientReply accepts the decision: granting first would leave an
	// in-memory session grant behind for an already-answered or expired ask we
	// are about to report as `expired`/`applied: false` — a decision that
	// reports itself as NOT applied must not partially apply itself. The grant
	// is in-memory and conversation-scoped.
	if !ask.clientReply(req.Msg.Choice) {
		return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
			Applied: false,
			Expired: true,
			Detail:  "this ask was already answered or expired — the decision was not applied",
		}), nil
	}
	if req.Msg.Choice == apiv1.PermissionChoice_ALLOW_SESSION {
		s.grants.Grant(convID, ask.Key)
	}
	return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
		Applied: true,
		Detail:  "decision recorded for ask " + askID,
	}), nil
}
