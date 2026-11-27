package execution

// executionHub — ONE broker consumer for all execution events, fanned out in-process per execution.
//
// THE PROBLEM THIS REPLACES. StreamExecutionEvents used to call eventbus.Subscribe per RPC, and
// NATSSubscriber.Subscribe does js.CreateOrUpdateConsumer — so EVERY open execution tab created its own
// JetStream consumer on the ORCHICON_EVENTS stream, filtered to the whole `orchicon.events.execution.>`
// firehose, and then discarded every event for another execution inside a Go loop:
//
//	if req.Msg.ExecutionId != "" && evt.ExecutionId != req.Msg.ExecutionId { continue }
//
// Six tabs meant six broker consumers, each fed every execution event in the tenant, six of them
// throwing >99% away. That is not six connections; it is six server-side consumers, and it is what
// starved the browser's per-origin socket budget badly enough that a NEW tab's queries never resolved
// (the "Loading…" that cleared only when the execution tab was closed).
//
// THE SHAPE NOW (modelled on the Ask conversation, which never had this problem): one consumer for the
// plane, started when the subscriber is available; each execution gets a hub only while someone is
// watching it, and subscribers ATTACH to the hub rather than to the broker.
//
// WHAT A SUBSCRIBER GETS, and why it is equivalent to what it had before:
//   - a replay of the events the hub has retained, so a tab that opens mid-run is not blind to what it
//     missed between the transcript's last flush and now;
//   - everything live from the moment it attached;
//   - a CLOSE when the hub is retired (terminal status, or retention eviction), so the RPC returns and
//     the tab's socket is released. THAT is the property Ask has and executions did not: the stream ends
//     itself instead of parking for the life of the tab.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
)

// hubAllExecutions is the pseudo-key for a subscriber that wants EVERY execution (an empty
// execution_id in the request — the TUI's tenant-wide subscription). Publishing feeds both the specific
// execution's entry and this one, so a wildcard subscriber costs one in-process channel rather than a
// broker consumer.
const hubAllExecutions = "*"

const (
	// hubReplayMax bounds the retained events per execution. The durable transcript is the real history
	// (the frontend merges it with the live stream), so the hub only has to cover the window between the
	// transcript's last flush and "now" — a few seconds of chunks. 512 is generous for that and bounded,
	// which matters: an unbounded ring per live execution is the same memory leak with a nicer name.
	hubReplayMax = 512
	// hubIdleTTL retires a hub nobody is subscribed to. An execution that ends without the server
	// observing a terminal event (a crash, a reap) must not leave a hub behind forever.
	hubIdleTTL = 2 * time.Minute
	// hubSweepInterval is how often idle hubs are considered.
	hubSweepInterval = 30 * time.Second
)

// execEvent is one event as the hub retains it: the parsed payload the RPC sends, plus the stream
// sequence the client resumes from.
type execEvent struct {
	execID   string
	sequence int64
	data     []byte
}

// hubSubscriber is one attached RPC. Its channel is buffered so a slow client cannot stall the
// dispatcher that feeds every other subscriber of the same execution.
type hubSubscriber struct {
	ch     chan execEvent
	closed bool
}

// executionHub fans execution events out to the RPCs watching each execution.
type executionHub struct {
	log *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*hubSubscriber]struct{} // execID -> attached subscribers
	ring map[string][]execEvent                 // execID -> retained events (capped at hubReplayMax)
	// lastSeen is when a hub last had a subscriber; used by the idle sweep.
	lastSeen map[string]time.Time
	// lastSeq is the highest stream sequence forwarded per execution, so a subscriber's
	// from_sequence resume is answered from the ring rather than by replaying the broker.
	lastSeq map[string]int64

	started  bool
	stopOnce sync.Once
	stop     context.CancelFunc
}

func newExecutionHub(log *slog.Logger) *executionHub {
	return &executionHub{
		log:      log,
		subs:     make(map[string]map[*hubSubscriber]struct{}),
		ring:     make(map[string][]execEvent),
		lastSeen: make(map[string]time.Time),
		lastSeq:  make(map[string]int64),
	}
}

// start begins the single plane-wide consumer. It is idempotent and a no-op without a subscriber
// (event streaming disabled), which keeps the "event streaming is unavailable" RPC error honest.
func (h *executionHub) start(parent context.Context, sub eventbus.Subscriber) {
	if sub == nil {
		return
	}
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	ctx, cancel := context.WithCancel(parent)
	h.stop = cancel
	h.mu.Unlock()

	go h.sweepLoop(ctx)

	// ONE consumer for every execution. fromSeq 0 = DeliverNewPolicy: the ring is seeded from live
	// traffic, and the durable transcript supplies anything older.
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			if err := h.consume(ctx, sub); err != nil && ctx.Err() == nil {
				h.log.Warn("execution hub: consumer ended — restarting", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// consume runs the shared subscription until it fails or the context ends.
func (h *executionHub) consume(ctx context.Context, sub eventbus.Subscriber) error {
	ch, err := sub.Subscribe(ctx, "orchicon.events.execution.>", 0)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			// The event's own execution id is the routing key. It is read here, ONCE, for every
			// subscriber — instead of being re-checked in a Go loop per tab.
			execID := executionIDFromEvent(msg.Data)
			if execID == "" {
				continue
			}
			h.publish(execEvent{execID: execID, sequence: int64(msg.Seq), data: msg.Data})
		}
	}
}

// publish retains an event and forwards it to that execution's subscribers.
func (h *executionHub) publish(ev execEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := append(h.ring[ev.execID], ev)
	if len(ring) > hubReplayMax {
		ring = ring[len(ring)-hubReplayMax:]
	}
	h.ring[ev.execID] = ring
	// The wildcard's ring is separate and also bounded, so a tenant-wide subscriber can replay a short
	// window across executions.
	if _, watched := h.subs[hubAllExecutions]; watched {
		wild := append(h.ring[hubAllExecutions], ev)
		if len(wild) > hubReplayMax {
			wild = wild[len(wild)-hubReplayMax:]
		}
		h.ring[hubAllExecutions] = wild
	}
	if ev.sequence > h.lastSeq[ev.execID] {
		h.lastSeq[ev.execID] = ev.sequence
	}
	// Feed the wildcard hub too: a tenant-wide subscriber sees every execution, from the SAME retained
	// ring, without a consumer of its own.
	if set := h.subs[hubAllExecutions]; len(set) > 0 {
		for s := range set {
			if s.closed {
				continue
			}
			select {
			case s.ch <- ev:
			default:
				close(s.ch)
				s.closed = true
				delete(set, s)
				h.log.Warn("execution hub: wildcard subscriber fell behind and was dropped")
			}
		}
	}
	terminal := terminalExecutionEvent(ev.data)
	for s := range h.subs[ev.execID] {
		if s.closed {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// A subscriber that cannot keep up is DROPPED, not allowed to back-pressure every other
			// viewer of the same execution. It reconnects with from_sequence and replays the ring.
			close(s.ch)
			s.closed = true
			delete(h.subs[ev.execID], s)
			h.log.Warn("execution hub: subscriber fell behind and was dropped", "execution", ev.execID)
		}
	}
	if terminal {
		// THE EXECUTION IS OVER: return every watching RPC so the tab releases its socket by itself.
		// This is the Ask property — the stream ends with the work, not with the page.
		h.retireLocked(ev.execID)
	}
}

// attach registers a subscriber for execID and returns the retained events it should replay.
//
// fromSequence > 0 resumes after that stream sequence: the retained ring is the source (the transcript
// covers anything older), so a reconnect costs no broker round-trip.
func (h *executionHub) attach(execID string, fromSequence int64) (*hubSubscriber, []execEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &hubSubscriber{ch: make(chan execEvent, hubReplayMax)}
	if h.subs[execID] == nil {
		h.subs[execID] = make(map[*hubSubscriber]struct{})
	}
	h.subs[execID][s] = struct{}{}
	h.lastSeen[execID] = time.Now()
	var replay []execEvent
	for _, ev := range h.ring[execID] {
		if fromSequence > 0 && ev.sequence <= fromSequence {
			continue
		}
		replay = append(replay, ev)
	}
	return s, replay
}

// detach removes a subscriber. The hub itself is kept until it goes idle, so a quick reconnect (or a
// second tab on the same execution) still finds the retained ring.
func (h *executionHub) detach(execID string, s *hubSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[execID]; ok {
		if !s.closed {
			close(s.ch)
			s.closed = true
		}
		delete(set, s)
		if len(set) == 0 {
			h.lastSeen[execID] = time.Now()
		}
	}
}

// retire closes every subscriber of execID and drops its retained state.
//
// CALLED WHEN THE EXECUTION REACHES A TERMINAL STATUS. This is the lifecycle property the Ask turn
// stream has and the execution stream lacked: the subscriber's RPC RETURNS, so the tab releases its
// socket by itself instead of holding it for the life of the page.
func (h *executionHub) retire(execID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retireLocked(execID)
}

func (h *executionHub) retireLocked(execID string) {
	for s := range h.subs[execID] {
		if !s.closed {
			close(s.ch)
			s.closed = true
		}
	}
	delete(h.subs, execID)
	delete(h.ring, execID)
	delete(h.lastSeen, execID)
	delete(h.lastSeq, execID)
}

// sweepLoop retires hubs nobody has watched for hubIdleTTL.
func (h *executionHub) sweepLoop(ctx context.Context) {
	t := time.NewTicker(hubSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweep(time.Now())
		}
	}
}

func (h *executionHub) sweep(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for execID, set := range h.subs {
		if len(set) > 0 {
			continue
		}
		if seen, ok := h.lastSeen[execID]; ok && now.Sub(seen) < hubIdleTTL {
			continue
		}
		if _, watched := h.lastSeen[execID]; !watched {
			continue
		}
		h.retireLocked(execID)
	}
}

// counts reports the live fan-out state, for logging and a metric.
func (h *executionHub) counts() (hubs, subscribers int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.subs {
		if len(set) > 0 {
			hubs++
			subscribers += len(set)
		}
	}
	return hubs, subscribers
}

func (h *executionHub) close() {
	h.stopOnce.Do(func() {
		if h.stop != nil {
			h.stop()
		}
	})
}

// executionIDFromEvent reads the routing key from an outbox event envelope, cheaply: the id is a
// top-level field, so this decodes only what it needs rather than the whole event.
func executionIDFromEvent(data []byte) string {
	var env struct {
		ExecutionID string `json:"execution_id"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return ""
	}
	return env.ExecutionID
}

// terminalExecutionEvent reports whether this event means the execution has finished, so its hub can
// be retired and every watching RPC returned.
//
// The signal is the envelope's own status, not the event type: `result`/`error` are the obvious
// terminal events, but a reaped or cancelled execution's LAST event can be a control or health change,
// and retiring only on `result` would leave those tabs parked for the life of the page — the defect
// this whole change exists to remove.
func terminalExecutionEvent(data []byte) bool {
	var env struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return false
	}
	switch env.Status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionTerminated,
		domain.ExecutionFailedToStart:
		return true
	}
	return false
}
