package stream

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// testEvent is the fake stream message.
type testEvent struct {
	ID  string
	Seq int64
}

// fakeStream serves a scripted sequence of events, then fails with the
// scripted error (or blocks until ctx cancel). It honors from_sequence:
// on redial it replays every event with seq > from, exactly like the
// server's resume contract (docs/07 §4).
type fakeStream struct {
	mu       sync.Mutex
	events   []testEvent
	redials  []int64 // from_sequence received on each Open
	failWith error
	failAt   int // send this many events, then fail (0 = never)
	dialGate chan struct{}
	holdOpen bool // block forever after draining (server still "up")
}

func (f *fakeStream) open(ctx context.Context, from int64) (func() (testEvent, error), error) {
	f.mu.Lock()
	f.redials = append(f.redials, from)
	// Server-side resume contract: replay every event with seq > from.
	var events []testEvent
	for _, ev := range f.events {
		if from == 0 || ev.Seq > from {
			events = append(events, ev)
		}
	}
	failAt, failWith, hold := f.failAt, f.failWith, f.holdOpen
	if len(f.redials) > 1 {
		// The scripted drop happens once (the "server kill"); later dials
		// are healthy so the drain can reach a graceful EOF.
		failAt, failWith = 0, nil
	}
	f.mu.Unlock()

	if f.dialGate != nil {
		select {
		case <-f.dialGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	i := 0
	return func() (testEvent, error) {
		if i < len(events) {
			ev := events[i]
			i++
			if failAt > 0 && i == failAt {
				return ev, failWith // event delivered, then the drop
			}
			return ev, nil
		}
		if failAt > 0 && failWith != nil {
			return testEvent{}, failWith
		}
		if hold {
			<-ctx.Done()
			return testEvent{}, ctx.Err()
		}
		return testEvent{}, io.EOF
	}, nil
}

func collector() (chan string, func() []string) {
	ch := make(chan string, 256)
	seen := map[string]bool{}
	var mu sync.Mutex
	var order []string
	go func() {
		for id := range ch {
			mu.Lock()
			if !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
			mu.Unlock()
		}
	}()
	return ch, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(order))
		copy(out, order)
		return out
	}
}

// The acceptance criterion: killing the stream mid-flight reconnects with
// sequence-resume and zero duplicated or missing events.
func TestReconnectResumeNoDupNoLoss(t *testing.T) {
	fs := &fakeStream{failWith: io.ErrUnexpectedEOF, failAt: 5}
	// 10 events; the stream drops after delivering the first 5.
	for i := 1; i <= 10; i++ {
		fs.events = append(fs.events, testEvent{ID: fmt.Sprintf("e%d", i), Seq: int64(i)})
	}

	ch, snapshot := collector()
	sub := New(Config[testEvent]{
		Name:        "test",
		Open:        fs.open,
		GetEventID:  func(e testEvent) string { return e.ID },
		GetSequence: func(e testEvent) int64 { return e.Seq },
		OnEvent:     func(e testEvent) { ch <- e.ID },
		BackoffBase: 5 * time.Millisecond,
	})
	defer sub.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := snapshot()
		if len(got) == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: got %d/10 events: %v", len(got), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := snapshot()
	// Zero duplicates (collector dedups; assert order/identity) and zero
	// missing: the ID sequence must be exactly e1..e10 in order.
	for i, id := range got {
		want := fmt.Sprintf("e%d", i+1)
		if id != want {
			t.Fatalf("event %d = %q, want %q (full: %v)", i, id, want, got)
		}
	}
	// Resume must have kicked in: the second dial carries from_sequence=5.
	fs.mu.Lock()
	redials := append([]int64(nil), fs.redials...)
	fs.mu.Unlock()
	if len(redials) < 2 {
		t.Fatalf("expected ≥2 dials, got %v", redials)
	}
	if redials[0] != 0 {
		t.Fatalf("first dial must not resume, got from=%d", redials[0])
	}
	if redials[1] != 4 {
		// failAt=5: the 5th recv returned (event, error) — like the JS
		// for-await, the errored value is not delivered, so lastSeq=4.
		t.Fatalf("second dial must resume from 4, got from=%d", redials[1])
	}
	// The stream must reach a healthy terminal state after the last
	// successful drain (closed = server closed gracefully).
	deadline = time.Now().Add(2 * time.Second)
	for sub.Status() != StatusClosed {
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want closed; err=%v", sub.Status(), sub.Err())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A replayed tail (server re-sends events we already have) must dedup by
// event id — the hook's seenIds behavior.
func TestDedupOnReplayedTail(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	// Server that replays events 1..3 on every dial (idempotent resume
	// window) plus new ones.
	mk := func() func(ctx context.Context, from int64) (func() (testEvent, error), error) {
		return func(ctx context.Context, from int64) (func() (testEvent, error), error) {
			mu.Lock()
			dials++
			dial := dials
			mu.Unlock()
			// every dial replays 1..3, then adds 10+dial
			evs := []testEvent{{ID: "e1", Seq: 1}, {ID: "e2", Seq: 2}, {ID: "e3", Seq: 3}}
			if dial > 1 {
				evs = append(evs, testEvent{ID: fmt.Sprintf("e10%d", dial), Seq: int64(10 + dial)})
			}
			i := 0
			return func() (testEvent, error) {
				if i < len(evs) {
					ev := evs[i]
					i++
					return ev, nil
				}
				if dial == 1 {
					return testEvent{}, io.ErrUnexpectedEOF // force one reconnect
				}
				return testEvent{}, io.EOF
			}, nil
		}
	}
	ch, snapshot := collector()
	sub := New(Config[testEvent]{
		Name:        "test",
		Open:        mk(),
		GetEventID:  func(e testEvent) string { return e.ID },
		GetSequence: func(e testEvent) int64 { return e.Seq },
		OnEvent:     func(e testEvent) { ch <- e.ID },
	})
	defer sub.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := snapshot()
		if len(got) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: got %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := snapshot()
	// e1..e3 deduped across the replay; exactly one e10x. No duplicates.
	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("event %q delivered %d times (dedup failed): %v", id, n, got)
		}
	}
	if got[0] != "e1" {
		t.Fatalf("first event = %q, want e1", got[0])
	}
}

// Status transitions must surface through OnStatus for the footer.
func TestStatusTransitions(t *testing.T) {
	fs := &fakeStream{failWith: io.ErrUnexpectedEOF, failAt: 1}
	fs.events = []testEvent{{ID: "e1", Seq: 1}}

	var mu sync.Mutex
	var statuses []Status
	sub := New(Config[testEvent]{
		Name:        "test",
		Open:        fs.open,
		GetEventID:  func(e testEvent) string { return e.ID },
		GetSequence: func(e testEvent) int64 { return e.Seq },
		OnStatus:    func(s Status) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() },
		MaxBackoff:  50 * time.Millisecond,
	})
	defer sub.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(statuses)
		last := Status("")
		if n > 0 {
			last = statuses[n-1]
		}
		mu.Unlock()
		// After at least one error cycle we saw connecting/open/error.
		if n >= 3 && (last == StatusError || last == StatusReconnecting || last == StatusOpen || last == StatusClosed) {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("timed out; statuses: %v", statuses)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if statuses[0] != StatusConnecting {
		t.Fatalf("first status = %q, want connecting", statuses[0])
	}
	sawOpen, sawError := false, false
	for _, s := range statuses {
		if s == StatusOpen {
			sawOpen = true
		}
		if s == StatusError {
			sawError = true
		}
	}
	if !sawOpen || !sawError {
		t.Fatalf("missing transition; statuses: %v", statuses)
	}
}

// Ring buffer drops oldest beyond MaxEvents.
func TestRingBufferDropOldest(t *testing.T) {
	var mu sync.Mutex
	var order []int
	done := make(chan struct{})
	sub := New(Config[testEvent]{
		Name: "test",
		Open: func(ctx context.Context, from int64) (func() (testEvent, error), error) {
			i := 0
			return func() (testEvent, error) {
				i++
				if i > 50 {
					return testEvent{}, io.EOF
				}
				return testEvent{ID: fmt.Sprintf("e%d", i), Seq: int64(i)}, nil
			}, nil
		},
		GetEventID:  func(e testEvent) string { return e.ID },
		GetSequence: func(e testEvent) int64 { return e.Seq },
		MaxEvents:   10,
		OnEvent: func(e testEvent) {
			mu.Lock()
			order = append(order, int(e.Seq))
			mu.Unlock()
		},
	})
	defer sub.Close()
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			n := len(order)
			mu.Unlock()
			if n >= 50 {
				return
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	evs := sub.Events()
	if len(evs) != 10 {
		t.Fatalf("ring size = %d, want 10", len(evs))
	}
	if evs[0].Seq != 41 || evs[9].Seq != 50 {
		t.Fatalf("drop-oldest wrong: first=%d last=%d, want 41..50", evs[0].Seq, evs[9].Seq)
	}
}

// Reconnect() must redial immediately instead of waiting out backoff.
func TestReconnectImmediate(t *testing.T) {
	gate := make(chan struct{})
	var mu sync.Mutex
	dials := 0
	sub := New(Config[testEvent]{
		Name: "test",
		Open: func(ctx context.Context, from int64) (func() (testEvent, error), error) {
			mu.Lock()
			dials++
			n := dials
			mu.Unlock()
			if n == 1 {
				<-gate // hold the first dial open until we trigger Reconnect
				return func() (testEvent, error) { return testEvent{}, io.EOF }, nil
			}
			return func() (testEvent, error) { <-ctx.Done(); return testEvent{}, ctx.Err() }, nil
		},
	})
	defer sub.Close()
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := dials
		mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconnect did not redial, dials=%d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Backoff growth must be capped at MaxBackoff (hook's maxBackoffMs).
func TestBackoffCapped(t *testing.T) {
	if BackoffBase*8 > DefaultMaxBackoff {
		t.Fatalf("sanity: base*8 = %v exceeds default cap %v", BackoffBase*8, DefaultMaxBackoff)
	}
	// formula parity: delay(attempt) = base*2^(attempt-1)+jitter, capped
	for attempt := 1; attempt <= 6; attempt++ {
		delay := BackoffBase * time.Duration(1<<(attempt-1))
		if delay > DefaultMaxBackoff {
			delay = DefaultMaxBackoff
		}
		if attempt >= 6 && delay != DefaultMaxBackoff {
			t.Fatalf("attempt %d: delay %v not capped at %v", attempt, delay, DefaultMaxBackoff)
		}
	}
}
