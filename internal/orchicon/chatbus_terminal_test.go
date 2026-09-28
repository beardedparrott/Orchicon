package orchicon

import (
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestChatBusNeverDropsTerminalEvents is the guard for the turn-ending signal.
//
// The bus is small (32 slots) and drops best-effort when full, which is fine for
// progress reporting and FATAL for `idle`: a dropped idle leaves the collector
// waiting forever, so the turn wedges with no error to diagnose. Emitting a
// tool_result per tool call made a full buffer reachable in an ordinary
// multi-tool turn (TestChatTurnClientManyToolRoundsUnbounded reported "no idle at
// turn end"), so the rule is now explicit: a terminal event waits for room.
func TestChatBusNeverDropsTerminalEvents(t *testing.T) {
	b := newChatBus()
	defer b.Close()

	// Fill the buffer COMPLETELY, with nothing draining.
	for i := 0; i < cap(b.events); i++ {
		b.emit(scheduler.SessionEvent{Kind: "delta", Text: "x"})
	}

	// CONTROL: a non-terminal signal is still best-effort — it returns at once
	// and is dropped, which is the documented behaviour for progress reporting.
	// Without this, the assertion below could pass for the wrong reason.
	ordinary := make(chan struct{})
	go func() { b.emit(scheduler.SessionEvent{Kind: "delta", Text: "dropped"}); close(ordinary) }()
	select {
	case <-ordinary:
	case <-time.After(2 * time.Second):
		t.Fatal("control: an ordinary signal must NOT block on a full buffer — it is best-effort by design")
	}

	// The terminal signal must NOT be dropped: it blocks until there is room.
	terminal := make(chan struct{})
	go func() { b.emit(scheduler.SessionEvent{Kind: "idle"}); close(terminal) }()
	select {
	case <-terminal:
		t.Fatal("a terminal event returned while the buffer was full — it was dropped, and the turn would never end")
	case <-time.After(100 * time.Millisecond):
		// Still waiting for room. Correct.
	}

	// Drain until the idle arrives. It must, because the emitter is holding it.
	got := false
	deadline := time.After(5 * time.Second)
	for !got {
		select {
		case e, ok := <-b.events:
			if !ok {
				t.Fatal("bus closed before the terminal event was delivered")
			}
			if e.Kind == "idle" {
				got = true
			}
		case <-deadline:
			t.Fatal("the terminal idle event was never delivered — a full buffer ate it and the turn would wedge")
		}
	}
	select {
	case <-terminal:
	case <-time.After(time.Second):
		t.Fatal("the emitter is still blocked after its event was delivered")
	}
}

// TestChatBusTerminalEventWaitsAreBoundedByDone: a consumer that has gone away
// must not hang the emitting goroutine. This is what the done arm of the wait is
// for.
func TestChatBusTerminalEventWaitsAreBoundedByDone(t *testing.T) {
	b := newChatBus()
	for i := 0; i < cap(b.events); i++ {
		b.emit(scheduler.SessionEvent{Kind: "delta", Text: "x"})
	}
	released := make(chan struct{})
	go func() {
		// Blocks on the full buffer, then is released by Close below.
		b.emit(scheduler.SessionEvent{Kind: "idle"})
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("control: emit should still be waiting on the full buffer")
	case <-time.After(50 * time.Millisecond):
	}
	b.Close()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a terminal emit waiting on a full buffer — the drain goroutine would leak")
	}
}
