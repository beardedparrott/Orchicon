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
	// OnStatus is called on every status transition (footer subscription).
	OnStatus func(Status)
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
	s.wg.Wait()
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

	recv, err := s.Open(ctx, from)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		s.setStatus(StatusError)
		return
	}
	s.setStatus(StatusOpen) // hook: setStatus("open") + reset attempt AFTER the request resolves
	s.mu.Lock()
	s.attempt = 0
	s.err = nil
	s.mu.Unlock()
	for {
		resp, err := recv()
		if err != nil {
			if ctx.Err() != nil {
				return // closed — do not schedule reconnect
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
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
