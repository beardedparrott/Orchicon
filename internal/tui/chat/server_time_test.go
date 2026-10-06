package chat

// server_time_test.go — THE ACTIVITY VERB ROTATION GETS ITS CLOCK FROM THE HEARTBEAT.
//
// The operator: "rotating through a series of words that means 'orchicon is thinking' ... that changes every
// few seconds." The word itself is a pure function of server time (verbs.go), and the ONLY place that clock
// reaches this client is Heartbeat.server_time_unix_ms — a field the server already emits (internal/askorchicon
// /chat.go sets it so "the client [can] measure socket age/skew") that the TUI used to throw away, keeping
// only the reconnect flag it also clears.
//
// These tests pin the WIRING, which is where a rotation silently dies: a heartbeat that records nothing
// leaves the line on the fallback word forever, and a local clock used as the SOURCE (rather than as a delta)
// would let two clients with skewed clocks disagree — the one thing the server-stamp design exists to
// prevent.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// A HEARTBEAT PLANTS THE SERVER STAMP; NOTHING ELSE DOES.
func TestHeartbeatRecordsTheServerClock(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true}

	// Before any heartbeat there is no stamp, and the caller is told so — this is the pre-content, first
	// second of a turn, when the operator is most likely to be looking.
	if _, ok := c.ServerTimeSince("c1"); ok {
		t.Fatal("ServerTimeSince reported a stamp before any heartbeat arrived — the fallback word would " +
			"be skipped and the line could compute from an unset clock")
	}

	// A text chunk is NOT a clock: only the heartbeat carries the server's time.
	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_TextChunk{
			TextChunk: &apiv1.TextChunk{Content: "hello"},
		},
	})
	if _, ok := c.ServerTimeSince("c1"); ok {
		t.Fatal("a text chunk planted a server stamp — the rotation must index on the SERVER's clock, not " +
			"on whenever content happened to arrive")
	}

	const stamp int64 = 1_700_000_123_456
	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_Heartbeat{
			Heartbeat: &apiv1.Heartbeat{ServerTimeUnixMs: stamp},
		},
	})

	got, ok := c.ServerTimeSince("c1")
	if !ok {
		t.Fatal("the heartbeat was received but no server stamp was recorded — the activity verb would " +
			"stay on its fallback word for the whole turn")
	}
	// The reported value is the stamp plus a small LOCAL delta (milliseconds since receipt), never a
	// replacement for it.
	if got < stamp || got > stamp+5_000 {
		t.Errorf("ServerTimeSince = %d, want the heartbeat stamp %d plus a small delta — a value outside "+
			"that band means the local clock is being used as the SOURCE, which would let two skewed "+
			"clients draw different words", got, stamp)
	}
	// And the word drawn from it is the selector's own answer for that server time, so both clients agree.
	if want := VerbAt(stamp); got-1_000 > stamp && VerbAt(got) != VerbAt(stamp) {
		t.Errorf("the extrapolated stamp %d names %q but the raw stamp %d names %q", got, VerbAt(got),
			stamp, want)
	}
}

// A ZERO STAMP IS NOT A STAMP. A server that emitted a zero (or a Heartbeat with no field set, which the
// generated getter returns as 0) must not plant an anchor at the epoch; the fallback word is the correct
// answer until a real stamp arrives.
func TestZeroHeartbeatStampIsIgnored(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true}
	c.handleEvent("c1", &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_Heartbeat{Heartbeat: &apiv1.Heartbeat{}},
	})
	if _, ok := c.ServerTimeSince("c1"); ok {
		t.Fatal("a zero heartbeat stamp was recorded — the line would index off 1970 rather than falling " +
			"back to the list's first word")
	}
	if got := ActivityVerb(0); got != AskVerbs[0] {
		t.Errorf("the no-stamp fallback is %q, want the list's first entry %q", got, AskVerbs[0])
	}
}

// A CONVERSATION WITH NO SLOT AT ALL IS NOT A CRASH AND NOT A STAMP.
func TestServerTimeSinceWithNoSlot(t *testing.T) {
	c := NewController(nil)
	if got, ok := c.ServerTimeSince("never-opened"); ok || got != 0 {
		t.Errorf("ServerTimeSince on a conversation with no slot = (%d, %v), want (0, false)", got, ok)
	}
}
