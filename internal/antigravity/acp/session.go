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

	// promptMu serialises turns: agy defines no behaviour for a second session/prompt while one
	// is in flight (docs, GAP TABLE), so a caller has to queue behind the current turn.
	promptMu sync.Mutex
}

// newSession builds and registers a session.
func (c *conn) newSession(id string) *Session {
	s := &Session{
		conn:   c,
		id:     id,
		queue:  newEventQueue(),
		events: make(chan Update, eventsBuffer),
		done:   make(chan struct{}),
	}
	go s.queue.pump(s.events, s.done)
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
		s.queue.close()
		close(s.done)
	})
}

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
	if len(blocks) == 0 {
		return nil, errors.New("acp: session/prompt needs at least one content block")
	}

	// One turn at a time: agy defines nothing for overlapping prompts.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	id, ch, err := s.conn.begin(methodSessionPrompt, promptParams{SessionID: s.id, Prompt: blocks})
	if err != nil {
		return nil, err
	}
	defer s.conn.forget(id)

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
// request with `cancelled`. Failures are ignored on purpose: the turn is already being given up
// on.
func (s *Session) cancelQuietly() {
	s.conn.cancelPerms(s.id, errTeardown)
	if err := s.conn.notify(methodSessionCancel, sessionIDParams{SessionID: s.id}); err != nil {
		s.conn.logger.Debug("acp: could not send session/cancel", "err", err)
	}
}

// Cancel asks agy to end the current turn (`session/cancel`). The turn then finishes with
// `stopReason:"cancelled"`, including when a permission request is still outstanding — that
// outstanding request is answered `cancelled` here, exactly as agy's own recording does.
func (s *Session) Cancel() error {
	if err := s.conn.notify(methodSessionCancel, sessionIDParams{SessionID: s.id}); err != nil {
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
