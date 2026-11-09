package askorchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/chatlimits"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// defaultHandshakeTimeout bounds how long a chat turn's attempt waits for the serve to ACCEPT the
// sent message (send-accept).
const defaultHandshakeTimeout = 60 * time.Second

// defaultHandshakeAttempts is the accept-retry budget: the first send plus two retries. See
// askHandshakeAttempts for why the retry exists at all.
const defaultHandshakeAttempts = 3

// askHandshakeAttempts is how many times a turn will try to get its message ACCEPTED by the serve
// before the failure reaches the operator.
func askHandshakeAttempts() int {
	if v := os.Getenv("ORCHICON_ASK_HANDSHAKE_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultHandshakeAttempts
}

func askTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHandshakeTimeout
}

// defaultReplyWindow bounds how long a detached chat turn may go WITHOUT PROGRESS before its reply
// is persisted as a timeout error.
const defaultReplyWindow = 30 * time.Minute

func askReplyWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_REPLY_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultReplyWindow
}

// oneLine flattens a model-authored string to ONE readable row and clamps it, because the message
// it lands in is a transcript row.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if max < 1 {
		max = 1
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// turnReplyWindow is the reply window as an INACTIVITY deadline: it fires only
// after the turn has been quiet for the full window, and every sign of progress
// (a delta, a tool call, a completed part) RESTARTS it.
//
// WHY IT IS NOT A BARE time.Timer ANY MORE. The collector used to create
//
//	window := time.NewTimer(askReplyWindow())
//
// once per turn, hand that one timer to every attempt, and never reset it —
// there is no window.Reset anywhere in this file. It therefore measured the AGE
// OF THE TURN rather than the silence inside it, and thirty minutes after the
// send the collector returned, verbatim:
//
//	reply timed out after 30m0s on model orchicon/deepseek/deepseek-flash —
//	the model may be overloaded or unavailable. Check the Ask Orchicon model
//	in Settings → Default models, then retry.
//
// The operator: "Sessions seem to be timing out on me... it may be model
// related but it seems to be occuring quite frequently", against exactly that
// message. Nothing was wrong with the model. A turn doing twenty minutes of
// honest tool work and taking a slow final step was indistinguishable, to this
// timer, from a provider that had died at send time — and the message it
// produced sent the operator to Settings → Default models to fix it.
//
// WHAT IT MEASURES NOW: time since the last progress. A turn that is working is
// never killed for its age, however long the work takes; a turn that has gone
// quiet is still bounded, with the same honest, retryable message. The bound is
// not weakened — it is re-pointed at the evidence that actually describes a
// dead turn.
//
// Touch needs no channel drain because the timer channel is unbuffered as of
// Go 1.23 (go.mod: go 1.26) and Stop/Reset then guarantee no stale receive.
type turnReplyWindow struct {
	d time.Duration
	t *time.Timer
}

func newTurnReplyWindow(d time.Duration) *turnReplyWindow {
	return &turnReplyWindow{d: d, t: time.NewTimer(d)}
}

// C is the expiry channel: it fires once the window elapses with NO progress.
func (w *turnReplyWindow) C() <-chan time.Time { return w.t.C }

// Touch restarts the window from now — the turn just made progress, so its
// deadline is measured from THIS moment rather than from the send.
func (w *turnReplyWindow) Touch() { w.t.Reset(w.d) }

// Stop releases the timer (the collector defers it).
func (w *turnReplyWindow) Stop() { w.t.Stop() }

// defaultTurnMaxAge is how long a chat turn may sit in the registry WITHOUT PROGRESS before the
// background sweeper evicts it (cancelling the collector with errTurnExpired and aborting.
const defaultTurnMaxAge = 31 * time.Minute

func askTurnMaxAge() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_TURN_MAX_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultTurnMaxAge
}

// Sweeper tick: how often the background sweeper scans the turn registry for expired turns.
const defaultSweepInterval = time.Minute

func askSweepInterval() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultSweepInterval
}

// defaultReattachBackoff is the pause between re-attach attempts after the serve bus is lost
// mid-reply (serve restart / watchdog recovery).
const defaultReattachBackoff = 2 * time.Second

// defaultReattachAttemptBudget bounds how many times a turn re-attaches after the bus closes with a
// LIVE connection, before it gives up with a retryable error.
const defaultReattachAttemptBudget = 15

func askReattachAttemptBudget() int {
	if v := os.Getenv("ORCHICON_ASK_REATTACH_ATTEMPT_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultReattachAttemptBudget
}

func askReattachBackoff() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_REATTACH_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultReattachBackoff
}

// defaultServeDownGrace bounds how long a turn's collector keeps retrying a serve that has NEVER
// accepted a connection this turn (serve down at send time).
const defaultServeDownGrace = 15 * time.Second

func askServeDownGrace() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_SERVE_DOWN_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultServeDownGrace
}

// defaultAskMaxConcurrentTurns bounds how many Ask Orchicon turns may run concurrently across all
// conversations (ADR-0002 D7 — session admission bound).
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

// Turn-cancellation causes.
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
	// token uniquely identifies this turn generation.
	token uint64
	// started is when the turn was registered; the sweeper evicts entries
	// older than the TTL.
	started time.Time
	// tenant is the turn's tenant, needed by the sweeper to load the
	// conversation row (RLS) for the serve-session abort.
	tenant string
	// assistantMsgID is the acked assistant message id under which this turn's reply (or error) will be
	// persisted.
	assistantMsgID string
	// wedged records that this turn's session wedged on an unresolved tool call (MCP wedge, AC1).
	wedged bool
	// lastActivity is when the running turn last produced output (token, reasoning, step, or tool
	// activity).
	lastActivity time.Time

	// reuseSystem and modelRef are what an INJECTION re-sends with its message.
	reuseSystem string
	modelRef    string

	// injections counts user messages ACCEPTED into this turn's live session (see noteInjection).
	injections int64
	// injectedAt is when the most recent injection was ACCEPTED.
	injectedAt time.Time
}

// turnRegistry tracks in-flight Ask Orchicon turns (keyed by conversation id) with their collector
// cancellation functions.
type turnRegistry struct {
	mu      sync.Mutex
	nextTok uint64
	turns   map[string]turnEntry
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{turns: make(map[string]turnEntry)}
}

// register records a turn's cancellation function for a conversation.
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

// get reports whether a turn is in flight for a conversation and, if so, returns its entry (the
// caller uses entry.assistantMsgID to re-attach to the running turn).
func (r *turnRegistry) get(convID string) (turnEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.turns[convID]
	return entry, ok
}

// len reports the number of conversations with an in-flight turn.
func (r *turnRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

// markWedged records that a turn's session wedged on an unresolved tool (AC1), keyed by the
// caller's token so a stale marker from a superseded turn never clobbers the replacement.
func (r *turnRegistry) markWedged(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.wedged = true
		r.turns[convID] = entry
	}
}

// setTurnContext records the prompt and model an injection must re-send with its message.
func (r *turnRegistry) setTurnContext(convID string, token uint64, reuseSystem, modelRef string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.reuseSystem = reuseSystem
		entry.modelRef = modelRef
		r.turns[convID] = entry
	}
}

// noteInjection records that a user message was ACCEPTED into the turn's live session.
func (r *turnRegistry) noteInjection(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.injections++
		entry.injectedAt = time.Now()
		r.turns[convID] = entry
	}
}

// injectionAcceptedAt reports when this turn last accepted an injection, and 0 when it never has.
func (r *turnRegistry) injectionAcceptedAt(convID string, token uint64) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		return entry.injectedAt
	}
	return time.Time{}
}

// injectionIdleGrace is how long after an accepted injection an idle is treated as STALE rather
// than final.
const injectionIdleGrace = 2 * time.Second

// injectionSeq reports how many injections this turn has accepted. The collector compares it against the
// value it last observed to tell a stale idle from a real completion.
func (r *turnRegistry) injectionSeq(convID string, token uint64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		return entry.injections
	}
	return 0
}

// markActivity records that a running turn last produced output at now.
func (r *turnRegistry) markActivity(convID string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.turns[convID]; ok && entry.token == token {
		entry.lastActivity = time.Now()
		r.turns[convID] = entry
	}
}

// cancel fires the collector's cancellation for a conversation (if any) with the given cause and
// reports whether a turn was registered, returning its token.
func (r *turnRegistry) cancel(convID string, cause error) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.turns[convID]
	if ok {
		entry.cancel(cause)
	}
	return entry.token, ok
}

// remove drops the registry entry for a conversation only when its token matches the caller's
// (called by the collector on finalize, success or error).
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

// sweep evicts (cancels with errTurnExpired and removes) every entry that has been QUIET for longer
// than maxAge and returns the evicted conversations (with their tenants) so the caller can abort.
func (r *turnRegistry) sweep(now time.Time, maxAge time.Duration, parked func(convID string) bool) []turnEviction {
	type candidate struct {
		convID string
		tenant string
		token  uint64
	}
	r.mu.Lock()
	var cands []candidate
	for id, entry := range r.turns {
		if now.Sub(entry.lastActivity) > maxAge {
			cands = append(cands, candidate{convID: id, tenant: entry.tenant, token: entry.token})
		}
	}
	r.mu.Unlock()

	var evicted []turnEviction
	for _, cd := range cands {
		if parked != nil && parked(cd.convID) {
			continue
		}
		r.mu.Lock()
		// TOKEN-GUARDED on the way back in: the lock was released, so the entry may have been replaced by a
		// newer turn (which must not be cancelled for the old one's silence) or removed entirely.
		if entry, ok := r.turns[cd.convID]; ok && entry.token == cd.token {
			entry.cancel(errTurnExpired)
			delete(r.turns, cd.convID)
			evicted = append(evicted, turnEviction{convID: cd.convID, tenant: cd.tenant})
		}
		r.mu.Unlock()
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
	if err := chatlimits.TooLong(msg); err != nil {
		return err
	}

	assistantID, streamEventCh, st, err := s.startConversationTurn(ctx, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments)
	if err != nil {
		return err
	}
	return s.drainTurnStream(stream, req.Msg.ConversationId, assistantID, streamEventCh, st)
}

// InterjectConversationTurn adds the operator's message to the conversation WHILE the model is
// working, without interrupting it.
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
	if err := chatlimits.TooLong(msg); err != nil {
		return err
	}
	if len(req.Msg.Attachments) > 5 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("too many attachments (max 5)"))
	}

	entry, running := s.turns.get(req.Msg.ConversationId)

	// THE INJECTION PATH. Checked BEFORE anything is cancelled, so the healthy case cannot fall through into
	// the supersede path by accident.
	if running && !entry.wedged {
		return s.injectIntoRunningTurn(ctx, stream, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments, entry)
	}

	// Wedged (recycle) or idle (start a turn).
	assistantID, streamEventCh, st, err := s.startConversationTurnOpts(ctx, tenantID, req.Msg.ConversationId, msg, req.Msg.Attachments, turnDispatchOpts{supersede: running})
	if err != nil {
		return err
	}
	return s.drainTurnStream(stream, req.Msg.ConversationId, assistantID, streamEventCh, st)
}

// IT NEVER REPORTS THE MESSAGE AS FAILED, and that is a correction of the previous behaviour.
func (s *Service) injectIntoRunningTurn(ctx context.Context, stream *connect.ServerStream[apiv1.ChatStreamResponse], tenantID, convID, msg string, attachments []*apiv1.AttachmentInput, entry turnEntry) error {
	if err := s.deliverInjection(ctx, tenantID, convID, msg, attachments, entry); err != nil {
		// A PERSISTENCE FAILURE IS THE ONLY THING THE OPERATOR SEES, because it is the only case where their
		// message is genuinely lost. Everything else is delivery, and delivery is best-effort by design.
		return err
	}
	// Attach to the reply this message joins. It is the RUNNING turn's own assistant message, so the operator
	// watches the work continue rather than a second, empty reply appearing beside it.
	return s.watchRunningTurn(ctx, stream, convID, entry.assistantMsgID)
}

// deliverInjection persists msg as a user message and gets it into the model's context by whatever
// route the conversation's adapter can actually honour.
func (s *Service) deliverInjection(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput, entry turnEntry) error {
	conv, err := s.loadConversation(ctx, tenantID, convID)
	if err != nil {
		return err
	}

	// 1. Persist. Same path as a normal send, flagged as an injection in the audit trail. THIS is what makes
	// the message appear in the chat immediately, and it is the only step whose failure is reported.
	if _, err := s.persistUserMessage(ctx, tenantID, conv, msg, attachments, userSendFlags{injected: true}); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}

	// 2. Get it into the context, by the route this adapter supports.
	client, cerr := s.resolveChatClient(convID, conv.ModelRef)
	if cerr != nil {
		// The message is saved and will be answered by the next turn; there is nothing for the operator to do.
		s.log.Warn("ask orchicon interjection: adapter could not be resolved — the message is saved and will be answered on the next turn",
			"conversation", convID, "error", cerr)
		s.queueFollowUp(convID, msg, attachments)
		return nil
	}

	if !queuesPrompts(client) {
		// THE NATIVE BRIDGE.
		s.log.Info("ask orchicon interjection queued for the next turn (this adapter has no prompt queue)",
			"conversation", convID, "assistant_message", entry.assistantMsgID, "attachments", len(attachments))
		s.queueFollowUp(convID, msg, attachments)
		return nil
	}

	if strings.TrimSpace(conv.SessionID) == "" {
		s.log.Warn("ask orchicon interjection: running turn has no session — the message is saved and will be answered on the next turn",
			"conversation", convID, "assistant_message", entry.assistantMsgID)
		s.queueFollowUp(convID, msg, attachments)
		return nil
	}

	// THE PROMPT AND MODEL ARE THE RUNNING TURN'S, re-read from its registry entry rather than rebuilt.
	system := entry.reuseSystem
	modelRef := entry.modelRef
	if strings.TrimSpace(modelRef) == "" {
		modelRef = s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	}

	// NOT the request context: the stream this call arrived on may drop, and the injection must still
	// land — that is the point of adding context to work that continues without the operator watching.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), askTimeout())
	defer cancel()
	if len(attachments) > 0 {
		acc, ok := client.(scheduler.SendTurnMessageWithAttachments)
		if !ok {
			s.log.Warn("ask orchicon interjection: this adapter cannot attach files to a running turn — the message is saved and will be answered on the next turn",
				"conversation", convID)
			s.queueFollowUp(convID, msg, attachments)
			return nil
		}
		parts := make([]scheduler.ChatAttachment, 0, len(attachments))
		for _, a := range attachments {
			parts = append(parts, scheduler.ChatAttachment{Name: a.Name, MimeType: a.MimeType, Data: a.Data})
		}
		err = acc.SendTurnMessageWithAttachments(sendCtx, convID, conv.SessionID, system, modelRef, msg, parts)
	} else {
		err = client.SendTurnMessage(sendCtx, convID, conv.SessionID, system, modelRef, msg)
	}
	if err != nil {
		// A DELIVERY FAILURE IS NOT THE OPERATOR'S PROBLEM.
		s.log.Warn("ask orchicon interjection could not be delivered to the live session — queued for the next turn",
			"conversation", convID, "error", err)
		s.queueFollowUp(convID, msg, attachments)
		return nil
	}

	// 3. Tell the collector to expect work from it. AFTER the send, never before: an injection that failed put
	// nothing into the session, and counting it would keep a finishing turn open until the reply window expired.
	s.turns.noteInjection(convID, entry.token)
	s.log.Info("ask orchicon interjection injected into the running turn",
		"conversation", convID, "assistant_message", entry.assistantMsgID, "attachments", len(attachments))
	return nil
}

// startQueuedFollowUp answers a held interjection with a turn of its own.
func (s *Service) startQueuedFollowUp(ctx context.Context, tenantID, convID string) {
	if s.followUps == nil {
		return
	}
	item, ok := s.followUps.take(convID)
	if !ok {
		return
	}
	if _, running := s.turns.get(convID); running {
		// A turn is already in flight — the operator sent something else while this one finished. Leave the
		// message for that turn rather than racing it with a second.
		s.followUps.put(convID, item.text, item.attachments)
		return
	}
	s.log.Info("ask orchicon answering a held interjection on a follow-up turn",
		"conversation", convID)
	_, ch, _, err := s.startConversationTurnOpts(ctx, tenantID, convID, item.text, item.attachments, turnDispatchOpts{skipPersist: true})
	if err != nil {
		// NOT RE-QUEUED, deliberately: a follow-up that fails and re-queues itself is a request loop. The
		// message is already in the transcript.
		s.log.Warn("ask orchicon follow-up turn for a held interjection could not start — the message stays in the transcript for the next turn",
			"conversation", convID, "error", err)
		return
	}
	go func() {
		for range ch {
			// dropped: nobody is streaming this turn
		}
	}()
}

// queuesPrompts reports whether this adapter holds a message for the running turn's boundary (see
// scheduler.SessionQueuesPrompts).
func queuesPrompts(client scheduler.ChatTurnClient) bool {
	if q, ok := client.(scheduler.SessionQueuesPrompts); ok {
		return q.QueuesPrompts()
	}
	return false
}

// loadConversation reads one conversation row in the tenant's scope.
func (s *Service) loadConversation(ctx context.Context, tenantID, convID string) (db.ConversationRow, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.ConversationRow{}, connect.NewError(connect.CodeInternal, err)
	}
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	ttx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.ConversationRow{}, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return db.ConversationRow{}, connect.NewError(connect.CodeInternal, err)
	}
	return conv, nil
}

// WatchTurnStream re-attaches a dropped socket to an ACKED turn's live event stream WITHOUT
// dispatching a new turn.
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
	if _, found := s.hubs.get(req.Msg.ConversationId); !found {
		return connect.NewError(connect.CodeNotFound, errors.New("no live stream for this turn — the turn finished or was superseded"))
	}
	s.log.Info("ask orchicon watch re-attached to running turn", "conversation", req.Msg.ConversationId, "assistant_message", req.Msg.AssistantMessageId)
	return s.watchRunningTurn(ctx, stream, req.Msg.ConversationId, req.Msg.AssistantMessageId)
}

// watchRunningTurn attaches a client stream to a turn that is ALREADY RUNNING: it acks with that
// turn's assistant message id and drains the conversation's broadcast hub until the turn finalises.
func (s *Service) watchRunningTurn(ctx context.Context, stream *connect.ServerStream[apiv1.ChatStreamResponse], convID, assistantID string) error {
	h, found := s.hubs.get(convID)
	if !found {
		// The turn reached its end between the caller's check and this subscribe.
		_ = stream.Send(&apiv1.ChatStreamResponse{
			Event: &apiv1.ChatStreamResponse_TurnStarted{TurnStarted: &apiv1.TurnStarted{AssistantMessageId: assistantID}},
		})
		return nil
	}
	if err := stream.Send(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TurnStarted{TurnStarted: &apiv1.TurnStarted{AssistantMessageId: assistantID}},
	}); err != nil {
		return err
	}
	subID, ch := h.subscribe()
	defer h.unsubscribe(subID)
	// RE-ATTACH RECOVERY: this stream does not replay the turn's earlier events (the client fills those
	// from ListMessages), but a consent ask is a PENDING question.
	for _, a := range s.pending.list(convID) {
		if !a.isOpen() {
			continue
		}
		if err := stream.Send(permissionAskEvent(a)); err != nil {
			s.log.Warn("ask orchicon watch: replaying a pending consent ask failed",
				"conversation", convID, "ask", a.AskID, "error", err)
			return nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case resp, ok := <-ch:
			if !ok {
				return nil // hub closed: turn finalized or superseded
			}
			if err := stream.Send(resp); err != nil {
				s.log.Warn("ask orchicon watch stream send failed", "conversation", convID, "assistant_message", assistantID, "cause", "watcher-gone-send-fail")
				return nil
			}
		}
	}
}

// dispatchBufferSize bounds the per-turn dispatch channel (and the hub's per-subscriber buffer,
// which is sized to match).
const dispatchBufferSize = 64

// dispatchStreamState carries what a turn's dispatch channel needs so that a DROPPED live event is
// OBSERVABLE.
type dispatchStreamState struct {
	dropped     atomic.Int64
	drainActive atomic.Bool
}

// sendOrDrop enqueues a live event, or counts and reports the drop.
func sendOrDrop(ch chan<- *apiv1.ChatStreamResponse, resp *apiv1.ChatStreamResponse, st *dispatchStreamState, log *slog.Logger, convID string) {
	select {
	case ch <- resp:
		return
	default:
	}
	n := st.dropped.Add(1)
	const maxDropReports = 200
	if n != 1 && n%maxDropReports != 0 {
		return
	}
	// drain_active tells the two causes apart, and they need different fixes: TRUE means the client is
	// reading too slowly for the turn's output rate (the buffer is a queue).
	log.Warn("ask orchicon DROPPED live events — dispatch buffer full",
		"conversation", convID,
		"dropped_total", n,
		"drain_active", st.drainActive.Load(),
		"cause", "dispatch-channel-full")
}

// drainTurnStream acks a freshly-started turn with TurnStarted and then drains streaming events to
// the client until the channel closes (turn complete or error).
func (s *Service) drainTurnStream(stream *connect.ServerStream[apiv1.ChatStreamResponse], convID, assistantID string, streamEventCh <-chan *apiv1.ChatStreamResponse, st *dispatchStreamState) error {
	// Nothing is draining this turn's dispatch channel once this returns, so mark it: a drop after this
	// point is not "the client is slow", it is "the events have nowhere to go".
	defer st.drainActive.Store(false)
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
			// A heartbeat that fails to send IS the drop signal (idle/proxy timeout or gone client): log it
			// distinctly and stop draining.
			if err := send(hb); err != nil {
				s.log.Warn("ask orchicon chatstream heartbeat send failed", "conversation", convID, "assistant_message", assistantID, "cause", "idle-timeout-or-client-gone")
				return nil
			}
		}
	}
}

// askHeartbeatInterval is the ChatStream keepalive cadence (≤20s): wire traffic during silent
// generation so idle timeouts never mistake a healthy-but-quiet turn for a dead socket.
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
	// skipPersist says this message's USER ROW ALREADY EXISTS, so the dispatch must not insert it
	// again.
	skipPersist bool
}

// userSendFlags says how an operator message entered the conversation, for the audit trail.
type userSendFlags struct {
	// superseded is set when this message REPLACED a running turn (the wedged-session recycle).
	superseded bool
	// injected is set when this message was ADDED to a running turn's live session without interrupting it.
	injected bool
}

// persistUserMessage stores one operator message — with its attachments — and audits it,
// atomically.
func (s *Service) persistUserMessage(ctx context.Context, tenantID string, conv db.ConversationRow, msg string, attachments []*apiv1.AttachmentInput, flags userSendFlags) (string, error) {
	attachmentsJSON := []byte("[]")
	if len(attachments) > 0 {
		if j, err := json.Marshal(attachments); err == nil {
			attachmentsJSON = j
		}
	}
	// THE INJECTION FLAG IS PART OF THE ROW, not only of the audit trail.
	metaJSON := []byte("{}")
	if flags.injected {
		metaJSON = []byte(`{"injected":true}`)
	}
	userMsg := db.MessageRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		ConversationID: conv.ID,
		Role:           "user",
		Content:        msg,
		ToolCalls:      []byte("[]"),
		ToolResults:    []byte("[]"),
		Attachments:    attachmentsJSON,
		Metadata:       metaJSON,
		Reasoning:      []string{},
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if _, err := db.CreateMessage(ctx, ttx.Tx, userMsg); err != nil {
		ttx.Rollback(ctx)
		return "", fmt.Errorf("save user message: %w", err)
	}
	if err := db.UpdateConversationTimestamp(ctx, ttx.Tx, tenantID, conv.ID); err != nil {
		ttx.Rollback(ctx)
		return "", err
	}
	if conv.Title == "" {
		title := msg
		if len(title) > 80 {
			title = title[:80]
		}
		db.UpdateConversationTitle(ctx, ttx.Tx, tenantID, conv.ID, title)
	}
	// Audit the user send atomically with the message persistence (one row per user action). Message content is
	// excluded from the snapshot (echo-privacy / compact trail); the message id refs the stored row.
	if err := recordAudit(ctx, ttx.Tx, tenantID, "conversation.message_sent", "conversation", conv.ID,
		nil, audit.Snapshot(map[string]any{
			"message_id": userMsg.ID,
			"role":       "user",
			"mode":       conv.Mode,
			"superseded": flags.superseded,
			"injected":   flags.injected,
		})); err != nil {
		ttx.Rollback(ctx)
		return "", fmt.Errorf("audit conversation.message_sent: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return "", err
	}
	return userMsg.ID, nil
}

// startConversationTurn is ChatStream's core (extracted for testability): it persists the user
// message, registers the in-flight turn, and launches the detached reply collector.
func (s *Service) startConversationTurn(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput) (string, chan *apiv1.ChatStreamResponse, *dispatchStreamState, error) {
	return s.startConversationTurnOpts(ctx, tenantID, convID, msg, attachments, turnDispatchOpts{})
}

// startConversationTurnOpts is the shared dispatch core for ChatStream (supersede=false) and
// InterjectConversationTurn (supersede=true).
func (s *Service) startConversationTurnOpts(ctx context.Context, tenantID, convID, msg string, attachments []*apiv1.AttachmentInput, opts turnDispatchOpts) (string, chan *apiv1.ChatStreamResponse, *dispatchStreamState, error) {
	// --- 0. Load the conversation. Needed up front: the persisted session
	// id for the supersede serve-abort, plus existence for NotFound. ---
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	conv, err := db.GetConversation(ctx, ttx.Tx, tenantID, convID)
	ttx.Rollback(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return "", nil, nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
		return "", nil, nil, connect.NewError(connect.CodeInternal, err)
	}

	// THE MODE RIDES THE TURN'S CONTEXT, from here to the tool boundary.
	ctx = withAskMode(ctx, conv.Mode)

	// THE CONVERSATION ID RIDES WITH THE MODE, for the same reason and by the same rule: a tool that
	// must report THIS session's own facts (get_current_conversation: which conversation.
	ctx = withAskConversation(ctx, convID)

	// AND THE CONVERSATION'S PROJECT RIDES WITH THEM, by the same rule and for a stronger reason: the
	// file/shell suite's containment boundary is resolved from it (AskFileScopeFor in native_tools.go).
	ctx = withAskConversationProject(ctx, conv.ProjectID)

	// AND THE TENANT, for the same reason and by the same rule — this one closing a real gap rather
	// than tidying.
	ctx = tenant.WithID(ctx, tenantID)

	// sessionIDOverride is the session the new turn dispatches on.
	useSessionID := conv.SessionID

	// --- 0.5. Supersede an in-flight turn (the WEDGED-only path — see InterjectConversationTurn). ---
	if opts.supersede {
		prevEntry, prevOk := s.turns.get(convID)
		if token, ok := s.turns.cancel(convID, errTurnSuperseded); ok {
			s.turns.remove(convID, token)
			s.log.Info("conversation turn superseded by interjection", "conversation", convID)
		}
		// Abort the serve session so the model stops generating NOW (the same abort the Stop button uses).
		if conv.SessionID != "" {
			if client := s.resolveClientForAbort(conv.ModelRef); client != nil {
				_ = client.AbortConversationSession(context.WithoutCancel(ctx), conv.SessionID)
			}
		}
		// D4 (mid-run interjection on a wedged session): the superseded turn was wedged on an unresolved
		// tool (MCP wedge) — its session is stuck.
		if prevOk && prevEntry.wedged {
			useSessionID = ""
			s.log.Warn("interjection recycling a wedged session to a fresh one", "conversation", convID, "old_session", conv.SessionID)
		}
	}

	// --- 0.8 Validate attachments (size/count caps — server is authoritative) ---
	if len(attachments) > 5 {
		return "", nil, nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("too many attachments (max 5)"))
	}
	var totalBytes int
	for _, a := range attachments {
		if len(a.Data) > 10*1024*1024 {
			return "", nil, nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("attachment %q too large (max 10MB)", a.Name))
		}
		totalBytes += len(a.Data)
	}
	if totalBytes > 20*1024*1024 {
		return "", nil, nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("attachments too large (max 20MB total)"))
	}

	// --- 1.
	assistantID := db.NewID()
	// D7 (session admission bound): reject dispatch beyond the concurrent-turn cap with a clear
	// CodeResourceExhausted error instead of degrading under contention on the shared serve bus +.
	if _, running := s.turns.get(convID); !running && s.turns.len() >= askMaxConcurrentTurns() {
		return "", nil, nil, connect.NewError(connect.CodeResourceExhausted,
			errors.New("too many Ask Orchicon conversations are processing right now — wait for a turn to finish and try again"))
	}
	// The turn is registered before the user message is persisted so a rejected second send (one turn
	// per conversation) never orphans a persisted user message.
	detached := context.WithoutCancel(ctx)
	turnCtx, cancelTurn := context.WithCancelCause(detached)
	token, ok := s.turns.register(convID, tenantID, assistantID, cancelTurn)
	if !ok {
		return "", nil, nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a reply is still in progress for this conversation — wait for it to complete or stop it first"))
	}
	releaseTurn := func() {
		cancelTurn(errUserStop)
		s.turns.remove(convID, token)
	}

	// --- 2.
	if !opts.skipPersist {
		if _, err := s.persistUserMessage(ctx, tenantID, conv, msg, attachments, userSendFlags{superseded: opts.supersede}); err != nil {
			releaseTurn()
			return "", nil, nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	// --- 3. Resolve model and build prompts (DB-only, no serve
	// interaction — the reply is collected detached). ---
	modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)
	if modelRef == "" {
		// NO MODEL IS CONFIGURED ANYWHERE, AND THERE IS NO HONEST DEFAULT TO INVENT. THE MODEL IS
		// FREE-TIER-ONLY.
		releaseTurn()
		return "", nil, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
			"no model is configured for Ask Orchicon\n"+
				"  Set one in Settings → Defaults → \"Default Ask Orchicon model\", or pick a model for this\n"+
				"  conversation from the composer's model chip.\n"+
				"  The native \"orchicon\" adapter needs no external binary and is the usual choice; the\n"+
				"  \"opencode\" adapter additionally requires the opencode CLI on the plane's PATH."))
	}

	ttx, err = s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		releaseTurn()
		return "", nil, nil, connect.NewError(connect.CodeInternal, err)
	}
	prevMessages, _ := db.ListMessages(ctx, ttx.Tx, tenantID, convID, 50, "")
	// The DB window is what a fresh session replays (the seed prompt) and what the session-ownership
	// check below reads: sanitize it so no assistant row carrying tool_calls without matching.
	prevMessages = sanitizeHistoryRows(prevMessages)
	cfg, _ := db.GetAgentConfig(ctx, ttx.Tx, tenantID)
	// The tenant's stall window for THIS turn: read at dispatch time so a settings change applies to
	// the next turn, and the collector's stall monitor uses the same value the read path reports.
	settings, _ := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)

	// Inject the tenant's enabled projects so the agent always has
	// up-to-date context about what it operates on (fresh per message).
	projectContext := s.fetchProjectContext(ctx, tenantID)
	// AND WHICH ONE THIS CHAT IS IN, which is a different statement from the list below: the operator's
	// "we should add context to all three modes to know which chat belongs to which project folder".
	convProject := s.conversationProjectContext(ctx, tenantID, conv)
	// AND THIS CONVERSATION'S SKILLS — the SELECTABLE skill artifacts, rendered by the ONE shared
	// platform renderer (contextfiles.RenderManifest) over the union of the conversation's skill_files.
	skillsSection := s.skillManifestSection(ctx, tenantID, conv)

	// System prompt variants for the session transport: the seed variant (DB history included) is used
	// when a fresh session is created (first message, or a lost session recreated).
	seedSystem := buildSystemPrompt(conv.Mode, cfg, s.toolRegistry, prevMessages, true, attachments, projectContext, convProject, skillsSection)
	reuseSystem := buildSystemPrompt(conv.Mode, cfg, s.toolRegistry, prevMessages, false, attachments, projectContext, convProject, skillsSection)

	// AN INJECTION RE-SENDS THESE, so the running turn publishes them here.
	s.turns.setTurnContext(convID, token, reuseSystem, modelRef)

	// --- 4.
	s.hubs.create(convID)
	streamEventCh := make(chan *apiv1.ChatStreamResponse, dispatchBufferSize)
	st := &dispatchStreamState{}
	st.drainActive.Store(true)
	onStreamEvent := func(resp *apiv1.ChatStreamResponse) {
		sendOrDrop(streamEventCh, resp, st, s.log, convID)
	}

	// The conversation's adapter is resolved through the shared Dispatcher by its model_ref kind
	// (ADR-0003 §3/§5).
	if client, cerr := s.resolveChatClient(convID, modelRef); cerr != nil {
		// The turn was already registered above: release it so the conversation is not wedged behind a turn
		// that will never run (the TTL sweeper would otherwise hold it for up to 31 minutes).
		releaseTurn()
		s.hubs.remove(convID)
		return "", nil, nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("Ask Orchicon could not resolve an adapter for this conversation: %w", cerr))
	} else if client != nil {
		// THE MODE BOUNDARY, HANDED TO THE ADAPTER THAT WILL RUN THE TURN.
		s.applyAskToolPolicy(ctx, client, conv.Mode, modelRef)
		// Adapter-scoped session identity (AC: cross-adapter switch).
		useSessionID = s.adapterScopedSessionID(client, useSessionID)
		// Model-change invalidation (BUG: model switch fails with a dangling tool-call replay).
		if useSessionID != "" && sessionCreatedUnderDifferentModel(conv.SessionID, modelRef, prevMessages) {
			s.log.Warn("ask orchicon model change — creating a fresh session (the stored session belongs to the previous model)",
				"conversation", convID, "old_session", conv.SessionID, "model", modelRef)
			useSessionID = ""
		}
		// Partial-reply mirror: the collector's onPartial callbacks feed a throttled flusher that upserts
		// the running turn's collected text/reasoning AND live tool ledger under the ACKED assistant.
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
			// Drain the partial mirror before finalizing: stop the flusher, wait for any in-flight write, then
			// write whatever is still dirty so the finalize below is the LAST write to the row.
			partialCancel()
			<-partialDone
			partialMu.Lock()
			dirty := partialDirty
			pText, pReasoning := partialText, append([]string(nil), partialReasoning...)
			partialMu.Unlock()
			if dirty {
				s.upsertPartialMessage(detached, tenantID, convID, assistantID, modelRef, pText, pReasoning, turnLedger)
			}
			// The terminal ledger snapshot travels with the finalize: the collector owns the pointer, so read
			// the final state from it (every finalize path below persists text + reasoning + tools atomically.
			finalLedger := turnLedger
			// Cause-aware finalize (ADR-ASK-3).
			if terr != nil {
				switch {
				case errors.Is(terr, errTurnSuperseded):
					if content := strings.TrimSpace(reply); content != "" {
						s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, content, sid, "", reasoning, finalLedger)
					} else if finalLedger.hasCalls() {
						// The turn was interrupted between a tool call and its result: nothing arrived as text, but the
						// partial mirror already wrote the row.
						s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, "", sid, "", reasoning, finalLedger)
					}
				case errors.Is(terr, errUserStop):
					// KEEP THE PARTIAL TEXT.
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, "Turn stopped by the user.", reasoning, finalLedger)
				default:
					errText := terr.Error()
					// Surface the failure verbatim on the stream (the TUI dock notice) as well as in the persisted
					// error row the poll renders — a provider 400 must reach the operator in the provider's own words.
					emitTurnError(onStreamEvent, errText)
					if isDanglingToolCallProviderError(errText) {
						// The session that produced the rejection is poisoned: its replayed history carries a tool call
						// with no result.
						s.log.Warn("ask orchicon dropping session after a dangling-tool-call provider rejection",
							"conversation", convID, "session", sid)
						s.persistConversationSessionID(detached, tenantID, convID, "")
					}
					// KEEP WHAT THE TURN PRODUCED.
					s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, errText, reasoning, finalLedger)
				}
			} else {
				s.persistConversationReply(detached, tenantID, convID, assistantID, modelRef, strings.TrimSpace(reply), sid, "", reasoning, finalLedger)
			}
			// AN INJECTED MESSAGE THIS ADAPTER COULD NOT QUEUE IS ANSWERED HERE.
			if terr == nil {
				s.startQueuedFollowUp(detached, tenantID, convID)
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

	return assistantID, streamEventCh, st, nil
}

// AbortConversationTurn implements the Stop button: it cancels the detached collector for the
// conversation's running turn (so it finalizes promptly and persists the user-initiated-stop.
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
	// Audit the Stop action in its own short tx (Abort writes no row itself — the durable "turn
	// stopped" message is persisted by the detached collector, which is excluded system churn).
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

// buildSystemPrompt assembles the per-message `system` prompt for the Ask Orchicon agent.
const historyByteBudget = 48 * 1024

// elideHistoryMessage shortens one history message to at most budget bytes, keeping the START and
// the END and cutting the middle.
func elideHistoryMessage(content string, budget int) string {
	if budget <= 0 {
		return "[earlier message elided to fit the context budget]"
	}
	if len(content) <= budget {
		return content
	}
	const marker = "\n\n[… elided to fit the context budget — re-read the file if you need this …]\n\n"
	if budget <= len(marker)+2 {
		return content[:max(0, budget)]
	}
	head := (budget - len(marker)) * 3 / 5
	tail := budget - len(marker) - head
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	return content[:head] + marker + content[len(content)-tail:]
}

func buildSystemPrompt(mode string, cfg db.AgentConfigRow, registry *ToolRegistry, history []db.MessageRow, includeHistory bool, attachments []*apiv1.AttachmentInput, projectContext, convProject, skillsSection string) string {
	var b strings.Builder

	b.WriteString(BuildSystemPrompt(mode, cfg, registry, skillsSection))
	b.WriteString("\n\n")

	if includeHistory {
		b.WriteString("## Conversation history\n")
		// history is in DESC (newest-first) order from the DB.
		for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
			history[i], history[j] = history[j], history[i]
		}
		start := 0
		if len(history) > 10 {
			start = len(history) - 10
		}
		// A MESSAGE COUNT IS NOT A SIZE BOUND, and treating it as one is how a local model gets a window it
		// cannot fit.
		remaining := historyByteBudget
		truncated := 0
		for _, h := range history[start:] {
			if h.Content == "" {
				continue
			}
			// A `system` row is a NOTICE the platform wrote ABOUT the conversation (a compaction record today),
			// not something either party said.
			if h.Role == "system" {
				continue
			}
			roleLabel := "User"
			if h.Role == "assistant" {
				roleLabel = "Orchicon"
			}
			content := h.Content
			// THE BUDGET IS CHECKED **BEFORE** THE MESSAGE IS CUT, AND THE COST INCLUDES THE LABEL.
			overhead := len(roleLabel) + 2 // ": " + "\n"
			avail := remaining - overhead
			if avail <= 0 {
				content = "[earlier message elided to fit the context budget]"
				truncated++
			} else if len(content) > avail {
				content = elideHistoryMessage(content, avail)
				truncated++
			}
			remaining -= len(content) + overhead
			if remaining < 0 {
				remaining = 0
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", roleLabel, content))
		}
		if truncated > 0 {
			fmt.Fprintf(&b, "\n(%d earlier message(s) were shortened to fit the context budget — ask for a file "+
				"to be re-read if you need its full contents.)\n", truncated)
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

	b.WriteString("## This conversation's project\n")
	if convProject != "" {
		b.WriteString(convProject)
		b.WriteString("\n")
	} else {
		b.WriteString("This conversation is not assigned to a project, so it has no project directory of its own. " +
			"NOTHING is pre-approved for it: a write or a command is outside this chat's scope and asks the user " +
			"first, wherever it falls. Reading is different — reads never ask, anywhere on the operator's machine. " +
			"The file/shell suite still has a directory to work in — call ask_file_root to see which — but it is only " +
			"the tenant's default anchor: because this chat has no project, that anchor is used SOLELY to resolve " +
			"relative paths, NOT as this chat's own workspace and not as anything pre-approved. Assign a project with " +
			"SetConversationProject (the TUI's /project, or a project folder in the GUI) to give this chat a workspace " +
			"of its own.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Enabled projects\n")
	if projectContext != "" {
		b.WriteString(projectContext)
		b.WriteString("\n")
	} else {
		b.WriteString("None yet — use the create_project tool to create one.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Available tools\n")
	b.WriteString("Orchicon's tools are exposed to you as MCP tools named `orchicon_<tool>` — call them directly through your tool mechanism and the system executes them against Orchicon, returning real results. Mutating tools run only after user confirmation. The native file/shell suite (batch_read, batch_grep, batch_write, read, grep, write, edit, list, glob, bash, ask_file_root) is also on your session as native tools — its reach, its pre-approved directory and when it asks are stated in \"## Reach and scope\" above.\n\n")
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

// resolveChatClient resolves the ChatTurnClient capability for an Ask conversation from its
// model_ref adapter kind, routed through the shared Dispatcher (ADR-0003 §3/§5).
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

// resolveClientForAbort resolves a ChatTurnClient to abort a conversation's live turn (Stop /
// supersede / Delete / sweeper).
func (s *Service) resolveClientForAbort(modelRef string) scheduler.ChatTurnClient {
	if c, err := s.resolveChatClient("", modelRef); err == nil {
		return c
	}
	return nil
}

// adapterScopedSessionID enforces adapter-scoped conversation session identity across a
// mid-conversation model/adapter switch (WI-3).
func (s *Service) adapterScopedSessionID(client scheduler.ChatTurnClient, sid string) string {
	if sid == "" {
		return ""
	}
	// The native bridge is OWNS-ALL: it is sessionless and accepts any id, so
	// the reverse direction (opencode→native) never resets the session.
	if owner, ok := client.(scheduler.SessionOwnerKind); ok && owner.SessionOwnerKind() == "orchicon" {
		return sid
	}
	// Any non-native adapter must never dispatch a native-synthetic id to its serve — clear it so the
	// collector creates a fresh session on this adapter instead.
	if strings.HasPrefix(sid, scheduler.NativeSessionIDPrefix) {
		return ""
	}
	return sid
}

// hostServeClient returns the host serve's session client, or nil when the session transport is
// unavailable (serve disabled, not started, or failed).
func (s *Service) hostServeClient() scheduler.ChatTurnClient {
	if s.testServeClient != nil {
		return s.testServeClient
	}
	return s.clientFromHostServe()
}

// clientFromHostServe adapts the opencode host serve's SessionClient onto the
// scheduler.ChatTurnClient capability used by the Ask path.
func (s *Service) clientFromHostServe() scheduler.ChatTurnClient {
	if s.hostServe == nil {
		return nil
	}
	return opencode.NewClientSessionAdapter(s.hostServe.Client())
}

// persistConversationSessionID saves the opencode session id on the conversation row (best-effort,
// its own tiny tenant tx).
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

// turnCollectOpts carries the fixed inputs for one detached chat-turn reply collector.
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
	// stallNoProgressSeconds is the tenant's stall_no_progress_window_seconds read at dispatch time (0
	// when unset).
	stallNoProgressSeconds *int64
	// onPartial mirrors the running turn's collected text/reasoning into the acked assistant message
	// row (throttled by the caller) so a client that lost the live stream — refresh, another tab.
	onPartial func(text string, reasoning []string)
	// ledger is the turn's live tool ledger: tool starts/resolutions recorded by the drain loop as they
	// happen.
	ledger *toolLedger
	// consent is the per-turn consent handle (the decision path + the pending-ask registry + the client
	// wake-up channel), shared across the turn's re-attach attempts.
	consent *consentTurn
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

// collectConversationReply is the detached reply collector for one chat turn.
func (s *Service) collectConversationReply(ctx context.Context, c turnCollectOpts) (text string, reasoning []string, sid string, err error) {
	defer s.turns.remove(c.convID, c.token)
	defer s.hubs.remove(c.convID)

	sid = c.sessionID
	system := c.reuseSystem
	recreated := false
	// The live tool ledger survives re-attach attempts (one pointer shared by every attempt of this
	// turn) so serve loss mid-tool never drops tool history.
	if c.ledger == nil {
		c.ledger = newToolLedger()
	}
	// The consent handle is ONE per turn (not per attempt): its grant lookups, pending asks and wake-up
	// channel must survive a serve re-attach.
	if c.consent == nil {
		c.consent = newConsentTurn(s, c.convID, c.tenantID, nil, c.ledger)
	}
	defer func() {
		// Turn end (C8): apply a decision that landed, expire every still-open ask
		// (reject) so the serve holds no phantom permission, and clear the gate.
		c.consent.finalize(context.WithoutCancel(ctx), c.client, c.onStreamEvent)
	}()
	// reconnects counts the bounded session recycles performed on an MCP wedge.
	reconnects := 0

	// A first message (no persisted session id) creates the session up front and persists the id
	// immediately; follow-ups reuse the persisted one.
	if sid == "" {
		fresh, cerr := c.client.CreateConversationSession(ctx, c.convID, "ask-orchicon:"+c.convID)
		if cerr != nil {
			return "", nil, "", fmt.Errorf("create conversation session: %w", cerr)
		}
		sid = fresh
		system = c.seedSystem
		s.persistConversationSessionID(ctx, c.tenantID, c.convID, sid)
	}

	window := newTurnReplyWindow(askReplyWindow())
	defer window.Stop()

	// serveDownDeadline bounds how long a turn whose serve has NEVER accepted a connection keeps
	// retrying: a serve down at send time fails fast with a clean.
	serveDownDeadline := time.Now().Add(askServeDownGrace())

	// everLive reports whether the serve accepted a connection at least once during this turn.
	everLive := false
	// reattaches counts how many times this turn has re-attached after losing a LIVE connection.
	reattaches := 0
	// contextLimitRetried reports whether this turn has ALREADY spent its one context-limit retry
	// (chatlimit.go).
	contextLimitRetried := false

	for {
		res := s.runOneTurnAttempt(ctx, window, c, sid, system, recreated)
		// Reasoning chunks observed across attempts survive serve loss: whatever arrived before a re-attach
		// is carried forward, matching the "partial reasoning is preserved" spirit for error paths.
		reasoning = append(reasoning, res.reasoning...)
		// Diff pipeline (AC 4): a turn's terminal transition reconciles the conversation's file-edit ledger
		// against the project dir's git state (best-effort.
		if res.kind == turnCollected || res.kind == turnFailed {
			if s.fileEditReconciler != nil {
				s.fileEditReconciler(ctx, c.tenantID, c.convID)
			}
		}
		switch res.kind {
		case turnCollected:
			return res.text, reasoning, sid, nil
		case turnFailed:
			// CONTEXT-LIMIT RECOVERY (chatlimit.go). HIDE THE ERROR ONLY WHEN IT WORKED.
			if !contextLimitRetried && isContextLimitFailure(res.err) {
				s.log.Warn("ask orchicon turn hit the context limit — compacting and retrying the turn",
					"conversation", c.convID, "session", sid, "model", c.modelRef, "error", res.err)
				contextLimitRetried = true
				if compacted, detail := s.compactAfterContextLimit(ctx, c.tenantID, c.convID, sid, c.modelRef, c.client); compacted {
					s.log.Info("ask orchicon context-limit compaction done — re-dispatching the turn",
						"conversation", c.convID, "session", sid, "detail", detail)
					// EVERYTHING THE ATTEMPT ACCUMULATED MUST BE DROPPED, or the retry would hand back the abandoned
					// attempt's work as this turn's THINKING: `reasoning` already absorbed res.reasoning above.
					reasoning = nil
					c.ledger = newToolLedger()
					continue
				} else {
					s.log.Warn("ask orchicon context-limit compaction could not recover the turn — reporting the original failure",
						"conversation", c.convID, "session", sid, "reason", detail)
				}
			}
			return res.text, reasoning, sid, res.err
		case turnRecreated:
			sid = res.newSid
			system = c.seedSystem
			recreated = true
			everLive = true
			s.persistConversationSessionID(ctx, c.tenantID, c.convID, sid)
		case turnToolWedge:
			// When the budget is spent, DO NOT abort the session — the tool may still be running legitimately
			// on the serve (the wedge window can only misfire on a slow-but-alive call).
			if reconnects >= askMCPReconnectAttempts() {
				s.log.Warn("ask orchicon tool-wedge recycle budget spent — continuing on the live session (turn stays alive; bounded by the reply window)",
					"conversation", c.convID, "session", sid, "tool", res.wedgeTool, "recycles", reconnects)
				// Re-enter the attempt loop WITHOUT aborting: the current session keeps streaming; if the tool
				// completes, the turn completes.
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
			everLive = true
			reattaches++
			if reattaches > askReattachAttemptBudget() {
				s.log.Warn("ask orchicon turn could not re-attach after the serve restarted — failing the turn",
					"conversation", c.convID, "session", sid, "attempts", reattaches)
				return res.text, reasoning, sid, errors.New(
					"the opencode serve restarted mid-reply and this conversation's session could not be " +
						"re-established — please send your message again")
			}
			select {
			case <-ctx.Done():
				return res.text, reasoning, sid, context.Cause(ctx)
			case <-window.C():
				return res.text, reasoning, sid, errors.New("the opencode serve did not recover within the reply window — please retry")
			case <-time.After(askReattachBackoff()):
			}
		case turnServeDown:
			// The serve never accepted a connection this attempt.
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
				case <-window.C():
					return res.text, reasoning, sid, errors.New("the opencode serve did not recover within the reply window — please retry")
				case <-time.After(askReattachBackoff()):
				}
			}
		}
	}
}

// runOneTurnAttempt drives a single subscribe+send+drain attempt of the detached collector.
func (s *Service) runOneTurnAttempt(ctx context.Context, window *turnReplyWindow, c turnCollectOpts, sid, system string, recreated bool) turnAttemptResult {

	// progress is the ONE place this attempt reports forward motion, and it must stay one place:
	// markActivity feeds the registry's lastActivity (the sweeper's TTL basis and the frontend's.
	progress := func() {
		s.turns.markActivity(c.convID, c.token)
		window.Touch()
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sub, err := c.client.Subscribe(subCtx, c.convID)
	if err != nil {
		// The serve never accepted a connection this attempt.
		return turnAttemptResult{kind: turnServeDown}
	}
	defer sub.Close()

	// Send the user message while the drain loop below is already live.
	sendCh := make(chan error, 1)
	// RE-DISPATCHABLE, AND THAT IS THE POINT: a serve that does not ACCEPT the message in time is a
	// transient far more often than a failure — it is still binding its socket.
	var sendCancels []context.CancelFunc
	defer func() {
		for _, c := range sendCancels {
			c()
		}
	}()
	sendCtx, cancelSend := context.WithCancel(subCtx)
	sendCancels = append(sendCancels, cancelSend)
	dispatchSend := func() {
		go func() {
			// Attachments require the OPTIONAL attachment-aware capability.
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
				sendCh <- acc.SendTurnMessageWithAttachments(sendCtx, c.convID, sid, system, c.modelRef, c.userMsg, parts)
				return
			}
			sendCh <- c.client.SendTurnMessage(sendCtx, c.convID, sid, system, c.modelRef, c.userMsg)
		}()
	}
	dispatchSend()
	// handshakeAttempts counts ACCEPT attempts. The first is dispatched above; a timeout re-dispatches
	// up to the budget, and only then does the operator see the message.
	handshakeAttempts := 1

	var reply strings.Builder
	var reasoning []string
	// observedInjections is how many injections have already been accounted for in THIS attempt.
	var observedInjections int64
	// Live-delta buffers for the partial-reply mirror: completed parts are authoritative
	// (reply/reasoning), but between parts the model streams token deltas.
	var liveText strings.Builder
	var liveReasoning strings.Builder
	// segThink demuxes folded think segments (GLM/DeepSeek) out of the TEXT delta stream: think bodies
	// route into the reasoning channel as ONE growing entry, plain text grows liveText.
	segThink := thinkSegmenter{}
	// committedThink tracks bodies already committed to the durable reasoning slice so a folded body
	// that arrives via BOTH the live delta path and a later completed text part is not double-counted.
	committedThink := map[string]bool{}
	// lastMirror throttles how often the drain loop snapshots the live buffers
	// into the partial mirror (the flusher further throttles the DB writes).
	var lastMirror time.Time
	// The timer is armed/stopped ONLY on the drain loop's goroutine and its expiry is consumed by the
	// loop's select below, so mirrorSnapshot's reads of the live buffers stay single-goroutine.
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
	// mirrorSnapshot builds the live partial snapshot for onPartial: authoritative completed parts +
	// the delta tail.
	mirrorSnapshot := func() (string, []string) {
		rsn := append([]string(nil), reasoning...)
		if liveReasoning.Len() > 0 {
			rsn = append(rsn, liveReasoning.String())
		}
		return reply.String() + liveText.String(), rsn
	}
	// commitThink appends a demuxed folded-think body to the durable reasoning slice and emits it as a
	// reasoning stream event.
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
	// The durable values were built ONLY from COMPLETED parts, while the livetail buffers held the
	// deltas not yet superseded by one.
	settleAttempt := func() (string, []string) {
		tail := liveReasoning.String()
		flushed := ""
		segThink.flushBody(func(b string) {
			if b != "" {
				flushed = b
			}
			commitThink(b)
		})
		if rest := foldReasoningTail(tail, flushed); strings.TrimSpace(rest) != "" {
			reasoning = append(reasoning, rest)
		}
		liveReasoning.Reset()
		text := reply.String()
		if liveText.Len() > 0 {
			text += liveText.String()
		}
		return text, reasoning
	}
	sent := false
	// The handshake bound (ORCHICON_ASK_TIMEOUT) starts after subscribe and only fires while the
	// message is still un-accepted, so a wedged serve that never acknowledges the send fails fast.
	handshake := time.NewTimer(askTimeout())
	defer handshake.Stop()

	// Stall detection (ADR-ASK-1): a model that hangs mid-generation (no events) or loops on the same
	// tool call trips the monitor, which aborts the serve session NOW and fails the turn with a clear.
	monitor := newChatStallMonitor(c.modelRef, c.stallNoProgressSeconds)
	// AND TELL IT WHICH CALLS IT CANNOT JUDGE BY SILENCE.
	if inProcessToolRunner(c.client) {
		monitor.setLocallyBoundedTools(hostSuiteToolNames)
	}
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

	// The consent layer's wake-up channel: a client decision on one of this turn's asks nudges the
	// select below.
	var consentReplies <-chan struct{}
	if c.consent != nil {
		c.consent.monitor = monitor
		consentReplies = c.consent.Reply()
	}

	for {
		select {
		case <-subCtx.Done():
			// Registry cancel (Stop / supersede / TTL expiry) or the request-context's cancellation — the turn
			// ends without a reply.
			finalText, finalReasoning := settleAttempt()
			return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: context.Cause(subCtx)}
		case <-window.C():
			// A TURN WAITING ON A HUMAN IS NOT A STALL, SO THIS WINDOW DOES NOT APPLY TO IT: it RE-ARMS instead
			// of ending anything.
			if c.consent != nil {
				if asking, ok := c.consent.waitingOnOperator(); ok {
					s.log.Info("ask orchicon: reply window reached while the turn waits on the operator — re-arming, the model is not the one being waited on",
						"conversation", c.convID, "waiting_on", oneLine(asking, 120))
					// Touch, NOT progress(): the turn did nothing, so it must not claim
					// activity — only its own deadline is restarted, from this moment.
					window.Touch()
					continue
				}
			}
			finalText, finalReasoning := settleAttempt()
			return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: fmt.Errorf("reply timed out after %s on model %s — the model may be overloaded or unavailable. Check the Ask Orchicon model in Settings → Default models, then retry.", askReplyWindow(), c.modelRef)}
		case <-handshake.C:
			if !sent {
				// RETRY IT OURSELVES FIRST.
				if handshakeAttempts < askHandshakeAttempts() {
					handshakeAttempts++
					// Cancel the in-flight attempt BEFORE re-dispatching, so the serve can never be
					// handed the same user message twice (a duplicate reply is worse than a timeout).
					cancelSend()
					sendCtx, cancelSend = context.WithCancel(subCtx)
					sendCancels = append(sendCancels, cancelSend)
					s.log.Warn("ask orchicon: the serve did not accept the message in time — retrying",
						"conversation", c.convID, "session", sid, "model", c.modelRef,
						"attempt", handshakeAttempts, "of", askHandshakeAttempts(), "window", askTimeout())
					dispatchSend()
					handshake.Reset(askTimeout())
					continue
				}
				finalText, finalReasoning := settleAttempt()
				return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: fmt.Errorf("the opencode serve did not accept the message after %d attempt(s) of %s — it may still be starting. Try again, or check the serve log.", handshakeAttempts, askTimeout())}
			}
		case <-stallTicker.C:
			// First, the AC1 MCP-wedge signal: a tool call issued but never resolved.
			if tool, wedged := monitor.toolWedge(); wedged {
				s.log.Warn("ask orchicon turn wedged on a tool call", "conversation", c.convID, "session", sid, "model", c.modelRef, "tool", tool)
				finalText, finalReasoning := settleAttempt()
				return turnAttemptResult{kind: turnToolWedge, text: finalText, reasoning: finalReasoning, wedgeTool: tool, err: fmt.Errorf("tool %s did not respond", tool)}
			}
			if reason := monitor.stallReason(); reason != "" {
				// The model has stopped making progress: interrupt it NOW (the same abort the Stop button uses) and
				// fail the turn with a clear, retryable message that names the model.
				_ = c.client.AbortConversationSession(context.WithoutCancel(subCtx), sid)
				s.log.Warn("ask orchicon turn stalled", "conversation", c.convID, "session", sid, "model", c.modelRef, "reason", reason)
				// SETTLE, OR THE ABORT TAKES THE WORK WITH IT.
				finalText, finalReasoning := settleAttempt()
				// NAME THE LIKELIEST CAUSE FIRST, and it is not the model: a model whose PROVIDER has no
				// credentials for this plane produces NO TOKENS AT ALL, which is exactly what this stall measures.
				return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: fmt.Errorf("The model (%s) produced no output (%s). The most common cause is that its PROVIDER IS NOT AUTHENTICATED for this plane — a model can be listed by `opencode models` and still have no credentials, in which case every turn is silent. Check the provider with `opencode auth list`, sign it in with `opencode auth login`, or choose a model whose provider is already authenticated. A rate-limited or unavailable provider looks the same.", c.modelRef, reason)}
			}
		case <-flushTick.C:
			// Trailing mirror flush: the throttle window elapsed with an unflushed delta tail.
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
						finalText, finalReasoning := settleAttempt()
						return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: fmt.Errorf("recreate conversation session: %w", cerr)}
					}
					return turnAttemptResult{kind: turnRecreated, newSid: fresh}
				}
				finalText, finalReasoning := settleAttempt()
				return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: fmt.Errorf("conversation session send: %w", res)}
			}
			sent = true
		case <-consentReplies:
			// A client decision landed for one of this turn's asks: answer the serve and resume.
			for _, r := range c.consent.applyClientReplies(context.WithoutCancel(subCtx), c.client) {
				emitAskResolution(c.onStreamEvent, c.convID, r)
			}
			// AND PUSH THE LEDGER TO DISK NOW, because the ledger is the record every OTHER client reconciles
			// against and it is only persisted when the partial mirror flushes.
			if c.onPartial != nil {
				snapText, snapRsn := mirrorSnapshot()
				c.onPartial(snapText, snapRsn)
			}
		case evt, ok := <-sub.Events():
			if !ok {
				// Bus closed — the serve died mid-reply. Re-attach (bounded
				// by the reply window in the collector loop).
				s.log.Warn("ask orchicon serve bus closed mid-turn", "conversation", c.convID, "session", sid, "cause", "sse-bus-close")
				finalText, finalReasoning := settleAttempt()
				return turnAttemptResult{kind: turnReattach, text: finalText, reasoning: finalReasoning}
			}
			if evt.SessionID != "" && evt.SessionID != sid {
				continue
			}
			switch evt.Kind {
			case "idle":
				// Turn complete — but only once OUR message was accepted (sent).
				if sent {
					// The counter is read from the registry, which is incremented ONLY after the adapter confirmed
					// acceptance — an injection that failed cannot keep a turn alive.
					if seq := s.turns.injectionSeq(c.convID, c.token); seq > observedInjections {
						observedInjections = seq
						if age := time.Since(s.turns.injectionAcceptedAt(c.convID, c.token)); age < injectionIdleGrace {
							s.log.Info("ask orchicon idle deferred — an injection was accepted after it",
								"conversation", c.convID, "injections", seq, "age", age.String())
							continue
						}
						// THE INJECTION IS OLD AND THE SERVE HAS GONE QUIET ANYWAY: this idle is a real completion.
						s.log.Warn("ask orchicon idle accepted despite an older injection",
							"conversation", c.convID, "injections", seq, "age", time.Since(s.turns.injectionAcceptedAt(c.convID, c.token)).String())
					}
					finalText, finalReasoning := settleAttempt()
					return turnAttemptResult{kind: turnCollected, text: strings.TrimSpace(finalText), reasoning: finalReasoning}
				}
			case "question":
				// THE PAUSE.
				if pid := evt.PermissionID; pid != "" {
					if qt := c.consent; qt != nil {
						ask, refusal := qt.raiseQuestion(sid, evt)
						if ask != nil {
							emitPermissionAsk(c.onStreamEvent, ask)
							qt.logAsk(ask)
						}
						if refusal != "" {
							s.log.Warn("ask orchicon: refused to raise a question",
								"conversation", c.convID, "reason", refusal)
							// THE REFUSAL CARRIES THE PREFIX, AND THAT IS THE WHOLE FIX.
							_ = c.client.ReplyPermissionDecision(context.WithoutCancel(subCtx), sid, pid,
								orchicon.ConsentRefusedPrefix+refusal)
						}
					}
				}
			case "permission":
				// We NEVER blind auto-approve: the interactive Ask profile is what makes these asks real.
				if pid := evt.PermissionID; pid != "" {
					if ct := c.consent; ct != nil {
						resp, ask, refusal := ct.decide(subCtx, sid, evt)
						if ask != nil {
							emitPermissionAsk(c.onStreamEvent, ask)
							ct.logAsk(ask)
						}
						if refusal != "" {
							s.log.Warn("ask orchicon consent: refused a tool call",
								"conversation", c.convID, "reason", refusal)
						}
						if resp != "" {
							rc, rsid, rpid := c.client, sid, pid
							// CARRY THE REASON WHEN THE LAYER REFUSED THE CALL.
							decision := resp
							if ask == nil && refusal != "" {
								decision = orchicon.ConsentRefusedPrefix + refusal
							}
							go func() { _ = rc.ReplyPermissionDecision(context.WithoutCancel(subCtx), rsid, rpid, decision) }()
						}
					} else {
						// No consent handle (a bare attempt test): keep the historical
						// auto-approve so the turn cannot wedge.
						go func() { _ = c.client.ReplyPermission(subCtx, sid, pid) }()
					}
				}
			case "error":
				// The turn failed at the model/API level: record it and end the turn (the session is kept).
				s.log.Warn("ask orchicon session error", "conversation", c.convID, "message", evt.Text)
				// CARRY WHAT ARRIVED.
				finalText, finalReasoning := settleAttempt()
				return turnAttemptResult{kind: turnFailed, text: finalText, reasoning: finalReasoning, err: errors.New(evt.Text)}
			case "delta":
				// Mid-generation token deltas are liveness evidence and the live partial-reply mirror.
				if !sent {
					continue
				}
				monitor.observe("text", nil)
				progress()
				// Suspect-kind gate: while the folded-think segmenter has a think run open, even a native
				// `reasoning` delta is suspect (the serve streams text AND reasoning through the same field:"text".
				if evt.IsReasoning && !segThink.inThink() {
					liveReasoning.WriteString(evt.Text)
					// NOT FORWARDED ON THE STREAM, and that is deliberate — it was, briefly, and it duplicated every
					// reasoning block in the TUI ("your reasoning block is repeating over and over as you stream").
				} else {
					segThink.feed(evt.Text,
						// emitText is the DEMUXED clean-text seam: a folded think body never reaches this callback, so
						// forwarding it streams the answer as it is typed WITHOUT leaking thinking tags into the visible.
						func(t string) {
							liveText.WriteString(t)
							if c.onStreamEvent != nil {
								c.onStreamEvent(&apiv1.ChatStreamResponse{
									Event: &apiv1.ChatStreamResponse_TextChunk{
										TextChunk: &apiv1.TextChunk{Content: t},
									},
								})
							}
						},
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
					// Throttled: arm the one-shot trailing flush for the throttle-window remainder (min 10ms) so the
					// delta tail drains shortly after the LAST delta.
					delay := 200*time.Millisecond - time.Since(lastMirror)
					if delay < 10*time.Millisecond {
						delay = 10 * time.Millisecond
					}
					flushTick.Reset(delay)
					flushArmed = true
				}
			case "tool_part":
				// A tool call ISSUED but not yet resolved (AC1 MCP-wedge).
				if !sent {
					continue
				}
				monitor.observeToolStart(evt.Text)
				progress()
				// Live tool ledger: the call is recorded the moment it is issued (not at completion) and mirrored
				// immediately, so a session killed while the tool runs still shows the call.
				c.ledger.recordStart(evt.Text)
				if c.onPartial != nil {
					snapText, snapRsn := mirrorSnapshot()
					c.onPartial(snapText, snapRsn)
				}
			case "tool_result":
				// A tool call RESOLVED, carrying its arguments and output as TYPED fields (adapter-neutral — no
				// Part map to imitate).
				if !sent {
					continue
				}
				c.ledger.recordToolResolution(evt.ToolName, evt.ArgsJSON, evt.Output, evt.IsError)
				// This event is the ONLY resolution signal the native adapter sends — there is no
				// LegacyEventFromBus "tool_use" part on this transport.
				monitor.closeTool()
				// A RESOLVED TOOL IS FORWARD MOTION, and the monitor's clocks measure the absence of exactly that.
				progress()
			case "part":
				// Completed telemetry part (the same LegacyEventFromBus mapping executions use — the adapter
				// classified it).
				if !sent {
					continue
				}
				monitor.observe(evt.Type, evt.Part)
				progress()
				// Diff pipeline (AC 1): completed mutating tool_use parts on an Ask turn ledger file edits from
				// real file-state snapshots — owner_kind ask_conversation, same ground truth executions record.
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
					// Completed text part may itself carry one or more folded think segments (GLM/DeepSeek).
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
					// Step completion carries token usage + cost (docs/04 §6.1).
					s.recordTurnUsage(ctx, c, evt.Part)
				case "tool_use":
					// Live tool ledger: a completed tool call resolves the in-flight entry (arguments backfilled,
					// result appended) and mirrors immediately — a killed session leaves the tool activity visible.
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

// recordTurnUsage captures a live usage sample from an Ask step_finish event (docs/04 §6.1: the
// opencode tokens/cost shape) through the canonical aigateway dual-write worker executions use.
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
	// Derive the provider/model via the adapter-agnostic split and the adapter kind from segment 1 of
	// the model ref.
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

// turnCacheToken reads a sub-count from the opencode tokens.cache sub-object (e.g.
// {"cache":{"read":N,"write":M}} → read/write).
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

// persistConversationReply persists the collected assistant message for a turn under the acked
// message id and bumps the conversation timestamp.
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
	// The live tool ledger is snapshotted with the terminal reply: the same tx carries text + reasoning
	// + tool activity, so a crash lands the full turn atomically and the finalize's overwrite can.
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

// upsertPartialMessage writes the running turn's partial reply under the acked assistant message id
// (best-effort, its own tiny tenant tx so the collector's hot loop never blocks on the DB).
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

// runOpenCodeTurn drives one chat turn over a persistent opencode session on the host serve (the
// session transport):
func (s *Service) runOpenCodeTurn(ctx context.Context, client scheduler.ChatTurnClient, tenantID, convID, sessionID, modelRef, seedSystem, reuseSystem, userMsg string, cb streamCallback) (msgID, newSessionID string, elapsed time.Duration, err error) {
	start := time.Now()
	msgID = db.NewID()

	// Resolve the session: reuse the persisted id, or create a fresh one (first message).
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

	// Send the user message while the drain loop below is already live.
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

	// The consent handle for this legacy path (no client stream to carry the card, no stall monitor):
	// the decision chain still runs and the reply RPC still finds the ask in the shared registry.
	legacyConsent := newConsentTurn(s, convID, tenantID, nil, nil)
	defer legacyConsent.finalize(context.WithoutCancel(ctx), client, nil)

	for {
		select {
		case <-ctx.Done():
			// Client disconnected (Stop button / browser close): abort the turn so the model stops burning
			// tokens answering a gone client; the session is preserved for the next message.
			_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
			return msgID, sid, time.Since(start), ctx.Err()
		case <-timeout.C:
			_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
			return msgID, sid, time.Since(start), fmt.Errorf("request timed out after %s — the model may be overloaded or unavailable", askTimeout())
		case res := <-sendCh:
			// Our message was accepted (or rejected).
			if res.err != nil {
				return msgID, sid, time.Since(start), fmt.Errorf("conversation session send: %w", res.err)
			}
			sent = true
		case <-legacyConsent.Reply():
			legacyConsent.applyClientReplies(context.WithoutCancel(ctx), client)
		case evt, ok := <-sub.Events():
			if !ok {
				return msgID, sid, time.Since(start), fmt.Errorf("opencode session stream ended")
			}
			if evt.SessionID != "" && evt.SessionID != sid {
				continue
			}
			switch evt.Kind {
			case "idle":
				// Turn complete — but only once OUR message was accepted (sent).
				if sent {
					return msgID, sid, time.Since(start), nil
				}
			case "question":
				// THE PAUSE, on the legacy non-streaming path.
				if pid := evt.PermissionID; pid != "" {
					if qt := legacyConsent; qt != nil {
						if _, refusal := qt.raiseQuestion(sid, evt); refusal != "" {
							s.log.Warn("ask orchicon: refused to raise a question", "conversation", convID, "reason", refusal)
						}
						_ = client.ReplyPermissionDecision(context.WithoutCancel(ctx), sid, pid,
							"ask_user cannot be answered on this transport — ask it again in the Ask UI")
					}
				}
			case "permission":
				// Same consent decision path as the primary drain loop.
				if pid := evt.PermissionID; pid != "" {
					resp, ask, refusal := legacyConsent.decide(ctx, sid, evt)
					if ask != nil {
						legacyConsent.logAsk(ask)
					}
					if refusal != "" {
						s.log.Warn("ask orchicon consent: refused a tool call",
							"conversation", convID, "reason", refusal)
					}
					if resp != "" {
						rc := client
						go func() { _ = rc.ReplyPermissionDecision(context.WithoutCancel(ctx), sid, pid, resp) }()
					}
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
					// stream.Send failed — the client is gone.
					_ = client.AbortConversationSession(context.WithoutCancel(ctx), sid)
					return msgID, sid, time.Since(start), nil
				}
			}
		}
	}
}

// fetchProjectContext returns a compact, current list of the tenant's enabled projects (title,
// status, directory, goals) for injection into the prompt.
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

// conversationProjectContext names the project THIS conversation belongs to, and says what that
// means for the turn about to run.
func (s *Service) skillManifestSection(ctx context.Context, tenantID string, conv db.ConversationRow) string {
	var convSkills []string
	if len(conv.SkillFiles) > 0 {
		_ = json.Unmarshal(conv.SkillFiles, &convSkills)
	}
	var projectSkills []string
	projectDir := ""
	if conv.ProjectID != "" {
		if p, err := conversationProjectRow(ctx, s.pool, tenantID, conv.ProjectID); err == nil {
			projectDir = p.ProjectDir
			if len(p.SkillFiles) > 0 {
				_ = json.Unmarshal(p.SkillFiles, &projectSkills)
			}
		}
	}
	union := contextfiles.Union(projectSkills, convSkills)
	if len(union) == 0 {
		return ""
	}
	if err := contextfiles.ValidateWithin(union, projectDir); err != nil {
		s.log.Info("dropping out-of-project skill files from the Ask system prompt",
			"conversation", conv.ID, "error", err.Error())
		return ""
	}
	// ManifestPolicySkill, NOT the default: a skill is the operator's instruction set, so its CONTENTS
	// are inlined regardless of size.
	return contextfiles.RenderManifestWithPolicy("# Skills", union, projectDir, contextfiles.ManifestPolicySkill)
}

func (s *Service) conversationProjectContext(ctx context.Context, tenantID string, conv db.ConversationRow) string {
	if conv.ProjectID == "" {
		return ""
	}
	// The SAME load the tool layer's scope resolver uses (conversationProjectRow), so the directory named here and
	// the directory the file/shell suite binds to are one read of one row — they cannot drift.
	p, err := conversationProjectRow(ctx, s.pool, tenantID, conv.ProjectID)
	if err != nil {
		// A project deleted out from under the conversation (archived, or hard-deleted with its tenant).
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("This chat belongs to the project **%s** (ID %s, status %s)", p.Name, p.ID, p.Status))
	if p.ProjectDir != "" {
		b.WriteString(fmt.Sprintf(", whose directory is `%s`", p.ProjectDir))
	}
	b.WriteString(".\n")
	if p.ProjectDir != "" {
		b.WriteString(fmt.Sprintf("That directory is the folder this conversation's work happens in — treat paths "+
			"in this chat as relative to `%s` unless a message says otherwise, and create or edit files there rather "+
			"than in some other project's tree.\n", p.ProjectDir))
		b.WriteString(fmt.Sprintf("It is also this conversation's DEFAULT SCOPE and its PRE-APPROVED directory: a write "+
			"or an execution inside `%s` proceeds without asking, and that is what the association buys. The suite can "+
			"still reach the rest of the host — reads never ask anywhere — but work outside this directory, a sibling "+
			"project's tree included, is out of scope and asks the user first, so say what you intend to touch there "+
			"rather than assuming it is approved.\n", p.ProjectDir))
	}
	if p.Status != "" && p.Status != "active" {
		b.WriteString(fmt.Sprintf("NOTE: this project is `%s`, not active — say so if a request depends on it running.\n", p.Status))
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

// foldReasoningTail returns the live reasoning tail with the body the segmenter flush JUST
// committed stripped from it, so an unterminated folded body is not recorded twice.
func foldReasoningTail(tail, flushed string) string {
	if flushed == "" {
		return tail
	}
	if strings.HasSuffix(tail, flushed) {
		return strings.TrimSuffix(tail, flushed)
	}
	return tail
}

// inProcessToolRunner reports whether this turn's transport executes the model's tool calls itself.
func inProcessToolRunner(client scheduler.ChatTurnClient) bool {
	r, ok := client.(scheduler.InProcessToolRunner)
	return ok && r != nil && r.ToolsRunInProcess()
}
