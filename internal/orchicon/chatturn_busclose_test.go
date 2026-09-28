package orchicon

import (
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// The bus is closed by a turn's deferred Close while ANOTHER goroutine may
// still be emitting on it. Sending on a closed channel is a panic, and it is
// not survivable: it took the whole serve process down, which is why an
// operator's interjection killed their control plane and left the client
// frozen mid-turn.
//
// These pin the two halves of the fix: emit must be inert once the bus is
// closed, and Close must not race an in-flight emit. Both are driven against a
// REAL chatBus; the concurrent one is meaningful under -race.

// emitAfterClose is the deterministic reproduction: close, then emit (including
// a TERMINAL event, which is the path that waits for room rather than dropping).
// Before the fix this panicked with "send on closed channel".
func TestChatBusEmitAfterCloseIsInert(t *testing.T) {
	bus := newChatBus()
	bus.Close()

	// Must not panic, and must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.emit(scheduler.SessionEvent{Kind: "delta", Text: "progress"})
		bus.emit(scheduler.SessionEvent{Kind: "tool_part", ToolName: "bash"})
		bus.emit(scheduler.SessionEvent{Kind: "tool_result", ToolName: "bash", IsError: true})
		// Terminal kinds take the waiting path; a closed bus must release it.
		bus.emit(scheduler.SessionEvent{Kind: "idle"})
		bus.emit(scheduler.SessionEvent{Kind: "error", Text: "boom"})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on a closed bus — the terminal path did not release on done")
	}
}

// Close twice must stay safe (once), and a second close must not re-close.
func TestChatBusCloseIsIdempotent(t *testing.T) {
	bus := newChatBus()
	bus.Close()
	bus.Close() // must not panic (would be "close of closed channel")
	select {
	case <-bus.Done():
	default:
		t.Fatal("Done() is not closed after Close()")
	}
}

// The racy half: emitters running concurrently with Close. Run under -race this
// also proves the close/send ordering is synchronised rather than merely
// usually-correct — the bug it fixes was a race, and a race is not fixed by a
// comment.
func TestChatBusConcurrentEmitAndClose(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		bus := newChatBus()
		var wg sync.WaitGroup

		// Emitters: a mix of droppable and terminal events, so both the
		// non-blocking and the waiting paths are exercised against the close.
		for e := 0; e < 4; e++ {
			wg.Add(1)
			go func(e int) {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					kind := "delta"
					if i%7 == 0 {
						kind = "idle" // terminal: waits for room
					}
					bus.emit(scheduler.SessionEvent{Kind: kind, Text: "x"})
				}
			}(e)
		}

		// Drain, so the buffer genuinely fills and empties and the terminal
		// path really waits rather than always finding room.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for {
				select {
				case _, ok := <-bus.Events():
					if !ok {
						return
					}
				case <-bus.Done():
					for {
						select {
						case _, ok := <-bus.Events():
							if !ok {
								return
							}
						default:
							return
						}
					}
				}
			}
		}()

		// Close from a distinct goroutine, mid-flight.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			bus.Close()
		}()

		waitAll := make(chan struct{})
		go func() { wg.Wait(); close(waitAll) }()

		select {
		case <-waitAll:
		case <-time.After(10 * time.Second):
			t.Fatalf("iter %d: emitters blocked against Close — a deadlock, not just a race", iter)
		}
		<-closed
		<-drained
	}
}

// The scenario that took the serve down: two turns of ONE conversation share a
// bus (an interjection adopts the bus of the turn it supersedes). When the
// superseded turn finished it closed the bus, and the still-running turn's next
// emit panicked the process — and even without the panic, the live turn's
// stream was over. The bus must stay open until the LAST turn is done.
func TestChatBusSharedByTwoTurnsClosesOnlyWhenLastLeaves(t *testing.T) {
	bus := newChatBus()
	bus.adopt() // turn A: the turn that gets superseded
	bus.adopt() // turn B: the interjection, which adopted the same bus

	// A finishes first (superseded). This must NOT close the bus.
	bus.release()

	select {
	case <-bus.Done():
		t.Fatal("the bus closed while a turn still held it — that is the live turn's emit target")
	default:
	}

	// B is still working: its events must reach the consumer.
	bus.emit(scheduler.SessionEvent{Kind: "delta", Text: "still alive"})
	select {
	case evt := <-bus.Events():
		if evt.Text != "still alive" {
			t.Fatalf("expected the live turn's event, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the live turn's event never arrived — its bus had been closed under it")
	}

	// B finishes. Now the bus closes.
	bus.release()
	select {
	case <-bus.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the bus did not close after the last turn released it — the collector would wait forever")
	}
}
