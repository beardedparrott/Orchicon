package acp

// session.go — SESSIONS AND TURNS.
//
// A Session wraps one agy conversation. Updates stream on Events(); Prompt blocks until agy
// answers with a stopReason. The two are independent on purpose: a consumer may drain Events()
// slowly (or not at all) without stalling the turn, because updates are queued rather than
// handed straight to the channel.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// eventsBuffer is the depth of a session's public Events() channel. It only bounds how far the
// pump may run ahead of the consumer; the queue behind it is unbounded, so a slow consumer delays
// nothing on the wire.
const eventsBuffer = 512

// Session is one ACP conversation with agy.
type Session struct {
	conn *conn
	id   string

	queue  *eventQueue
	events chan Update

	// done closes when the session or client ends; it releases the queue pump.
	done     chan struct{}
	doneOnce sync.Once
	closed   atomic.Bool

	// promptMu serialises turns: agy defines no behaviour for a second session/prompt while one
	// is in flight (docs, GAP TABLE), so a caller has to queue behind the current turn.
	promptMu sync.Mutex
}

// newSession returns the Session for id, building and registering one if there is not already a
// live one.
//
// REUSE, NOT REPLACE, IS THE POINT: `session/resume` re-attaches the SAME agy conversation, so its
// updates keep arriving with the same sessionId. Registering a second Session object for that id
// would silently orphan the first one's Events() channel — the caller holding it would wait
// forever. One id, one Session.
func (c *conn) newSession(id string) *Session {
	c.sessMu.Lock()
	if existing, ok := c.sessions[id]; ok && existing.isLive() {
		c.sessMu.Unlock()
		return existing
	}
	s := &Session{
		conn:   c,
		id:     id,
		queue:  newEventQueue(),
		events: make(chan Update, eventsBuffer),
		done:   make(chan struct{}),
	}
	go s.queue.pump(s.events, s.done)
	c.sessMu.Unlock()
	c.registerSession(s)
	return s
}

// ID is agy's session id.
func (s *Session) ID() string { return s.id }

// Events streams decoded updates for this session, in wire order. It is closed when the session
// closes or the client's process exits.
func (s *Session) Events() <-chan Update { return s.events }

// push enqueues one update. It never blocks.
func (s *Session) push(u Update) { s.queue.push(u) }

// closeEvents ends the events stream exactly once.
func (s *Session) closeEvents() {
	s.doneOnce.Do(func() {
		s.closed.Store(true)
		s.queue.close()
		close(s.done)
	})
}

// isLive reports whether this session can still receive updates.
func (s *Session) isLive() bool { return !s.closed.Load() }

// NewSession opens a conversation (`session/new`).
func (c *Client) NewSession(ctx context.Context, p NewSessionParams) (*Session, *SessionState, error) {
	servers := p.MCPServers
	if servers == nil {
		// agy expects the key present and an array; `null` is not the same as `[]`.
		servers = []MCPServer{}
	}
	wire := newSessionWire{
		Cwd:                   p.Cwd,
		MCPServers:            servers,
		AdditionalDirectories: p.AdditionalDirectories,
		Meta:                  newSessionMeta(p.SystemPromptAppend),
	}
	var res sessionResult
	if err := c.conn.call(ctx, methodSessionNew, wire, &res); err != nil {
		return nil, nil, err
	}
	if res.SessionID == "" {
		return nil, nil, errors.New("acp: session/new returned no sessionId")
	}
	s := c.conn.newSession(res.SessionID)
	return s, stateFrom(res, res.SessionID), nil
}

// LoadSession replays an existing conversation (`session/load`). agy replays the history as
// session/update notifications, which arrive on the returned Session's Events(). The session id
// is registered BEFORE the request is written so the replay cannot outrun it.
func (c *Client) LoadSession(ctx context.Context, sessionID string, p LoadParams) (*Session, *SessionState, error) {
	return c.attachSession(ctx, methodSessionLoad, sessionID, p)
}

// ResumeSession re-attaches to an existing conversation (`session/resume`) without replaying its
// history.
func (c *Client) ResumeSession(ctx context.Context, sessionID string, p LoadParams) (*Session, *SessionState, error) {
	return c.attachSession(ctx, methodSessionResume, sessionID, p)
}

func (c *Client) attachSession(ctx context.Context, method, sessionID string, p LoadParams) (*Session, *SessionState, error) {
	if sessionID == "" {
		return nil, nil, errors.New("acp: " + method + " needs a session id")
	}
	servers := p.MCPServers
	if servers == nil {
		servers = []MCPServer{}
	}
	wire := loadWire{
		SessionID:             sessionID,
		Cwd:                   p.Cwd,
		MCPServers:            servers,
		AdditionalDirectories: p.AdditionalDirectories,
		Meta:                  newSessionMeta(p.SystemPromptAppend),
	}
	// Registered first: agy's load replay can arrive before the response that returns it.
	s := c.conn.newSession(sessionID)
	var res sessionResult
	if err := c.conn.call(ctx, method, wire, &res); err != nil {
		c.conn.unregisterSession(sessionID)
		return nil, nil, err
	}
	id := res.SessionID
	if id == "" {
		id = sessionID
	}
	return s, stateFrom(res, id), nil
}

// stateFrom builds the caller-facing session state from a wire result.
func stateFrom(res sessionResult, sessionID string) *SessionState {
	return &SessionState{
		SessionID:      sessionID,
		Models:         res.Models.AvailableModels,
		CurrentModelID: res.Models.CurrentModelID,
		Modes:          res.Modes,
		ConfigOptions:  res.ConfigOptions,
		Meta:           res.Meta,
		Raw:            res.Raw,
	}
}

// ListSessions enumerates agy's known conversations (`session/list`).
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	var res listResult
	if err := c.conn.call(ctx, methodSessionList, struct{}{}, &res); err != nil {
		return nil, err
	}
	return res.Sessions, nil
}

// SetModel selects a model for the session (`session/set_model`). An unknown model id is an
// RPC -32602 error and leaves the session usable.
func (s *Session) SetModel(ctx context.Context, modelID string) error {
	return s.conn.call(ctx, methodSessionSetModel, setModelParams{SessionID: s.id, ModelID: modelID}, nil)
}

// SetMode selects a mode (`session/set_mode`).
func (s *Session) SetMode(ctx context.Context, modeID string) error {
	return s.conn.call(ctx, methodSessionSetMode, setModeParams{SessionID: s.id, ModeID: modeID}, nil)
}

// Prompt runs one turn. It blocks until agy answers with a stopReason; the streamed updates
// arrive on Events() meanwhile.
//
// Cancellation is cooperative: when ctx ends, ONE `session/cancel` is sent and the call then waits
// up to CancelGrace for agy's `stopReason:"cancelled"` result, which is returned alongside
// ctx.Err(). Without the grace the caller would lose the turn's usage numbers and, worse, leave a
// response arriving later for a request nobody is waiting on.
func (s *Session) Prompt(ctx context.Context, blocks []ContentBlock) (*PromptResult, error) {
	p, err := s.startPrompt(ctx, blocks, true)
	if err != nil {
		return nil, err
	}
	return p.Wait()
}

// errBusyTurn is the refusal StartPrompt returns when the session already has a turn in flight.
//
// agy defines no behaviour for a second `session/prompt` while one is outstanding (docs, GAP TABLE), so
// this is a hard rejection rather than a queue: a caller that wants to wait its turn uses Prompt.
var errBusyTurn = errors.New("acp: a turn is already running on this session — agy defines no behaviour " +
	"for a second session/prompt while one is in flight")

// ErrTurnBusy is errBusyTurn exported, so a caller can tell "this session is mid-turn" apart from a
// transport failure and report it as the operator-visible condition it is.
var ErrTurnBusy = errBusyTurn

// PendingPrompt is a turn that has been SENT but not yet answered.
//
// WHY IT EXISTS: `session/prompt` is one request whose RESPONSE is the end of the turn, but a turn is
// ACCEPTED the instant the request is on the wire, not when it finishes. Any consumer whose contract is
// "return once the message is accepted" (scheduler.ChatTurnClient.SendTurnMessage) would otherwise hold
// its send call open for the WHOLE turn — including every minute it spends parked on a
// `session/request_permission` card waiting for a human. That is not cosmetic: askorchicon bounds the
// send with a handshake timer and re-dispatches on expiry, so an antigravity turn parked on a consent
// card was being cancelled and re-sent every handshake window, and the parked card then expired
// unanswered at turn end. MEASURED on a local-mode plane: `permissionAsk` for `run_command` was raised
// promptly, then `permissionAskResolved{outcome:"expired"}` arrived at the handshake boundary followed
// by `turn_failed`.
//
// Wait blocks for the answer exactly as Prompt does. The session's one-turn-at-a-time rule is still
// enforced: promptMu is held from the send until the answer arrives.
type PendingPrompt struct {
	ch   chan rpcMessage
	done chan struct{}

	mu  sync.Mutex
	res *PromptResult
	err error
}

// Wait blocks until agy answers the turn and reports that answer. Safe to call more than once: the
// result is recorded, so a second call returns it rather than blocking forever.
func (p *PendingPrompt) Wait() (*PromptResult, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.res, p.err
}

// Done reports whether the turn has already been answered.
func (p *PendingPrompt) Done() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// StartPrompt sends one turn and returns as soon as the request is ON THE WIRE, without waiting for the
// turn to finish. The caller drains Events() as usual and calls Wait for the outcome.
//
// Cancellation travels through ctx exactly as for Prompt: when ctx ends, `session/cancel` is sent so agy
// stops generating and answers any outstanding permission request with `cancelled`.
func (s *Session) StartPrompt(ctx context.Context, blocks []ContentBlock) (*PendingPrompt, error) {
	return s.startPrompt(ctx, blocks, false)
}

// startPrompt is the one implementation. waitForSlot chooses what happens when the session already has a
// turn in flight: Prompt QUEUES behind it (the worker path's long-standing behaviour, where a second
// prompt is legitimate), while StartPrompt REFUSES (the Ask path's send must either be accepted now or
// report that the turn is busy — silently blocking would recreate the very stall this type exists to
// remove).
func (s *Session) startPrompt(ctx context.Context, blocks []ContentBlock, waitForSlot bool) (*PendingPrompt, error) {
	if len(blocks) == 0 {
		return nil, errors.New("acp: session/prompt needs at least one content block")
	}

	// One turn at a time: agy defines nothing for overlapping prompts.
	if waitForSlot {
		s.promptMu.Lock()
	} else if !s.promptMu.TryLock() {
		return nil, errBusyTurn
	}

	id, ch, err := s.conn.begin(methodSessionPrompt, promptParams{SessionID: s.id, Prompt: blocks})
	if err != nil {
		s.promptMu.Unlock()
		return nil, err
	}

	p := &PendingPrompt{ch: ch, done: make(chan struct{})}
	go func() {
		defer s.conn.forget(id)
		defer s.promptMu.Unlock()
		res, err := s.awaitPrompt(ctx, ch)
		p.mu.Lock()
		p.res, p.err = res, err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// awaitPrompt waits for agy's answer to a prompt already on the wire, sending `session/cancel` and
// waiting out the grace period when ctx ends.
func (s *Session) awaitPrompt(ctx context.Context, ch chan rpcMessage) (*PromptResult, error) {
	select {
	case msg := <-ch:
		return promptResultFrom(msg)
	case <-ctx.Done():
		// Send the cancel, then wait out the grace period for agy's own terminator.
		s.cancelQuietly()
		grace := CancelGrace
		if grace <= 0 {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case msg := <-ch:
			res, rerr := promptResultFrom(msg)
			return res, errors.Join(ctx.Err(), rerr)
		case <-timer.C:
			return nil, ctx.Err()
		case <-s.conn.done:
			return nil, errors.Join(ctx.Err(), s.conn.closedErr())
		}
	case <-s.conn.done:
		return nil, s.conn.closedErr()
	}
}

// promptResultFrom decodes a prompt response, tolerating an error answer.
func promptResultFrom(msg rpcMessage) (*PromptResult, error) {
	var res PromptResult
	if err := decodeResult(methodSessionPrompt, msg, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// cancelQuietly sends session/cancel for the session and answers any outstanding permission
// request with `cancelled`, in the recording's order (cancel first, answer second). Failures are
// ignored on purpose: the turn is already being given up on.
func (s *Session) cancelQuietly() {
	if err := s.conn.notify(methodSessionCancel, sessionIDParams{SessionID: s.id}); err != nil {
		s.conn.logger.Debug("acp: could not send session/cancel", "err", err)
	}
	s.conn.cancelPerms(s.id, errTeardown)
}

// Cancel asks agy to end the current turn (`session/cancel`). The turn then finishes with
// `stopReason:"cancelled"`, including when a permission request is still outstanding — that
// outstanding request is answered `cancelled` here, exactly as agy's own recording does.
//
// THE ORDER IS THE RECORDING'S: `session/cancel` goes out first, and the outstanding permission is
// answered second (cancel_midturn.jsonl has exactly that order). cancelPerms then WAITS for that
// answer to reach the wire, so Cancel returning means the answer was written — without which a
// caller inspecting the stream right after Prompt returns is racing a write it cannot see. That race
// is measured: TestReplay_CancelMidturn failed once under a fully loaded gate and never once alone.
func (s *Session) Cancel() error {
	if err := s.conn.notify(methodSessionCancel, sessionIDParams{SessionID: s.id}); err != nil {
		// The child is already gone; the parked permission must STILL be answered, so the
		// request cannot be left hanging behind a failed notify.
		s.conn.cancelPerms(s.id, errTeardown)
		return err
	}
	s.conn.cancelPerms(s.id, errTeardown)
	return nil
}

// Close ends the session (`session/close`) and closes Events(). agy advertises the capability but
// 1.5.13 was never recorded exercising it, so an RPC error from it is tolerated.
func (s *Session) Close(ctx context.Context) error {
	err := s.conn.call(ctx, methodSessionClose, sessionIDParams{SessionID: s.id}, nil)
	s.conn.unregisterSession(s.id)
	return err
}
