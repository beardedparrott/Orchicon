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
	"fmt"
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
	case apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE, apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION, apiv1.PermissionChoice_PERMISSION_CHOICE_DENY, apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("choice must be ALLOW_ONCE, ALLOW_SESSION or DENY (UNSPECIFIED is only for a question answer)"))
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
		// A PROMOTED ASK IS ANSWERED BY CONTINUING THE CONVERSATION.
		//
		// Reported: "I tried an ask conversation with both opencode and claude adapters. Both say they
		// sent a card, but no actual card appeared." On an MCP-served adapter the ask tool runs in the
		// sidecar and RETURNS IMMEDIATELY — it never enters this process's pending registry, because it
		// is a different process. The card the operator answers is therefore derived from the
		// transcript (see tool_ledger.promoteRecordedAsk), and there is nothing here to "apply" a
		// decision to.
		//
		// THAT IS NOT AN EXPIRED ASK, and reporting it as one threw the answer away: the card settled
		// as expired and the operator's words were lost. The tool's own contract says what to do
		// instead — "The question is recorded… the user answers in their next message" — so the answer
		// becomes the next message. That is the same shape as the native path from the model's point of
		// view: it asked, and it now receives a turn carrying the operator's reply.
		if req.Msg.Answer != "" && s.promotedAskOpen(ctx, tenantID, convID, askID) {
			answer := strings.TrimSpace(req.Msg.Answer)
			if _, _, _, terr := s.startConversationTurn(ctx, tenantID, convID, answer, nil); terr != nil {
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("deliver the answer to the asked question: %w", terr))
			}
			return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
				Applied: true,
				Detail:  "answer sent as your next message",
			}), nil
		}
		return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
			Applied: false,
			Expired: true,
			Detail:  "this ask is no longer open — the turn ended or was superseded",
		}), nil
	}
	// A QUESTION IS ANSWERED WITH CONTENT, NOT A CHOICE.
	//
	// The two are deliberately not conflated: a permission's outcome is a grant
	// decision (ALLOW_ONCE / ALLOW_SESSION / DENY), a question's is the operator's
	// words, which BECOME the ask_user tool result so the model resumes the turn
	// holding them. The question path is checked FIRST because a question ask has no
	// permission semantics at all — running it through the allow/deny/grant
	// machinery would record a grant for something that grants nothing.
	if ask.isQuestion() {
		ans := strings.TrimSpace(req.Msg.Answer)
		if ans == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("answer must not be empty for a clarifying question"))
		}
		if !ask.recordClientAnswer(ans) {
			return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
				Applied: false,
				Expired: true,
				Detail:  "this question was already answered or expired — the answer was not applied",
			}), nil
		}
		return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
			Applied: true,
			Detail:  "answer recorded for ask " + askID,
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
	if req.Msg.Choice == apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_SESSION {
		s.grants.Grant(convID, ask.Key)
	}
	// ALLOW_ONCE arms the exact absolute paths the decision covered for the
	// execution guard's shim (which cannot ask): without this, the command the
	// operator just approved would be refused by the shim. A sibling path is not
	// armed and stays refused.
	if req.Msg.Choice == apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE {
		s.once.Record(convID, ask.AbsTargets...)
	}
	return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
		Applied: true,
		Detail:  "decision recorded for ask " + askID,
	}), nil
}
