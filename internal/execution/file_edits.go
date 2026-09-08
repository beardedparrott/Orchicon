package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/fileedit"
)

// fileEditOwnerKind validates the polymorphic owner selector.
func fileEditOwnerKind(kind string) (string, error) {
	switch kind {
	case db.FileEditOwnerExecution, db.FileEditOwnerAskConversation:
		return kind, nil
	case "":
		return db.FileEditOwnerExecution, nil // default: executions
	default:
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("owner_kind must be %q or %q", db.FileEditOwnerExecution, db.FileEditOwnerAskConversation))
	}
}

// GetSessionFileEdits returns the file-edit ledger for one owner (an
// execution or an Ask conversation), optionally resuming from a sequence —
// the initial load + catch-up read for the GUI sidebar / TUI pane. API-key
// auth rides the shared interceptor chain (same read policy as executions).
func (s *Service) GetSessionFileEdits(ctx context.Context, req *connect.Request[apiv1.GetSessionFileEditsRequest]) (*connect.Response[apiv1.GetSessionFileEditsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	kind, err := fileEditOwnerKind(req.Msg.OwnerKind)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.OwnerId) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner_id must not be empty"))
	}
	var fromSeq int64
	if req.Msg.FromSeq != nil {
		fromSeq = *req.Msg.FromSeq
	}

	if s.fileEditLister == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("file-edit ledger is unavailable"))
	}
	rows, maxSeq, err := s.fileEditLister(ctx, tenantID, kind, req.Msg.OwnerId, fromSeq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list file edits: %w", err))
	}
	edits := make([]*apiv1.FileEdit, 0, len(rows))
	for _, r := range rows {
		edits = append(edits, fileEditRowToProto(r))
	}
	return connect.NewResponse(&apiv1.GetSessionFileEditsResponse{Edits: edits, MaxSeq: maxSeq}), nil
}

// fileEditLister is injected by the server (over fileedit.PGStore) so this
// package keeps no direct fileedit→pool coupling in tests.
type fileEditListerFunc func(ctx context.Context, tenantID, ownerKind, ownerID string, fromSeq int64) ([]*db.FileEditLedgerRow, int64, error)

// SetFileEditLister injects the ledger read function (server wiring).
func (s *Service) SetFileEditLister(fn fileEditListerFunc) { s.fileEditLister = fn }

// StreamFileEdits fans out new ledger entries for one owner as they are
// persisted. Replay-then-live: rows > from_sequence are listed first, then
// the event bus subscription (subject filter execution.file_edit) delivers
// live rows. event_id = ledger row id (stable → dedup survives reconnect);
// sequence = ledger seq (monotonic → resume) — the useStream.ts conventions.
func (s *Service) StreamFileEdits(ctx context.Context, req *connect.Request[apiv1.StreamFileEditsRequest], stream *connect.ServerStream[apiv1.StreamFileEditsResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	kind, err := fileEditOwnerKind(req.Msg.OwnerKind)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Msg.OwnerId) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("owner_id must not be empty"))
	}
	var fromSeq int64
	if req.Msg.FromSequence != nil {
		fromSeq = *req.Msg.FromSequence
	}
	if s.fileEditLister == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("file-edit ledger is unavailable"))
	}

	// Replay: everything the DB already has beyond the resume point.
	rows, _, err := s.fileEditLister(ctx, tenantID, kind, req.Msg.OwnerId, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("replay file edits: %w", err))
	}
	for _, r := range rows {
		resp := &apiv1.StreamFileEditsResponse{
			Event:    fileEditRowToProto(r),
			EventId:  r.ID,
			Sequence: r.Seq,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	// Live: subscribe to the file-edit subject filter and forward rows for
	// this owner only. The publisher embeds the full FileEdit JSON.
	if s.subscriber == nil {
		// No event bus (tests / degraded plane): the replay above is the
		// whole answer; end the stream cleanly instead of erroring.
		return nil
	}
	filter := eventbus.SubjectFor("execution", "file_edit")
	ch, err := s.subscriber.Subscribe(ctx, filter, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to file edit events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var env struct {
				TenantID    string          `json:"tenant_id"`
				OwnerKind   string          `json:"owner_kind"`
				OwnerID     string          `json:"owner_id"`
				ExecutionID string          `json:"execution_id"`
				Edit        json.RawMessage `json:"edit"`
			}
			if err := json.Unmarshal(msg.Data, &env); err != nil {
				s.log.Warn("file edit event parse failed", "error", err)
				continue
			}
			ownerID := env.OwnerID
			if ownerID == "" {
				ownerID = env.ExecutionID // execution-published rows
			}
			if env.TenantID != tenantID || ownerID != req.Msg.OwnerId {
				continue
			}
			var edit apiv1.FileEdit
			if err := json.Unmarshal(env.Edit, &edit); err != nil {
				s.log.Warn("file edit payload parse failed", "error", err)
				continue
			}
			if edit.Seq <= fromSeq {
				continue
			}
			if err := stream.Send(&apiv1.StreamFileEditsResponse{
				Event:    &edit,
				EventId:  edit.Id,
				Sequence: edit.Seq,
			}); err != nil {
				return err
			}
		}
	}
}

// fileEditRowToProto maps a ledger row onto its proto message via the
// shared fileedit mapper — one mapping for fetch, stream-replay, and bus
// fan-out, so every consumer sees identical fields.
func fileEditRowToProto(r *db.FileEditLedgerRow) *apiv1.FileEdit {
	return fileedit.RowToProto(r)
}
