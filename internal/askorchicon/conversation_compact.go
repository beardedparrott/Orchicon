package askorchicon

// conversation_compact.go — the CompactConversation RPC: the escape hatch for a
// conversation whose accumulated context has outgrown its model's window.
//
// Scope note: this is the SERVER half only. It resolves the conversation's
// adapter, delegates to that adapter's scheduler.ChatCompactor capability, and
// audits the outcome. It deliberately does not decide WHEN to compact — the
// proactive pressure gate and the reactive context-limit recovery are separate
// callers of this same path, and the manual trigger is /compact in the clients.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// CompactConversation compacts a conversation's accumulated context so it can
// keep going instead of failing on the model's context limit.
func (s *Service) CompactConversation(ctx context.Context, req *connect.Request[apiv1.CompactConversationRequest]) (*connect.Response[apiv1.CompactConversationResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ConversationId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id must not be empty"))
	}

	// Refuse while a turn is in flight. Compaction REWRITES the history the
	// running turn reads from and persists to, so doing it underneath a live
	// turn would race the collector (the native path would clobber its append).
	// Callers that want to compact mid-turn interject first — abort, then
	// compact — rather than compacting under a live turn.
	if _, running := s.turns.get(req.Msg.ConversationId); running {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a turn is in flight — stop it or wait for it to finish before compacting"))
	}

	conv, err := s.loadConversationRow(ctx, tenantID, req.Msg.ConversationId)
	if err != nil {
		return nil, err
	}

	kind := adapter.AdapterKind(conv.ModelRef)
	client, err := s.resolveChatClient(req.Msg.ConversationId, conv.ModelRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	compactor, ok := client.(scheduler.ChatCompactor)
	if !ok {
		// The capability is OPTIONAL by design (mirroring
		// SendTurnMessageWithAttachments): an adapter that cannot compact says
		// so, actionably, instead of appearing to succeed.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("adapter kind %q does not support conversation compaction", kind))
	}

	res, err := compactor.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: req.Msg.ConversationId,
		SessionID:      conv.SessionID,
		ModelRef:       conv.ModelRef,
		Reason:         req.Msg.Reason,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	s.auditCompaction(ctx, tenantID, req.Msg.ConversationId, req.Msg.Reason, res)

	s.log.Info("ask orchicon: conversation compaction",
		"conversation", req.Msg.ConversationId,
		"adapter", kind,
		"reason", req.Msg.Reason,
		"compacted", res.Compacted,
		"detail", res.Detail)

	return connect.NewResponse(&apiv1.CompactConversationResponse{
		Compacted:           res.Compacted,
		Detail:              res.Detail,
		Summary:             res.Summary,
		ContextTokensBefore: res.TokensBefore,
		ContextTokensAfter:  res.TokensAfter,
	}), nil
}

// loadConversationRow loads a conversation in its own short tenant tx.
func (s *Service) loadConversationRow(ctx context.Context, tenantID, convID string) (db.ConversationRow, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.ConversationRow{}, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.ConversationRow{}, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return db.ConversationRow{}, connect.NewError(connect.CodeInternal, err)
	}
	return conv, nil
}

// auditCompaction records the compaction in the actor-based trail. Best-effort
// and in its OWN transaction: the compaction has already been applied by the
// time this runs (it rewrites adapter state, not DB rows), so an audit failure
// must not report the -- possibly successful -- compaction as failed. It is
// logged loudly instead.
func (s *Service) auditCompaction(ctx context.Context, tenantID, convID, reason string, res scheduler.ChatCompaction) {
	if s.pool == nil {
		return
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		s.log.Warn("ask orchicon: compaction audit tx failed", "conversation", convID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	after, err := json.Marshal(map[string]any{
		"reason":    reason,
		"compacted": res.Compacted,
		"detail":    res.Detail,
	})
	if err != nil {
		s.log.Warn("ask orchicon: compaction audit marshal failed", "conversation", convID, "error", err)
		return
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.compacted", "conversation", convID, nil, after); err != nil {
		s.log.Warn("ask orchicon: compaction audit failed", "conversation", convID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Warn("ask orchicon: compaction audit commit failed", "conversation", convID, "error", err)
	}
}
