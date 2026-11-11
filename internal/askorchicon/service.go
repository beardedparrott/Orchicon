package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/blobstore"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
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
	// adapterKinds returns the adapter kinds registered with the
	// Dispatcher (ADR-0004 D1) — powers the list_adapter_kinds tool.
	adapterKinds func() []string
	// chatKinds returns the adapter kinds whose bridge implements the ChatTurnClient (Ask chat)
	// capability (ADR-0004 D1).
	chatKinds func() []string
	// dispatcher is the shared adapter routing substrate the Ask path uses to resolve a conversation's
	// adapter kind to its ChatTurnClient capability (ADR-0003).
	dispatcher *scheduler.Dispatcher
	// sendMessage injects a mid-run message into a live worker session (Stage 3).
	sendMessage func(ctx context.Context, execID, message string) error
	// hostServe is the always-on host opencode serve.
	hostServe *opencode.HostServe
	// usageRecorder captures Ask session LLM usage via the canonical aigateway dual-write (Postgres +
	// OTel), mirroring worker executions.
	usageRecorder usageRecorder
	// turns is the in-flight turn registry (one turn per conversation):
	// the one-turn gate + the Stop path's deterministic collector cancel.
	turns *turnRegistry
	// grants is the in-memory, directory-keyed, per-conversation session grant store ("allow for this
	// directory" answers).
	grants *grantStore
	// fullsend records the conversations in FULLSEND: the operator has waived the permission PROMPT for
	// them.
	fullsend *fullsendStore
	// once records the absolute targets the operator answered ALLOW_ONCE for, per conversation: the
	// execution guard's shim cannot ask.
	once *onceStore
	// pending is the registry of asks awaiting a human decision, keyed (conversation, ask id).
	pending *pendingAskRegistry
	// followUps holds an injected message that the running turn could not take, so the next turn
	// answers it.
	followUps *followUpQueue
	// hubs is the live-turn broadcast registry (one hub per conversation with a running turn): every
	// response drained to the dispatch stream is also published here so WatchTurnStream can re-attach.
	hubs *turnHubRegistry
	// testServeClient is a test-only injection point that bypasses the real host serve so handler tests
	// can drive ChatStream/Abort with a fake session client.
	testServeClient scheduler.ChatTurnClient
	// validationRegistry is the model-ref validation catalog (ADR-0003) threaded into the
	// update_settings tool write path (the Ask-own second write.
	validationRegistry adapter.ProviderRegistry

	// fileEditHook is the diff-pipeline ledger hook for Ask turns (optional, wired by the server over
	// the shared fileedit.Service).
	fileEditHook func(ctx context.Context, tenantID, convID, toolName string, input map[string]any, output string)
	// fileEditReconciler is the terminal-turn git reconciliation hook (optional, same diff pipeline):
	// invoked once when a turn finalizes so the conversation's ledger reconciles against the project.
	fileEditReconciler func(ctx context.Context, tenantID, convID string)

	// mcpResolver is the ONE scope-addressed MCP resolver (internal/mcpsettings).
	mcpResolver mcpclient.ScopeResolver
	// kek is the tenant-secrets key used to expand ${SECRET_NAME} references in a resolved MCP
	// definition's env/headers before connect.
	kek []byte
	// askMCP caches one LIVE MCP client per conversation (see ask_mcp.go).
	askMCP   map[string]*askMCPEntry
	askMCPMu sync.Mutex

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
		hubs:         newTurnHubRegistry(),
		grants:       newGrantStore(),
		fullsend:     newFullsendStore(),
		once:         newOnceStore(),
		pending:      newPendingAskRegistry(),
		followUps:    newFollowUpQueue(),
	}
	s.registerSessionTools()
	s.startSweeper()
	return s
}

// startSweeper launches the background turn-registry sweeper (ADR-ASK-3): it ticks every interval,
// evicts entries older than the TTL (cancelling the collector with errTurnExpired).
func (s *Service) startSweeper() {
	go func() {
		ticker := time.NewTicker(askSweepInterval())
		defer ticker.Stop()
		for range ticker.C {
			for _, ev := range s.turns.sweep(time.Now(), askTurnMaxAge(), s.pending.hasOpenAsk) {
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
				if client := s.resolveClientForAbort(conv.ModelRef); client != nil {
					_ = client.AbortConversationSession(ctx, conv.SessionID)
				}
			}
		}
	}()
}

// SetSendExecutionMessage wires the live-session message router (the opencode adapter's
// SendExecutionMessage) into the send_execution_message tool.
func (s *Service) SetSendExecutionMessage(fn func(ctx context.Context, execID, message string) error) {
	s.sendMessage = fn
}

// SetHostServe wires the always-on host opencode serve into the chat so conversation turns run as
// persistent sessions on it (first message CreateSession.
func (s *Service) SetHostServe(hs *opencode.HostServe) {
	s.hostServe = hs
}

// usageRecorder records an LLM usage sample from an Ask step_finish.
type usageRecorder interface {
	Record(ctx context.Context, in aigateway.UsageInput) (db.UsageRecordRow, error)
}

// SetUsageRecorder wires the shared worker usage recorder into the chat so Ask sessions capture
// live token/cost usage per adapter.
func (s *Service) SetUsageRecorder(rec *aigateway.UsageRecorder) {
	s.usageRecorder = rec
}

// SetRuntimeClient wires the runtime daemon client for image builds.
func (s *Service) SetRuntimeClient(rt *runtime.Client) {
	s.runtimeClient = rt
	toolRuntimeClient = rt
}

// SetScopeResolver wires the ONE MCP scope resolver (internal/mcpsettings) into the native Ask
// path.
func (s *Service) SetScopeResolver(src mcpclient.ScopeResolver) {
	s.mcpResolver = src
}

// SetSecretKEK wires the tenant-secrets key the native Ask path uses to expand ${SECRET_NAME}
// references in a resolved MCP definition.
func (s *Service) SetSecretKEK(kek []byte) {
	s.kek = kek
}

// SetAdapterKinds wires the Dispatcher's registered adapter kinds (ADR-0004 D1) into the
// list_adapter_kinds tool.
func (s *Service) SetAdapterKinds(fn func() []string) {
	s.adapterKinds = fn
}

// SetChatKinds wires the Dispatcher's Ask-capable adapter kinds (ADR-0004 D1) into the
// conversation-creation + tenant-default-save guards.
func (s *Service) SetChatKinds(fn func() []string) {
	s.chatKinds = fn
}

// SetDispatcher wires the shared Dispatcher into the Ask path.
func (s *Service) SetDispatcher(d *scheduler.Dispatcher) {
	s.dispatcher = d
}

// SetValidationRegistry injects the model-ref validation catalog into the Ask-own update_settings
// write path (a second write path into tenant_settings.default_ask_orchicon_model besides.
func (s *Service) SetValidationRegistry(reg adapter.ProviderRegistry) {
	s.validationRegistry = reg
	toolValidateModelRef = func(ref string) error { return s.validateModelRef(ref) }
}

// SetFileEditHook wires the diff-pipeline ledger hook into Ask turns.
func (s *Service) SetFileEditHook(hook func(ctx context.Context, tenantID, convID, toolName string, input map[string]any, output string)) {
	s.fileEditHook = hook
}

// SetFileEditReconciler wires the terminal-turn git reconciliation hook (diff pipeline AC 4): once
// a turn finalizes, the conversation's ledger reconciles against the conversation project dir's.
func (s *Service) SetFileEditReconciler(rec func(ctx context.Context, tenantID, convID string)) {
	s.fileEditReconciler = rec
}

// registry returns the injected validation registry or the static builtin
// fallback.
func (s *Service) registry() adapter.ProviderRegistry {
	if s.validationRegistry != nil {
		return s.validationRegistry
	}
	return adapter.NewBuiltinProviderCatalog()
}

// validateModelRef checks a model ref against the adapter/provider/model
// grammar (ADR-0003) via the injected registry. Empty means unset — valid.
func (s *Service) validateModelRef(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if _, err := adapter.ParseModelRef(ref, s.registry()); err != nil {
		return err
	}
	// Ask-capability guard (ADR-0004 D1): a ref whose adapter kind is registered but does NOT implement
	// Ask chat (ChatTurnClient) is rejected here so the tenant-default save path never persists a ref.
	if s.dispatcher != nil && s.chatKinds != nil {
		kind := adapter.AdapterKind(ref)
		if kind != "" && s.kindRegistered(kind) && !s.kindAskCapable(kind) {
			return fmt.Errorf("adapter kind %q does not support Ask chat — pick an Ask-capable adapter", kind)
		}
	}
	return nil
}

// kindRegistered reports whether the adapter kind is registered with the Dispatcher.
func (s *Service) kindRegistered(kind string) bool {
	if s.dispatcher == nil {
		return false
	}
	_, err := s.dispatcher.Resolve(kind)
	return err == nil
}

// kindAskCapable reports whether the adapter kind is in the Ask-capable set
// (its bridge implements ChatTurnClient).
func (s *Service) kindAskCapable(kind string) bool {
	if s.chatKinds == nil {
		return true // no capability knowledge — don't block
	}
	for _, k := range s.chatKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// toolValidateModelRef is the package-global validation hook used by the update_settings tool
// (tool_diagnostics.go).
var toolValidateModelRef = func(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	if _, err := adapter.ParseModelRef(ref, adapter.NewBuiltinProviderCatalog()); err != nil {
		return err
	}
	return nil
}

// registerSessionTools adds tools that depend on service-injected
// dependencies (beyond pool/log).
func (s *Service) registerSessionTools() {
	s.toolRegistry.Add(ToolDefinition{
		Name:        "list_adapter_kinds",
		Description: "List the adapter kinds currently registered with the dispatcher (e.g. \"opencode\"). New adapters appear automatically once registered. The model_ref grammar is adapter/provider/model; this tool tells you which adapter kinds exist to put in segment 1.",
		Mutating:    false,
		Fn: func(_ context.Context, _ *db.Pool, _ json.RawMessage) (json.RawMessage, error) {
			kinds := []string{adapter.DefaultAdapterKind}
			if s.adapterKinds != nil {
				if k := s.adapterKinds(); len(k) > 0 {
					kinds = k
				}
			}
			b, err := json.Marshal(map[string]any{"adapter_kinds": kinds})
			if err != nil {
				return nil, err
			}
			return b, nil
		},
	})
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
		resp.Conversations = append(resp.Conversations, s.conversationRowToProto(r, r.MessageCount, preview, s.turnStatus(r.ID, stallWindow)))
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
		Conversation: s.conversationRowToProto(row, count, preview, st),
	}), nil
}

func (s *Service) CreateConversation(ctx context.Context, req *connect.Request[apiv1.CreateConversationRequest]) (*connect.Response[apiv1.CreateConversationResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Ask-capability guard (ADR-0004 D1): reject a conversation whose model_ref adapter kind is
	// registered but does NOT implement Ask chat (ChatTurnClient) BEFORE the first message send.
	if req.Msg.ModelRef != "" && s.dispatcher != nil && s.chatKinds != nil {
		kind := adapter.AdapterKind(req.Msg.ModelRef)
		if kind != "" && s.kindRegistered(kind) && !s.kindAskCapable(kind) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("adapter kind %q does not support Ask chat — pick an Ask-capable adapter", kind))
		}
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
		ID:        db.NewID(),
		TenantID:  tenantID,
		ModelRef:  req.Msg.ModelRef,
		Mode:      mode,
		ProjectID: strings.TrimSpace(req.Msg.ProjectId),
	}
	// GetProject filters on tenant and id ONLY (no status predicate), so an ARCHIVED project is a valid
	// home for a conversation and only an unknown id is refused.
	if convRow.ProjectID != "" {
		if _, err := db.GetProject(ctx, ttx.Tx, tenantID, convRow.ProjectID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound,
					fmt.Errorf("project %q not found — create it first, or leave project_id empty", convRow.ProjectID))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	row, err := db.CreateConversation(ctx, ttx.Tx, convRow)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// AN INITIAL MESSAGE MUST ACTUALLY START A TURN, which is what this RPC documents: "optionally accepts an
	// initial message to start the conversation immediately". It used to persist a user row and return, so no
	// turn was ever dispatched — the message sat unanswered until a LATER ChatStream happened to dispatch it,
	// and an operator who created a conversation from a harness (or any client that only sets InitialMessage)
	// saw their first question ignored. The send is deferred until after commit because the turn opens its own
	// transactions and would otherwise contend with this one.
	msg := strings.TrimSpace(req.Msg.InitialMessage)

	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.created", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"mode": mode, "model_ref": req.Msg.ModelRef, "project_id": convRow.ProjectID})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// START THE TURN NOW THAT THE CONVERSATION AND ITS FIRST MESSAGE EXIST.
	//
	// startConversationTurn will not find an initial message already persisted by us — see below for why it
	// is NOT pre-inserted — so it persists the user row itself, exactly as a normal send does. That keeps one
	// code path for "a message is sent" rather than two that can drift.
	if msg != "" {
		if _, _, _, terr := s.startConversationTurn(context.WithoutCancel(ctx), tenantID, row.ID, msg, nil); terr != nil {
			// THE CONVERSATION SURVIVES A FAILED FIRST SEND: it exists and is usable, so the operator can
			// retry from the client rather than losing the conversation. The error is still returned so the
			// caller knows the first message did not go.
			s.log.Warn("ask: conversation created but its initial message could not be dispatched",
				"conversation", row.ID, "error", terr)
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("conversation %s was created, but its initial message could not be sent: %w", row.ID, terr))
		}
	}

	preview := ""
	if req.Msg.InitialMessage != "" {
		preview = req.Msg.InitialMessage
		if len(preview) > 120 {
			preview = preview[:120]
		}
	}
	return connect.NewResponse(&apiv1.CreateConversationResponse{
		Conversation: s.conversationRowToProto(row, 0, preview, turnStatusInfo{}),
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
	// De-link usage attributed to this conversation — deliberately NOT delete it.
	delinkedUsage, err := db.ClearUsageSessionIDs(ctx, ttx.Tx, tenantID, req.Msg.Id, conv.SessionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	auditAfter, err := json.Marshal(map[string]any{
		"delinked_usage_records": delinkedUsage,
		"session_id":             conv.SessionID,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.deleted", "conversation", req.Msg.Id,
		nil, auditAfter); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Best-effort teardown on the conversation's resolved adapter: abort the live session/turn, then
	// purge any durable session history the adapter owns.
	if client := s.resolveClientForAbort(conv.ModelRef); client != nil {
		if conv.SessionID != "" {
			_ = client.AbortConversationSession(ctx, conv.SessionID)
		}
		if purger, ok := client.(scheduler.ConversationHistoryPurger); ok {
			if err := purger.PurgeConversationHistory(ctx, req.Msg.Id, conv.SessionID); err != nil {
				s.log.Warn("ask orchicon: purge conversation history failed",
					"conversation", req.Msg.Id, "error", err)
			}
		}
	}
	// Cancel any in-flight collector for this conversation so it finalizes immediately and never
	// persists into the deleted conversation (the collector's persist path also re-checks.
	if token, ok := s.turns.cancel(req.Msg.Id, errUserStop); ok {
		s.turns.remove(req.Msg.Id, token)
	}
	// Conversation end clears the conversation's consent state (C9): its in-memory directory grants (a
	// new conversation must ask again) and any ask still awaiting a decision (a late reply.
	s.grants.ClearConversation(req.Msg.Id)
	s.once.ClearConversation(req.Msg.Id)
	s.pending.removeConversation(req.Msg.Id)
	// And its MCP client (child 6): the conversation's stdio children must not outlive it.
	s.closeAskMCP(req.Msg.Id)
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
		Conversation: s.conversationRowToProto(row, count, preview, st),
	}), nil
}

// SetConversationMode switches a conversation's persona (see BuildSystemPrompt for the three
// modes).
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
		Conversation: s.conversationRowToProto(row, count, preview, st),
	}), nil
}

// SetConversationModel retargets a conversation's model_ref.
func (s *Service) SetConversationModel(ctx context.Context, req *connect.Request[apiv1.SetConversationModelRequest]) (*connect.Response[apiv1.SetConversationModelResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ref := strings.TrimSpace(req.Msg.ModelRef)
	// Empty clears the override; anything else must be a ref Ask can serve.
	if err := s.validateModelRef(ref); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.UpdateConversationModel(ctx, ttx.Tx, tenantID, req.Msg.Id, ref)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The adapter kind is what the dispatcher resolves on, so it belongs in the audit row: a model
	// change can silently move a conversation between adapters.
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.model_changed", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{
			"model_ref": ref,
			"adapter":   adapter.AdapterKind(ref),
		})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.model_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	st := s.turnStatus(row.ID, s.chatStallWindow(ctx, ttx.Tx, tenantID))
	return connect.NewResponse(&apiv1.SetConversationModelResponse{
		Conversation: s.conversationRowToProto(row, count, preview, st),
	}), nil
}

// SetConversationFullsend turns FULLSEND on or off for one conversation.
func (s *Service) SetConversationFullsend(ctx context.Context, req *connect.Request[apiv1.SetConversationFullsendRequest]) (*connect.Response[apiv1.SetConversationFullsendResponse], error) {
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.fullsend_changed", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"fullsend": req.Msg.Enabled})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.fullsend_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// SET AFTER THE COMMIT: the audit row and the mode must not be able to disagree. A
	// flag set before a failed commit would be a bypass with no record of who armed it.
	s.fullsend.Set(row.ID, req.Msg.Enabled)

	// The operator's decision: "Clear it too — fullsend means stop asking." And it is the consistent
	// behaviour rather than a convenience.
	if req.Msg.Enabled {
		s.approvePendingForFullsend(row.ID)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	st := s.turnStatus(row.ID, s.chatStallWindow(ctx, ttx.Tx, tenantID))
	return connect.NewResponse(&apiv1.SetConversationFullsendResponse{
		// The conversation is returned so the caller renders the state the SERVER holds rather than the one
		// it hoped for — fullsend is read back off the store by conversationRowToProto.
		Conversation: s.conversationRowToProto(row, count, preview, st),
	}), nil
}

// approvePendingForFullsend clears the permission cards a conversation already has on screen, and
// returns how many it cleared.
func (s *Service) approvePendingForFullsend(convID string) int {
	if s == nil || convID == "" {
		return 0
	}
	cleared := 0
	for _, a := range s.pending.list(convID) {
		// A QUESTION IS NOT A PERMISSION, and fullsend cannot clear one: a question's reply is the
		// operator's own WORDS, which the mode has no way to supply.
		if a.isQuestion() || !a.isOpen() {
			continue
		}
		if a.autoApproveOnce() {
			cleared++
			s.log.Info("fullsend cleared a pending permission card",
				"conversation", convID, "ask", a.AskID, "tool", a.Tool)
		}
	}
	return cleared
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
	// THE OPEN-ASK SET, read ONCE for this page. It is what lets a HISTORICAL resolution be withheld from an
	// ask that is OPEN RIGHT NOW — see withholdStaleResolutions.
	open := s.openAskIDs(req.Msg.ConversationId)
	for _, r := range rows {
		msg := messageRowToProto(r)
		withholdStaleResolutions(msg, open)
		resp.Messages = append(resp.Messages, msg)
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

// recordAudit writes an actor-based audit row in the caller's tx, resolving the actor from the
// request context.
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

// conversationRowToProto maps a stored conversation onto the wire.
func (s *Service) conversationRowToProto(r db.ConversationRow, messageCount int, lastPreview string, st turnStatusInfo) *apiv1.Conversation {
	p := &apiv1.Conversation{
		Id:        r.ID,
		TenantId:  r.TenantID,
		Title:     r.Title,
		ModelRef:  r.ModelRef,
		SessionId: r.SessionID,
		Mode:      conversationModeToProto(r.Mode),
		// FULLSEND is COMPUTED, never stored: it rides the same read-time seam as turn_in_flight below, so
		// every list/get answers with the state the plane holds right now rather than a column someone.
		Fullsend:  s.fullsend.Enabled(r.ID),
		ProjectId: r.ProjectID,
		// SkillFiles is the SELECTABLE skill path array (real on-disk paths),
		// DISTINCT from AgentConfig.skills (tenant-wide free-text prompt prose).
		SkillFiles:                skillFilesFromJSON(r.SkillFiles),
		MessageCount:              int32(messageCount),
		LastMessagePreview:        lastPreview,
		TurnInFlight:              st.inFlight,
		PendingAssistantMessageId: st.pendingMsgID,
		TurnProgressing:           st.progressing,
		TurnLastActivityAt:        st.lastActivity,
		CreatedAt:                 timestamppb.New(r.CreatedAt),
		UpdatedAt:                 timestamppb.New(r.UpdatedAt),
	}
	return p
}

// SetConversationProject places a conversation in a project, or clears the association when
// project_id is empty.
func (s *Service) SetConversationProject(ctx context.Context, req *connect.Request[apiv1.SetConversationProjectRequest]) (*connect.Response[apiv1.SetConversationProjectResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	projectID := strings.TrimSpace(req.Msg.ProjectId)
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	if projectID != "" {
		if _, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, connect.NewError(connect.CodeNotFound,
					fmt.Errorf("project %q not found", projectID))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	row, err := db.SetConversationProject(ctx, ttx.Tx, tenantID, req.Msg.Id, projectID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.project_changed", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"project_id": projectID})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.project_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	stallWindow := s.chatStallWindow(ctx, ttx.Tx, tenantID)
	return connect.NewResponse(&apiv1.SetConversationProjectResponse{
		Conversation: s.conversationRowToProto(row, count, preview, s.turnStatus(row.ID, stallWindow)),
	}), nil
}

// SetConversationSkillFiles replaces a conversation's skill_files path array (an empty list clears
// it).
func (s *Service) SetConversationSkillFiles(ctx context.Context, req *connect.Request[apiv1.SetConversationSkillFilesRequest]) (*connect.Response[apiv1.SetConversationSkillFilesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	files := req.Msg.Files
	if err := contextfiles.Validate(files); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// Read the conversation first so the containment check runs against the project it is ACTUALLY in, read in
	// the same transaction as the write — the conversation's project cannot move between the check and the save.
	current, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if current.ProjectID != "" {
		if p, err := db.GetProject(ctx, ttx.Tx, tenantID, current.ProjectID); err == nil {
			if err := contextfiles.ValidateWithin(files, p.ProjectDir); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}
	filesJSON, err := contextfiles.ToJSON(files)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	row, err := db.SetConversationSkillFiles(ctx, ttx.Tx, tenantID, req.Msg.Id, filesJSON)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.skill_files_changed", "conversation", row.ID,
		nil, audit.Snapshot(map[string]any{"skill_files": files})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.skill_files_changed: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	count, _ := db.CountConversationMessages(ctx, ttx.Tx, tenantID, row.ID)
	preview, _ := db.LastMessagePreview(ctx, ttx.Tx, tenantID, row.ID)
	stallWindow := s.chatStallWindow(ctx, ttx.Tx, tenantID)
	return connect.NewResponse(&apiv1.SetConversationSkillFilesResponse{
		Conversation: s.conversationRowToProto(row, count, preview, s.turnStatus(row.ID, stallWindow)),
	}), nil
}

// skillFilesFromJSON best-effort decodes a conversation's skill_files JSONB column into the proto's repeated
// string field. A corrupt payload degrades to empty rather than failing the read.
func skillFilesFromJSON(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// turnStatusInfo is the server-confirmed snapshot of a conversation's running turn, computed at
// read time from the in-memory turn registry (the same authoritative source as the one-turn gate).
type turnStatusInfo struct {
	inFlight     bool
	pendingMsgID string
	// progressing is true when the running turn is making genuine progress: it produced activity within
	// the no-progress window and is not wedged on an unresolved tool.
	progressing  bool
	lastActivity *timestamppb.Timestamp
}

// turnStatus returns the server-confirmed snapshot of a conversation's running turn.
func (s *Service) turnStatus(convID string, noProgressWindow time.Duration) turnStatusInfo {
	entry, ok := s.turns.get(convID)
	if !ok {
		return turnStatusInfo{}
	}
	info := turnStatusInfo{inFlight: true, pendingMsgID: entry.assistantMsgID}
	if !entry.lastActivity.IsZero() {
		info.lastActivity = timestamppb.New(entry.lastActivity)
	}
	// Progress = recent activity AND the session is not wedged.
	info.progressing = !entry.wedged && time.Since(entry.lastActivity) < noProgressWindow
	return info
}

// chatStallWindow resolves the tenant's effective chat no-progress stall window from its settings
// row (best-effort: any read failure falls back to the env/default resolution.
func (s *Service) chatStallWindow(ctx context.Context, tx pgx.Tx, tenantID string) time.Duration {
	settings, err := db.GetTenantSettings(ctx, tx, tenantID)
	if err != nil {
		return askStallNoProgressWindow()
	}
	return resolveChatStallNoProgressWindow(settings.StallNoProgressWindowSeconds)
}

// conversationMode constants mirror the DB column's text values ('brainstorm' default).
const (
	// ALIASED TO internal/askmode, not restated.
	modeBrainstorm = askmode.Brainstorm
	modeIteration  = askmode.Iteration
	modeQuickWork  = askmode.QuickWork
)

// conversationModeFromProto validates + normalizes a proto ConversationMode to its DB text value.
func conversationModeFromProto(m apiv1.ConversationMode) (string, error) {
	switch m {
	case apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED,
		apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM:
		return modeBrainstorm, nil
	case apiv1.ConversationMode_CONVERSATION_MODE_ITERATION:
		return modeIteration, nil
	case apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK:
		return modeQuickWork, nil
	default:
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown conversation mode value %d", int32(m)))
	}
}

// conversationModeToProto maps a DB mode text value back to the proto enum.
func conversationModeToProto(mode string) apiv1.ConversationMode {
	switch mode {
	case modeBrainstorm:
		return apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM
	case modeIteration:
		return apiv1.ConversationMode_CONVERSATION_MODE_ITERATION
	case modeQuickWork:
		return apiv1.ConversationMode_CONVERSATION_MODE_QUICK_WORK
	default:
		return apiv1.ConversationMode_CONVERSATION_MODE_UNSPECIFIED
	}
}

func messageRowToProto(r db.MessageRow) *apiv1.ChatMessage {
	meta := &apiv1.MessageMetadata{}
	// injected marks a USER message that was interjected into an ALREADY RUNNING turn.
	injected := false
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
			if v, ok := raw["injected"].(bool); ok {
				injected = v
			}
		}
	}
	return &apiv1.ChatMessage{
		Id:             r.ID,
		ConversationId: r.ConversationID,
		Role:           r.Role,
		Content:        r.Content,
		Reasoning:      r.Reasoning,
		ToolCalls:      toolCallsFromJSON(r.ToolCalls),
		ToolResults:    toolResultsFromJSON(r.ToolResults),
		Metadata:       meta,
		CreatedAt:      timestamppb.New(r.CreatedAt),
		Injected:       injected,
	}
}

// openAskIDs is the set of asks the server currently holds OPEN for a conversation.
func (s *Service) openAskIDs(convID string) map[string]bool {
	open := map[string]bool{}
	for _, a := range s.pending.list(convID) {
		if a != nil && a.isOpen() {
			open[a.AskID] = true
		}
	}
	return open
}

// permissionResolutionVerdicts are the ledger verdicts that SETTLE a card — the same vocabulary the
// clients treat as an outcome (frontend settleFromLedger's outcomeFromRecord.
var permissionResolutionVerdicts = map[string]bool{
	"permission.user_PERMISSION_CHOICE_ALLOW_ONCE":    true,
	"permission.user_PERMISSION_CHOICE_ALLOW_SESSION": true,
	"permission.user_PERMISSION_CHOICE_DENY":          true,
	"permission.expired":                              true,
	"permission.deny":                                 true,
	"permission.never_allow":                          true,
	"permission.policy_error":                         true,
	"permission.answered":                             true,
	"permission.fullsend_approved":                    true,
}

// withholdStaleResolutions drops any recorded resolution whose ask is OPEN RIGHT NOW. A resolution
// is withheld ONLY when the server itself is currently holding that exact ask open.
func withholdStaleResolutions(msg *apiv1.ChatMessage, open map[string]bool) {
	if msg == nil || len(open) == 0 || len(msg.ToolCalls) == 0 {
		return
	}
	kept := msg.ToolCalls[:0]
	for _, tc := range msg.ToolCalls {
		if tc != nil && open[tc.GetId()] && permissionResolutionVerdicts[tc.GetFunctionName()] {
			continue
		}
		kept = append(kept, tc)
	}
	msg.ToolCalls = kept
}

// toolCallsFromJSON maps the ask_orchicon_messages.tool_calls column (written live by the turn's
// tool ledger in ToolCall proto shape) onto the ChatMessage wire field.
func toolCallsFromJSON(raw []byte) []*apiv1.ToolCall {
	if len(raw) == 0 {
		return nil
	}
	var rows []struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		FunctionName string `json:"function_name"`
		Arguments    string `json:"arguments"`
		// IssuedAtUnixMs is child 1's per-entry issue stamp, written by toolCallEntry (tool_ledger.go).
		IssuedAtUnixMs int64 `json:"issued_at_unix_ms"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]*apiv1.ToolCall, 0, len(rows))
	for _, r := range rows {
		out = append(out, &apiv1.ToolCall{
			Id: r.ID, Type: r.Type, FunctionName: r.FunctionName, Arguments: r.Arguments,
			IssuedAtUnixMs: r.IssuedAtUnixMs,
		})
	}
	return out
}

// toolResultsFromJSON maps the ask_orchicon_messages.tool_results column
// onto the ChatMessage wire field (same nil-on-corrupt posture as calls).
func toolResultsFromJSON(raw []byte) []*apiv1.ToolResult {
	if len(raw) == 0 {
		return nil
	}
	var rows []struct {
		ToolCallID string `json:"tool_call_id"`
		Output     string `json:"output"`
		IsError    bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]*apiv1.ToolResult, 0, len(rows))
	for _, r := range rows {
		out = append(out, &apiv1.ToolResult{
			ToolCallId: r.ToolCallID, Output: r.Output, IsError: r.IsError,
		})
	}
	return out
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

// DeleteConversationMessage removes one message, or that message and everything after it, and
// RECYCLES the conversation's model session.
func (s *Service) DeleteConversationMessage(ctx context.Context, req *connect.Request[apiv1.DeleteConversationMessageRequest]) (*connect.Response[apiv1.DeleteConversationMessageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ConversationId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id must not be empty"))
	}
	if req.Msg.MessageId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message_id must not be empty"))
	}
	switch req.Msg.Mode {
	case apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_ONLY,
		apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("mode must be ONLY or AND_FOLLOWING"))
	}
	if _, running := s.turns.get(req.Msg.ConversationId); running {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a reply is still in progress for this conversation — stop it (or wait) before removing messages"))
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.ConversationId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// THE MESSAGE MUST BELONG TO THIS CONVERSATION. Without this the handler would be a deletion primitive over
	// any row in the tenant that the caller can name, which is a wider authority than the operator asked for.
	owned, err := db.MessageBelongsToConversation(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, req.Msg.MessageId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !owned {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("message not found in this conversation"))
	}

	var deleted int64
	if req.Msg.Mode == apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING {
		deleted, err = db.DeleteMessagesFrom(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, req.Msg.MessageId)
	} else {
		deleted, err = db.DeleteMessage(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, req.Msg.MessageId)
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("message not found in this conversation"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// THE RESET, unconditionally — even when deleted is 0.
	hadSession := strings.TrimSpace(conv.SessionID) != ""
	if hadSession {
		if err := db.UpdateConversationSessionID(ctx, ttx.Tx, tenantID, req.Msg.ConversationId, ""); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.log.Info("conversation messages removed",
		"conversation", req.Msg.ConversationId, "message", req.Msg.MessageId,
		"mode", req.Msg.Mode.String(), "deleted", deleted, "session_reset", hadSession)
	return connect.NewResponse(&apiv1.DeleteConversationMessageResponse{
		DeletedCount: int32(deleted),
		SessionReset: hadSession,
	}), nil
}
