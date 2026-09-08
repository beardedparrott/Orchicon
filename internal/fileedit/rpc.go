package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
)

// RPCService implements the generated FileEditServiceHandler: the standalone
// fetch + stream RPCs the GUI sidebar and TUI pane consume. API-key auth is
// NOT handled here — the shared interceptor chain (api.Mount) resolves the
// tenant before the handler runs, mirroring every other service.
type RPCService struct {
	store Store
	// list is the ledger read fn (PGStore.List shape). Injected from the
	// store when it provides List; nil without a PG-backed store.
	list       func(ctx context.Context, tenantID, ownerKind, ownerID string, fromSeq int64) ([]*db.FileEditLedgerRow, int64, error)
	log        *slog.Logger
	subscriber eventbus.Subscriber
}

// NewRPCService wires the RPC layer over the durable store. subscriber may
// be nil: StreamFileEdits then serves replay-only (rows the DB already has)
// and ends cleanly, the same degraded posture as the execution event stream.
func NewRPCService(store Store, log *slog.Logger, sub eventbus.Subscriber) *RPCService {
	if log == nil {
		log = slog.Default()
	}
	svc := &RPCService{store: store, log: log, subscriber: sub}
	if pg, ok := store.(interface {
		List(ctx context.Context, tenantID, ownerKind, ownerID string, fromSeq int64) ([]*db.FileEditLedgerRow, int64, error)
	}); ok {
		svc.list = pg.List
	}
	return svc
}

// ownerKind validates the polymorphic owner selector.
func ownerKind(kind string) (string, error) {
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

// GetSessionFileEdits returns the file-edit ledger for one owner, optionally
// resuming from a sequence — the initial load + catch-up read.
func (s *RPCService) GetSessionFileEdits(ctx context.Context, req *connect.Request[apiv1.GetSessionFileEditsRequest]) (*connect.Response[apiv1.GetSessionFileEditsResponse], error) {
	tenantID := req.Msg.GetTenantId()
	if strings.TrimSpace(tenantID) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id must not be empty"))
	}
	kind, err := ownerKind(req.Msg.GetOwnerKind())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.GetOwnerId()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner_id must not be empty"))
	}
	var fromSeq int64
	if req.Msg.FromSeq != nil {
		fromSeq = *req.Msg.FromSeq
	}
	rows, maxSeq, err := s.list(ctx, tenantID, kind, req.Msg.GetOwnerId(), fromSeq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list file edits: %w", err))
	}
	edits := make([]*apiv1.FileEdit, 0, len(rows))
	for _, r := range rows {
		edits = append(edits, RowToProto(r))
	}
	return connect.NewResponse(&apiv1.GetSessionFileEditsResponse{Edits: edits, MaxSeq: maxSeq}), nil
}

// StreamFileEdits fans out new ledger entries for one owner as they are
// persisted. Replay-then-live: rows > from_sequence are listed first, then
// the event bus subscription (subject filter execution.file_edit) delivers
// live rows. event_id = ledger row id (stable → dedup survives reconnect);
// sequence = ledger seq (monotonic → resume) — the useStream.ts conventions.
func (s *RPCService) StreamFileEdits(ctx context.Context, req *connect.Request[apiv1.StreamFileEditsRequest], stream *connect.ServerStream[apiv1.StreamFileEditsResponse]) error {
	tenantID := req.Msg.GetTenantId()
	if strings.TrimSpace(tenantID) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id must not be empty"))
	}
	kind, err := ownerKind(req.Msg.GetOwnerKind())
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Msg.GetOwnerId()) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("owner_id must not be empty"))
	}
	var fromSeq int64
	if req.Msg.FromSequence != nil {
		fromSeq = *req.Msg.FromSequence
	}

	// Replay: everything the DB already has beyond the resume point.
	rows, _, err := s.list(ctx, tenantID, kind, req.Msg.GetOwnerId(), fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("replay file edits: %w", err))
	}
	for _, r := range rows {
		if err := stream.Send(&apiv1.StreamFileEditsResponse{
			Event:    RowToProto(r),
			EventId:  r.ID,
			Sequence: r.Seq,
		}); err != nil {
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
			if env.TenantID != tenantID || ownerID != req.Msg.GetOwnerId() {
				continue
			}
			var edit apiv1.FileEdit
			if err := json.Unmarshal(env.Edit, &edit); err != nil {
				s.log.Warn("file edit payload parse failed", "error", err)
				continue
			}
			if edit.GetSeq() <= fromSeq {
				continue
			}
			if err := stream.Send(&apiv1.StreamFileEditsResponse{
				Event:    &edit,
				EventId:  edit.GetId(),
				Sequence: edit.GetSeq(),
			}); err != nil {
				return err
			}
		}
	}
}

