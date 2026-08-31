package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the AskOrchiconService Connect handler.
type Service struct {
	pool          *db.Pool
	log           *slog.Logger
	blobStore     blobstore.Store
	modelDisc     *aigateway.ModelDiscoverer
	toolRegistry  *ToolRegistry
	runtimeClient *runtime.Client
	// sendMessage injects a mid-run message into a live worker session
	// (Stage 3). Wired by the server to the opencode adapter; nil when
	// the session transport is unavailable.
	sendMessage func(ctx context.Context, execID, message string) error
	// hostServe is the always-on host opencode serve. Chat turns run as
	// persistent sessions on it; nil when the transport is disabled or the
	// serve could not start — ChatStream fails the turn fast with a clean
	// message (the one-shot `opencode run` path was removed).
	hostServe *opencode.HostServe
	// turns is the in-flight turn registry (one turn per conversation):
	// the one-turn gate + the Stop path's deterministic collector cancel.
	turns *turnRegistry
	// testServeClient is a test-only injection point that bypasses the real
	// host serve so handler tests can drive ChatStream/Abort with a fake
	// session client. Never set outside tests.
	testServeClient sessionTurnClient
	apiv1connect.UnimplementedAskOrchiconServiceHandler
}

var _ apiv1connect.AskOrchiconServiceHandler = (*Service)(nil)

func New(pool *db.Pool, log *slog.Logger, blobStore blobstore.Store, modelDisc *aigateway.ModelDiscoverer, secretsKEK []byte) *Service {
	s := &Service{
		pool:         pool,
		log:          log.With("component", "ask_orchicon"),
		blobStore:    blobStore,
		modelDisc:    modelDisc,
		toolRegistry: NewToolRegistry(pool, log, secretsKEK),
		turns:        newTurnRegistry(),
	}
	s.registerSessionTools()
	s.startSweeper()
	return s
}

// startSweeper launches the background turn-registry sweeper (ADR-ASK-3): it
// ticks every interval, evicts entries older than the TTL (cancelling the
// collector with errTurnExpired), and for each evicted conversation aborts
// the serve session — belt-and-suspenders on top of the stall monitor. This
// is the "every turn has a hard backstop" guarantee: a collector that can
// never finalize is reaped in bounded time instead of blocking the
// conversation forever (the reported server-restart-required behaviour).
func (s *Service) startSweeper() {
	go func() {
		ticker := time.NewTicker(askSweepInterval())
		defer ticker.Stop()
		for range ticker.C {
			for _, ev := range s.turns.sweep(time.Now(), askTurnMaxAge()) {
				s.log.Warn("conversation turn expired — aborting serve session", "conversation", ev.convID)
				if s.pool == nil {
					continue
				}
				ctx := context.Background()
				ttx, err := s.pool.BeginTenantTx(ctx, ev.tenant)
				if err != nil {
					continue
				}
				conv, err := db.GetConversation(ctx, ttx.Tx, ev.tenant, ev.convID)
				ttx.Rollback(ctx)
				if err != nil || conv.SessionID == "" {
					continue
				}
				if client := s.hostServeClient(); client != nil {
					_ = client.Abort(ctx, conv.SessionID)
				}
			}
		}
	}()
}

// SetSendExecutionMessage wires the live-session message router (the
// opencode adapter's SendExecutionMessage) into the send_execution_message
// tool.
func (s *Service) SetSendExecutionMessage(fn func(ctx context.Context, execID, message string) error) {
	s.sendMessage = fn
}

// SetHostServe wires the always-on host opencode serve into the chat so
// conversation turns run as persistent sessions on it (first message
// CreateSession, follow-ups prompt_async on the same session). The service
// treats a nil serve as "fail the turn fast" (no one-shot fallback).
func (s *Service) SetHostServe(hs *opencode.HostServe) {
	s.hostServe = hs
}

// SetRuntimeClient wires the runtime daemon client for image builds.
func (s *Service) SetRuntimeClient(rt *runtime.Client) {
	s.runtimeClient = rt
	toolRuntimeClient = rt
}

// registerSessionTools adds tools that depend on service-injected
// dependencies (beyond pool/log).
func (s *Service) registerSessionTools() {
	s.toolRegistry.Add(ToolDefinition{
		Name:        "send_execution_message",
		Description: "Send a mid-run message to a live worker execution's opencode session (e.g. a nudge or a clarifying question). This does NOT create a new execution or work item — the worker answers within its current session and the reply streams back to the execution. Fails when the execution is not running on the session transport.",
		Mutating:    true,
		Properties: map[string]PropertySchema{
			"execution_id": {Type: "string", Description: "The id of the live worker execution"},
			"message":      {Type: "string", Description: "The message to inject into the worker's session"},
		},
		Required: []string{"execution_id", "message"},
		Fn: func(ctx context.Context, _ *db.Pool, args json.RawMessage) (json.RawMessage, error) {
			var p struct {
				ExecutionID string `json:"execution_id"`
				Message     string `json:"message"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("invalid args: %w", err)
			}
			if strings.TrimSpace(p.ExecutionID) == "" || strings.TrimSpace(p.Message) == "" {
				return nil, errors.New("execution_id and message are required")
			}
			if s.sendMessage == nil {
				return nil, errors.New("session transport not available — execution is not running on a live session")
			}
			if err := s.sendMessage(ctx, p.ExecutionID, p.Message); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"sent":true}`), nil
		},
	})
}

// --- Conversations ---

func (s *Service) ListConversations(ctx context.Context, req *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	limit := int(req.Msg.PageSize)
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := db.ListConversations(ctx, ttx.Tx, tenantID, limit, req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Resolve the chat stall window once per request so every conversation's
	// turn_progressing is computed against the tenant's configured value.
	stallWindow := s.chatStallWindow(ctx, ttx.Tx, tenantID)
	resp := &apiv1.ListConversationsResponse{}
	for _, r := range rows {
		preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, r.ID)
		resp.Conversations = append(resp.Conversations, conversationRowToProto(r, 0, preview, s.turnStatus(r.ID, stallWindow)))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	if cats, err := db.ListCategories(ctx, ttx.Tx, tenantID, "conversation"); err == nil {
		for _, c := range cats {
			resp.Categories = append(resp.Categories, &apiv1.Category{Id: c.ID, TenantId: c.TenantID, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, Name: c.Name, Description: c.Description, Slug: c.Slug, SortOrder: int32(c.SortOrder)})
		}
		if assigns, err := db.ListAssignments(ctx, ttx.Tx, tenantID, "conversation"); err == nil {
			for _, a := range assigns {
				resp.Assignments = append(resp.Assignments, &apiv1.CategoryAssignment{EntityId: a.EntityID, CategoryId: a.CategoryID, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION})
			}
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) GetConversation(ctx context.Context, req *connect.Request[apiv1.GetConversationRequest]) (*connect.Response[apiv1.GetConversationResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	st := s.turnStatus(row.ID, s.chatStallWindow(ctx, ttx.Tx, tenantID))
	return connect.NewResponse(&apiv1.GetConversationResponse{
		Conversation: conversationRowToProto(row, count, preview, st),
	}), nil
}

func (s *Service) CreateConversation(ctx context.Context, req *connect.Request[apiv1.CreateConversationRequest]) (*connect.Response[apiv1.CreateConversationResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	mode, err := conversationModeFromProto(req.Msg.Mode)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	convRow := db.ConversationRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		ModelRef: req.Msg.ModelRef,
		Mode:     mode,
	}
	row, err := db.CreateConversation(ctx, ttx.Tx, convRow)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// If an initial message was provided, create it too.
	msg := strings.TrimSpace(req.Msg.InitialMessage)
	if msg != "" {
		msgRow := db.MessageRow{
			ID:             db.NewID(),
			TenantID:       tenantID,
			ConversationID: row.ID,
			Role:           "user",
			Content:        msg,
			ToolCalls:      []byte("[]"),
			ToolResults:    []byte("[]"),
			Attachments:    []byte("[]"),
			Metadata:       []byte("{}"),
			Reasoning:      []string{},
		}
		if _, err := db.CreateMessage(ctx, ttx.Tx, msgRow); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.created", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"mode": mode, "model_ref": req.Msg.ModelRef})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	preview := ""
	if req.Msg.InitialMessage != "" {
		preview = req.Msg.InitialMessage
		if len(preview) > 120 {
			preview = preview[:120]
		}
	}
	return connect.NewResponse(&apiv1.CreateConversationResponse{
		Conversation: conversationRowToProto(row, 0, preview, turnStatusInfo{}),
	}), nil
}

func (s *Service) DeleteConversation(ctx context.Context, req *connect.Request[apiv1.DeleteConversationRequest]) (*connect.Response[apiv1.DeleteConversationResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := db.DeleteConversationMessages(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := db.DeleteConversation(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.deleted", "conversation", req.Msg.Id,
		nil, nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Best-effort abort of the conversation's opencode session. There is no
	// delete-session API on the serve; abort cancels any running turn while
	// keeping the session, and is safe to ignore on error (the durable
	// record is already gone; the serve will reclaim the session eventually).
	if conv.SessionID != "" {
		if hs := s.hostServe; hs != nil {
			if client := hs.Client(); client != nil {
				_ = client.Abort(ctx, conv.SessionID)
			}
		}
	}
	// Cancel any in-flight collector for this conversation so it finalizes
	// immediately and never persists into the deleted conversation (the
	// collector's persist path also re-checks the conversation and would
	// skip the write — this just makes it prompt and clean).
	if token, ok := s.turns.cancel(req.Msg.Id, errUserStop); ok {
		s.turns.remove(req.Msg.Id, token)
	}
	return connect.NewResponse(&apiv1.DeleteConversationResponse{}), nil
}

func (s *Service) UpdateConversationTitle(ctx context.Context, req *connect.Request[apiv1.UpdateConversationTitleRequest]) (*connect.Response[apiv1.UpdateConversationTitleResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.UpdateConversationTitle(ctx, ttx.Tx, tenantID, req.Msg.Id, req.Msg.Title)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	stallWindow := s.chatStallWindow(ctx, ttx.Tx, tenantID)
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.title_updated", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"title": req.Msg.Title})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.title_updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	st := s.turnStatus(row.ID, stallWindow)
	return connect.NewResponse(&apiv1.UpdateConversationTitleResponse{
		Conversation: conversationRowToProto(row, count, preview, st),
	}), nil
}

// SetConversationMode switches a conversation's persona (brainstorm <-> orchicon).
// The new mode is persisted on the conversation and takes effect
// on the NEXT message: the turn reads it at dispatch time and applies it as
// the opencode per-turn system prompt — no session change or serve restart
// needed (the F4 task 9 toggle surface).
func (s *Service) SetConversationMode(ctx context.Context, req *connect.Request[apiv1.SetConversationModeRequest]) (*connect.Response[apiv1.SetConversationModeResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	mode, err := conversationModeFromProto(req.Msg.Mode)
	if err != nil {
		return nil, err
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.UpdateConversationMode(ctx, ttx.Tx, tenantID, req.Msg.Id, mode)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	stallWindow := s.chatStallWindow(ctx, ttx.Tx, tenantID)
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.mode_changed", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"mode": mode})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.mode_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	st := s.turnStatus(row.ID, stallWindow)
	return connect.NewResponse(&apiv1.SetConversationModeResponse{
		Conversation: conversationRowToProto(row, count, preview, st),
	}), nil
}

// --- Messages ---

func (s *Service) ListMessages(ctx context.Context, req *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ConversationId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	limit := int(req.Msg.PageSize)
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := db.ListMessages(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, limit, req.Msg.PageToken)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &apiv1.ListMessagesResponse{}
	for _, r := range rows {
		resp.Messages = append(resp.Messages, messageRowToProto(r))
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// --- Agent Config ---

func (s *Service) GetAgentConfig(ctx context.Context, req *connect.Request[apiv1.GetAgentConfigRequest]) (*connect.Response[apiv1.GetAgentConfigResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetAgentConfig(ctx, ttx.Tx, tenantID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Return default config.
			return connect.NewResponse(&apiv1.GetAgentConfigResponse{
				Config: defaultAgentConfigProto(),
			}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetAgentConfigResponse{
		Config: agentConfigRowToProto(row),
	}), nil
}

func (s *Service) UpdateAgentConfig(ctx context.Context, req *connect.Request[apiv1.UpdateAgentConfigRequest]) (*connect.Response[apiv1.UpdateAgentConfigResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.UpsertAgentConfig(ctx, ttx.Tx, tenantID, agentConfigProtoToRow(req.Msg.Config))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "agent_config.updated", "settings", tenantID,
		nil, audit.Snapshot(map[string]any{"updated_at": row.UpdatedAt})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit agent_config.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.UpdateAgentConfigResponse{
		Config: agentConfigRowToProto(row),
	}), nil
}

// --- Model Capabilities ---

func (s *Service) GetModelCapabilities(ctx context.Context, req *connect.Request[apiv1.GetModelCapabilitiesRequest]) (*connect.Response[apiv1.GetModelCapabilitiesResponse], error) {
	if s.modelDisc == nil {
		return connect.NewResponse(&apiv1.GetModelCapabilitiesResponse{
			Capabilities: &apiv1.ModelCapabilities{
				InputText:  true,
				OutputText: true,
			},
		}), nil
	}
	models, err := s.modelDisc.ListModels(ctx, "")
	if err != nil || models == nil {
		return connect.NewResponse(&apiv1.GetModelCapabilitiesResponse{
			Capabilities: &apiv1.ModelCapabilities{
				InputText:  true,
				OutputText: true,
			},
		}), nil
	}
	for _, m := range models {
		if m.ModelRef == req.Msg.ModelRef {
			if m.Capabilities != nil {
				return connect.NewResponse(&apiv1.GetModelCapabilitiesResponse{
					Capabilities: m.Capabilities,
				}), nil
			}
			return connect.NewResponse(&apiv1.GetModelCapabilitiesResponse{
				Capabilities: &apiv1.ModelCapabilities{
					InputText:  true,
					OutputText: true,
				},
			}), nil
		}
	}
	// Model not found — return text-only defaults.
	return connect.NewResponse(&apiv1.GetModelCapabilitiesResponse{
		Capabilities: &apiv1.ModelCapabilities{
			InputText:  true,
			OutputText: true,
		},
	}), nil
}

// --- helpers ---

// recordAudit writes an actor-based audit row in the caller's tx,
// resolving the actor from the request context. Must be called in the
// same transaction as the mutation so the row commits atomically.
func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("no tenant in context")
	}
	return id, nil
}

func conversationRowToProto(r db.ConversationRow, messageCount int, lastPreview string, s turnStatusInfo) *apiv1.Conversation {
	p := &apiv1.Conversation{
		Id:                        r.ID,
		TenantId:                  r.TenantID,
		Title:                     r.Title,
		ModelRef:                  r.ModelRef,
		SessionId:                 r.SessionID,
		Mode:                      conversationModeToProto(r.Mode),
		MessageCount:              int32(messageCount),
		LastMessagePreview:        lastPreview,
		TurnInFlight:              s.inFlight,
		PendingAssistantMessageId: s.pendingMsgID,
		TurnProgressing:           s.progressing,
		TurnLastActivityAt:        s.lastActivity,
		CreatedAt:                 timestamppb.New(r.CreatedAt),
		UpdatedAt:                 timestamppb.New(r.UpdatedAt),
	}
	return p
}

// turnStatusInfo is the server-confirmed snapshot of a conversation's running
// turn, computed at read time from the in-memory turn registry (the same
// authoritative source as the one-turn gate). It is what a refreshed frontend
// uses to re-attach the Stop button + completion poll (turn_in_flight /
// pending_assistant_message_id) AND to show an ACCURATE
// progressing-vs-stalled state (turn_progressing / turn_last_activity_at)
// instead of the misleading "connection lost — still working" (AC2, D3).
type turnStatusInfo struct {
	inFlight     bool
	pendingMsgID string
	// progressing is true when the running turn is making genuine progress:
	// it produced activity within the no-progress window and is not wedged on
	// an unresolved tool. False (while inFlight) ⇒ the turn is stalled/wedged.
	progressing  bool
	lastActivity *timestamppb.Timestamp
}

// turnStatus returns the server-confirmed snapshot of a conversation's running
// turn. Read from the in-memory turn registry — the same authoritative source
// as the one-turn gate. This is how a refreshed frontend learns "a turn is
// running here" and re-attaches the Stop button + the completion poll, plus
// whether that turn is genuinely progressing or stalled/wedged (D3).
//
// noProgressWindow is the effective chat stall window (resolved from the
// tenant's stall_no_progress_window_seconds via chatStallWindow) so the
// progressing signal uses the SAME window as the turn's stall monitor — they
// must agree on when "no progress" starts.
func (s *Service) turnStatus(convID string, noProgressWindow time.Duration) turnStatusInfo {
	entry, ok := s.turns.get(convID)
	if !ok {
		return turnStatusInfo{}
	}
	info := turnStatusInfo{inFlight: true, pendingMsgID: entry.assistantMsgID}
	if !entry.lastActivity.IsZero() {
		info.lastActivity = timestamppb.New(entry.lastActivity)
	}
	// Progress = recent activity AND the session is not wedged. If the turn
	// has been quiet for longer than the no-progress window, the server no
	// longer claims "still working" — the frontend shows a stalled/retry state.
	info.progressing = !entry.wedged && time.Since(entry.lastActivity) < noProgressWindow
	return info
}

// chatStallWindow resolves the tenant's effective chat no-progress stall
// window from its settings row (best-effort: any read failure falls back to
// the env/default resolution, matching the dispatch path's behavior).
func (s *Service) chatStallWindow(ctx context.Context, tx pgx.Tx, tenantID string) time.Duration {
	settings, err := db.GetTenantSettings(ctx, tx, tenantID)
	if err != nil {
		return askStallNoProgressWindow()
	}
	return resolveChatStallNoProgressWindow(settings.StallNoProgressWindowSeconds)
}

// conversationMode constants mirror the DB column's text values ('brainstorm'
// default). Orchicon mode removed 2026-08-26 — only brainstorm remains.
const (
	modeBrainstorm = "brainstorm"
)

// conversationModeFromProto validates + normalizes a proto ConversationMode
// to its DB text value. UNSPECIFIED (and the wire's empty/absent value) maps
// to the brainstorm default; an unknown enum value on the wire is rejected
// with CodeInvalidArgument (never silently coerced).
func conversationModeFromProto(m apiv1.ConversationMode) (string, error) {
	switch m {
	case apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED,
		apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM:
		return modeBrainstorm, nil
	default:
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown conversation mode value %d", int32(m)))
	}
}

// conversationModeToProto maps a DB mode text value back to the proto enum.
// Unknown/empty (defensive — the boundary validator keeps these out) maps to
// UNSPECIFIED.
func conversationModeToProto(mode string) apiv1.ConversationMode {
	switch mode {
	case modeBrainstorm:
		return apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM
	default:
		return apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED
	}
}

func messageRowToProto(r db.MessageRow) *apiv1.ChatMessage {
	meta := &apiv1.MessageMetadata{}
	if len(r.Metadata) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(r.Metadata, &raw); err == nil {
			if v, ok := raw["model_ref"].(string); ok {
				meta.ModelRef = v
			}
			if v, ok := raw["latency_ms"].(float64); ok {
				meta.LatencyMs = int64(v)
			}
			if v, ok := raw["error"].(string); ok {
				meta.Error = v
			}
		}
	}
	return &apiv1.ChatMessage{
		Id:             r.ID,
		ConversationId: r.ConversationID,
		Role:           r.Role,
		Content:        r.Content,
		Reasoning:      r.Reasoning,
		Metadata:       meta,
		CreatedAt:      timestamppb.New(r.CreatedAt),
	}
}

func defaultAgentConfigProto() *apiv1.AgentConfig {
	return &apiv1.AgentConfig{
		Id:              "default",
		SystemPrompt:    "",
		Role:            "You are Orchicon, an AI assistant for the Orchicon platform.",
		Skills:          "Create, read, update, and delete projects, workers, work items, workflows, policies, and other Orchicon entities. Diagnose workflow failures. Create and manage project directories. Answer questions about Orchicon's features and the user's data.",
		Behavior:        "Always ask clarifying questions before performing mutating actions. Never assume the user's intent. Refuse requests outside Orchicon or the user's Orchicon projects. Refuse general coding help, personal conversation, or non-Orchicon topics.",
		DefaultModelRef: "",
	}
}

func agentConfigRowToProto(r db.AgentConfigRow) *apiv1.AgentConfig {
	return &apiv1.AgentConfig{
		Id:              r.ID,
		SystemPrompt:    r.SystemPrompt,
		Role:            r.Role,
		Skills:          r.Skills,
		Behavior:        r.Behavior,
		AgentsMd:        r.AgentsMD,
		DefaultModelRef: "",
		CreatedAt:       timestamppb.New(r.CreatedAt),
		UpdatedAt:       timestamppb.New(r.UpdatedAt),
	}
}

func agentConfigProtoToRow(p *apiv1.AgentConfig) db.AgentConfigRow {
	if p == nil {
		return db.AgentConfigRow{}
	}
	return db.AgentConfigRow{
		SystemPrompt: p.SystemPrompt,
		Role:         p.Role,
		Skills:       p.Skills,
		Behavior:     p.Behavior,
		AgentsMD:     p.AgentsMd,
	}
}
