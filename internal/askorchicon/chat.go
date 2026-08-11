package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// defaultHandshakeTimeout bounds how long a chat turn's attempt waits for
// the serve to ACCEPT the sent message (send-accept). The timer starts after
// Subscribe succeeds and only fails the attempt while the message is still
// un-accepted, so a serve that accepts a connection but never acknowledges
// the send fails fast instead of silently queuing. It does NOT bound
// subscribe (a never-reachable serve is bounded by the serve-down grace) nor
// the reply itself (the reply window bounds the whole turn). Override via
// ORCHICON_ASK_TIMEOUT.
const defaultHandshakeTimeout = 600 * time.Second

func askTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHandshakeTimeout
}

// defaultReplyWindow bounds how long a detached chat turn may run before its
// reply is persisted as a timeout error. The reply is collected on a
// request-independent context, so the window can be generous enough to cover
// a long multi-tool answer without ever blocking the UI. Env-overridable via
// ORCHICON_ASK_REPLY_WINDOW.
const defaultReplyWindow = 30 * time.Minute

func askReplyWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_REPLY_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultReplyWindow
}

// defaultReattachBackoff is the pause between re-attach attempts after the
// serve bus is lost mid-reply (serve restart / watchdog recovery). Env
// override ORCHICON_ASK_REATTACH_BACKOFF is a dev/test knob.
const defaultReattachBackoff = 2 * time.Second

func askReattachBackoff() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_REATTACH_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultReattachBackoff
}

// defaultServeDownGrace bounds how long a turn's collector keeps retrying a
// serve that has NEVER accepted a connection this turn (serve down at send
// time). Once a serve has been reachable during the turn, a later loss is a
// restart and gets the full reply window to recover. The grace fails fast so
// a send issued while the serve is down surfaces a clean, retryable error
// message instead of looping silently up to the reply window. Env override
// ORCHICON_ASK_SERVE_DOWN_GRACE is a dev/test knob.
const defaultServeDownGrace = 15 * time.Second

func askServeDownGrace() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_SERVE_DOWN_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultServeDownGrace
}

// opencodeEvent is a single JSON event from opencode's stdout.
type opencodeEvent struct {
	Type string         `json:"type"`
	Part map[string]any `json:"part"`
}

// streamCallback is called for each streaming event from opencode.
type streamCallback func(evt opencodeEvent) error

// turnRegistry tracks in-flight Ask Orchicon turns (keyed by conversation
// id) with their collector cancellation functions. It serves two purposes:
//
//   - the one-turn-per-conversation gate: a second send while a turn is
//     pending is rejected with FailedPrecondition (the frontend also
//     disables the input, but the gate is the server-side backstop);
//   - the Stop path: AbortConversationTurn cancels the collector so it
//     finalizes promptly and persists the user-initiated-stop error, rather
//     than sitting idle waiting the full reply window for an idle the serve
//     may not emit after abort.
//
// cancel() fires the collector's cancellation but leaves the entry in place
// until the collector removes it on finalize, so a new send stays gated while
// the old turn drains (the gap is milliseconds).
type turnRegistry struct {
	mu    sync.Mutex
	turns map[string]context.CancelFunc
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{turns: make(map[string]context.CancelFunc)}
}

// register records a turn's cancellation function for a conversation. It
// returns false when a turn is already in flight for that conversation.
func (r *turnRegistry) register(convID string, cancel context.CancelFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.turns[convID]; ok {
		return false
	}
	r.turns[convID] = cancel
	return true
}

// cancel fires the collector's cancellation for a conversation (if any) and
// reports whether a turn was registered. The entry is removed by the
// collector via remove() once it finalizes.
func (r *turnRegistry) cancel(convID string) (context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.turns[convID]
	if ok {
		cancel()
	}
	return cancel, ok
}

// remove drops the registry entry for a conversation (called by the
// collector on finalize, success or error).
func (r *turnRegistry) remove(convID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.turns, convID)
}

func (s *Service) ChatStream(ctx context.Context, req *connect.Request[apiv1.ChatStreamRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	msg := strings.TrimSpace(req.Msg.Message)
	if msg == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("message must not be empty"))
	}
	if req.Msg.ConversationId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id must not be empty"))
	}
	if utf8.RuneCountInString(msg) > 10000 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("message too long (max 10000 characters)"))
	}

	assistantID, err := s.startConversationTurn(ctx, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments)
	if err != nil {
		return err
	}

	// Ack immediately. The reply is delivered by polling ListMessages for
	// the persisted message under assistantID.
	return stream.Send(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TurnStarted{
			TurnStarted: &apiv1.TurnStarted{AssistantMessageId: assistantID},
		},
	})
}

// startConversationTurn is ChatStream's core (extracted for testability): it
// persists the user message, registers the in-flight turn, and launches the
// detached reply collector. It returns the acked assistant message id under
// which the reply (or error) will be persisted. Errors are *connect.Error
// values with the right code (NotFound for a missing conversation,
// FailedPrecondition when a turn is already pending — one turn at a time).
func (s *Service) startConversationTurn(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput) (string, error) {
	// --- 1. Load conversation + register the turn. ---
	// The turn is registered BEFORE the user message is persisted so a
	// rejected second send (one turn per conversation) never orphans a
	// persisted user message. The detached context keeps the collector alive
	// across a stream disconnect / tab close; only the turn registry's
	// cancellation (Stop) ends it. On any error below, the turn is released.
	detached := context.WithoutCancel(ctx)
	turnCtx, cancelTurn := context.WithCancel(detached)
	if !s.turns.register(convID, cancelTurn) {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a reply is still in progress for this conversation — wait for it to complete or stop it first"))
	}
	releaseTurn := func() {
		cancelTurn()
		s.turns.remove(convID)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, err)
	}
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	ttx.Rollback(ctx)
	if err != nil {
		releaseTurn()
		if errors.Is(err, db.ErrNotFound) {
			return "", connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return "", connect.NewError(connect.CodeInternal, err)
	}

	// --- 2. Persist user message (and title on first message). ---
	userMsg := db.MessageRow{
		ID:              db.NewID(),
		TenantID:        tenantID,
		ConversationID:  convID,
		Role:            "user",
		Content:         msg,
		ToolCalls:       []byte("[]"),
		ToolResults:     []byte("[]"),
		Attachments:     []byte("[]"),
		Metadata:        []byte("{}"),
		Reasoning:       []string{},
	}
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, userMsg); err != nil {
		ttx.Rollback(ctx)
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("save user message: %w", err))
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, convID); err != nil {
		ttx.Rollback(ctx)
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if conv.Title == "" {
		title := msg
		if len(title) > 80 {
			title = title[:80]
		}
		db.UpdateConversationTitle(ctx, ttx.Tx, tenantID, convID, title)
	}
	if err := ttx.Commit(ctx); err != nil {
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, err)
	}

	// --- 3. Resolve model and build prompts (DB-only, no serve
	// interaction — the reply is collected detached). ---
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	if modelRef == "" {
		modelRef = "opencode/deepseek-v4-flash-free"
	}

	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", connect.NewError(connect.CodeInternal, err)
	}
	prevMessages, _ := db.ListMessages(ctx, ttx.Tx, tenantID, convID, 50, "")
	cfg, _ := db.GetAgentConfig(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)

	// Inject the tenant's enabled projects so the agent always has
	// up-to-date context about what it operates on (fresh per message).
	projectContext := s.fetchProjectContext(ctx, tenantID)

	// System prompt variants for the session transport: the seed variant
	// (DB history included) is used when a fresh session is created (first
	// message, or a lost session recreated); the reuse variant (no history —
	// it already lives in the session) is the steady-state system for
	// follow-up turns. Both variants are built from the conversation's CURRENT
	// mode: the mode is applied per message as the opencode per-turn `system`
	// field, so a mid-conversation mode switch changes the next message's
	// persona with no session change or serve restart.
	seedSystem := buildSystemPrompt(conv.Mode, cfg, s.toolRegistry, prevMessages, true, attachments, projectContext)
	reuseSystem := buildSystemPrompt(conv.Mode, cfg, s.toolRegistry, prevMessages, false, attachments, projectContext)

	// --- 4. Launch the detached reply collector and return immediately.
	// ChatStream does NOT hold the browser connection open for the turn:
	// the collector re-attaches after serve loss, falls back to a fresh
	// session seeded from the DB transcript, and persists the reply (or an
	// error) under the acked assistant message id — even if the tab is
	// closed. The TextChunk/ToolCallResult streaming events are retained in
	// the proto for a future SSE surface but are not emitted. ---
	assistantID := db.NewID()
	if client := s.hostServeClient(); client != nil {
		go func() {
			reply, reasoning, sid, terr := s.collectConversationReply(turnCtx, turnCollectOpts{
				client:         client,
				tenantID:       tenantID,
				convID:         convID,
				assistantMsgID: assistantID,
				sessionID:      conv.SessionID,
				modelRef:       modelRef,
				seedSystem:     seedSystem,
				reuseSystem:    reuseSystem,
				userMsg:        msg,
			})
			// Reasoning chunks that arrived before the turn ended (success or
			// error/stop/timeout) are always persisted on the assistant
			// message — partial reasoning is preserved, matching the
			// "partial content is preserved as an error message" spirit.
			if terr != nil {
				errText := terr.Error()
				if errors.Is(terr, context.Canceled) {
					errText = "Turn stopped by the user."
				}
				s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, errText, reasoning)
				return
			}
			s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, "", reasoning)
		}()
	} else {
		// No session transport (serve disabled / not started): fail the
		// turn fast with a clean, visible, retryable error message.
		releaseTurn()
		s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", conv.SessionID,
			"Ask Orchicon is temporarily unavailable — the opencode serve is starting. Please try again in a moment.", []string{})
	}

	return assistantID, nil
}

// AbortConversationTurn implements the Stop button: it cancels the detached
// collector for the conversation's running turn (so it finalizes promptly
// and persists the user-initiated-stop error) and aborts the turn on the
// conversation's opencode session via the SessionClient — the same abort
// executions use. No subprocess exists to kill; the session stays alive for
// the next message. Idempotent: a conversation with no running turn is a
// no-op.
func (s *Service) AbortConversationTurn(ctx context.Context, req *connect.Request[apiv1.AbortConversationTurnRequest]) (*connect.Response[apiv1.AbortConversationTurnResponse], error) {
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
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.ConversationId)
	ttx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if _, ok := s.turns.cancel(req.Msg.ConversationId); ok {
		s.log.Info("conversation turn aborted by user", "conversation", req.Msg.ConversationId)
	}

	// Best-effort serve abort of the session's running turn (idempotent; no
	// session or no running turn is a no-op). The session itself is kept.
	if conv.SessionID != "" {
		if client := s.hostServeClient(); client != nil {
			_ = client.Abort(ctx, conv.SessionID)
		}
	}
	return connect.NewResponse(&apiv1.AbortConversationTurnResponse{}), nil
}

// buildSystemPrompt assembles the per-message `system` prompt for the Ask
// Orchicon agent. It carries the mode's identity block (BuildSystemPrompt),
// the enabled-projects context, the tools list, and this message's
// attachments. mode selects the persona (brainstorm | orchicon); it is the
// conversation's persisted mode read at turn-dispatch time.
//
// When includeHistory is true the DB conversation history is ALSO injected
// (last 10 messages, chronological) plus the "refer to earlier" hint — this
// is the SEED variant used when a fresh opencode session is created (first
// message, or a lost session recreated), where the session has no memory of
// prior turns. When false (the steady-state follow-up on a live session) no
// history block is emitted: the history already lives in the session, and
// re-injecting it would double tokens and can confuse the model.
func buildSystemPrompt(mode string, cfg db.AgentConfigRow, registry *ToolRegistry, history []db.MessageRow, includeHistory bool, attachments []*apiv1.AttachmentInput, projectContext string) string {
	var b strings.Builder

	b.WriteString(BuildSystemPrompt(mode, cfg, registry))
	b.WriteString("\n\n")

	if includeHistory {
		b.WriteString("## Conversation history\n")
		// history is in DESC (newest-first) order from the DB. Reverse it so
		// we can take the LAST N items (which are the most recent in a
		// chronologically-ordered slice).
		for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
			history[i], history[j] = history[j], history[i]
		}
		start := 0
		if len(history) > 10 {
			start = len(history) - 10
		}
		for _, h := range history[start:] {
			if h.Content == "" {
				continue
			}
			roleLabel := "User"
			if h.Role == "assistant" {
				roleLabel = "Orchicon"
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", roleLabel, h.Content))
		}
		b.WriteString("\n")
		b.WriteString("Note: If the user refers to something mentioned earlier in this conversation (like a work item, project, or result), the details are in the conversation history above — use them directly.\n\n")
	}

	if len(attachments) > 0 {
		b.WriteString("## Attachments\n")
		for _, a := range attachments {
			b.WriteString(fmt.Sprintf("File: %s (%s, %d bytes)\n", a.Name, a.MimeType, len(a.Data)))
			if strings.HasPrefix(a.MimeType, "text/") || strings.HasPrefix(a.MimeType, "application/json") || strings.HasSuffix(a.Name, ".md") {
				b.WriteString("```\n" + string(a.Data) + "\n```\n")
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Enabled projects\n")
	if projectContext != "" {
		b.WriteString(projectContext)
		b.WriteString("\n")
	} else {
		b.WriteString("None yet — use the create_project tool to create one.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Available tools\n")
	b.WriteString("Orchicon's tools are exposed to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation.\n\n")
	for _, td := range registry.List() {
		mutability := "read-only"
		if td.Mutating {
			mutability = "mutates data — requires user confirmation"
		}
		b.WriteString(fmt.Sprintf("- `orchicon_%s`: %s (%s)\n", td.Name, td.Description, mutability))
	}
	b.WriteString("\n")

	return b.String()
}

// sessionTurnClient is the session surface the chat turn loop drives
// (the session transport). *opencode.SessionClient satisfies it; tests
// inject a fake to replay bus events without a live serve or a model.
type sessionTurnClient interface {
	Subscribe(ctx context.Context) (opencode.BusSub, error)
	CreateSession(ctx context.Context, title string) (string, error)
	SendMessage(ctx context.Context, sessionID, system, modelRef, text string) error
	Abort(ctx context.Context, sessionID string) error
	ReplyPermission(ctx context.Context, sessionID, permissionID string) error
}

// hostServeClient returns the host serve's session client, or nil when the
// session transport is unavailable (serve disabled, not started, or failed)
// — the caller fails the turn fast with a clean message (no one-shot
// fallback). Tests may inject a fake via Service.testServeClient.
func (s *Service) hostServeClient() sessionTurnClient {
	if s.testServeClient != nil {
		return s.testServeClient
	}
	if s.hostServe == nil {
		return nil
	}
	return s.hostServe.Client()
}

// persistConversationSessionID saves the opencode session id on the
// conversation row (best-effort, its own tiny tenant tx). Called as soon as
// a fresh session is created so a crash mid-turn cannot orphan a session
// the next message would have to rediscover.
func (s *Service) persistConversationSessionID(ctx context.Context, tenantID, convID, sessionID string) {
	if s.pool == nil {
		return
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		s.log.Warn("persist conversation session id: begin tx", "conversation", convID, "error", err)
		return
	}
	if err := db.UpdateConversationSessionID(ctx, ttx.Tx, tenantID, convID, sessionID); err != nil {
		ttx.Rollback(ctx)
		s.log.Warn("persist conversation session id", "conversation", convID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Warn("persist conversation session id: commit", "conversation", convID, "error", err)
	}
}

// turnCollectOpts carries the fixed inputs for one detached chat-turn reply
// collector. The collector resolves/recreates the session and re-dispatches
// across serve restarts, so the session identity here is the starting point,
// not the final one.
type turnCollectOpts struct {
	client         sessionTurnClient
	tenantID       string
	convID         string
	assistantMsgID string
	// sessionID is the persisted opencode session id (empty on a first
	// message, recreated on serve loss when the serve no longer knows it).
	sessionID string
	modelRef  string
	// seedSystem is the system prompt for a fresh session (DB history
	// included); reuseSystem is the steady-state follow-up system (no
	// history — it already lives in the session).
	seedSystem  string
	reuseSystem string
	userMsg     string
}

// turnAttemptKind is the outcome of a single subscribe+send+drain attempt.
type turnAttemptKind int

const (
	turnCollected turnAttemptKind = iota // reply complete; text is final
	turnFailed                            // terminal error; err is set
	turnRecreated                         // session 404'd — a fresh seeded session was created
	turnReattach                          // bus lost after a live connection — retry after a backoff
	turnServeDown                         // the serve never accepted a connection this attempt
)

type turnAttemptResult struct {
	kind      turnAttemptKind
	text      string
	reasoning []string
	newSid    string
	err       error
}

// collectConversationReply is the detached reply collector for one chat
// turn. It mirrors the executions follow-up (ContinueSession / collectReply)
// pattern: subscribe to the serve bus, ensure the user message is accepted,
// drain for the reply (bounded by the reply window), and persist. It runs on
// a request-independent context — a ChatStream disconnect / tab close never
// cancels it — and re-attaches across serve loss:
//
//   - bus loss (serve died): re-subscribe after a backoff on the SAME
//     session (the serve watchdog preserves the data dir, so the session id
//     survives and the conversation continues with real continuity);
//   - session gone (404 — data dir wiped): create a FRESH session seeded
//     from the DB transcript (seedSystem), persist the new id, re-dispatch
//     once;
//   - retries stay inside the reply window; when exhausted the turn is
//     persisted as an error by the caller.
//
// It returns the collected assistant text, the collected reasoning chunks
// (partial on error/stop/timeout turns — whatever arrived is preserved), the
// (possibly recreated) session id used for the final dispatch, and a terminal
// error (reply timeout, session.error, serve loss exhausted, user stop via
// the registry cancel).
func (s *Service) collectConversationReply(ctx context.Context, c turnCollectOpts) (text string, reasoning []string, sid string, err error) {
	defer s.turns.remove(c.convID)

	sid = c.sessionID
	system := c.reuseSystem
	recreated := false

	// A first message (no persisted session id) creates the session up front
	// and persists the id immediately; follow-ups reuse the persisted one. A
	// later 404 (serve data dir wiped) triggers exactly one more
	// recreation + DB-transcript re-seed inside the attempt loop.
	if sid == "" {
		fresh, cerr := c.client.CreateSession(ctx, "ask-orchicon:"+c.convID)
		if cerr != nil {
			return "", nil, "", fmt.Errorf("create conversation session: %w", cerr)
		}
		sid = fresh
		system = c.seedSystem
		s.persistConversationSessionID(ctx, c.tenantID, c.convID, sid)
	}

	window := time.NewTimer(askReplyWindow())
	defer window.Stop()

	// serveDownDeadline bounds how long a turn whose serve has NEVER accepted
	// a connection keeps retrying: a serve down at send time fails fast with
	// a clean, retryable error instead of looping silently up to the reply
	// window. Once the serve has been live during the turn (everLive), a
	// later loss is a restart and the full reply window applies.
	serveDownDeadline := time.Now().Add(askServeDownGrace())

	// everLive reports whether the serve accepted a connection at least once
	// during this turn. It flips true on any attempt that reached the bus
	// (turnReattach = bus died after a live connection; turnRecreated = the
	// serve answered a send with 404), so only the never-live case is bounded
	// by the short serve-down grace.
	everLive := false

	for {
		res := s.runOneTurnAttempt(ctx, window, c, sid, system, recreated)
		// Reasoning chunks observed across attempts survive serve loss:
		// whatever arrived before a re-attach is carried forward, matching
		// the "partial reasoning is preserved" spirit for error paths.
		reasoning = append(reasoning, res.reasoning...)
		switch res.kind {
		case turnCollected:
			return res.text, reasoning, sid, nil
		case turnFailed:
			return res.text, reasoning, sid, res.err
		case turnRecreated:
			sid = res.newSid
			system = c.seedSystem
			recreated = true
			everLive = true
			s.persistConversationSessionID(ctx, c.tenantID, c.convID, sid)
		case turnReattach:
			// Bus closed after a live connection — the serve died mid-reply.
			// Keep retrying inside the reply window (the watchdog brings the
			// serve back and the session id survives).
			everLive = true
			select {
			case <-ctx.Done():
				return res.text, reasoning, sid, ctx.Err()
			case <-window.C:
				return res.text, reasoning, sid, errors.New("the opencode serve did not recover within the reply window — please retry")
			case <-time.After(askReattachBackoff()):
			}
		case turnServeDown:
			// The serve never accepted a connection this attempt. If the turn
			// was never live, this is a serve-down at send time: bounded by
			// the short grace so a clean error surfaces quickly. A serve that
			// WAS live earlier is presumably restarting — keep the full
			// window like turnReattach.
			if !everLive {
				if time.Now().After(serveDownDeadline) {
					return res.text, reasoning, sid, errors.New("the opencode serve is unavailable — please try again in a moment")
				}
				select {
				case <-ctx.Done():
					return res.text, reasoning, sid, ctx.Err()
				case <-time.After(askReattachBackoff()):
				}
			} else {
				select {
				case <-ctx.Done():
					return res.text, reasoning, sid, ctx.Err()
				case <-window.C:
					return res.text, reasoning, sid, errors.New("the opencode serve did not recover within the reply window — please retry")
				case <-time.After(askReattachBackoff()):
				}
			}
		}
	}
}

// runOneTurnAttempt drives a single subscribe+send+drain attempt of the
// detached collector. The send runs concurrently with the drain so events
// arriving between subscribe and the serve accepting our message are
// observed while sent == false and ignored (the stale-idle guard). A 404 on
// a reused session returns turnRecreated (the caller re-seeds + re-dispatches
// once); a closed bus returns turnReattach (the caller waits + retries).
func (s *Service) runOneTurnAttempt(ctx context.Context, window *time.Timer, c turnCollectOpts, sid, system string, recreated bool) turnAttemptResult {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sub, err := c.client.Subscribe(subCtx)
	if err != nil {
		// The serve never accepted a connection this attempt. When this is
		// the turn's FIRST connection (serve down at send time) the caller
		// fails fast after a short grace instead of looping to the reply
		// window; when the turn has already been live, a transient subscribe
		// failure during a serve restart keeps the full-window retry.
		return turnAttemptResult{kind: turnServeDown}
	}
	defer sub.Close()

	// Send the user message while the drain loop below is already live. The
	// send runs on its own goroutine so events that arrive between subscribe
	// and the serve accepting our message are observed with sent == false
	// and ignored.
	sendCh := make(chan error, 1)
	go func() {
		if err := c.client.SendMessage(subCtx, sid, system, c.modelRef, c.userMsg); err != nil {
			sendCh <- err
			return
		}
		sendCh <- nil
	}()

	var reply strings.Builder
	var reasoning []string
	sent := false
	// The handshake bound (ORCHICON_ASK_TIMEOUT) starts after subscribe and
	// only fires while the message is still un-accepted, so a wedged serve
	// that never acknowledges the send fails fast; the reply window bounds
	// the whole turn (and a serve that accepts but never replies).
	handshake := time.NewTimer(askTimeout())
	defer handshake.Stop()

	for {
		select {
		case <-subCtx.Done():
			// User stop (registry cancel) or the request-context's
			// cancellation — the turn ends without a reply.
			return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: subCtx.Err()}
		case <-window.C:
			return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("reply timed out after %s — the model may be overloaded or unavailable", askReplyWindow())}
		case <-handshake.C:
			if !sent {
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("the opencode serve did not accept the message within %s — please try again", askTimeout())}
			}
		case res := <-sendCh:
			if res != nil {
				if errors.Is(res, opencode.ErrSessionNotFound) && !recreated {
					// The serve no longer knows the session (data dir wiped):
					// recreate + re-seed the DB history and re-dispatch once.
					fresh, cerr := c.client.CreateSession(ctx, "ask-orchicon:"+c.convID)
					if cerr != nil {
						return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("recreate conversation session: %w", cerr)}
					}
					return turnAttemptResult{kind: turnRecreated, newSid: fresh}
				}
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("conversation session send: %w", res)}
			}
			sent = true
		case evt, ok := <-sub.Events():
			if !ok {
				// Bus closed — the serve died mid-reply. Re-attach (bounded
				// by the reply window in the collector loop).
				return turnAttemptResult{kind: turnReattach, reasoning: reasoning}
			}
			if esid, _ := evt.Properties["sessionID"].(string); esid != "" && esid != sid {
				continue
			}
			switch evt.Type {
			case "session.idle":
				// Turn complete — but only once OUR message was accepted
				// (sent). A stale idle from a prior turn (sent == false)
				// must never complete a new turn.
				if sent {
					return turnAttemptResult{kind: turnCollected, text: strings.TrimSpace(reply.String()), reasoning: reasoning}
				}
			case "permission.asked":
				// Auto-approve (the --auto equivalent). Session-level deny
				// rules mean this should rarely fire — defensive only.
				if pid, _ := evt.Properties["id"].(string); pid != "" {
					go func() { _ = c.client.ReplyPermission(subCtx, sid, pid) }()
				}
			case "session.error":
				// The turn failed at the model/API level: record it and end
				// the turn (the session is kept).
				msg := "opencode session error"
				if errObj, ok := evt.Properties["error"].(map[string]any); ok {
					if m, ok2 := errObj["message"].(string); ok2 && m != "" {
						msg = m
					}
				}
				s.log.Warn("opencode session error", "conversation", c.convID, "message", msg)
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: errors.New(msg)}
			default:
				// Telemetry: collect completed text and reasoning parts (the
				// same LegacyEventFromBus mapping executions use). Adjacent
				// parts are separated so distinct text parts don't
				// concatenate without a boundary. Reasoning is accumulated
				// separately — never folded into assistant content (matching
				// executions). Events observed BEFORE our message was
				// accepted (sent == false) belong to a prior turn still
				// draining on the shared bus — they must not leak into this
				// turn's persisted reply.
				if !sent {
					continue
				}
				if legacy, ok := opencode.LegacyEventFromBus(evt); ok {
					t, _ := legacy["type"].(string)
					part, _ := legacy["part"].(map[string]any)
					switch t {
					case "text":
						if text, ok2 := part["text"].(string); ok2 && text != "" {
							reply.WriteString(text)
							reply.WriteString("\n\n")
						}
					case "reasoning":
						if text, ok2 := part["text"].(string); ok2 && text != "" {
							reasoning = append(reasoning, text)
						}
					}
				}
			}
		}
	}
}

// persistConversationReply persists the collected assistant message for a
// turn under the acked message id and bumps the conversation timestamp. An
// error turn persists empty content with metadata.error set (surfaced as an
// error bubble with a retry affordance by the frontend). reasoning carries
// the reasoning chunks that arrived during the turn (possibly partial on an
// error/stop/timeout turn) and is persisted as-is on the assistant message.
// It runs on the detached context (never the request's). Fail-safe: if the
// conversation was deleted while the turn ran, the write is skipped (no
// orphan row).
func (s *Service) persistConversationReply(ctx context.Context, tenantID, convID, assistantMsgID, modelRef, content, sid, errText string, reasoning []string) {
	if s.pool == nil {
		return
	}
	metadata := map[string]any{"model_ref": modelRef}
	if sid != "" {
		metadata["session_id"] = sid
	}
	if errText != "" {
		metadata["error"] = errText
	}
	metaJSON, _ := json.Marshal(metadata)

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		s.log.Warn("persist conversation reply: begin tx", "conversation", convID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if _, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID); err != nil {
		s.log.Info("conversation gone, dropping turn reply", "conversation", convID)
		return
	}
	assistantMsg := db.MessageRow{
		ID:             assistantMsgID,
		TenantID:       tenantID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        content,
		ToolCalls:      []byte("[]"),
		ToolResults:    []byte("[]"),
		Attachments:    []byte("[]"),
		Metadata:       metaJSON,
		Reasoning:      reasoning,
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, assistantMsg); err != nil {
		s.log.Warn("persist conversation reply", "conversation", convID, "error", err)
		return
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, convID); err != nil {
		s.log.Warn("persist conversation reply: timestamp", "conversation", convID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Warn("persist conversation reply: commit", "conversation", convID, "error", err)
	}
}

// runOpenCodeTurn drives one chat turn over a persistent opencode session
// on the host serve (the session transport):
//
//   - first message (or a pre-migration conversation that never chatted):
//     CreateSession (directory-less) + persist the id immediately;
//   - follow-ups: prompt_async (SendMessage) on the SAME session — the goal
//     is NOT reset, history lives in the session;
//   - the send runs concurrently with the SSE drain so events arriving
//     between subscribe and the serve accepting our message are observed
//     while sent == false and ignored;
//   - turn complete = the first session.idle AFTER our message was accepted
//     (the `sent` guard ignores stale idle events from a prior turn);
//   - a follow-up send that 404s (serve data dir wiped / session gone)
//     recreates a fresh session, persists the new id, and re-seeds the DB
//     history so the durable record saves the conversation;
//   - timeout (askTimeout) and client disconnect both Abort the turn while
//     keeping the session for the next message.
//
// seedSystem is the system prompt for a freshly created session (includes
// the DB history block); reuseSystem is the steady-state follow-up system
// (no history — it already lives in the session). Returns the assistant
// message id, the (possibly recreated) session id, and the elapsed time.
func (s *Service) runOpenCodeTurn(ctx context.Context, client sessionTurnClient, tenantID, convID, sessionID, modelRef, seedSystem, reuseSystem, userMsg string, cb streamCallback) (msgID, newSessionID string, elapsed time.Duration, err error) {
	start := time.Now()
	msgID = db.NewID()

	// Resolve the session: reuse the persisted id, or create a fresh one
	// (first message). A fresh session gets the seeded system prompt (DB
	// history included); a live session gets the reuse variant.
	sid := sessionID
	system := reuseSystem
	if sid == "" {
		sid, err = client.CreateSession(ctx, "ask-orchicon:"+convID)
		if err != nil {
			return msgID, "", time.Since(start), fmt.Errorf("create conversation session: %w", err)
		}
		system = seedSystem
		s.persistConversationSessionID(ctx, tenantID, convID, sid)
	}

	// Subscribe BEFORE send so early text chunks aren't missed.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sub, err := client.Subscribe(subCtx)
	if err != nil {
		return msgID, sid, time.Since(start), fmt.Errorf("conversation session subscribe: %w", err)
	}
	defer sub.Close()

	// Send the user message while the drain loop below is already live. The
	// send runs on its own goroutine so events that arrive between subscribe
	// and the serve accepting our message (e.g. a stale session.idle from a
	// prior turn still draining from the bus) are observed by the drain loop
	// with sent == false and ignored. A 404 on a REUSED session means the
	// serve no longer knows it (data dir wiped / restarted against a fresh
	// store): recreate + persist + seed history, then retry once on the
	// fresh session (the subscription streams ALL sessions, so it needs no
	// reset).
	recreated := false
	type sendResult struct {
		err error
	}
	sendCh := make(chan sendResult, 1)
	go func() {
		for {
			if err := client.SendMessage(subCtx, sid, system, modelRef, userMsg); err != nil {
				if errors.Is(err, opencode.ErrSessionNotFound) && sessionID != "" && !recreated {
					s.log.Info("conversation session lost on serve — recreating", "conversation", convID, "session", sid)
					fresh, cerr := client.CreateSession(ctx, "ask-orchicon:"+convID)
					if cerr != nil {
						sendCh <- sendResult{err: fmt.Errorf("recreate conversation session: %w", cerr)}
						return
					}
					sid = fresh
					system = seedSystem
					recreated = true
					s.persistConversationSessionID(ctx, tenantID, convID, sid)
					continue
				}
				sendCh <- sendResult{err: err}
				return
			}
			sendCh <- sendResult{}
			return
		}
	}()

	sent := false
	timeout := time.NewTimer(askTimeout())
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected (Stop button / browser close): abort the
			// turn so the model stops burning tokens answering a gone
			// client; the session is preserved for the next message.
			_ = client.Abort(context.WithoutCancel(ctx), sid)
			return msgID, sid, time.Since(start), ctx.Err()
		case <-timeout.C:
			_ = client.Abort(context.WithoutCancel(ctx), sid)
			return msgID, sid, time.Since(start), fmt.Errorf("request timed out after %s — the model may be overloaded or unavailable", askTimeout())
		case res := <-sendCh:
			// Our message was accepted (or rejected). A rejected send is
			// terminal — no turn is running, so nothing further can arrive
			// for this request.
			if res.err != nil {
				return msgID, sid, time.Since(start), fmt.Errorf("conversation session send: %w", res.err)
			}
			sent = true
		case evt, ok := <-sub.Events():
			if !ok {
				return msgID, sid, time.Since(start), fmt.Errorf("opencode session stream ended")
			}
			if esid, _ := evt.Properties["sessionID"].(string); esid != "" && esid != sid {
				continue
			}
			switch evt.Type {
			case "session.idle":
				// Turn complete — but only once OUR message was accepted
				// (sent). A stale idle from a prior turn (sent == false)
				// must never complete a new turn.
				if sent {
					return msgID, sid, time.Since(start), nil
				}
			case "permission.asked":
				// Auto-approve (the --auto equivalent). Session-level deny
				// rules mean this should rarely fire — defensive only.
				if pid, _ := evt.Properties["id"].(string); pid != "" {
					go func() { _ = client.ReplyPermission(context.WithoutCancel(ctx), sid, pid) }()
				}
			case "session.error":
				// The turn failed at the model/API level: record it and end
				// the turn with an error chunk (the session is kept).
				msg := "opencode session error"
				if errObj, ok := evt.Properties["error"].(map[string]any); ok {
					if m, ok2 := errObj["message"].(string); ok2 && m != "" {
						msg = m
					}
				}
				s.log.Warn("opencode session error", "conversation", convID, "message", msg)
				return msgID, sid, time.Since(start), errors.New(msg)
			default:
				// Telemetry (text / tool_use / step / reasoning): feed the
				// SAME mapping executions use (LegacyEventFromBus) into the
				// chat's callback.
				if legacy, ok := opencode.LegacyEventFromBus(evt); ok {
					var part map[string]any
					if p, ok2 := legacy["part"].(map[string]any); ok2 {
						part = p
					}
					etype, _ := legacy["type"].(string)
					if etype == "" {
						continue
					}
					if err := cb(opencodeEvent{Type: etype, Part: part}); err != nil {
						// stream.Send failed — the client is gone. Abort the
						// turn and stop streaming (the partial response is
						// still persisted).
						_ = client.Abort(context.WithoutCancel(ctx), sid)
						return msgID, sid, time.Since(start), nil
					}
				}
			}
		}
	}
}

// fetchProjectContext returns a compact, current list of the tenant's
// enabled projects (title, status, directory, goals) for injection into the
// prompt, so the agent always knows what it operates on. Best-effort — a
// failure yields an empty string rather than blocking the conversation.
func (s *Service) fetchProjectContext(ctx context.Context, tenantID string) string {
	raw, err := s.toolRegistry.Execute(ctx, s.pool, "list_projects", nil)
	if err != nil {
		return ""
	}
	var projects []map[string]any
	if err := json.Unmarshal(raw, &projects); err != nil || len(projects) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range projects {
		name, _ := p["Name"].(string)
		if name == "" {
			name, _ = p["name"].(string)
		}
		status, _ := p["Status"].(string)
		if status == "" {
			status, _ = p["status"].(string)
		}
		dir, _ := p["ProjectDir"].(string)
		if dir == "" {
			dir, _ = p["project_dir"].(string)
		}
		goals, _ := p["Goals"].(string)
		if goals == "" {
			goals, _ = p["goals"].(string)
		}
		id, _ := p["ID"].(string)
		if id == "" {
			id, _ = p["id"].(string)
		}
		b.WriteString(fmt.Sprintf("- %s (ID %s, status %s", name, id, status))
		if dir != "" {
			b.WriteString(fmt.Sprintf(", dir %s", dir))
		}
		b.WriteString(")")
		if goals != "" {
			g := goals
			if len(g) > 160 {
				g = g[:160] + "..."
			}
			b.WriteString(fmt.Sprintf(" — %s", g))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *Service) modelRefOrFallback(ctx context.Context, tenantID, convModelRef string) string {
	if convModelRef != "" {
		return convModelRef
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return ""
	}
	defer ttx.Rollback(ctx)
	settings, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return ""
	}
	return settings.DefaultAskOrchiconModel
}
