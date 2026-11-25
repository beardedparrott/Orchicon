package execution

// THE EXECUTION STREAM MUST NOT COST ONE BROKER CONSUMER PER TAB.
//
// Measured cause of "open another tab and it hangs on Loading… until you close the execution tab":
// StreamExecutionEvents called eventbus.Subscribe per RPC, and NATSSubscriber.Subscribe does
// js.CreateOrUpdateConsumer — so every open execution tab registered its own JetStream consumer on the
// whole `orchicon.events.execution.>` firehose and discarded other executions' events in a Go loop.
//
// These tests drive the hub directly. They need no broker: the fan-out is the thing that was wrong.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/eventbus"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// eventData builds an outbox envelope of the shape the relay publishes.
func eventData(execID, status string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event_type":   "started",
		"execution_id": execID,
		"status":       status,
		"occurred_at":  time.Now().UTC().Format(time.RFC3339Nano),
	})
	return b
}

// countingSubscriber records how many BROKER SUBSCRIPTIONS were made. The whole point of the hub is that
// this number stays at 1 no matter how many viewers attach.
type countingSubscriber struct {
	mu    sync.Mutex
	calls int
	ch    chan eventbus.EventMsg
}

func (c *countingSubscriber) Subscribe(_ context.Context, _ string, _ uint64) (<-chan eventbus.EventMsg, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.ch, nil
}

func (c *countingSubscriber) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// THE HEADLINE ASSERTION: N viewers, ONE consumer.
func TestManyViewersShareOneBrokerConsumer(t *testing.T) {
	sub := &countingSubscriber{ch: make(chan eventbus.EventMsg, 64)}
	h := newExecutionHub(quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx, sub)
	defer h.close()

	// Let the consumer goroutine subscribe once.
	deadline := time.Now().Add(2 * time.Second)
	for sub.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// Six tabs on six executions, plus a second tab on one of them.
	var subs []*hubSubscriber
	for _, id := range []string{"e1", "e2", "e3", "e4", "e5", "e6", "e1"} {
		s, _ := h.attach(id, 0)
		subs = append(subs, s)
	}

	// Deliver a burst while all seven are attached.
	for i := 0; i < 20; i++ {
		sub.ch <- eventbus.EventMsg{Seq: uint64(i + 1), Data: eventData("e1", "running")}
	}
	time.Sleep(50 * time.Millisecond)

	if got := sub.count(); got != 1 {
		t.Fatalf("broker subscriptions = %d, want 1. Every extra one is a JetStream consumer on the "+
			"whole execution firehose — the defect this hub exists to remove.", got)
	}
}

// Events go to the viewers of THAT execution only, and both viewers of a shared execution get them.
func TestEventsFanOutPerExecution(t *testing.T) {
	sub := &countingSubscriber{ch: make(chan eventbus.EventMsg, 64)}
	h := newExecutionHub(quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx, sub)
	defer h.close()
	time.Sleep(30 * time.Millisecond)

	a1, _ := h.attach("a", 0)
	a2, _ := h.attach("a", 0) // second tab on the same execution
	b1, _ := h.attach("b", 0)

	sub.ch <- eventbus.EventMsg{Seq: 1, Data: eventData("a", "running")}
	time.Sleep(50 * time.Millisecond)

	// Both 'a' viewers must have it.
	for i, s := range []*hubSubscriber{a1, a2} {
		select {
		case ev := <-s.ch:
			if ev.execID != "a" {
				t.Errorf("viewer %d got execID %q, want a", i, ev.execID)
			}
		case <-time.After(300 * time.Millisecond):
			t.Errorf("viewer %d of execution 'a' received nothing", i)
		}
	}
	// 'b' must NOT: that cross-delivery is what every former tab filtered out by hand.
	select {
	case ev := <-b1.ch:
		t.Errorf("execution 'b' received a's event (%+v) — fan-out is not per-execution", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// THE LIFECYCLE PROPERTY: a terminal event RETIRES the hub, which closes the subscriber channel, which
// makes the RPC return. That is how the tab releases its socket without anyone closing the tab.
func TestTerminalEventRetiresTheHubAndEndsTheStream(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "terminated", "failed_to_start"} {
		sub := &countingSubscriber{ch: make(chan eventbus.EventMsg, 64)}
		h := newExecutionHub(quietLog())
		ctx, cancel := context.WithCancel(context.Background())
		h.start(ctx, sub)
		time.Sleep(20 * time.Millisecond)

		s, _ := h.attach("x", 0)
		sub.ch <- eventbus.EventMsg{Seq: 1, Data: eventData("x", status)}
		time.Sleep(50 * time.Millisecond)

		drained := false
		deadline := time.After(time.Second)
	wait:
		for {
			select {
			case _, ok := <-s.ch:
				if !ok {
					drained = true
					break wait
				}
			case <-deadline:
				break wait
			}
		}
		if !drained {
			t.Errorf("status %q did not retire the hub — the subscriber channel stayed open, so the RPC "+
				"never returns and the tab holds its socket for the life of the page", status)
		}
		h.close()
		cancel()
	}
}

// A non-terminal event must NOT retire anything: a running execution's hub has to survive.
func TestRunningEventKeepsTheHubOpen(t *testing.T) {
	sub := &countingSubscriber{ch: make(chan eventbus.EventMsg, 64)}
	h := newExecutionHub(quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx, sub)
	defer h.close()
	time.Sleep(20 * time.Millisecond)

	s, _ := h.attach("y", 0)
	sub.ch <- eventbus.EventMsg{Seq: 1, Data: eventData("y", "running")}
	time.Sleep(50 * time.Millisecond)
	// Drain and then assert the channel is still open.
	select {
	case <-s.ch:
	default:
	}
	select {
	case _, ok := <-s.ch:
		if !ok {
			t.Fatal("a 'running' event retired the hub")
		}
	case <-time.After(100 * time.Millisecond):
		// open and empty: correct
	}
}

// A late viewer must be able to replay from from_sequence without a broker round-trip.
func TestAttachReplaysTheRetainedRing(t *testing.T) {
	h := newExecutionHub(quietLog())
	for i := 1; i <= 5; i++ {
		h.publish(execEvent{execID: "r", sequence: int64(i), data: eventData("r", "running")})
	}
	// A fresh viewer gets everything.
	_, replay := h.attach("r", 0)
	if len(replay) != 5 {
		t.Errorf("fresh attach replayed %d events, want 5", len(replay))
	}
	// A resuming viewer gets only what is newer.
	_, replay = h.attach("r", 3)
	if len(replay) != 2 {
		t.Errorf("resume from 3 replayed %d events, want 2", len(replay))
	}
	for _, ev := range replay {
		if ev.sequence <= 3 {
			t.Errorf("resume from 3 replayed sequence %d", ev.sequence)
		}
	}
}

// The retained ring is BOUNDED: an unbounded buffer per live execution is a memory leak.
func TestTheRetainedRingIsBounded(t *testing.T) {
	h := newExecutionHub(quietLog())
	for i := 0; i < hubReplayMax+50; i++ {
		h.publish(execEvent{execID: "bounded", sequence: int64(i + 1), data: eventData("bounded", "running")})
	}
	_, replay := h.attach("bounded", 0)
	if len(replay) > hubReplayMax {
		t.Errorf("retained %d events, cap is %d", len(replay), hubReplayMax)
	}
	// And it kept the NEWEST, not the oldest — a stale window is worse than none.
	if len(replay) > 0 && replay[len(replay)-1].sequence != int64(hubReplayMax+50) {
		t.Errorf("newest retained sequence = %d, want %d", replay[len(replay)-1].sequence, hubReplayMax+50)
	}
}

// A tenant-wide viewer (empty execution_id) attaches to the wildcard and sees every execution.
func TestWildcardViewerSeesEveryExecution(t *testing.T) {
	sub := &countingSubscriber{ch: make(chan eventbus.EventMsg, 64)}
	h := newExecutionHub(quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx, sub)
	defer h.close()
	time.Sleep(20 * time.Millisecond)

	all, _ := h.attach(hubAllExecutions, 0)
	sub.ch <- eventbus.EventMsg{Seq: 1, Data: eventData("p", "running")}
	sub.ch <- eventbus.EventMsg{Seq: 2, Data: eventData("q", "running")}
	time.Sleep(60 * time.Millisecond)

	got := 0
	for {
		select {
		case <-all.ch:
			got++
			if got == 2 {
				return
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("wildcard viewer saw %d events, want 2", got)
		}
	}
}

// A subscriber that stops draining is DROPPED rather than allowed to stall everyone else.
func TestASlowSubscriberIsDroppedNotAllowedToBlock(t *testing.T) {
	h := newExecutionHub(quietLog())
	slow, _ := h.attach("s", 0)
	_ = slow // never drained
	fast, _ := h.attach("s", 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < hubReplayMax*2; i++ {
			h.publish(execEvent{execID: "s", sequence: int64(i + 1), data: eventData("s", "running")})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("publish blocked on a slow subscriber — one stalled tab would freeze every other viewer " +
			"of the same execution")
	}

	drained := 0
	for {
		select {
		case _, ok := <-fast.ch:
			if !ok {
				goto doneDraining
			}
			drained++
		default:
			goto doneDraining
		}
	}
doneDraining:
	if drained == 0 {
		t.Error("the healthy subscriber received nothing")
	}
}

var _ = bytes.MinRead
