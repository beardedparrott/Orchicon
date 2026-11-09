package askorchicon

// consent_service.go — the ReplyPermissionAsk RPC: the client's answer to a PermissionAsk the turn
// is waiting on.

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
	if s.pool != nil {
		if _, err := s.loadConversationRow(ctx, tenantID, convID); err != nil {
			return nil, err
		}
	}
	ask, ok := s.pending.get(convID, askID)
	if !ok {
		// A PROMOTED ASK IS ANSWERED BY CONTINUING THE CONVERSATION.
		if msgID, awaiting, found := s.findPromotedAsk(ctx, tenantID, convID, askID); found && awaiting {
			answer := strings.TrimSpace(req.Msg.Answer)
			if answer == "" {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("answer must not be empty for a clarifying question"))
			}
			// SETTLE THE CARD FIRST, so the transcript records the answer even if the turn fails.
			if rerr := s.recordPromotedAnswer(ctx, tenantID, msgID, askID, answer); rerr != nil {
				s.log.Warn("ask orchicon: could not record the answer against its card", "ask", askID, "error", rerr)
			}
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
	// The transitions are ordered so that nothing is applied unless the reply actually WINS the ask.
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
	// ALLOW_ONCE arms the exact absolute paths the decision covered for the execution guard's shim
	// (which cannot ask): without this.
	if req.Msg.Choice == apiv1.PermissionChoice_PERMISSION_CHOICE_ALLOW_ONCE {
		s.once.Record(convID, ask.AbsTargets...)
	}
	return connect.NewResponse(&apiv1.ReplyPermissionAskResponse{
		Applied: true,
		Detail:  "decision recorded for ask " + askID,
	}), nil
}
