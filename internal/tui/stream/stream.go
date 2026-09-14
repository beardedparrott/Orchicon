// Package stream is the Go mirror of the GUI's useStream.ts
// (frontend/src/api/useStream.ts): a server-stream subscription with
// automatic reconnect + exponential backoff, resume from the last
// sequence number, dedup by event id, and a drop-oldest ring buffer —
// the semantics parity the acceptance criteria pin.
//
// Status strings are identical to the hook's StreamStatus union:
// idle | connecting | open | reconnecting | closed | error.
package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// Status mirrors useStream.ts StreamStatus.
type Status string

const (
	StatusIdle         Status = "idle"
	StatusConnecting   Status = "connecting"
	StatusOpen         Status = "open"
	StatusReconnecting Status = "reconnecting"
	StatusClosed       Status = "closed"
	StatusError        Status = "error"
)

// Defaults from the hook.
const (
	DefaultMaxEvents  = 200
	DefaultMaxBackoff = 30 * time.Second
	// Backoff base: delay = base * 2^(attempt-1) + jitter(0..500ms).
	BackoffBase   = 1 * time.Second
	BackoffJitter = 500 * time.Millisecond
	// DialWindow is the default for Config.DialWindow — long enough that an
	// immediate RPC rejection (unimplemented, unauthenticated, refused) wins the
	// race, short enough that a healthy quiet stream reads as connected.
	DefaultDialWindow = 2 * time.Second
	// closeGrace bounds how long Close waits for the loop goroutine to exit.
	//
	// Cancelling unblocks a real transport promptly, so a healthy Close returns
	// immediately and never reaches this; the grace only matters when the loop is
	// stuck in a call that ignores cancellation, where waiting longer cannot help
	// and would freeze the caller (Close runs on every tab switch).
	closeGrace = 500 * time.Millisecond
)

// Config is the subscription template passed to New. It is lock-free;
// New copies it into the heap-allocated Sub.
type Config[Resp any] struct {
	// Name identifies the subscription (logging / debugging).
	Name string
	// Open dials the stream. fromSequence is the last sequence seen (0 on
	// first connect — the hook only adds fromSequence once a sequence was
	// observed). Open must be safe to call with a canceled ctx.
	Open func(ctx context.Context, fromSequence int64) (func() (Resp, error), error)
	// GetEventID extracts the dedup key ("" = no id, never deduped).
	GetEventID func(Resp) string
	// GetSequence extracts the sequence number for resume.
	GetSequence func(Resp) int64
	// Filter optionally drops responses (post-dedup, like the hook).
	Filter func(Resp) bool
	// OnEvent is called for each kept event (cache invalidation hook).
	OnEvent func(Resp)
	// DialWindow is how long a dial may stay IN FLIGHT before the subscription
	// reports itself open anyway.
	//
	// It exists because of a real property of the streaming client: a
	// SERVER-streaming call is dispatched through duplexHTTPCall.sendUnary, which
	// makes the request SYNCHRONOUSLY and therefore does not return until the
	// response HEADERS arrive — and a Connect server only flushes those on its
	// first message. A subscription whose server stays quiet (project events fire
	// only when something changes) therefore blocked inside Open indefinitely, so
	// the footer showed "connecting…" forever with no error and no retry, even
	// though the connection was perfectly healthy. Once a dial has been in flight
	// this long without failing, the transport has accepted it, so the honest
	// status is "open"; a later failure still flips to error as usual.
	// Default DefaultDialWindow.
	DialWindow time.Duration
	// OnStatus is called on every status transition (footer subscription).
	OnStatus func(Status)
	// OnError is called with the underlying dial/stream error whenever one is
	// recorded. Without it a failing subscription reports only a status, so the
	// operator sees "connecting…" forever with no reason — which is exactly how
	// a DEAD ENDPOINT (nothing listening on the configured URL) read as a
	// mysterious eternal connect.
	OnError func(error)
	// MaxEvents ring size (drop-oldest), default DefaultMaxEvents.
	MaxEvents int
	// MaxBackoff caps the reconnect delay, default DefaultMaxBackoff.
	MaxBackoff time.Duration
	// BackoffBase overrides the 1s exponential base (test seam; the hook
	// hardcodes 1000ms).
	BackoffBase time.Duration
	// Logger is optional.
	Log *slog.Logger
}

// Sub is one running subscription. Generic over the stream response type;
// the Open func wraps a Connect server-stream (client.ConnectRecv) so each
// reconnect issues a fresh RPC with from_sequence = last sequence.
type Sub[Resp any] struct {
	Config[Resp]

	wakeCh chan struct{}

	mu        sync.Mutex
	events    []Resp
	status    Status
	lastSeq   int64
	attempt   int
	err       error
	seen      map[string]struct{}
	cancel    context.CancelFunc
	timer     *time.Timer
	stopped   bool
	lastEvent time.Time
	wg        sync.WaitGroup
}

// New creates and starts a subscription. The Sub value is copied onto the
// heap; use the returned handle (the parameter is a template, not shared).
func New[Resp any](cfg Config[Resp]) *Sub[Resp] {
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = DefaultMaxEvents
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = BackoffBase
	}
	if cfg.DialWindow <= 0 {
		cfg.DialWindow = DefaultDialWindow
	}
	s := &Sub[Resp]{Config: cfg}
	s.seen = map[string]struct{}{}
	s.status = StatusIdle
	s.wakeCh = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.loop(ctx)
	return s
}

// Events returns a copy of the buffered events (oldest first).
func (s *Sub[Resp]) Events() []Resp {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Resp, len(s.events))
	copy(out, s.events)
	return out
}

// Status returns the current status.
func (s *Sub[Resp]) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// LastSequence returns the last sequence number observed.
func (s *Sub[Resp]) LastSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// Err returns the last stream error, if any.
func (s *Sub[Resp]) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Reconnect cancels pending backoff and redials immediately (backoff
// reset, exactly like the hook's reconnect()).
func (s *Sub[Resp]) Reconnect() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.attempt = 0
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.wake()
}

// Close stops the subscription and releases its goroutine. Idempotent.
func (s *Sub[Resp]) Close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// A BOUNDED wait. An unbounded wg.Wait() here is a UI freeze waiting to
	// happen: Close runs on EVERY tab switch (the shell's CloseAll), while the loop
	// goroutine can be blocked inside a recv() that does not observe cancellation —
	// a wedged transport, or a stub whose recv never returns. When that happens the
	// Wait never completes and the caller hangs forever.
	//
	// This is not hypothetical: the stream suite HUNG for the full 10m test timeout
	// (rather than failing) because TestOpenWhileTheDialIsStillInFlight races
	// connect()'s `<-dialed` against `<-ctx.Done()`, and whenever the dial won the
	// loop sat in an uninterruptible recv() while Close waited on it — which blocked
	// `make rebuild-dev` outright.
	//
	// The goroutine exits as soon as its blocking call returns (cancelling the
	// request does unblock a real transport), and if it never does, leaking one
	// goroutine is strictly better than freezing the UI.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
	}
}

// wake pokes the loop's select so a pending backoff timer is abandoned
// (Reconnect / Close responsiveness).
func (s *Sub[Resp]) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// loop is the subscription's goroutine: connect → drain → schedule
// reconnect, mirroring the hook's connect()/scheduleReconnect cycle.
func (s *Sub[Resp]) loop(ctx context.Context) {
	defer s.wg.Done()
	for {
		s.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		// scheduleReconnect: attempt += 1; delay = min(base * 2^(attempt-1)
		// + jitter, maxBackoff).
		s.mu.Lock()
		s.attempt++
		attempt := s.attempt
		max := s.MaxBackoff
		s.mu.Unlock()
		shift := attempt - 1
		if shift > 32 {
			shift = 32 // guard: 1<<63 overflows int64 on pathological attempt counts
		}
		delay := s.BackoffBase * time.Duration(1<<shift)
		delay += time.Duration(rand.Int63n(int64(BackoffJitter)))
		if delay > max {
			delay = max
		}
		timer := time.NewTimer(delay)
		s.mu.Lock()
		s.timer = timer
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wakeCh:
			timer.Stop()
			// immediate redial (Reconnect already reset attempt)
		case <-timer.C:
		}
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
	}
}

// connect runs one dial+drain cycle (the hook's connect() body).
func (s *Sub[Resp]) connect(ctx context.Context) {
	s.mu.Lock()
	stopped := s.stopped
	if stopped {
		s.mu.Unlock()
		return
	}
	st := StatusConnecting
	if s.attempt > 0 {
		st = StatusReconnecting
	}
	from := int64(0)
	if s.lastSeq > 0 {
		from = s.lastSeq // resume only once a sequence was seen
	}
	s.mu.Unlock()
	s.setStatus(st)

	// The dial runs in a goroutine because Open can legitimately block: for a
	// server-streaming call the client makes the request SYNCHRONOUSLY and waits
	// for response headers, which a Connect server flushes only on its FIRST
	// message. A quiet stream would otherwise pin the status at "connecting"
	// forever — no error, no retry, no explanation.
	type dialResult struct {
		recv func() (Resp, error)
		err  error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		recv, err := s.Open(ctx, from)
		dialed <- dialResult{recv: recv, err: err}
	}()

	// markOpen records that the transport accepted the request.
	markOpen := func() {
		s.setStatus(StatusOpen)
		s.mu.Lock()
		s.attempt = 0
		s.err = nil
		s.mu.Unlock()
	}

	var recv func() (Resp, error)
	select {
	case r := <-dialed:
		if r.err != nil {
			s.setErr(r.err)
			s.setStatus(StatusError)
			return
		}
		recv = r.recv
		markOpen()
	case <-time.After(s.DialWindow):
		// Still in flight and not failing: the connection is established, the
		// server simply has not sent its first message yet.
		markOpen()
		select {
		case r := <-dialed:
			if r.err != nil {
				s.setErr(r.err)
				s.setStatus(StatusError)
				return
			}
			recv = r.recv
		case <-ctx.Done():
			return
		}
	case <-ctx.Done():
		return
	}
	for {
		resp, err := recv()
		if err != nil {
			if ctx.Err() != nil {
				return // closed — do not schedule reconnect
			}
			s.setErr(err)
			if errors.Is(err, io.EOF) {
				// Stream ended normally (server closed): the hook sets
				// status "closed" then schedules a reconnect anyway.
				s.setStatus(StatusClosed)
				return
			}
			s.setStatus(StatusError)
			return
		}
		s.pushEvent(resp)
	}
}

// setErr records the subscription's last error and forwards it to OnError.
func (s *Sub[Resp]) setErr(err error) {
	s.mu.Lock()
	s.err = err
	hook := s.OnError
	s.mu.Unlock()
	if hook != nil && err != nil {
		hook(err)
	}
}

// pushEvent ports the hook's pushEvent: dedup by id → filter → update
// lastSeq → ring buffer append (drop-oldest) → onEvent. Callbacks run
// after the lock is released, in event order; they must be quick and
// non-blocking (TUI callbacks hand off to buffered tea channels).
func (s *Sub[Resp]) pushEvent(resp Resp) {
	var onEvent func(Resp)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := ""
		if s.GetEventID != nil {
			id = s.GetEventID(resp)
		}
		if id != "" {
			if _, dup := s.seen[id]; dup {
				return
			}
			s.seen[id] = struct{}{}
		}
		if s.Filter != nil && !s.Filter(resp) {
			return
		}
		if s.GetSequence != nil {
			s.lastSeq = s.GetSequence(resp)
		}
		s.events = append(s.events, resp)
		if len(s.events) > s.MaxEvents {
			s.events = s.events[len(s.events)-s.MaxEvents:]
		}
		s.lastEvent = time.Now()
		onEvent = s.OnEvent
	}()
	if onEvent != nil {
		onEvent(resp)
	}
}

// setStatus transitions status and fires OnStatus after unlock.
func (s *Sub[Resp]) setStatus(st Status) {
	var fire []func(Status)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.status == st {
			return
		}
		s.status = st
		if s.OnStatus != nil {
			fire = append(fire, s.OnStatus)
		}
	}()
	for _, cb := range fire {
		cb(st)
	}
}
