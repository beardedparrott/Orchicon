package acp

// conn.go — THE TRANSPORT: newline-delimited JSON-RPC 2.0 over the child's stdio.
//
// One reader goroutine, correlation by raw id, a mutex around writes, and dispatch of agent
// requests onto their own goroutines. The two invariants that matter:
//
//   - THE READER NEVER BLOCKS ON A CONSUMER. Agent requests are served on their own goroutines
//     (a permission handler may wait on an operator for minutes) and session updates go into an
//     unbounded per-session queue drained by a pump. A blocked consumer therefore cannot stall
//     the stream, and a blocked handler cannot stop the answer streaming past it.
//   - A MALFORMED LINE IS NEVER FATAL. It is logged and skipped; the reader carries on. agy
//     writes diagnostics to the same fd on some platforms, and one bad line must not end a turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// CancelGrace is how long `session/prompt` keeps waiting for agy's `stopReason:"cancelled"`
// after a cancel was sent. A var so a test can shorten it.
var CancelGrace = 5 * time.Second

// permSettleGrace bounds how long cancelPerms waits for a cancelled permission request's answer to
// reach the wire. It is generous because the work it waits on is a single JSON write to a pipe, and
// only a handler that ignores its own cancellation can miss it — the same handler that
// decidePermission already gives up on.
var permSettleGrace = 2 * time.Second

// cancelCauseTeardown marks a cancellation that ends the turn from the client side (session
// cancel, session close, client close). It is distinguished from an ordinary context
// cancellation because the two want DIFFERENT permission answers: a torn-down turn answers
// `{"outcome":{"outcome":"cancelled"}}` (as agy's own cancel_midturn replay does), while any
// other cancellation FAILS CLOSED on the reject option.
type cancelCauseTeardown struct{}

func (cancelCauseTeardown) Error() string { return "acp: turn torn down" }

var errTeardown = cancelCauseTeardown{}

// eventQueue is an unbounded, mutex+cond queue between the reader and a session's Events()
// channel. It exists so the reader can enqueue without ever blocking: a slow consumer must not
// be able to stop the transport.
type eventQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Update
	closed bool
}

func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *eventQueue) push(u Update) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, u)
	q.cond.Signal()
}

func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.cond.Broadcast()
}

// pump drains the queue into out, preserving order, and closes out when the queue is closed and
// drained (or when done fires).
func (q *eventQueue) pump(out chan<- Update, done <-chan struct{}) {
	defer close(out)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		u := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()

		select {
		case out <- u:
		case <-done:
			return
		}
	}
}

// conn is the JSON-RPC connection to one agy process.
type conn struct {
	proc    Proc
	handler Handler
	logger  *slog.Logger
	stderr  *stderrRing

	nextID atomic.Int64

	wmu sync.Mutex

	pmu     sync.Mutex
	pending map[string]chan rpcMessage

	sessMu   sync.Mutex
	sessions map[string]*Session
	// early buffers updates whose session is not registered yet (belt and braces: by protocol
	// order they follow the request, but a load replay races the response).
	early map[string][]Update

	// perms tracks the cancellable context of each in-flight permission request, per session.
	permMu sync.Mutex
	perms  map[string]map[uint64]*inflightPerm
	permID atomic.Uint64

	serveCtx    context.Context
	serveCancel context.CancelCauseFunc

	done           chan struct{}
	doneOnce       sync.Once
	exitCode       int
	exitErr        error
	readerDone     chan struct{}
}

func newConn(proc Proc, handler Handler, logger *slog.Logger, stderr *stderrRing) *conn {
	if handler == nil {
		handler = rejectEverything{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &conn{
		proc:        proc,
		handler:     handler,
		logger:      logger,
		stderr:      stderr,
		pending:     make(map[string]chan rpcMessage),
		sessions:    make(map[string]*Session),
		early:       make(map[string][]Update),
		perms:       make(map[string]map[uint64]*inflightPerm),
		serveCtx:    ctx,
		serveCancel: cancel,
		done:        make(chan struct{}),
		readerDone:  make(chan struct{}),
	}
}

// closedErr renders the transport failure, with the stderr tail that explains it.
func (c *conn) closedErr() error {
	return &ClosedError{ExitCode: c.exitCode, Cause: c.exitErr, Stderr: c.stderr.Tail()}
}

// write serialises one outbound document onto the child's stdin.
func (c *conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("acp: encode %T: %w", v, err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.proc.WriteLine(b); err != nil {
		return err
	}
	return nil
}

// notify sends a JSON-RPC notification (no id, no response).
func (c *conn) notify(method string, params any) error {
	return c.write(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// respond answers an agent request with a result.
func (c *conn) respond(id json.RawMessage, result any) {
	if err := c.write(rpcResult{JSONRPC: "2.0", ID: id, Result: result}); err != nil {
		// The child is gone; nothing left to tell it. Log only — the fail-closed decision has
		// already been made and must not be turned into an error path.
		c.logger.Debug("acp: could not answer agent request", "err", err)
	}
}

// respondErr answers an agent request with a JSON-RPC error.
func (c *conn) respondErr(id json.RawMessage, code int, msg string) {
	doc := rpcFailure{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
	if err := c.write(doc); err != nil {
		c.logger.Debug("acp: could not answer agent request", "err", err)
	}
}

// call issues a request and waits for its response.
func (c *conn) call(ctx context.Context, method string, params any, out any) error {
	id, ch, err := c.begin(method, params)
	if err != nil {
		return err
	}
	defer c.forget(id)

	select {
	case msg := <-ch:
		return decodeResult(method, msg, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
}

// begin allocates an id, registers the pending entry and writes the request.
func (c *conn) begin(method string, params any) (string, chan rpcMessage, error) {
	id := c.nextID.Add(1)
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	key := string(idRaw)
	ch := make(chan rpcMessage, 1)

	c.pmu.Lock()
	c.pending[key] = ch
	c.pmu.Unlock()

	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			c.forget(key)
			return "", nil, fmt.Errorf("acp: encode %s params: %w", method, err)
		}
		raw = b
	}
	select {
	case <-c.done:
		c.forget(key)
		return "", nil, c.closedErr()
	default:
	}
	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: idRaw, Method: method, Params: raw}); err != nil {
		c.forget(key)
		return "", nil, err
	}
	return key, ch, nil
}

func (c *conn) forget(key string) {
	c.pmu.Lock()
	delete(c.pending, key)
	c.pmu.Unlock()
}

// decodeResult turns a response document into out (or its error).
func decodeResult(method string, msg rpcMessage, out any) error {
	if msg.Error != nil {
		return msg.Error
	}
	if out == nil {
		return nil
	}
	if len(msg.Result) == 0 {
		// A bare `result: null` is a valid empty answer (session/set_model returns {}).
		return nil
	}
	if err := json.Unmarshal(msg.Result, out); err != nil {
		return fmt.Errorf("acp: decode %s result: %w", method, err)
	}
	return nil
}

// read is the reader goroutine. It owns the lifecycle: when the child's stdout ends, every
// pending call is failed and every session's events channel is closed.
func (c *conn) read() {
	defer close(c.readerDone)
	for line := range c.proc.Lines() {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// NEVER FATAL. A non-JSON line is a child logging to the wrong fd, not a protocol
			// violation worth killing a session over.
			c.logger.Warn("acp: skipping malformed line from agy", "err", err, "bytes", len(line))
			continue
		}
		switch {
		case msg.Method != "" && hasID(msg.ID):
			// An agent→client request. Its own goroutine: the handler may block for minutes and
			// must not stop the reader delivering the rest of the turn.
			go c.serveRequest(msg)
		case msg.Method != "":
			c.dispatchNotification(msg)
		case hasID(msg.ID):
			c.deliver(msg)
		default:
			c.logger.Debug("acp: ignoring unrecognised message from agy")
		}
	}
	c.finish()
}

// deliver routes a response to its pending call.
func (c *conn) deliver(msg rpcMessage) {
	key := string(bytes.TrimSpace(msg.ID))
	c.pmu.Lock()
	ch, ok := c.pending[key]
	c.pmu.Unlock()
	if !ok {
		// A response to a call we already gave up on (cancel grace expired). Accept silently.
		c.logger.Debug("acp: response for unknown request id", "id", key)
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// dispatchNotification handles agent→client notifications. Only session/update is defined.
func (c *conn) dispatchNotification(msg rpcMessage) {
	if msg.Method != methodSessionUpdate {
		c.logger.Debug("acp: ignoring unknown notification", "method", msg.Method)
		return
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.logger.Warn("acp: undecodable session/update envelope", "err", err)
		return
	}
	u := decodeUpdate(p.SessionID, p.Update)
	c.routeUpdate(p.SessionID, u)
}

// routeUpdate hands an update to its session, or buffers it if the session is not registered
// yet (a `session/load` replay can arrive before the response that returns its Session).
func (c *conn) routeUpdate(sessionID string, u Update) {
	c.sessMu.Lock()
	s, ok := c.sessions[sessionID]
	if !ok {
		if len(c.early[sessionID]) < 64 {
			c.early[sessionID] = append(c.early[sessionID], u)
		} else {
			c.logger.Debug("acp: dropping update for unknown session", "session", sessionID)
		}
		c.sessMu.Unlock()
		return
	}
	c.sessMu.Unlock()
	s.push(u)
}

// registerSession makes a session id routable and flushes anything buffered for it.
func (c *conn) registerSession(s *Session) {
	c.sessMu.Lock()
	c.sessions[s.ID()] = s
	early := c.early[s.ID()]
	delete(c.early, s.ID())
	c.sessMu.Unlock()
	for _, u := range early {
		s.push(u)
	}
}

// unregisterSession stops routing to a session (it was closed) and ends its events channel.
func (c *conn) unregisterSession(id string) {
	c.sessMu.Lock()
	s, ok := c.sessions[id]
	delete(c.sessions, id)
	c.sessMu.Unlock()
	if ok {
		s.closeEvents()
	}
	c.cancelPerms(id, errTeardown)
}

// serveRequest answers one agent→client request. It runs on its own goroutine.
func (c *conn) serveRequest(msg rpcMessage) {
	if msg.Method != methodRequestPermission {
		// agy 1.5.13 sends nothing else, and this client implements nothing else. Answering
		// method-not-found is the protocol's honest response, and it keeps the agent from
		// waiting for an answer that will never come.
		c.respondErr(msg.ID, CodeMethodNotFound, "method not found: "+msg.Method)
		return
	}

	var p permissionParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.respondErr(msg.ID, CodeInvalidParams, "invalid session/request_permission params: "+err.Error())
		return
	}

	perm := newInflightPerm()
	outcome := c.decidePermission(p, msg.Params, perm)
	c.respond(msg.ID, outcome)
	// THE ANSWER IS ON THE WIRE NOW, so a canceller waiting in cancelPerms may proceed. Closing
	// here (not inside decidePermission) is what makes "cancel returned" mean "the answer was
	// written": decidePermission only DECIDES, and a decision no caller can see is not an answer.
	perm.settle()
}

// decidePermission asks the handler and resolves the answer, failing CLOSED on every abnormal
// path. The handler runs on its own goroutine so a handler that ignores its context can still be
// given up on.
func (c *conn) decidePermission(p permissionParams, raw json.RawMessage, perm *inflightPerm) permissionOutcome {
	ctx, cancel := context.WithCancelCause(c.serveCtx)
	defer cancel(nil)

	perm.cancel = cancel
	slot := c.permID.Add(1)
	c.registerPerm(p.SessionID, slot, perm)
	defer c.unregisterPerm(p.SessionID, slot)

	if timeout := permissionTimeout(); timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, timeout)
		defer stop()
	}

	type answer struct {
		decision PermissionDecision
		err      error
	}
	answers := make(chan answer, 1)
	req := PermissionRequest{
		SessionID: p.SessionID,
		ToolCall:  p.ToolCall,
		Options:   p.Options,
		Raw:       append(json.RawMessage(nil), raw...),
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				answers <- answer{err: fmt.Errorf("acp: permission handler panicked: %v", r)}
			}
		}()
		d, err := c.handler.RequestPermission(ctx, req)
		answers <- answer{decision: d, err: err}
	}()

	select {
	case a := <-answers:
		if a.err != nil {
			c.logger.Warn("acp: permission handler failed; answering with the reject option", "err", a.err)
			return failClosed(p.Options)
		}
		if a.decision.Cancelled {
			return CancelledPermissionOutcome()
		}
		if a.decision.OptionID == "" || !isOffered(p.Options, a.decision.OptionID) {
			c.logger.Warn("acp: permission handler chose an option agy did not offer; answering with the reject option",
				"chosen", a.decision.OptionID)
			return failClosed(p.Options)
		}
		return SelectedPermissionOutcome(a.decision.OptionID)

	case <-ctx.Done():
		// A torn-down turn answers `cancelled` (exactly what agy's own cancel_midturn replay
		// shows); any other cancellation — a timeout, or the caller's context ending — FAILS
		// CLOSED on the reject option. There is no path here that answers "allow".
		if errors.Is(context.Cause(ctx), errTeardown) {
			return CancelledPermissionOutcome()
		}
		c.logger.Warn("acp: permission decision context ended; answering with the reject option",
			"cause", context.Cause(ctx))
		return failClosed(p.Options)

	case <-c.done:
		return failClosed(p.Options)
	}
}

// SetPermissionTimeout bounds how long a permission handler may take before the client answers
// with the reject option. 0 (the default) waits indefinitely, which is what an operator decision
// needs; a bridge with its own deadline sets a value here. Safe to call at any time.
func SetPermissionTimeout(d time.Duration) {
	permTimeoutMu.Lock()
	permTimeout = d
	permTimeoutMu.Unlock()
}

// permissionTimeout reads the current handler timeout.
func permissionTimeout() time.Duration {
	permTimeoutMu.Lock()
	defer permTimeoutMu.Unlock()
	return permTimeout
}

var (
	permTimeoutMu sync.Mutex
	permTimeout   time.Duration
)

// failClosed picks the never-allow answer: the reject option, or — if agy offered no reject at
// all — an explicit `cancelled`, which also does not grant the tool call.
func failClosed(opts []PermissionOption) permissionOutcome {
	if id, ok := RejectOptionID(opts); ok {
		return SelectedPermissionOutcome(id)
	}
	return CancelledPermissionOutcome()
}

// inflightPerm is one permission request that has not been answered yet: the cause-cancel that
// tears its handler down, and a channel closed once the ANSWER HAS BEEN WRITTEN.
//
// settled is what makes cancellation deterministic. Cancelling only wakes the handler; the answer
// is written afterwards, on whichever goroutine the handler happens to be running on. A caller that
// cancels and then inspects the wire would otherwise be racing that write, which is exactly how
// TestReplay_CancelMidturn failed once under a fully loaded gate and never on its own.
type inflightPerm struct {
	cancel  context.CancelCauseFunc
	settled chan struct{}
	once    sync.Once
}

// newInflightPerm builds a request slot. cancel is attached by decidePermission, once the
// cancellable handler context exists.
func newInflightPerm() *inflightPerm {
	return &inflightPerm{settled: make(chan struct{})}
}

// settle records that the answer reached the wire. Idempotent, so the error paths that answer
// without going through decidePermission cannot close the channel twice.
func (p *inflightPerm) settle() {
	p.once.Do(func() { close(p.settled) })
}

func (c *conn) registerPerm(sessionID string, slot uint64, p *inflightPerm) {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	m := c.perms[sessionID]
	if m == nil {
		m = make(map[uint64]*inflightPerm)
		c.perms[sessionID] = m
	}
	m[slot] = p
}

func (c *conn) unregisterPerm(sessionID string, slot uint64) {
	c.permMu.Lock()
	defer c.permMu.Unlock()
	if m := c.perms[sessionID]; m != nil {
		delete(m, slot)
		if len(m) == 0 {
			delete(c.perms, sessionID)
		}
	}
}

// cancelPerms tears down every in-flight permission request for one session, so an outstanding
// request is answered `cancelled` rather than left hanging (cancel_midturn.jsonl).
//
// IT WAITS FOR THE ANSWERS TO REACH THE WIRE (bounded by permSettleGrace). Cancelling is not
// answering: the handler wakes on the cancel and the ANSWER is written by the goroutine that owns
// it, so without this wait cancelPerms returns with the request still unanswered on the stream and
// every caller that inspects the stream next is racing a write it cannot see. Fail-closed is
// unaffected — the wait is bounded and a handler that never settles is given up on, exactly as a
// handler that ignores its context already is.
func (c *conn) cancelPerms(sessionID string, cause error) {
	c.permMu.Lock()
	slots := make([]*inflightPerm, 0, len(c.perms[sessionID]))
	for _, p := range c.perms[sessionID] {
		slots = append(slots, p)
	}
	c.permMu.Unlock()
	if len(slots) == 0 {
		return
	}
	for _, p := range slots {
		p.cancel(cause)
	}
	timer := time.NewTimer(permSettleGrace)
	defer timer.Stop()
	for _, p := range slots {
		select {
		case <-p.settled:
		case <-timer.C:
			c.logger.Warn("acp: a cancelled permission request was not answered in time",
				"session", sessionID, "grace", permSettleGrace)
			return
		case <-c.done:
			return
		}
	}
}

// finish fails every pending call and ends every session when the child's stdout ends.
func (c *conn) finish() {
	code, werr := c.proc.Wait()
	c.doneOnce.Do(func() {
		c.exitCode = code
		c.exitErr = werr
		// SETTLE THE DIAGNOSTIC BEFORE ANYONE CAN READ IT. The stderr drain is a separate
		// goroutine, so "stdout ended" (which is what drives this function) does not imply the
		// drain has consumed the lines the child wrote just before it died — they can still be
		// sitting in the process's stderr buffer. Every closed error is rendered from that ring
		// (closedErr -> stderrRing.Tail), so without this wait a pending call could be failed with
		// an EMPTY Stderr and the child's own fatal line lost. Waiting here, before `close(c.done)`,
		// is what establishes the happens-before edge: a caller that observes the closed
		// connection is guaranteed to observe the drained tail. The wait is bounded by
		// stderrStopGrace and returns as soon as the pump ends, which for every Proc
		// implementation happens when the child exits.
		c.stderr.stop()
		// Handlers currently blocked fail closed before the connection is marked done.
		c.serveCancel(errTeardown)

		// Pending calls are woken by `close(c.done)` below, which makes conn.call return
		// c.closedErr() — carrying the exit code AND the stderr tail. Pushing a synthesised
		// RPCError onto each of their channels instead would RACE that path and, worse, would be
		// the error a caller actually saw: a bare "-32099 agy process exited" with no diagnostic,
		// which is the unactionable shape this package exists to avoid. So the map is only
		// cleared here.
		c.pmu.Lock()
		c.pending = make(map[string]chan rpcMessage)
		c.pmu.Unlock()

		c.sessMu.Lock()
		sessions := make([]*Session, 0, len(c.sessions))
		for _, s := range c.sessions {
			sessions = append(sessions, s)
		}
		c.sessions = make(map[string]*Session)
		c.sessMu.Unlock()
		for _, s := range sessions {
			s.closeEvents()
		}

		close(c.done)
	})
}
