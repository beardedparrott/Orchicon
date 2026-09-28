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

	// Resolve the model the SAME way a TURN does, via modelRefOrFallback: a
	// conversation created without its own model_ref runs on the tenant's
	// DefaultAskOrchiconModel, and compaction must resolve the SAME adapter that
	// answered the turns. Reading conv.ModelRef raw was an inconsistent chain:
	// the turn fell back, compaction did not, so compacting any conversation with
	// an empty model_ref failed outright with "no adapter kind specified (empty
	// model_ref adapter segment) — cannot resolve a bridge" — precisely when the
	// operator needs compaction most (a long conversation about to overflow).
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	kind := adapter.AdapterKind(modelRef)
	client, err := s.resolveChatClient(req.Msg.ConversationId, modelRef)
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
		ModelRef:       modelRef,
		Reason:         req.Msg.Reason,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	s.auditCompaction(ctx, tenantID, req.Msg.ConversationId, req.Msg.Reason, res)

	// AND LEAVE A MARK IN THE TRANSCRIPT. The audit trail answers "what ran"; this
	// answers the operator's actual question — "why does the assistant no longer
	// remember what we said earlier" — in the place they are looking when they ask
	// it. Advisory: the compaction has already happened, so a failure to record it is
	// logged and never fails the RPC.
	reason := req.Msg.Reason
	if strings.TrimSpace(reason) == "" {
		reason = "manual"
	}
	if res.Compacted {
		if err := s.RecordCompactionNotice(ctx, scheduler.AskCompactNotice{
			ConversationID: req.Msg.ConversationId,
			SessionID:      conv.SessionID,
			Reason:         reason,
			Detail:         res.Detail,
			ArchivePath:    res.ArchivePath,
			TokensBefore:   res.TokensBefore,
			TokensAfter:    res.TokensAfter,
		}); err != nil {
			s.log.Warn("ask orchicon: compaction notice was not recorded — the compaction stands, but the transcript will not show it",
				"conversation", req.Msg.ConversationId, "error", err)
		}
	}

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

// RecordCompactionNotice persists a durable, in-transcript record that this
// conversation's context was collapsed — so a loss that removes most of what was
// said is VISIBLE where the work is, rather than only in a server log.
//
// It is exported because the native adapter reports its AUTOMATIC compactions
// through it (SetAskCompactNotice). The proactive pressure gate runs inside the
// bridge, which has no database handle, so before this a conversation that turned
// 2,343 messages into a summary left no trace the operator would ever see — and
// "long conversations lose context" was the only place it surfaced.
//
// ADVISORY BY CONTRACT. The compaction has already happened and cannot be undone, so
// callers log a failure and carry on: a notice that could not be written must never
// fail a turn. The row is written in its own transaction for the same reason — it
// must not be able to roll back anything real.
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
		// The schema's `system` role, which is exactly what this is: something the
		// PLATFORM said about the conversation, not something either party said.
		// Both clients render it as a notice rather than as the model's words, and
		// the seed digest skips it (see buildSystemPrompt).
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
//
// It deliberately says WHAT WAS LOST and WHERE THE ORIGINAL WENT. A marker reading
// only "context compacted" would leave the operator exactly where they already
// were — unable to tell whether the thing they need is still there — which is the
// silence this whole path exists to end.
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
