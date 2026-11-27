package antigravity

// ask_bus.go — ONE Ask conversation's SessionBus, and nothing else.
//
// IT IS THE SAME BUS THE CLAUDE ASK TRANSPORT USES, deliberately: the collectors that consume
// scheduler.SessionEvent (internal/askorchicon's drain loop, the TUI's session stream, the GUI's) were
// built against ONE shape, and a second shape per adapter would mean a second drain loop. The two
// behaviours that matter and are easy to get wrong:
//
//  1. A TERMINAL EVENT IS NEVER DROPPED. `idle` and `error` end the collector's drain, so a bus that
//     dropped one under back-pressure would hang the turn until the reply window expired. They block
//     instead (bounded by the consumer, which is always draining), and a superseded bus releases them.
//
//  2. A NON-TERMINAL EVENT IS DROPPED RATHER THAN WAITED FOR. A delta nobody is reading yet must not
//     stall the ACP reader goroutine: the wire would stop, the turn would stop, and the cause would look
//     like an unresponsive model.

import (
	"sync"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// askBusCapacity is the per-conversation event buffer — the same depth claude's Ask bus uses.
const askBusCapacity = 256

// askBus is one conversation's SessionBus.
type askBus struct {
	events chan scheduler.SessionEvent
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
}

// newAskBus builds an open bus.
func newAskBus() *askBus {
	return &askBus{events: make(chan scheduler.SessionEvent, askBusCapacity), done: make(chan struct{})}
}

// Events implements scheduler.SessionBus.
func (b *askBus) Events() <-chan scheduler.SessionEvent { return b.events }

// Done implements scheduler.SessionBus.
func (b *askBus) Done() <-chan struct{} { return b.done }

// Close ends the bus. Idempotent: a superseded bus is closed by the next Subscribe AND by a teardown,
// and a double close on a channel is a panic.
func (b *askBus) Close() {
	b.once.Do(func() {
		close(b.done)
		b.mu.Lock()
		close(b.events)
		b.mu.Unlock()
	})
}

// emit delivers one event, dropping a non-terminal one under back-pressure and never dropping a
// terminal one (see the file comment).
func (b *askBus) emit(evt scheduler.SessionEvent) {
	terminal := evt.Kind == "idle" || evt.Kind == "error"

	b.mu.RLock()
	defer b.mu.RUnlock()

	select {
	case <-b.done:
		// Closed: this turn was superseded and has no consumer.
		return
	default:
	}

	if terminal {
		select {
		case b.events <- evt:
		case <-b.done:
		}
		return
	}
	select {
	case b.events <- evt:
	default:
	}
}
