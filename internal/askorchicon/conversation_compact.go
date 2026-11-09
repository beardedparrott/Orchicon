package askorchicon

// conversation_compact.go — the CompactConversation RPC: the escape hatch for a conversation whose
// accumulated context has outgrown its model's window.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
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

	// Refuse while a turn is in flight.
	if _, running := s.turns.get(req.Msg.ConversationId); running {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a turn is in flight — stop it or wait for it to finish before compacting"))
	}

	return s.compactConversationForTurn(ctx, tenantID, req.Msg.ConversationId, req.Msg.Reason)
}

// compactConversationForTurn is the shared compaction core: the manual RPC above and the reactive
// context-limit recovery (chatlimit.go) both run THROUGH it.
func (s *Service) compactConversationForTurn(ctx context.Context, tenantID, convID, reason string) (*connect.Response[apiv1.CompactConversationResponse], error) {
	conv, err := s.loadConversationRow(ctx, tenantID, convID)
	if err != nil {
		return nil, err
	}

	// Resolve the model the SAME way a TURN does, via modelRefOrFallback: a conversation created
	// without its own model_ref runs on the tenant's DefaultAskOrchiconModel.
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	kind := adapter.AdapterKind(modelRef)
	client, err := s.resolveChatClient(convID, modelRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	compactor, ok := client.(scheduler.ChatCompactor)
	if !ok {
		// The capability is OPTIONAL by design (mirroring SendTurnMessageWithAttachments): an adapter that
		// cannot compact says so, actionably, instead of appearing to succeed.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("adapter kind %q does not support conversation compaction", kind))
	}

	res, err := compactor.CompactConversationSession(ctx, scheduler.CompactConversationOpts{
		ConversationID: convID,
		SessionID:      conv.SessionID,
		ModelRef:       modelRef,
		Reason:         reason,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	s.auditCompaction(ctx, tenantID, convID, reason, res)

	// AND LEAVE A MARK IN THE TRANSCRIPT.
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual"
	}
	if res.Compacted {
		if err := s.RecordCompactionNotice(ctx, scheduler.AskCompactNotice{
			ConversationID: convID,
			SessionID:      conv.SessionID,
			Reason:         reason,
			Detail:         res.Detail,
			ArchivePath:    res.ArchivePath,
			TokensBefore:   res.TokensBefore,
			TokensAfter:    res.TokensAfter,
		}); err != nil {
			s.log.Warn("ask orchicon: compaction notice was not recorded — the compaction stands, but the transcript will not show it",
				"conversation", convID, "error", err)
		}
	}

	s.log.Info("ask orchicon: conversation compaction",
		"conversation", convID,
		"adapter", kind,
		"reason", reason,
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

// RecordCompactionNotice persists a durable, in-transcript record that this conversation's context
// was collapsed — so a loss that removes most of what was said is VISIBLE where the work.
func (s *Service) RecordCompactionNotice(ctx context.Context, n scheduler.AskCompactNotice) error {
	if s.pool == nil || n.ConversationID == "" {
		return nil
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return errors.New("no tenant in context — cannot record the compaction notice")
	}
	// The notice describes work ALREADY DONE, so a turn that ends (or is aborted)
	// must not take the record of it with it.
	ctx = context.WithoutCancel(ctx)

	row := db.MessageRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		ConversationID: n.ConversationID,
		// The schema's `system` role, which is exactly what this is: something the PLATFORM said about the
		// conversation, not something either party said.
		Role:        "system",
		Content:     compactionNoticeContent(n),
		ToolCalls:   []byte("[]"),
		ToolResults: []byte("[]"),
		Attachments: []byte("[]"),
		Metadata:    []byte("{}"),
		Reasoning:   []string{},
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("compaction notice tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateMessage(ctx, ttx.Tx, row); err != nil {
		return fmt.Errorf("write compaction notice: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit compaction notice: %w", err)
	}
	return nil
}

// compactionNoticeContent is the durable, operator-facing text of the marker.
func compactionNoticeContent(n scheduler.AskCompactNotice) string {
	var b strings.Builder
	b.WriteString("Context compacted to keep this conversation inside the model's window. ")
	if d := strings.TrimSpace(n.Detail); d != "" {
		b.WriteString(d)
		if !strings.HasSuffix(d, ".") {
			b.WriteString(".")
		}
	} else {
		b.WriteString("The earlier part of this conversation was summarized.")
	}

	switch n.Reason {
	case "pressure":
		b.WriteString("\n\nThis ran automatically: the conversation reached the model's context window, so its older messages were summarized ahead of this turn rather than letting the turn fail.")
	case "context_limit":
		b.WriteString("\n\nThis ran automatically: the model reported a context-limit overflow, and the history was reduced so the conversation could continue.")
	case "manual":
		b.WriteString("\n\nThis was requested with /compact.")
	}
	if n.TokensBefore > 0 {
		fmt.Fprintf(&b, "\n\nThe conversation measured %d prompt tokens against the model's window when this ran.", n.TokensBefore)
	}

	b.WriteString("\n\nEverything above this notice is now a SUMMARY rather than the original messages, so its detail is reduced. The identifiers from the collapsed period — entity ids, file paths, commit names, the tools used — were extracted before the collapse and are carried through verbatim, but the words around them were not.")
	if n.ArchivePath != "" {
		b.WriteString("\n\nThe pre-collapse transcript was preserved at " + n.ArchivePath + " and can be read directly.")
	}
	return b.String()
}

// auditCompaction records the compaction in the actor-based trail.
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
