package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/audit"
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
const defaultHandshakeTimeout = 60 * time.Second

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

// defaultTurnMaxAge is the hard upper bound on how long a chat turn may live
// in the registry before the background sweeper evicts it (cancelling the
// collector with errTurnExpired and aborting the serve session). It is the
// "every turn has a hard backstop" guarantee for chat: a collector that can
// never finalize (wedged serve, lost goroutine) is reaped in bounded time
// instead of blocking the conversation forever. The reply window covers the
// normal case, so the TTL is reply window + a generous margin (the sweeper
// tick granularity + re-attach slack). Env override ORCHICON_ASK_TURN_MAX_AGE
// is a dev/test knob.
const defaultTurnMaxAge = 31 * time.Minute

func askTurnMaxAge() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_TURN_MAX_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultTurnMaxAge
}

// Sweeper tick: how often the background sweeper scans the turn registry for
// expired turns. Fixed at one minute; the TTL is computed with this slack in
// mind. Override via ORCHICON_ASK_SWEEP_INTERVAL (dev/test knob).
const defaultSweepInterval = time.Minute

func askSweepInterval() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultSweepInterval
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

// defaultAskMaxConcurrentTurns bounds how many Ask Orchicon turns may run
// concurrently across all conversations (ADR-0002 D7 — session admission
// bound). A turn would exceed the cap is rejected with a clear
// CodeResourceExhausted error instead of degrading under contention on the
// shared serve bus + connection pool. Env override
// ORCHICON_ASK_MAX_CONCURRENT_TURNS.
const defaultAskMaxConcurrentTurns = 16

func askMaxConcurrentTurns() int {
	if v := os.Getenv("ORCHICON_ASK_MAX_CONCURRENT_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultAskMaxConcurrentTurns
}

// opencodeEvent is a single JSON event from opencode's stdout.
type opencodeEvent struct {
	Type string         `json:"type"`
	Part map[string]any `json:"part"`
}

// streamCallback is called for each streaming event from opencode.
type streamCallback func(evt opencodeEvent) error

// Turn-cancellation causes. These are the distinct reasons a turn's
// collector context is cancelled; the collector finalizes differently per
// cause (ADR-ASK-3):
//
//   - errUserStop: the Stop button — persist "Turn stopped by the user."
//   - errTurnSuperseded: an interjection took over the conversation — persist
//     the partial content as a PLAIN message (no error bubble) or skip the
//     write entirely when nothing arrived.
//   - errTurnExpired: the TTL sweeper reaped the turn — persist an error.
//
// The collector returns context.Cause(subCtx) (never subCtx.Err()) so the
// caller can distinguish them.
var (
	errUserStop       = errors.New("turn stopped by the user")
	errTurnSuperseded = errors.New("turn superseded by an interjection")
	errTurnExpired    = errors.New("turn expired without completing")
)

// turnEntry is one in-flight turn in the registry.
type turnEntry struct {
	// cancel fires the collector's cancellation with an explicit cause
	// (Stop / supersede / expiry).
	cancel context.CancelCauseFunc
	// token uniquely identifies this turn generation. remove() only deletes
	// the entry when the stored token matches the caller's, so a superseded
	// collector's deferred remove can never clobber the replacement turn.
	token uint64
	// started is when the turn was registered; the sweeper evicts entries
	// older than the TTL.
	started time.Time
	// tenant is the turn's tenant, needed by the sweeper to load the
	// conversation row (RLS) for the serve-session abort.
	tenant string
	// assistantMsgID is the acked assistant message id under which this turn's
	// reply (or error) will be persisted. Surfaced to the frontend via
	// Conversation.pending_assistant_message_id so a refreshed page can
	// re-attach to the running turn (Stop + completion poll), not just know
	// that one exists.
	assistantMsgID string
	// wedged records that this turn's session wedged on an unresolved tool call
	// (MCP wedge, AC1). Read by InterjectConversationTurn so a mid-run
	// interjection recycles a wedged session to a fresh one rather than
	// dispatching onto (or queuing behind) the wedged turn (ADr-0002 D4).
	wedged bool
	// lastActivity is when the running turn last produced output (token,
	// reasoning, step, or tool activity). Updated by the collector on each
	// activity signal; read by turnStatus to compute the server-confirmed
	// turn_progressing / turn_last_activity_at the frontend uses to show an
	// accurate "still working" vs "stalled" state after a refresh (AC2, D3).
	lastActivity time.Time
}

// turnRegistry tracks in-flight Ask Orchicon turns (keyed by conversation
// id) with their collector cancellation functions. It serves three purposes:
//
//   - the one-turn-per-conversation gate: a second send while a turn is
//     pending is rejected with FailedPrecondition (the frontend also
//     disables the input, but the gate is the server-side backstop);
//   - the Stop path: AbortConversationTurn cancels the collector so it
//     finalizes promptly and persists the user-initiated-stop error, rather
//     than sitting idle waiting the full reply window for an idle the serve
//     may not emit after abort;
//   - the no-orphan guarantee: every entry carries a started timestamp and
//     the background sweeper evicts (cancels + removes) entries older than
//     the TTL, so a wedged collector can never block a conversation forever.
//
// Token-guarded removal (remove only when the token matches) eliminates the
// stale-finalize race once supersede exists: the superseding turn's register
// assigns a NEW token, so the superseded collector's deferred remove with its
// own token is a no-op on the replacement entry.
type turnRegistry struct {
	mu      sync.Mutex
	nextTok uint64
	turns   map[string]turnEntry
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{turns: make(map[string]turnEntry)}
}

// register records a turn's cancellation function for a conversation. It
// returns the caller's token and ok=true, or (0, false) when a turn is
// already in flight for that conversation. assistantMsgID is the acked
// assistant message id under which the reply will be persisted — the running
// turn's identity exposed to readers of the registry.
func (r *turnRegistry) register(convID, tenant, assistantMsgID string, cancel context.CancelCauseFunc) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.turns[convID]; ok {
		return 0, false
	}
	r.nextTok++
	token := r.nextTok
	r.turns[convID] = turnEntry{cancel: cancel, token: token, started: time.Now(), tenant: tenant, assistantMsgID: assistantMsgID, lastActivity: time.Now()}
	return token, true
}

// get reports whether a turn is in flight for a conversation and, if so,
// returns its entry (the caller uses entry.assistantMsgID to re-attach to the
// running turn). This is the server-side source of truth for "is this
// conversation busy right now" — the frontend reconciles its in-memory stream
// slot against it after a page refresh.
func (r *turnRegistry) get(convID string) (turnEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.turns[convID]
	return entry, ok
}

// len reports the number of conversations with an in-flight turn. Used by the
// admission bound (ADR-0002 D7) to reject dispatch beyond the concurrent-turn
// cap with a clear error instead of degrading under contention.
func (r *turnRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

// markWedged records that a turn's session wedged on an unresolved tool (AC1),
// keyed by the caller's token so a stale marker from a superseded turn never
// clobbers the replacement. Read by InterjectConversationTurn (D4).
func (r *turnRegistry) markWedged(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.wedged = true
		r.turns[convID] = entry
	}
}

// markActivity records that a running turn last produced output at now,
// advancing the server-confirmed turn_progressing signal the frontend reads to
// distinguish a genuinely-working turn from a stalled/wedged one (AC2, D3).
// Keyed by the caller's token so a stale marker from a superseded turn never
// clobbers the replacement.
func (r *turnRegistry) markActivity(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.lastActivity = time.Now()
		r.turns[convID] = entry
	}
}

// cancel fires the collector's cancellation for a conversation (if any) with
// the given cause and reports whether a turn was registered, returning its
// token. The entry is removed by the collector via remove() once it
// finalizes — unless the caller supersedes/expires it and removes it itself.
func (r *turnRegistry) cancel(convID string, cause error) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.turns[convID]
	if ok {
		entry.cancel(cause)
	}
	return entry.token, ok
}

// remove drops the registry entry for a conversation only when its token
// matches the caller's (called by the collector on finalize, success or
// error). A stale finalize from a superseded turn never clobbers the
// replacement turn's entry.
func (r *turnRegistry) remove(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		delete(r.turns, convID)
	}
}

// turnEviction is one turn evicted by the sweeper.
type turnEviction struct {
	convID string
	tenant string
}

// sweep evicts (cancels with errTurnExpired and removes) every entry older
// than maxAge and returns the evicted conversations (with their tenants) so
// the caller can abort their serve sessions (belt-and-suspenders on top of
// the stall monitor — a collector that finalizes normally never reaches the
// TTL).
func (r *turnRegistry) sweep(now time.Time, maxAge time.Duration) []turnEviction {
	r.mu.Lock()
	defer r.mu.Unlock()
	var evicted []turnEviction
	for id, entry := range r.turns {
		if now.Sub(entry.started) > maxAge {
			entry.cancel(errTurnExpired)
			delete(r.turns, id)
			evicted = append(evicted, turnEviction{convID: id, tenant: entry.tenant})
		}
	}
	return evicted
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

	assistantID, streamEventCh, err := s.startConversationTurn(ctx, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments)
	if err != nil {
		return err
	}
	return s.drainTurnStream(stream, assistantID, streamEventCh)
}

// InterjectConversationTurn is the chat equivalent of a worker-execution
// nudge, with an interrupt: it SUPERSEDES the conversation's in-flight turn
// (if any) and dispatches the interjection on a fresh turn. The superseded
// turn's collector is cancelled (its partial content is persisted as a plain
// assistant message — or dropped when empty), the conversation's opencode
// session is aborted so the model stops generating NOW, and the interjection
// is answered at the next turn boundary rather than queued behind a stuck
// turn. When nothing is running it behaves exactly like ChatStream
// (idempotent).
func (s *Service) InterjectConversationTurn(ctx context.Context, req *connect.Request[apiv1.InterjectConversationTurnRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
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

	assistantID, streamEventCh, err := s.startConversationTurnOpts(ctx, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments, turnDispatchOpts{supersede: true})
	if err != nil {
		return err
	}
	return s.drainTurnStream(stream, assistantID, streamEventCh)
}

// drainTurnStream acks a freshly-started turn with TurnStarted and then
// drains streaming events to the client until the channel closes (turn
// complete or error), which lets the RPC return and close the HTTP stream.
func (s *Service) drainTurnStream(stream *connect.ServerStream[apiv1.ChatStreamResponse], assistantID string, streamEventCh <-chan *apiv1.ChatStreamResponse) error {
	if err := stream.Send(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TurnStarted{
			TurnStarted: &apiv1.TurnStarted{AssistantMessageId: assistantID},
		},
	}); err != nil {
		return err
	}
	for resp := range streamEventCh {
		if err := stream.Send(resp); err != nil {
			break // client gone — stop draining
		}
	}
	return nil
}

// turnDispatchOpts carries the dispatch-mode switches for
// startConversationTurnOpts.
type turnDispatchOpts struct {
	// supersede makes the dispatch first interrupt any in-flight turn for
	// the conversation (interject semantics) instead of rejecting it.
	supersede bool
}

// startConversationTurn is ChatStream's core (extracted for testability): it
// persists the user message, registers the in-flight turn, and launches the
// detached reply collector. It returns the acked assistant message id under
// which the reply (or error) will be persisted. Errors are *connect.Error
// values with the right code (NotFound for a missing conversation,
// FailedPrecondition when a turn is already pending — one turn at a time).
func (s *Service) startConversationTurn(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput) (string, chan *apiv1.ChatStreamResponse, error) {
	return s.startConversationTurnOpts(ctx, tenantID, convID, msg, attachments, turnDispatchOpts{})
}

// startConversationTurnOpts is the shared dispatch core for ChatStream
// (supersede=false) and InterjectConversationTurn (supersede=true). With
// supersede, an in-flight turn is first interrupted: its collector is
// cancelled with errTurnSuperseded, its registry entry removed (token-guarded
// so the superseded collector's finalize cannot clobber the replacement), and
// the conversation's opencode session aborted so the model stops generating
// NOW — the interjection is answered at the next turn boundary rather than
// queued behind a stuck turn.
func (s *Service) startConversationTurnOpts(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput, opts turnDispatchOpts) (string, chan *apiv1.ChatStreamResponse, error) {
	// --- 0. Load the conversation. Needed up front: the persisted session
	// id for the supersede serve-abort, plus existence for NotFound. ---
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	ttx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return "", nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}

	// sessionIDOverride is the session the new turn dispatches on. Normally
	// the conversation's persisted session; set to "" below (forcing a fresh
	// seeded session) when the interject supersedes a WEDGED turn (D4) so the
	// interjection lands on a healthy session rather than the stuck one.
	useSessionID := conv.SessionID

	// --- 0.5. Supersede an in-flight turn (interject only). ---
	if opts.supersede {
		prevEntry, prevOk := s.turns.get(convID)
		if token, ok := s.turns.cancel(convID, errTurnSuperseded); ok {
			s.turns.remove(convID, token)
			s.log.Info("conversation turn superseded by interjection", "conversation", convID)
		}
		// Abort the serve session so the model stops generating NOW (the
		// same abort the Stop button uses). Best-effort; idempotent.
		if conv.SessionID != "" {
			if client := s.hostServeClient(); client != nil {
				_ = client.Abort(context.WithoutCancel(ctx), conv.SessionID)
			}
		}
		// D4 (mid-run interjection on a wedged session): the superseded turn
		// was wedged on an unresolved tool (MCP wedge) — its session is stuck,
		// so dispatching the interjection onto it would wedge the new turn
		// too. Force a fresh seeded session: the collector creates one (and
		// seeds the DB history) when sessionID is empty, so the interjection
		// is answered by a healthy session and never silently dropped.
		if prevOk && prevEntry.wedged {
			useSessionID = ""
			s.log.Warn("interjection recycling a wedged session to a fresh one", "conversation", convID, "old_session", conv.SessionID)
		}
	}

	// --- 0.8 Validate attachments (size/count caps — server is authoritative) ---
	if len(attachments) > 5 {
		return "", nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("too many attachments (max 5)"))
	}
	var totalBytes int
	for _, a := range attachments {
		if len(a.Data) > 10*1024*1024 {
			return "", nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("attachment %q too large (max 10MB)", a.Name))
		}
		totalBytes += len(a.Data)
	}
	if totalBytes > 20*1024*1024 {
		return "", nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("attachments too large (max 20MB total)"))
	}

	// --- 1. Register the turn. ---
	// The acked assistant message id is generated up front so the registry
	// entry can carry it — a refreshed page re-attaches to the running turn
	// via Conversation.pending_assistant_message_id, which is read from this
	// entry.
	assistantID := db.NewID()
	// D7 (session admission bound): reject dispatch beyond the concurrent-turn
	// cap with a clear CodeResourceExhausted error instead of degrading under
	// contention on the shared serve bus + connection pool. A re-dispatch onto
	// a conversation that ALREADY has a running turn is not counted against the
	// cap — it is handled by the one-turn gate below (register returns false).
	// Checked BEFORE the detached collector context is created so a rejected
	// dispatch never allocates an un-cancelled context (lostcancel).
	if _, running := s.turns.get(convID); !running && s.turns.len() >= askMaxConcurrentTurns() {
		return "", nil, connect.NewError(connect.CodeResourceExhausted,
			errors.New("too many Ask Orchicon conversations are processing right now — wait for a turn to finish and try again"))
	}
	// The turn is registered before the user message is persisted so a
	// rejected second send (one turn per conversation) never orphans a
	// persisted user message. The detached context keeps the collector alive
	// across a stream disconnect / tab close; only the turn registry's
	// cancellation (Stop / supersede / TTL expiry) ends it. On any error
	// below, the turn is released.
	detached := context.WithoutCancel(ctx)
	turnCtx, cancelTurn := context.WithCancelCause(detached)
	token, ok := s.turns.register(convID, tenantID, assistantID, cancelTurn)
	if !ok {
		return "", nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a reply is still in progress for this conversation — wait for it to complete or stop it first"))
	}
	releaseTurn := func() {
		cancelTurn(errUserStop)
		s.turns.remove(convID, token)
	}

	// --- 2. Persist user message (and title on first message). ---
	// Persist attachments as JSON so history + recreation carries them.
	attachmentsJSON := []byte("[]")
	if len(attachments) > 0 {
		if j, err := json.Marshal(attachments); err == nil {
			attachmentsJSON = j
		}
	}
	userMsg := db.MessageRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		ConversationID: convID,
		Role:           "user",
		Content:        msg,
		ToolCalls:      []byte("[]"),
		ToolResults:    []byte("[]"),
		Attachments:    attachmentsJSON,
		Metadata:       []byte("{}"),
		Reasoning:      []string{},
	}
	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, userMsg); err != nil {
		ttx.Rollback(ctx)
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, fmt.Errorf("save user message: %w", err))
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, convID); err != nil {
		ttx.Rollback(ctx)
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}
	if conv.Title == "" {
		title := msg
		if len(title) > 80 {
			title = title[:80]
		}
		db.UpdateConversationTitle(ctx, ttx.Tx, tenantID, convID, title)
	}
	// Audit the user send atomically with the message persistence (one
	// row per user action — covers ChatStream and InterjectConversationTurn,
	// which both dispatch through here). Message content is excluded from
	// the snapshot (echo-privacy / compact trail); the message id refs the
	// stored row.
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.message_sent", "conversation", convID,
		nil, audit.Snapshot(map[string]any{
			"message_id": userMsg.ID,
			"role":       "user",
			"mode":       conv.Mode,
			"superseded": opts.supersede,
		})); err != nil {
		ttx.Rollback(ctx)
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit conversation.message_sent: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}

	// --- 3. Resolve model and build prompts (DB-only, no serve
	// interaction — the reply is collected detached). ---
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	if modelRef == "" {
		// No model configured anywhere (conversation, tenant default): fall
		// back to the free model. This is the silent failure that turned
		// into "Ask Orchicon is stuck" for users — the free model is
		// rate-limited and wedges turns. Log it loudly so the operator
		// knows a model setting is missing.
		modelRef = "opencode/deepseek-v4-flash-free"
		s.log.Warn("ask orchicon using fallback free model — no model configured for conversation or tenant default",
			"conversation", convID, "tenant", tenantID, "model", modelRef)
	}

	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}
	prevMessages, _ := db.ListMessages(ctx, ttx.Tx, tenantID, convID, 50, "")
	cfg, _ := db.GetAgentConfig(ctx, ttx.Tx, tenantID)
	// The tenant's stall window for THIS turn: read at dispatch time so a
	// settings change applies to the next turn, and the collector's stall
	// monitor uses the same value the read path reports as turn_progressing.
	settings, _ := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
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

	// --- 4. Launch the detached reply collector and stream events to the
	// client. The stream channel is buffered so the collector never blocks.
	// When the channel closes (turn complete or error), the drain goroutine
	// exits, which lets ChatStream return, closing the HTTP stream. ---
	streamEventCh := make(chan *apiv1.ChatStreamResponse, 64)
	onStreamEvent := func(resp *apiv1.ChatStreamResponse) {
		select {
		case streamEventCh <- resp:
		default:
			// Channel full — drop event (client may be slow or gone).
		}
	}

	if client := s.hostServeClient(); client != nil {
		// Partial-reply mirror: the collector's onPartial callbacks feed a
		// throttled flusher that upserts the running turn's collected
		// text/reasoning under the ACKED assistant message id. A client that
		// lost the live stream (refresh, another tab/device) polls ListMessages
		// and watches the reply grow instead of a bare spinner. The flusher is
		// cancelled and drained BEFORE the finalize so the complete reply
		// always lands last (no stale partial can clobber it). Its own tiny
		// tenant tx keeps it off the collector's hot path.
		partialMu := &sync.Mutex{}
		partialDirty := false
		var partialText string
		var partialReasoning []string
		partialCtx, partialCancel := context.WithCancel(detached)
		partialDone := make(chan struct{})
		go func() {
			defer close(partialDone)
			for {
				partialMu.Lock()
				dirty := partialDirty
				text := partialText
				rsn := append([]string(nil), partialReasoning...)
				if dirty {
					partialDirty = false
				}
				partialMu.Unlock()
				if dirty {
					s.upsertPartialMessage(partialCtx, tenantID, convID, assistantID, modelRef, text, rsn)
					continue
				}
				select {
				case <-partialCtx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
		onPartial := func(text string, reasoning []string) {
			partialMu.Lock()
			partialText = text
			partialReasoning = append([]string(nil), reasoning...)
			partialDirty = true
			partialMu.Unlock()
		}

		go func() {
			reply, reasoning, sid, terr := s.collectConversationReply(turnCtx, turnCollectOpts{
				client:                  client,
				tenantID:                tenantID,
				convID:                  convID,
				token:                   token,
				assistantMsgID:          assistantID,
				sessionID:               useSessionID,
				modelRef:                modelRef,
				seedSystem:              seedSystem,
				reuseSystem:             reuseSystem,
				userMsg:                 msg,
				attachments:             attachments,
				onStreamEvent:           onStreamEvent,
				stallNoProgressSeconds:  settings.StallNoProgressWindowSeconds,
				onPartial:               onPartial,
			})
			// Drain the partial mirror before finalizing: stop the flusher,
			// wait for any in-flight write, then write whatever is still dirty
			// so the finalize below is the LAST write to the row.
			partialCancel()
			<-partialDone
			partialMu.Lock()
			dirty := partialDirty
			pText, pReasoning := partialText, append([]string(nil), partialReasoning...)
			partialMu.Unlock()
			if dirty {
				s.upsertPartialMessage(detached, tenantID, convID, assistantID, modelRef, pText, pReasoning)
			}
			// Cause-aware finalize (ADR-ASK-3). The collector returns
			// context.Cause on cancellation so Stop / supersede / expiry are
			// distinguished:
			//   - superseded: partial content persisted as a PLAIN assistant
			//     message (the interjection is intentional — no error bubble),
			//     skipped entirely when nothing arrived;
			//   - user stop: the friendly "Turn stopped by the user." error;
			//   - anything else: the error text (timeout, stall, expiry).
			// Reasoning chunks that arrived before the turn ended are always
			// persisted (partial reasoning is preserved, matching the
			// "partial content is preserved" spirit).
			if terr != nil {
				switch {
				case errors.Is(terr, errTurnSuperseded):
					if content := strings.TrimSpace(reply); content != "" {
						s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, content, sid, "", reasoning)
					}
				case errors.Is(terr, errUserStop):
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, "Turn stopped by the user.", reasoning)
				default:
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, terr.Error(), reasoning)
				}
			} else {
				s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, "", reasoning)
			}
			close(streamEventCh)
		}()
	} else {
		// No session transport (serve disabled / not started): fail the
		// turn fast with a clean, visible, retryable error message.
		releaseTurn()
		s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", conv.SessionID,
			"Ask Orchicon is temporarily unavailable — the opencode serve is starting. Please try again in a moment.", []string{})
		close(streamEventCh)
	}

	return assistantID, streamEventCh, nil
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

	if _, ok := s.turns.cancel(req.Msg.ConversationId, errUserStop); ok {
		s.log.Info("conversation turn aborted by user", "conversation", req.Msg.ConversationId)
	}

	// Best-effort serve abort of the session's running turn (idempotent; no
	// session or no running turn is a no-op). The session itself is kept.
	if conv.SessionID != "" {
		if client := s.hostServeClient(); client != nil {
			_ = client.Abort(ctx, conv.SessionID)
		}
	}
	// Audit the Stop action in its own short tx (Abort writes no row itself
	// — the durable "turn stopped" message is persisted by the detached
	// collector, which is excluded system churn).
	attx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err == nil {
		defer attx.Rollback(ctx)
		if err := recordAudit(ctx, attx.Tx, tenantID, "conversation.turn_aborted", "conversation", req.Msg.ConversationId,
			nil, audit.Snapshot(map[string]any{"stopped": true})); err == nil {
			_ = attx.Commit(ctx)
		}
	}
	return connect.NewResponse(&apiv1.AbortConversationTurnResponse{}), nil
}

// buildSystemPrompt assembles the per-message `system` prompt for the Ask
// Orchicon agent. It carries the mode's identity block (BuildSystemPrompt),
// the enabled-projects context, the tools list, and this message's
// attachments. mode selects the persona (brainstorm only — orchicon removed); it is the
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
		imageCount := 0
		for _, a := range attachments {
			if strings.HasPrefix(a.MimeType, "image/") {
				imageCount++
				b.WriteString(fmt.Sprintf("- Image: %s (%s, %d bytes) — forwarded as vision input to the model\n", a.Name, a.MimeType, len(a.Data)))
				continue
			}
			b.WriteString(fmt.Sprintf("File: %s (%s, %d bytes)\n", a.Name, a.MimeType, len(a.Data)))
			if strings.HasPrefix(a.MimeType, "text/") || strings.HasPrefix(a.MimeType, "application/json") || strings.HasSuffix(a.Name, ".md") {
				b.WriteString("```\n" + string(a.Data) + "\n```\n")
			}
		}
		if imageCount > 0 {
			b.WriteString(fmt.Sprintf("(%d image(s) sent as vision parts to the model)\n", imageCount))
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
	token          uint64
	assistantMsgID string
	sessionID      string
	modelRef       string
	seedSystem     string
	reuseSystem    string
	userMsg        string
	attachments    []*apiv1.AttachmentInput
	onStreamEvent  func(*apiv1.ChatStreamResponse)
	// stallNoProgressSeconds is the tenant's stall_no_progress_window_seconds
	// read at dispatch time (0 when unset). The turn's stall monitor resolves
	// its effective no-progress window from it, matching executions.
	stallNoProgressSeconds int64
	// onPartial mirrors the running turn's collected text/reasoning into the
	// acked assistant message row (throttled by the caller) so a client that
	// lost the live stream — refresh, another tab, another device — can watch
	// the reply grow via ListMessages instead of a bare spinner. The finalize
	// upserts the complete reply over the partial row. Nil when the dispatch
	// path has no partial mirror (e.g. the no-serve fast-fail).
	onPartial func(text string, reasoning []string)
}

// turnAttemptKind is the outcome of a single subscribe+send+drain attempt.
type turnAttemptKind int

const (
	turnCollected turnAttemptKind = iota // reply complete; text is final
	turnFailed                           // terminal error; err is set
	turnRecreated                        // session 404'd — a fresh seeded session was created
	turnReattach                         // bus lost after a live connection — retry after a backoff
	turnServeDown                        // the serve never accepted a connection this attempt
	turnToolWedge                        // a tool call was issued but never resolved — the session is wedged (recycle)
)

type turnAttemptResult struct {
	kind      turnAttemptKind
	text      string
	reasoning []string
	newSid    string
	// wedgeTool names the tool whose call wedged (set only for turnToolWedge).
	wedgeTool string
	err       error
}

// activeToolName returns the name of a tool call that is issued but not yet
// resolved, from a raw bus event. The serve emits a tool part with
// state.status "running"/"pending" (non-terminal) while the tool executes;
// LegacyEventFromBus drops these (it only maps completed/errored tools to
// "tool_use"), so the chat collector uses this to feed the stall monitor's
// tool-wedge signal (AC1). ok=false means the event is not an unresolved tool.
func activeToolName(evt opencode.BusEvent) (string, bool) {
	if evt.Type != "message.part.updated" {
		return "", false
	}
	props := evt.Properties
	part, _ := props["part"].(map[string]any)
	if part == nil {
		return "", false
	}
	if ptype, _ := part["type"].(string); ptype != "tool" {
		return "", false
	}
	state, _ := part["state"].(map[string]any)
	status, _ := state["status"].(string)
	if status == "completed" || status == "error" {
		return "", false
	}
	tool, _ := part["tool"].(string)
	if tool == "" {
		return "", false
	}
	return tool, true
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
	defer s.turns.remove(c.convID, c.token)

	sid = c.sessionID
	system := c.reuseSystem
	recreated := false
	// reconnects counts the bounded session recycles performed on an MCP
	// wedge. Bounded by ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS (D2) so a wedged
	// session is healed once (or a small bound) instead of looping.
	reconnects := 0

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
		case turnToolWedge:
			// A tool call was issued on the session but never resolved (MCP
			// wedge — AC1). Heal, don't fail: abort the wedged session, create
			// a FRESH seeded session, and re-dispatch the SAME user message
			// once (bounded by the reconnect budget). The reply window applies
			// and the turn continues transparently on a healthy session.
			// Record the wedge so a mid-run interjection (D4) recycles rather
			// than dispatching onto the (now-stuck) session.
			s.turns.markWedged(c.convID, c.token)
			if reconnects >= askMCPReconnectAttempts() {
				return res.text, reasoning, sid, fmt.Errorf("the conversation session wedged on a tool (%s) and could not be recovered after %d attempt(s) — please retry", res.wedgeTool, reconnects+1)
			}
			oldSid := sid
			reconnects++
			s.log.Warn("ask orchicon session wedged on a tool — recycling to a fresh session",
				"conversation", c.convID, "old_session", oldSid, "tool", res.wedgeTool, "reconnects", reconnects)
			// Interrupt the stuck model NOW (idempotent best-effort).
			_ = c.client.Abort(context.WithoutCancel(ctx), oldSid)
			fresh, cerr := c.client.CreateSession(ctx, "ask-orchicon:"+c.convID)
			if cerr != nil {
				return res.text, reasoning, sid, fmt.Errorf("recreate conversation session after tool wedge: %w", cerr)
			}
			sid = fresh
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
				return res.text, reasoning, sid, context.Cause(ctx)
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
					return res.text, reasoning, sid, context.Cause(ctx)
				case <-time.After(askReattachBackoff()):
				}
			} else {
				select {
				case <-ctx.Done():
					return res.text, reasoning, sid, context.Cause(ctx)
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
		var sendErr error
		// If attachments contain images/files, use the extended sender when available.
		if len(c.attachments) > 0 {
			if sc, ok := c.client.(*opencode.SessionClient); ok {
				parts := make([]opencode.AttachmentPart, 0, len(c.attachments))
				for _, a := range c.attachments {
					parts = append(parts, opencode.AttachmentPart{Name: a.Name, MimeType: a.MimeType, Data: a.Data})
				}
				sendErr = sc.SendMessageWithAttachments(subCtx, sid, system, c.modelRef, c.userMsg, parts)
			} else {
				sendErr = c.client.SendMessage(subCtx, sid, system, c.modelRef, c.userMsg)
			}
		} else {
			sendErr = c.client.SendMessage(subCtx, sid, system, c.modelRef, c.userMsg)
		}
		if sendErr != nil {
			sendCh <- sendErr
			return
		}
		sendCh <- nil
	}()

	var reply strings.Builder
	var reasoning []string
	// Live-delta buffers for the partial-reply mirror: completed parts are
	// authoritative (reply/reasoning), but between parts the model streams
	// token deltas — those stream into liveText/liveReasoning so a re-attached
	// client sees output grow live instead of freezing until the next part
	// completes. Each completed part RESETS its live buffer (the part's text
	// subsumes the deltas that built it, so the mirror never double-counts).
	var liveText strings.Builder
	var liveReasoning strings.Builder
	// lastMirror throttles how often the drain loop snapshots the live buffers
	// into the partial mirror (the flusher further throttles the DB writes).
	var lastMirror time.Time
	// mirrorSnapshot builds the live partial snapshot for onPartial:
	// authoritative completed parts + the delta tail. The reasoning tail is
	// appended as ONE growing entry (the frontend joins reasoning parts into a
	// single thinking bubble).
	mirrorSnapshot := func() (string, []string) {
		rsn := append([]string(nil), reasoning...)
		if liveReasoning.Len() > 0 {
			rsn = append(rsn, liveReasoning.String())
		}
		return reply.String() + liveText.String(), rsn
	}
	sent := false
	// The handshake bound (ORCHICON_ASK_TIMEOUT) starts after subscribe and
	// only fires while the message is still un-accepted, so a wedged serve
	// that never acknowledges the send fails fast; the reply window bounds
	// the whole turn (and a serve that accepts but never replies).
	handshake := time.NewTimer(askTimeout())
	defer handshake.Stop()

	// Stall detection (ADR-ASK-1): a model that hangs mid-generation (no
	// events) or loops on the same tool call trips the monitor, which aborts
	// the serve session NOW and fails the turn with a clear, retryable error
	// — instead of the user watching a spinner until the reply window. The
	// ticker polls on a bounded interval (≤30s, ≥1s) so a trip is detected
	// promptly; the monitor is fed only after sent == true (pre-accept
	// events belong to a prior turn draining on the shared bus).
	monitor := newChatStallMonitor(c.modelRef, c.stallNoProgressSeconds)
	stallTick := monitor.noProgressWindow
	if rw := monitor.repetitionWindow; rw < stallTick {
		stallTick = rw
	}
	if stallTick > 30*time.Second {
		stallTick = 30 * time.Second
	}
	if stallTick < time.Second {
		stallTick = time.Second
	}
	stallTicker := time.NewTicker(stallTick)
	defer stallTicker.Stop()

	for {
		select {
		case <-subCtx.Done():
			// Registry cancel (Stop / supersede / TTL expiry) or the
			// request-context's cancellation — the turn ends without a
			// reply. Return the CAUSE so the caller can distinguish the
			// finalize behaviour per cause, and carry the partial text (the
			// superseded turn's partial content is persisted as a plain
			// message).
			return turnAttemptResult{kind: turnFailed, text: reply.String(), reasoning: reasoning, err: context.Cause(subCtx)}
		case <-window.C:
			return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("reply timed out after %s on model %s — the model may be overloaded or unavailable. Check the Ask Orchicon model in Settings → Default models, then retry.", askReplyWindow(), c.modelRef)}
		case <-handshake.C:
			if !sent {
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("the opencode serve did not accept the message within %s — please try again", askTimeout())}
			}
		case <-stallTicker.C:
			// First, the AC1 MCP-wedge signal: a tool call issued but never
			// resolved. This is UNLIKE no_progress/repetition — it is healed
			// (the collector recycles to a fresh session and re-dispatches the
			// same message), not failed. The monitor's toolWedge is precise: a
			// slow tool that still streams activity never trips it.
			if tool, wedged := monitor.toolWedge(); wedged {
				s.log.Warn("ask orchicon turn wedged on a tool call", "conversation", c.convID, "session", sid, "model", c.modelRef, "tool", tool)
				return turnAttemptResult{kind: turnToolWedge, reasoning: reasoning, wedgeTool: tool, err: fmt.Errorf("tool %s did not respond", tool)}
			}
			if reason := monitor.stallReason(); reason != "" {
				// The model has stopped making progress: interrupt it NOW
				// (the same abort the Stop button uses) and fail the turn
				// with a clear, retryable message that names the model —
				// a rate-limited or unavailable provider looks exactly
				// like a "stuck" model to the user.
				_ = c.client.Abort(context.WithoutCancel(subCtx), sid)
				s.log.Warn("ask orchicon turn stalled", "conversation", c.convID, "session", sid, "model", c.modelRef, "reason", reason)
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("The model (%s) stopped responding (%s). This is often a provider/model issue (rate limit, quota, or an unavailable model). Check the Ask Orchicon model in Settings → Default models, then retry.", c.modelRef, reason)}
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
				// Mid-generation token deltas are liveness evidence: feed
				// them to the stall monitor so a long, slow generation on
				// a local model resets the no_progress clock instead of
				// false-tripping the stall. Deltas must NOT flow into the
				// durable reply text / reasoning / stream events (completed
				// parts carry the durable record — TokenDeltaFromBus).
				if delta, kind, ok := opencode.TokenDeltaInfoFromBus(evt); ok {
					// observe("text") resets lastActivity — for "text" the
					// monitor ignores the part, so pass nil (no per-delta
					// allocation).
					monitor.observe("text", nil)
					s.turns.markActivity(c.convID, c.token)
					// Mirror the delta into the live partial row (throttled):
					// completed parts alone would freeze the mirror between
					// parts, so a re-attached client would see NO output while
					// the model streams a long part — the exact 'nothing until
					// the final message' report. Reasoning deltas grow the
					// thinking tail; text (or unknown-kind) deltas stream as
					// text. The finalize overwrites the row with the
					// authoritative reply, so deltas never corrupt it.
					if kind == "reasoning" {
						liveReasoning.WriteString(delta)
					} else {
						liveText.WriteString(delta)
					}
					if c.onPartial != nil && time.Since(lastMirror) >= 200*time.Millisecond {
						lastMirror = time.Now()
						snapText, snapRsn := mirrorSnapshot()
						c.onPartial(snapText, snapRsn)
					}
					continue
				}
				// A tool call ISSUED but not yet resolved (AC1 MCP-wedge). The
				// serve emits a tool part with a non-terminal status before the
				// tool resolves; LegacyEventFromBus drops these (it only maps
				// completed/errored tools to "tool_use"), so a wedged tool
				// would otherwise be invisible to the stall monitor. Feed an
				// explicit tool start so the monitor can detect the wedge.
				if tool, ok2 := activeToolName(evt); ok2 {
					monitor.observeToolStart(tool)
					s.turns.markActivity(c.convID, c.token)
					continue
				}
				if legacy, ok := opencode.LegacyEventFromBus(evt); ok {
					t, _ := legacy["type"].(string)
					part, _ := legacy["part"].(map[string]any)
					// Activity resets the stall clock: text, reasoning,
					// step_finish and tool_use all count as progress.
					monitor.observe(t, part)
					s.turns.markActivity(c.convID, c.token)
					switch t {
					case "text":
						if text, ok2 := part["text"].(string); ok2 && text != "" {
							reply.WriteString(text)
							reply.WriteString("\n\n")
							// The completed part subsumes the deltas that
							// built it — reset the live text tail so the
							// mirror doesn't double-count.
							liveText.Reset()
							if c.onPartial != nil {
								snapText, snapRsn := mirrorSnapshot()
								c.onPartial(snapText, snapRsn)
							}
							if c.onStreamEvent != nil {
								c.onStreamEvent(&apiv1.ChatStreamResponse{
									Event: &apiv1.ChatStreamResponse_TextChunk{
										TextChunk: &apiv1.TextChunk{Content: text},
									},
								})
							}
						}
					case "reasoning":
						if text, ok2 := part["text"].(string); ok2 && text != "" {
							reasoning = append(reasoning, text)
							liveReasoning.Reset()
							if c.onPartial != nil {
								snapText, snapRsn := mirrorSnapshot()
								c.onPartial(snapText, snapRsn)
							}
							if c.onStreamEvent != nil {
								c.onStreamEvent(&apiv1.ChatStreamResponse{
									Event: &apiv1.ChatStreamResponse_Reasoning{
										Reasoning: &apiv1.ReasoningChunk{Content: text},
									},
								})
							}
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
	if _, err := db.UpsertMessage(ctx, ttx.Tx, assistantMsg); err != nil {
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

// upsertPartialMessage writes the running turn's partial reply under the acked
// assistant message id (best-effort, its own tiny tenant tx so the collector's
// hot loop never blocks on the DB). Only visible while the turn is in flight:
// the finalize (persistConversationReply) upserts the complete reply over it.
func (s *Service) upsertPartialMessage(ctx context.Context, tenantID, convID, assistantMsgID, modelRef, content string, reasoning []string) {
	if s.pool == nil {
		return
	}
	metaJSON, _ := json.Marshal(map[string]any{"model_ref": modelRef})
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		s.log.Warn("upsert partial message: begin tx", "conversation", convID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if _, err := db.UpsertMessage(ctx, ttx.Tx, db.MessageRow{
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
	}); err != nil {
		s.log.Warn("upsert partial message", "conversation", convID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		s.log.Warn("upsert partial message: commit", "conversation", convID, "error", err)
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
