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
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
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
	return s.drainTurnStream(stream, req.Msg.ConversationId, assistantID, streamEventCh)
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
	return s.drainTurnStream(stream, req.Msg.ConversationId, assistantID, streamEventCh)
}

// WatchTurnStream re-attaches a dropped socket to an ACKED turn's live
// event stream WITHOUT dispatching a new turn. It validates the
// conversation's registry entry AND the assistant message id (a supersede
// replaces both — a stale watcher gets NotFound, never another turn's
// chunks), subscribes to the conversation's broadcast hub, and drains until
// the hub closes (turn finalized/superseded) or the watcher disconnects.
func (s *Service) WatchTurnStream(ctx context.Context, req *connect.Request[apiv1.WatchTurnStreamRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ConversationId == "" || req.Msg.AssistantMessageId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("conversation_id and assistant_message_id must not be empty"))
	}
	entry, running := s.turns.get(req.Msg.ConversationId)
	if !running || entry.assistantMsgID != req.Msg.AssistantMessageId || entry.tenant != tenantID {
		return connect.NewError(connect.CodeNotFound, errors.New("no running turn for this message — the turn finished or was superseded"))
	}
	h, found := s.hubs.get(req.Msg.ConversationId)
	if !found {
		return connect.NewError(connect.CodeNotFound, errors.New("no live stream for this turn — the turn finished or was superseded"))
	}
	s.log.Info("ask orchicon watch re-attached to running turn", "conversation", req.Msg.ConversationId, "assistant_message", req.Msg.AssistantMessageId)
	subID, ch := h.subscribe()
	defer h.unsubscribe(subID)
	for {
		select {
		case <-ctx.Done():
			return nil
		case resp, ok := <-ch:
			if !ok {
				return nil // hub closed: turn finalized or superseded
			}
			if err := stream.Send(resp); err != nil {
				s.log.Warn("ask orchicon watch stream send failed", "conversation", req.Msg.ConversationId, "assistant_message", req.Msg.AssistantMessageId, "cause", "watcher-gone-send-fail")
				return nil
			}
		}
	}
}

// drainTurnStream acks a freshly-started turn with TurnStarted and then
// drains streaming events to the client until the channel closes (turn
// complete or error), which lets the RPC return and close the HTTP stream.
//
// A ≤20s heartbeat ticker emits Heartbeat keepalives while the turn runs
// but produces no TextChunk/ReasoningChunk (reasoning-heavy silent phases
// trip proxy/browser idle timeouts otherwise — the provider-agnostic drop).
// Heartbeats carry no content; the frontend ignores them for rendering but
// treats them as socket-liveness proof. Every drained response (chunks AND
// heartbeats) is also published to the conversation's broadcast hub so a
// dropped socket can re-dial via WatchTurnStream.
func (s *Service) drainTurnStream(stream *connect.ServerStream[apiv1.ChatStreamResponse], convID, assistantID string, streamEventCh <-chan *apiv1.ChatStreamResponse) error {
	if err := stream.Send(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TurnStarted{
			TurnStarted: &apiv1.TurnStarted{AssistantMessageId: assistantID},
		},
	}); err != nil {
		s.log.Warn("ask orchicon chatstream send failed", "conversation", convID, "assistant_message", assistantID, "cause", "client-gone-send-fail")
		return err
	}
	heartbeat := time.NewTicker(askHeartbeatInterval())
	defer heartbeat.Stop()
	send := func(resp *apiv1.ChatStreamResponse) error {
		if err := stream.Send(resp); err != nil {
			s.log.Warn("ask orchicon chatstream send failed", "conversation", convID, "assistant_message", assistantID, "cause", "client-gone-send-fail")
			return err
		}
		return nil
	}
	for {
		select {
		case resp, ok := <-streamEventCh:
			if !ok {
				return nil
			}
			if h, found := s.hubs.get(convID); found {
				h.publish(resp)
			}
			if err := send(resp); err != nil {
				return nil // client gone — stop draining, turn runs on
			}
		case <-heartbeat.C:
			hb := &apiv1.ChatStreamResponse{
				Event: &apiv1.ChatStreamResponse_Heartbeat{
					Heartbeat: &apiv1.Heartbeat{ServerTimeUnixMs: time.Now().UnixMilli()},
				},
			}
			if h, found := s.hubs.get(convID); found {
				h.publish(hb)
			}
			// A heartbeat that fails to send IS the drop signal
			// (idle/proxy timeout or gone client): log it distinctly
			// and stop draining — the turn continues server-side
			// and the client re-dials via WatchTurnStream.
			if err := send(hb); err != nil {
				s.log.Warn("ask orchicon chatstream heartbeat send failed", "conversation", convID, "assistant_message", assistantID, "cause", "idle-timeout-or-client-gone")
				return nil
			}
		}
	}
}

// askHeartbeatInterval is the ChatStream keepalive cadence (≤20s): wire
// traffic during silent generation so idle timeouts never mistake a
// healthy-but-quiet turn for a dead socket. Env override
// ORCHICON_ASK_HEARTBEAT_INTERVAL is a dev/test knob.
func askHeartbeatInterval() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Second
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
		// same abort the Stop button uses). Best-effort; idempotent. Routed
		// through the conversation's resolved adapter so a non-opencode
		// adapter aborts its own session (no opencode hardcoding).
		if conv.SessionID != "" {
			if client := s.resolveClientForAbort(conv.ModelRef); client != nil {
				_ = client.AbortConversationSession(context.WithoutCancel(ctx), conv.SessionID)
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
	// The DB window is what a fresh session replays (the seed prompt) and what
	// the session-ownership check below reads: sanitize it so no assistant row
	// carrying tool_calls without matching tool_results can ever be replayed
	// to a provider (BUG: dangling tool_call replayed to the new provider).
	prevMessages = sanitizeHistoryRows(prevMessages)
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
	// exits, which lets ChatStream return, closing the HTTP stream.
	// A broadcast hub is (re)created for the conversation alongside the
	// turn: every drained response is published there too, so a dropped
	// socket re-dials the SAME turn via WatchTurnStream. Supersede replaces
	// the hub so stale watchers drain and fall back to the poll. ---
	s.hubs.create(convID)
	streamEventCh := make(chan *apiv1.ChatStreamResponse, 64)
	onStreamEvent := func(resp *apiv1.ChatStreamResponse) {
		select {
		case streamEventCh <- resp:
		default:
			// Channel full — drop event (client may be slow or gone).
		}
	}

	// The conversation's adapter is resolved through the shared Dispatcher
	// by its model_ref kind (ADR-0003 §3/§5). A nil dispatcher (tests /
	// pre-wiring) falls back to the host opencode serve client. A resolution
	// failure (unknown kind / adapter lacks the chat capability) is surfaced
	// as a clean, retryable turn failure — never as a silent opencode fallback
	// (the AC routing landmine).
	if client, cerr := s.resolveChatClient(convID, modelRef); cerr != nil {
		// The turn was already registered above: release it so the
		// conversation is not wedged behind a turn that will never run
		// (the TTL sweeper would otherwise hold it for up to 31 minutes).
		releaseTurn()
		s.hubs.remove(convID)
		return "", nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("Ask Orchicon could not resolve an adapter for this conversation: %w", cerr))
	} else if client != nil {
		// Adapter-scoped session identity (AC: cross-adapter switch). The
		// persisted conversation session id belongs to the adapter that
		// created it (the transport the model originally ran on). When the
		// CURRENT model_ref resolves to a DIFFERENT adapter, dispatching the
		// stored id to that adapter is a cross-adapter leak: a session-ful
		// adapter (opencode) rejects a foreign/unknown id with an opaque 500
		// (observed: native synthetic "orchicon-ask:<conv>" sent to opencode
		// serve → http 500 UnknownError). So before dispatching, if the
		// stored id is NOT owned by the resolved adapter, force a fresh
		// session on the new adapter (a non-empty calcSessionID will be
		// created by the collector's first-path; clearing useSessionID makes
		// the collector create + persist it). The native adapter is treated
		// as OWNS-ALL (it accepts any id), so reverse-direction
		// opencode→native switches keep working without a session reset.
		useSessionID = s.adapterScopedSessionID(client, useSessionID)
		// Model-change invalidation (BUG: model switch fails with a dangling
		// tool-call replay). A session is created under the model that built
		// it, and the stored one is replayed to whatever model runs the next
		// turn. When the conversation's model changed, reusing that session
		// hands the NEW provider history it did not produce — including a
		// dangling tool call from an interrupted turn. Force a fresh session
		// so the switch dispatches on sanitized history instead.
		if useSessionID != "" && sessionCreatedUnderDifferentModel(conv.SessionID, modelRef, prevMessages) {
			s.log.Warn("ask orchicon model change — creating a fresh session (the stored session belongs to the previous model)",
				"conversation", convID, "old_session", conv.SessionID, "model", modelRef)
			useSessionID = ""
		}
		// Partial-reply mirror: the collector's onPartial callbacks feed a
		// throttled flusher that upserts the running turn's collected
		// text/reasoning AND live tool ledger under the ACKED assistant message
		// id. A client that lost the live stream (refresh, another tab/device)
		// polls ListMessages and watches the reply grow instead of a bare
		// spinner — and a session killed mid-turn (timeout/abort/crash) leaves
		// everything up to the kill visible. The flusher is cancelled and
		// drained BEFORE the finalize so the complete reply always lands last
		// (no stale partial can clobber it). Its own tiny tenant tx keeps it
		// off the collector's hot path.
		// turnLedger is the turn's live tool ledger, shared with the collector
		// (pointer identity) and snapshotted by every mirror write and the
		// terminal finalize.
		turnLedger := newToolLedger()
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
					s.upsertPartialMessage(partialCtx, tenantID, convID, assistantID, modelRef, text, rsn, turnLedger)
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
				client:                 client,
				tenantID:               tenantID,
				convID:                 convID,
				token:                  token,
				assistantMsgID:         assistantID,
				sessionID:              useSessionID,
				modelRef:               modelRef,
				seedSystem:             seedSystem,
				reuseSystem:            reuseSystem,
				userMsg:                msg,
				attachments:            attachments,
				onStreamEvent:          onStreamEvent,
				stallNoProgressSeconds: settings.StallNoProgressWindowSeconds,
				onPartial:              onPartial,
				ledger:                 turnLedger,
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
				s.upsertPartialMessage(detached, tenantID, convID, assistantID, modelRef, pText, pReasoning, turnLedger)
			}
			// The terminal ledger snapshot travels with the finalize: the
			// collector owns the pointer, so read the final state from it
			// (every finalize path below persists text + reasoning + tools
			// atomically — a killed session leaves the tool activity, not
			// just the text). The superseded-empty case skips the write
			// entirely (nothing arrived: no row), matching prior behavior.
			finalLedger := turnLedger
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
						s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, content, sid, "", reasoning, finalLedger)
					} else if finalLedger.hasCalls() {
						// The turn was interrupted between a tool call and its
						// result: nothing arrived as text, but the partial mirror
						// already wrote the row. Finalize it with the REPAIRED ledger
						// rather than leaving an assistant row whose tool_calls have
						// no results (the dangling call the new provider rejects).
						s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, "", reasoning, finalLedger)
					}
				case errors.Is(terr, errUserStop):
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, "Turn stopped by the user.", reasoning, finalLedger)
				default:
					errText := terr.Error()
					// Surface the failure verbatim on the stream (the TUI dock
					// notice) as well as in the persisted error row the poll
					// renders — a provider 400 must reach the operator in the
					// provider's own words.
					emitTurnError(onStreamEvent, errText)
					if isDanglingToolCallProviderError(errText) {
						// The session that produced the rejection is poisoned: its
						// replayed history carries a tool call with no result. Drop
						// the session so the NEXT send creates a fresh one seeded
						// with sanitized history instead of replaying the poison
						// forever (the conversation must not wedge on a 400).
						s.log.Warn("ask orchicon dropping session after a dangling-tool-call provider rejection",
							"conversation", convID, "session", sid)
						s.persistConversationSessionID(detached, tenantID, convID, "")
					}
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, errText, reasoning, finalLedger)
				}
			} else {
				s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, "", reasoning, finalLedger)
			}
			close(streamEventCh)
		}()
	} else {
		// No session transport (serve disabled / not started): fail the
		// turn fast with a clean, visible, retryable error message.
		releaseTurn()
		s.hubs.remove(convID)
		s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", conv.SessionID,
			"Ask Orchicon is temporarily unavailable — the opencode serve is starting. Please try again in a moment.", []string{}, nil)
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
		if client := s.resolveClientForAbort(conv.ModelRef); client != nil {
			_ = client.AbortConversationSession(ctx, conv.SessionID)
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
	b.WriteString("Orchicon's tools are exposed to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation. The native file/shell suite (batch_read, batch_grep, batch_write, read, grep, write, edit, list, glob, bash, ask_file_root) is also on your session as native tools — it operates on the tenant's first active project_dir (ask_file_root reports it).\n\n")
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

// resolveChatClient resolves the ChatTurnClient capability for an Ask
// conversation from its model_ref adapter kind, routed through the shared
// Dispatcher (ADR-0003 §3/§5). It is the adapter-neutral transport swap that
// replaced the opencode-only hostServeClient: the Ask turn drives ANY
// registered adapter that implements the chat-session capability, with the
// default/opencode path behaving identically to before.
//
// A nil dispatcher (tests / pre-wiring) falls back to the host opencode serve
// client so behavior is unchanged when no adapter namespace is configured.
// An unknown adapter kind, or a registered adapter that does NOT implement
// the chat-session capability, yields an actionable error — never a hardcoded
// opencode fallback.
func (s *Service) resolveChatClient(convID, modelRef string) (scheduler.ChatTurnClient, error) {
	if s.dispatcher == nil {
		if c := s.testServeClient; c != nil {
			return c, nil
		}
		if c := s.hostServeClient(); c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("Ask Orchicon chat transport is unavailable")
	}
	kind := adapter.AdapterKind(modelRef)
	bridge, err := s.dispatcher.Resolve(kind)
	if err != nil {
		return nil, err
	}
	client, ok := bridge.(scheduler.ChatTurnClient)
	if !ok {
		return nil, fmt.Errorf("adapter kind %q is registered but does not support Ask chat", kind)
	}
	return client, nil
}

// resolveClientForAbort resolves a ChatTurnClient to abort a conversation's
// live turn (Stop / supersede / Delete / sweeper). It is best-effort: on a
// resolution failure (no dispatcher, unknown/missing kind) it returns nil and
// the caller skips the serve abort. This is the abort-path analogue to
// resolveChatClient but never fails the RPC — abort is idempotent and the
// durable record is already handled.
func (s *Service) resolveClientForAbort(modelRef string) scheduler.ChatTurnClient {
	if c, err := s.resolveChatClient("", modelRef); err == nil {
		return c
	}
	return nil
}

// adapterScopedSessionID enforces adapter-scoped conversation session
// identity across a mid-conversation model/adapter switch (WI-3). The
// persisted conversation session id belongs to the adapter that created it.
// When the current model_ref resolves to a DIFFERENT adapter, the stored id
// must not be dispatched to it: a session-ful adapter (opencode) rejects a
// foreign/unknown id with an opaque 500. This returns the session id the
// turn may actually use:
//
//   - an empty input stays empty (a fresh session is created by the collector);
//   - the native ("orchicon") adapter owns ALL ids it accepts (it is
//     deliberately sessionless) — so an opencode→native switch keeps the
//     stored session id and keeps working;
//   - any OTHER adapter (or a client that cannot report an owner kind) is
//     treated as not owning a native-synthetic id ("orchicon-ask:" prefix),
//     so that id is cleared and the collector creates a fresh session on the
//     newly-resolved adapter instead of dispatching a foreign id to it.
func (s *Service) adapterScopedSessionID(client scheduler.ChatTurnClient, sid string) string {
	if sid == "" {
		return ""
	}
	// The native bridge is OWNS-ALL: it is sessionless and accepts any id, so
	// the reverse direction (opencode→native) never resets the session.
	if owner, ok := client.(scheduler.SessionOwnerKind); ok && owner.SessionOwnerKind() == "orchicon" {
		return sid
	}
	// Any non-native adapter must never dispatch a native-synthetic id to its
	// serve — clear it so the collector creates a fresh session on this
	// adapter instead.
	if strings.HasPrefix(sid, scheduler.NativeSessionIDPrefix) {
		return ""
	}
	return sid
}

// hostServeClient returns the host serve's session client, or nil when the
// session transport is unavailable (serve disabled, not started, or failed).
// It is the legacy/nil-dispatcher fallback used by resolveChatClient and the
// abort paths. Tests may inject a fake via Service.testServeClient.
func (s *Service) hostServeClient() scheduler.ChatTurnClient {
	if s.testServeClient != nil {
		return s.testServeClient
	}
	return s.clientFromHostServe()
}

// clientFromHostServe adapts the opencode host serve's SessionClient onto the
// scheduler.ChatTurnClient capability used by the Ask path. It is the
// nil-dispatcher / legacy fallback so a plain host serve (no adapter
// namespace) keeps working. Returns nil when the serve is unavailable.
func (s *Service) clientFromHostServe() scheduler.ChatTurnClient {
	if s.hostServe == nil {
		return nil
	}
	return opencode.NewClientSessionAdapter(s.hostServe.Client())
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
	client         scheduler.ChatTurnClient
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
	// ledger is the turn's live tool ledger: tool starts/resolutions recorded
	// by the drain loop as they happen. Shared across re-attach attempts (the
	// pointer survives the collector loop) so serve loss never drops tool
	// history; the partial mirror snapshots it and the finalize persists it
	// terminally. Nil-safe (methods tolerate a nil receiver); the collector
	// lazy-inits it when the dispatch path did not provide one (unit tests).
	ledger *toolLedger
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
	defer s.hubs.remove(c.convID)

	sid = c.sessionID
	system := c.reuseSystem
	recreated := false
	// The live tool ledger survives re-attach attempts (one pointer shared by
	// every attempt of this turn) so serve loss mid-tool never drops tool
	// history. Lazy-init covers direct collector tests that build opts by hand.
	if c.ledger == nil {
		c.ledger = newToolLedger()
	}
	// reconnects counts the bounded session recycles performed on an MCP
	// wedge. Bounded by ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS (D2) so a wedged
	// session is healed once (or a small bound) instead of looping.
	reconnects := 0

	// A first message (no persisted session id) creates the session up front
	// and persists the id immediately; follow-ups reuse the persisted one. A
	// later 404 (serve data dir wiped) triggers exactly one more
	// recreation + DB-transcript re-seed inside the attempt loop.
	if sid == "" {
		fresh, cerr := c.client.CreateConversationSession(ctx, c.convID, "ask-orchicon:"+c.convID)
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
		// Diff pipeline (AC 4): a turn's terminal transition reconciles the
		// conversation's file-edit ledger against the project dir's git
		// state (best-effort — the hook owns its error posture and never
		// affects the turn result). Both collected and failed turns
		// reconcile: a failed turn still wrote files worth accounting for.
		if res.kind == turnCollected || res.kind == turnFailed {
			if s.fileEditReconciler != nil {
				s.fileEditReconciler(ctx, c.tenantID, c.convID)
			}
		}
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
			// (bounded by the reconnect budget). The reply window applies
			// and the turn continues transparently on a healthy session.
			// Record the wedge so a mid-run interjection (D4) recycles rather
			// than dispatching onto the (now-stuck) session.
			//
			// 2026-09-09 recycle-fix (operator: this path killed live Ask
			// sessions mid-merge with "wedged on a tool (bash) and could not
			// be recovered after 2 attempt(s)"): the recycle must NOT fail
			// the turn on the LAST budget slot. When the budget is spent,
			// DO NOT abort the session — the tool may still be running
			// legitimately on the serve (the wedge window can only
			// misfire on a slow-but-alive call) — and keep WAITING inside
			// the reply window instead: the turn stays alive, the tool's
			// completion (tool_use) still lands through the collector's
			// event stream, and the reply is preserved. A genuinely hung
			// session is bounded by the reply window, not by this gate.
			// The budget now only bounds how many ABORT+RE-DISPATCH
			// recycles we perform, never whether the turn may continue
			// listening.
			if reconnects >= askMCPReconnectAttempts() {
				s.log.Warn("ask orchicon tool-wedge recycle budget spent — continuing on the live session (turn stays alive; bounded by the reply window)",
					"conversation", c.convID, "session", sid, "tool", res.wedgeTool, "recycles", reconnects)
				// Re-enter the attempt loop WITHOUT aborting: the current
				// session keeps streaming; if the tool completes, the turn
				// completes. runOneTurnAttempt re-arms the monitor on a
				// fresh attempt; the reply window (askReplyWindow) bounds
				// the total lifetime, so this cannot loop forever.
				continue
			}
			s.turns.markWedged(c.convID, c.token)
			oldSid := sid
			reconnects++
			s.log.Warn("ask orchicon session wedged on a tool — recycling to a fresh session",
				"conversation", c.convID, "old_session", oldSid, "tool", res.wedgeTool, "reconnects", reconnects)
			// Interrupt the stuck model NOW (idempotent best-effort).
			_ = c.client.AbortConversationSession(context.WithoutCancel(ctx), oldSid)
			fresh, cerr := c.client.CreateConversationSession(ctx, c.convID, "ask-orchicon:"+c.convID)
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

	sub, err := c.client.Subscribe(subCtx, c.convID)
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
		// Attachments require the OPTIONAL attachment-aware capability. When
		// the adapter lacks it, the turn FAILS LOUDLY (never silently falls
		// back to text-only) — the AC routing landmine: a non-opencode client
		// must not drop attachments.
		if len(c.attachments) > 0 {
			acc, ok := c.client.(scheduler.SendTurnMessageWithAttachments)
			if !ok {
				sendCh <- fmt.Errorf("adapter for this conversation does not support attachments to Ask chat")
				return
			}
			parts := make([]scheduler.ChatAttachment, 0, len(c.attachments))
			for _, a := range c.attachments {
				parts = append(parts, scheduler.ChatAttachment{Name: a.Name, MimeType: a.MimeType, Data: a.Data})
			}
			sendCh <- acc.SendTurnMessageWithAttachments(subCtx, c.convID, sid, system, c.modelRef, c.userMsg, parts)
			return
		}
		sendCh <- c.client.SendTurnMessage(subCtx, c.convID, sid, system, c.modelRef, c.userMsg)
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
	// segThink demuxes folded think segments (GLM/DeepSeek) out of the TEXT
	// delta stream: think bodies route into the reasoning channel as ONE
	// growing entry, plain text grows liveText, and tag fragments / leaked
	// bodies never reach the mirror's text side or the authoritative reply.
	// Its state carries ACROSS deltas so a tag split into pieces is
	// recognized. It is REPLACED (reset) at the completed-text-part boundary
	// because the completed part subsumes the deltas that built it.
	segThink := thinkSegmenter{}
	// committedThink tracks bodies already committed to the durable reasoning
	// slice so a folded body that arrives via BOTH the live delta path and a
	// later completed text part is not double-counted. This is a dedupe of
	// the SAME folded body across the two demux channels only — it never
	// dedupes against native reasoning parts (they are distinct segments).
	committedThink := map[string]bool{}
	// lastMirror throttles how often the drain loop snapshots the live buffers
	// into the partial mirror (the flusher further throttles the DB writes).
	var lastMirror time.Time
	// Trailing flush for the throttled mirror: when a delta lands inside the
	// 200ms throttle window, flushTick is armed for the remainder so the
	// unflushed delta tail drains shortly after the LAST delta — a short
	// final burst (or the stream ending entirely) would otherwise freeze the
	// partial row on the previous snapshot until the turn finalizes (the
	// exact "nothing until the final message" symptom the mirror exists to
	// fix). The timer is armed/stopped ONLY on the drain loop's goroutine and
	// its expiry is consumed by the loop's select below, so mirrorSnapshot's
	// reads of the live buffers stay single-goroutine by construction (no
	// locks, no race with the finalize — the timer cannot fire after the
	// loop returns).
	flushTick := time.NewTimer(time.Hour)
	defer flushTick.Stop()
	flushArmed := false
	disarmFlush := func() {
		if !flushTick.Stop() {
			select {
			case <-flushTick.C:
			default:
			}
		}
		flushArmed = false
	}
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
	// commitThink appends a demuxed folded-think body to the durable reasoning
	// slice and emits it as a reasoning stream event. It is the SINGLE commit
	// authority for folded bodies: it is called by the live delta path (a
	// terminated folded body arriving in the text stream), by the completed-
	// text-part demux (a folded body carried by a completed part), and by the
	// terminal flush (an unterminated body at turn end). Each folded body is
	// committed exactly once — committedThink guards the subsumed case where a
	// delta-streamed body is REPEATED in the completed part text. This is NOT
	// a dedupe against native reasoning parts (those are distinct segments and
	// are always appended separately by the "reasoning" case below).
	commitThink := func(body string) {
		if body == "" {
			return
		}
		if committedThink[body] {
			return
		}
		committedThink[body] = true
		reasoning = append(reasoning, body)
		if c.onStreamEvent != nil {
			c.onStreamEvent(&apiv1.ChatStreamResponse{
				Event: &apiv1.ChatStreamResponse_Reasoning{
					Reasoning: &apiv1.ReasoningChunk{Content: body},
				},
			})
		}
	}
	// flushThinkDrain commits any unterminated folded-think body accumulated in
	// the live segmenter to the durable reasoning slice (provider truncation /
	// abort / supersede at turn end — the body must land in the reasoning
	// channel, NEVER in text). It also clears the live reasoning tail so a
	// committed body replaces its growing tail in the mirror (matching how a
	// completed reasoning part resets liveReasoning after appending to the
	// durable slice).
	flushThinkDrain := func() {
		segThink.flushBody(commitThink)
		liveReasoning.Reset()
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
			flushThinkDrain()
			return turnAttemptResult{kind: turnFailed, text: reply.String(), reasoning: reasoning, err: context.Cause(subCtx)}
		case <-window.C:
			flushThinkDrain()
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
				_ = c.client.AbortConversationSession(context.WithoutCancel(subCtx), sid)
				s.log.Warn("ask orchicon turn stalled", "conversation", c.convID, "session", sid, "model", c.modelRef, "reason", reason)
				flushThinkDrain()
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: fmt.Errorf("The model (%s) stopped responding (%s). This is often a provider/model issue (rate limit, quota, or an unavailable model). Check the Ask Orchicon model in Settings → Default models, then retry.", c.modelRef, reason)}
			}
		case <-flushTick.C:
			// Trailing mirror flush: the throttle window elapsed with an
			// unflushed delta tail. Drain it NOW on the drain loop's own
			// goroutine (single-goroutine snapshot reads by construction).
			flushArmed = false
			if c.onPartial != nil {
				lastMirror = time.Now()
				snapText, snapRsn := mirrorSnapshot()
				c.onPartial(snapText, snapRsn)
			}
		case res := <-sendCh:
			if res != nil {
				if errors.Is(res, scheduler.ErrSessionNotFound) && !recreated {
					// The serve no longer knows the session (data dir wiped):
					// recreate + re-seed the DB history and re-dispatch once.
					fresh, cerr := c.client.CreateConversationSession(ctx, c.convID, "ask-orchicon:"+c.convID)
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
				s.log.Warn("ask orchicon serve bus closed mid-turn", "conversation", c.convID, "session", sid, "cause", "sse-bus-close")
				flushThinkDrain()
				return turnAttemptResult{kind: turnReattach, reasoning: reasoning}
			}
			if evt.SessionID != "" && evt.SessionID != sid {
				continue
			}
			switch evt.Kind {
			case "idle":
				// Turn complete — but only once OUR message was accepted
				// (sent). A stale idle from a prior turn (sent == false)
				// must never complete a new turn.
				if sent {
					flushThinkDrain()
					return turnAttemptResult{kind: turnCollected, text: strings.TrimSpace(reply.String()), reasoning: reasoning}
				}
			case "permission":
				// Auto-approve (the --auto equivalent). Session-level deny
				// rules mean this should rarely fire — defensive only.
				if pid := evt.PermissionID; pid != "" {
					go func() { _ = c.client.ReplyPermission(subCtx, sid, pid) }()
				}
			case "error":
				// The turn failed at the model/API level: record it and end
				// the turn (the session is kept). Text carries the failed
				// message.
				s.log.Warn("ask orchicon session error", "conversation", c.convID, "message", evt.Text)
				flushThinkDrain()
				return turnAttemptResult{kind: turnFailed, reasoning: reasoning, err: errors.New(evt.Text)}
			case "delta":
				// Mid-generation token deltas are liveness evidence and the
				// live partial-reply mirror. Events observed BEFORE our
				// message was accepted (sent == false) belong to a prior turn
				// still draining on the shared bus — they must not leak into
				// this turn's persisted reply.
				if !sent {
					continue
				}
				monitor.observe("text", nil)
				s.turns.markActivity(c.convID, c.token)
				// Suspect-kind gate: while the folded-think segmenter has a
				// think run open, even a native `reasoning` delta is suspect
				// (the serve streams text AND reasoning through the same
				// field:"text", so the kind is best-effort) — it stays on the
				// segmenter road instead of raw-appending to the tail.
				if evt.IsReasoning && !segThink.inThink() {
					liveReasoning.WriteString(evt.Text)
				} else {
					segThink.feed(evt.Text,
						func(t string) { liveText.WriteString(t) },
						func(b string) { liveReasoning.WriteString(b) },
						func(b string) {
							liveReasoning.Reset()
							commitThink(b)
						},
					)
				}
				if c.onPartial != nil && time.Since(lastMirror) >= 200*time.Millisecond {
					lastMirror = time.Now()
					disarmFlush()
					snapText, snapRsn := mirrorSnapshot()
					c.onPartial(snapText, snapRsn)
				} else if c.onPartial != nil && !flushArmed {
					// Throttled: arm the one-shot trailing flush for the
					// throttle-window remainder (min 10ms) so the delta tail
					// drains shortly after the LAST delta — a burst that ends
					// within the throttle window (short final generation, or
					// the stream ending entirely) would otherwise freeze the
					// partial row on the previous snapshot until the turn
					// finalizes. An on-time flush or a completed part disarms
					// it; arming keeps the earliest deadline (only arm when
					// not already armed).
					delay := 200*time.Millisecond - time.Since(lastMirror)
					if delay < 10*time.Millisecond {
						delay = 10 * time.Millisecond
					}
					flushTick.Reset(delay)
					flushArmed = true
				}
			case "tool_part":
				// A tool call ISSUED but not yet resolved (AC1 MCP-wedge). The
				// adapter maps the non-terminal tool part to this signal;
				// LegacyEventFromBus only maps completed/errored tools to
				// "tool_use", so a wedged tool would otherwise be invisible to
				// the stall monitor. Feed an explicit tool start so the monitor
				// can detect the wedge.
				if !sent {
					continue
				}
				monitor.observeToolStart(evt.Text)
				s.turns.markActivity(c.convID, c.token)
				// Live tool ledger: the call is recorded the moment it is
				// issued (not at completion) and mirrored immediately, so a
				// session killed while the tool runs still shows the call.
				c.ledger.recordStart(evt.Text)
				if c.onPartial != nil {
					snapText, snapRsn := mirrorSnapshot()
					c.onPartial(snapText, snapRsn)
				}
			case "part":
				// Completed telemetry part (the same LegacyEventFromBus
				// mapping executions use — the adapter classified it). Events
				// before our message was accepted (sent == false) belong to a
				// prior turn — never leak into this turn's reply.
				if !sent {
					continue
				}
				monitor.observe(evt.Type, evt.Part)
				s.turns.markActivity(c.convID, c.token)
				// Diff pipeline (AC 1): completed mutating tool_use parts on
				// an Ask turn ledger file edits from real file-state
				// snapshots — owner_kind ask_conversation, same ground truth
				// executions record. The hook (server-wired fileedit.Service)
				// owns the error posture; a ledger gap never fails the turn.
				// Fired BEFORE the stream callback so the ledger write sees
				// the full output (the callback may have already forwarded a
				// capped view).
				if evt.Type == "tool_use" && s.fileEditHook != nil {
					toolName, _ := evt.Part["tool"].(string)
					state, _ := evt.Part["state"].(map[string]any)
					input, _ := state["input"].(map[string]any)
					if input == nil {
						input = map[string]any{}
					}
					output, _ := state["output"].(string)
					s.fileEditHook(ctx, c.tenantID, c.convID, toolName, input, output)
				}
				switch evt.Type {
				case "text":
					text := evt.Text
					if text == "" {
						continue
					}
					// Completed text part may itself carry one or more folded
					// think segments (GLM/DeepSeek). Demux with a FRESH
					// segmenter (never the live one — the completed part is
					// authoritative and must not reuse carry-over state from
					// the delta tail). Folded bodies commit to the durable
					// reasoning array via commitThink (deduped against a body
					// the live delta path already committed); the leftover
					// clean text is what the reply / TextChunk / partial
					// mirror carry.
					pseg := thinkSegmenter{}
					var cleanText strings.Builder
					var foldedBodies []string
					pseg.feed(text,
						func(t string) { cleanText.WriteString(t) },
						func(b string) {},
						func(b string) { foldedBodies = append(foldedBodies, b) },
					)
					pseg.flushBody(func(b string) { foldedBodies = append(foldedBodies, b) })
					liveText.Reset()
					liveReasoning.Reset()
					segThink.reset()
					for _, b := range foldedBodies {
						commitThink(b)
					}
					ct := cleanText.String()
					if ct != "" {
						reply.WriteString(ct)
						reply.WriteString("\n\n")
					}
					disarmFlush()
					if c.onPartial != nil {
						snapText, snapRsn := mirrorSnapshot()
						c.onPartial(snapText, snapRsn)
					}
					if ct != "" && c.onStreamEvent != nil {
						c.onStreamEvent(&apiv1.ChatStreamResponse{
							Event: &apiv1.ChatStreamResponse_TextChunk{
								TextChunk: &apiv1.TextChunk{Content: ct},
							},
						})
					}
				case "reasoning":
					if evt.Text != "" {
						reasoning = append(reasoning, evt.Text)
						liveReasoning.Reset()
						disarmFlush()
						if c.onPartial != nil {
							snapText, snapRsn := mirrorSnapshot()
							c.onPartial(snapText, snapRsn)
						}
						if c.onStreamEvent != nil {
							c.onStreamEvent(&apiv1.ChatStreamResponse{
								Event: &apiv1.ChatStreamResponse_Reasoning{
									Reasoning: &apiv1.ReasoningChunk{Content: evt.Text},
								},
							})
						}
					}
				case "step_finish":
					// Step completion carries token usage + cost
					// (docs/04 §6.1). Ask sessions capture LIVE usage via the
					// SAME canonical aigateway dual-write worker executions use —
					// the adapter's previously-dropped step_finish is now
					// recorded. Live-usage-only: no estimated/synthesized usage.
					s.recordTurnUsage(ctx, c, evt.Part)
				case "tool_use":
					// Live tool ledger: a completed tool call resolves the
					// in-flight entry (arguments backfilled, result appended)
					// and mirrors immediately — a killed session leaves the
					// tool activity visible, not just the text.
					c.ledger.recordResolve(evt.Part)
					if c.onPartial != nil {
						snapText, snapRsn := mirrorSnapshot()
						c.onPartial(snapText, snapRsn)
					}
				}
			}
		}
	}
}

// recordTurnUsage captures a live usage sample from an Ask step_finish event
// (docs/04 §6.1: the opencode tokens/cost shape) through the canonical
// aigateway dual-write worker executions use — Postgres usage_records + OTel
// metrics (docs/08 §5.2). This is how Ask sessions finally record usage across
// adapters: the adapter kind is derived from the turn's model_ref, and the
// usage is attributed to the Ask session (conversation id) via SessionID
// since a chat turn has no execution/task/project. Live-usage-only: only real
// step_finish tokens/cost are recorded; no estimated/synthesized usage.
//
// A genuinely empty sample (no tokens, no cost) is dropped, mirroring the
// worker adapter's recordUsage so telemetry stays clean. Best-effort: the
// recorder's internal errors never block the chat turn (docs/08 §8).
func (s *Service) recordTurnUsage(ctx context.Context, c turnCollectOpts, part map[string]any) {
	if s.usageRecorder == nil {
		return
	}
	tokens, _ := part["tokens"].(map[string]any)
	cost, _ := part["cost"].(float64)
	promptTokens := toTurnInt64(tokens["input"])
	cacheReadTokens := toTurnInt64(turnCacheToken(tokens, "read"))
	cacheWriteTokens := toTurnInt64(turnCacheToken(tokens, "write"))
	completionTokens := toTurnInt64(tokens["output"])
	reasoningTokens := toTurnInt64(tokens["reasoning"])
	if promptTokens == 0 && cacheReadTokens == 0 && cacheWriteTokens == 0 &&
		completionTokens == 0 && reasoningTokens == 0 && cost == 0 {
		return
	}
	// Derive the provider/model via the adapter-agnostic split and the adapter
	// kind from segment 1 of the model ref. A malformed ref attributes to
	// "unknown" so the record is never dropped on the parse.
	provider, model, ok := adapter.SplitForServe(c.modelRef)
	if !ok {
		provider, model = "unknown", "unknown"
	}
	_, _ = s.usageRecorder.Record(context.WithoutCancel(ctx), aigateway.UsageInput{
		TenantID:         c.tenantID,
		Provider:         provider,
		Model:            model,
		PromptTokens:     promptTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: cacheWriteTokens,
		CompletionTokens: completionTokens,
		ReasoningTokens:  reasoningTokens,
		CostUSD:          cost,
		AdapterKind:      adapter.AdapterKind(c.modelRef),
		SessionID:        c.convID,
	})
}

// turnCacheToken reads a sub-count from the opencode tokens.cache sub-object
// (e.g. {"cache":{"read":N,"write":M}} → read/write). opencode emits cache
// counts as a nested object, so a plain lookup of tokens["cache"] would be 0.
func turnCacheToken(tokens map[string]any, key string) any {
	if cache, ok := tokens["cache"].(map[string]any); ok {
		return cache[key]
	}
	return nil
}

// toTurnInt64 normalizes a token count from the JSON-wire opencode shape
// (float64 / int64 / int) to an int64, defaulting to 0.
func toTurnInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
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
func (s *Service) persistConversationReply(ctx context.Context, tenantID, convID, assistantMsgID, modelRef, content, sid, errText string, reasoning []string, ledger *toolLedger) {
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
	// The live tool ledger is snapshotted with the terminal reply: the same
	// tx carries text + reasoning + tool activity, so a crash lands the full
	// turn atomically and the finalize's overwrite can never resurrect a
	// stale partial's tool columns (the UpsertMessage conflict clause
	// overwrites them with this snapshot).
	// TERMINAL write: the ledger is repaired (an explicit aborted result is
	// attached to every call that never resolved) so the DB never holds an
	// assistant row whose tool_calls have no matching tool_results, however
	// abnormally the turn ended.
	ledgerCalls, ledgerResults := ledger.repairedSnapshot()
	assistantMsg := db.MessageRow{
		ID:             assistantMsgID,
		TenantID:       tenantID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        content,
		ToolCalls:      ledgerCalls,
		ToolResults:    ledgerResults,
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
func (s *Service) upsertPartialMessage(ctx context.Context, tenantID, convID, assistantMsgID, modelRef, content string, reasoning []string, ledger *toolLedger) {
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
	ledgerCalls, ledgerResults := ledger.snapshot()
	if _, err := db.UpsertMessage(ctx, ttx.Tx, db.MessageRow{
		ID:             assistantMsgID,
		TenantID:       tenantID,
		ConversationID: convID,
		Role:           "assistant",
		Content:        content,
		ToolCalls:      ledgerCalls,
		ToolResults:    ledgerResults,
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
func (s *Service) runOpenCodeTurn(ctx context.Context, client scheduler.ChatTurnClient, tenantID, convID, sessionID, modelRef, seedSystem, reuseSystem, userMsg string, cb streamCallback) (msgID, newSessionID string, elapsed time.Duration, err error) {
	start := time.Now()
	msgID = db.NewID()

	// Resolve the session: reuse the persisted id, or create a fresh one
	// (first message). A fresh session gets the seeded system prompt (DB
	// history included); a live session gets the reuse variant.
	sid := sessionID
	system := reuseSystem
	if sid == "" {
		sid, err = client.CreateConversationSession(ctx, convID, "ask-orchicon:"+convID)
		if err != nil {
			return msgID, "", time.Since(start), fmt.Errorf("create conversation session: %w", err)
		}
		system = seedSystem
		s.persistConversationSessionID(ctx, tenantID, convID, sid)
	}

	// Subscribe BEFORE send so early text chunks aren't missed.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sub, err := client.Subscribe(subCtx, convID)
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
			if err := client.SendTurnMessage(subCtx, convID, sid, system, modelRef, userMsg); err != nil {
				if errors.Is(err, scheduler.ErrSessionNotFound) && sessionID != "" && !recreated {
					s.log.Info("conversation session lost on serve — recreating", "conversation", convID, "session", sid)
					fresh, cerr := client.CreateConversationSession(ctx, convID, "ask-orchicon:"+convID)
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
			_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
			return msgID, sid, time.Since(start), ctx.Err()
		case <-timeout.C:
			_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
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
			if evt.SessionID != "" && evt.SessionID != sid {
				continue
			}
			switch evt.Kind {
			case "idle":
				// Turn complete — but only once OUR message was accepted
				// (sent). A stale idle from a prior turn (sent == false)
				// must never complete a new turn.
				if sent {
					return msgID, sid, time.Since(start), nil
				}
			case "permission":
				// Auto-approve (the --auto equivalent). Session-level deny
				// rules mean this should rarely fire — defensive only.
				if pid := evt.PermissionID; pid != "" {
					go func() { _ = client.ReplyPermission(context.WithoutCancel(ctx), sid, pid) }()
				}
			case "error":
				// The turn failed at the model/API level: record it and end
				// the turn with an error chunk (the session is kept).
				s.log.Warn("opencode session error", "conversation", convID, "message", evt.Text)
				return msgID, sid, time.Since(start), errors.New(evt.Text)
			case "part":
				// Telemetry (text / tool_use / step / reasoning): feed the
				// SAME mapping executions use into the chat's callback.
				var part map[string]any
				if evt.Type == "text" || evt.Type == "reasoning" || evt.Type == "tool_use" {
					part = evt.Part
				}
				if err := cb(opencodeEvent{Type: evt.Type, Part: part}); err != nil {
					// stream.Send failed — the client is gone. Abort the
					// turn and stop streaming (the partial response is
					// still persisted).
					_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
					return msgID, sid, time.Since(start), nil
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
	// list_projects returns the compact envelope {count, truncated, items}.
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Items) == 0 {
		return ""
	}
	projects := env.Items
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
