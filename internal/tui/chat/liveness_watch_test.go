package chat

// liveness_watch_test.go — A STREAM THAT DIES SILENTLY MUST STILL BE RE-DIALLED.
//
// The operator, with a side-by-side screenshot: the GUI showed the reply streaming, a reasoning
// block, and "Connection interrupted — still working… Output continues below." with "Last activity
// 1s ago". The TUI showed only "Orchicon is thinking…" for the whole turn — and re-entering the
// conversation "fixed" it, because that runs a fresh ListMessages.
//
// The TUI had no client-side liveness check at all. A connection through the container network can
// die HALF-OPEN — no FIN reaches the client — so `stream.Receive()` blocks forever: no error, no
// EOF, so consume() never returns and dropStream (which owns the re-dial) is never reached. The
// server was sending a Heartbeat every 15 seconds the whole time, and the TUI received them; the
// Heartbeat case only cleared a flag. A heartbeat IS the server saying "alive", so its absence is
// the death signal.
//
// These tests drive the watchdog directly (its timeout is a constant, and waiting 40s per case is
// not a test). What they pin is the DECISION: staleness is measured from activity, activity includes
// heartbeats, and a stale stream takes the SAME re-dial path as a socket that broke loudly.

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A STREAM WITH NO EVENTS GOES STALE, and a stream that keeps receiving heartbeats does not.
//
// The two halves matter equally: without the first, the bug stands; without the second, a working
// turn would be torn down mid-flight every time the model thought for a while.
func TestStalenessIsMeasuredFromActivityIncludingHeartbeats(t *testing.T) {
	c := NewController(nil)

	// Nothing heard for longer than the timeout → stale.
	c.state["c1"] = &convState{streaming: true, lastActivity: now() - askStreamStallTimeout.Milliseconds() - 1}
	if !c.streamIsStale("c1") {
		t.Error("a stream with no event for longer than the timeout is not considered stale — this is the " +
			"streaming bug: the TUI would wait forever while the GUI re-attached")
	}

	// A heartbeat one moment ago → live, however long the turn has been running.
	c.state["c1"] = &convState{streaming: true, lastActivity: now() - 1}
	if c.streamIsStale("c1") {
		t.Error("a stream that just received an event is considered stale — a long-thinking turn would be " +
			"torn down and re-dialled for no reason")
	}

	// Exactly the timeout is still live; the check is strictly greater, so the boundary cannot
	// flicker between two ticks of the watchdog.
	c.state["c1"] = &convState{streaming: true, lastActivity: now() - askStreamStallTimeout.Milliseconds()}
	if c.streamIsStale("c1") {
		t.Error("a stream at exactly the timeout is stale — the boundary should not be inclusive")
	}
}

// A STREAM THAT HAS NOT DELIVERED ITS FIRST EVENT IS NOT STALE.
//
// lastActivity is zero until the stream is armed, and zero is not "silent since 1970" — the
// observable difference is a turn torn down during its own handshake.
func TestAStreamThatHasNotStartedIsNotStale(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true} // lastActivity unset
	if c.streamIsStale("c1") {
		t.Error("a stream whose first event has not arrived is reported stale — it would be re-dialled " +
			"before it ever ran")
	}
}

// SILENCE IS ONLY A FAULT WHILE STREAMING, which is what keeps a finished turn from being re-dialled.
func TestAFinishedTurnIsNeverStale(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: false, lastActivity: now() - 10*askStreamStallTimeout.Milliseconds()}
	if c.streamIsStale("c1") {
		t.Error("a turn that is no longer streaming is reported stale — the watchdog would re-dial a " +
			"completed turn forever")
	}
	if c.streamIsStale("nope") {
		t.Error("an unknown conversation is reported stale")
	}
}

// AND A STALLED STREAM TAKES THE SAME RE-DIAL PATH AS A BROKEN SOCKET.
//
// This is what makes the watchdog a FIX rather than a report: dropStream is where `reconnecting` is
// set and the Watch command is queued, and that is exactly what the shell acts on — it flips the
// pane to its connection banner and re-attaches. So the stalled case is driven THROUGH dropStream
// and asserted on the production state and the real command channel.
func TestAStalledStreamGoesReconnectingAndQueuesARedial(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 4) // bounded like the shell's own channel
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	// An ACKED, streaming turn that has gone quiet — the operator's situation. pendingReplyID is what
	// makes it recoverable: without an ack the server-side turn may never have started, and
	// dropStream tears that down instead.
	c.state["c1"] = &convState{
		streaming:      true,
		pendingReplyID: "a1",
		lastActivity:   now() - askStreamStallTimeout.Milliseconds() - 1,
	}
	if !c.streamIsStale("c1") {
		t.Fatal("fixture: the slot is not stale, so this test would measure nothing")
	}

	c.dropStream("c1", 0, errStreamStalled)

	st := c.state["c1"]
	if !st.streaming {
		t.Error("the slot stopped streaming — the server-side collector is still running, so the TUI would " +
			"show the turn as over while the reply was still being produced")
	}
	if !st.reconnecting {
		t.Error("a stalled stream did not go RECONNECTING, so the pane shows no connection banner and the " +
			"operator is left with a bare 'thinking' notice and no explanation")
	}
	if len(cmds) == 0 {
		t.Error("no re-dial command was queued — with a dead socket and no Watch, nothing would ever bring " +
			"the stream back, which is exactly the bug: the turn completed server-side and only reappeared on " +
			"re-entering the conversation")
	}
}

// A STALL BEFORE THE ACK IS TORN DOWN RATHER THAN RE-DIALLED, matching the rule a broken socket
// follows: there is no assistant message to watch, so waiting on one would hang forever.
func TestAStallBeforeTheAckTearsTheTurnDown(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 4)
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	c.state["c1"] = &convState{streaming: true, pendingReplyID: "", lastActivity: now()}
	c.dropStream("c1", 0, errStreamStalled)

	if c.state["c1"].streaming {
		t.Error("a pre-ack stream that died left the slot streaming — nothing will ever resolve it, so the " +
			"composer keeps its stop affordance for a turn that is not running")
	}
	if c.state["c1"].reconnecting {
		t.Error("a pre-ack stream was marked RECONNECTING with no assistant message to watch")
	}
}

// THE AGE THE ACTIVITY LINE SHOWS COMES FROM THIS CLOCK, so the two cannot disagree.
//
// The shell's notice renders chat.Controller.SilenceSince, and the watchdog tears a stream down on
// the SAME lastActivity. If the line computed its own elapsed time from a different start point, the
// pane could read "last activity 2s ago" for a stream the watchdog was about to declare stalled —
// a reassurance that is worse than silence, because it hides the failure.
func TestSilenceSinceReportsOnlyRealSilence(t *testing.T) {
	c := NewController(nil)

	// No slot at all: no information, not "silent forever".
	if got := c.SilenceSince("unknown"); got != 0 {
		t.Errorf("SilenceSince on an unknown conversation = %v, want 0", got)
	}
	// Present but finished: silence is not a fault once nothing is streaming.
	c.state["c1"] = &convState{streaming: false, lastActivity: now() - time.Minute.Milliseconds()}
	if got := c.SilenceSince("c1"); got != 0 {
		t.Errorf("SilenceSince on a non-streaming conversation = %v, want 0", got)
	}
	// Streaming with no event yet: an unstarted stream is not silence (the same distinction
	// livenessCheck makes, and for the same reason).
	c.state["c1"] = &convState{streaming: true}
	if got := c.SilenceSince("c1"); got != 0 {
		t.Errorf("SilenceSince before the first event = %v, want 0", got)
	}
	// An event just now.
	c.state["c1"] = &convState{streaming: true, lastActivity: now()}
	if got := c.SilenceSince("c1"); got < 0 || got > time.Second {
		t.Errorf("SilenceSince after an immediate event = %v, want a moment", got)
	}
	// A 30s silence reports as ~30s, which is what the notice band renders.
	c.state["c1"] = &convState{streaming: true, lastActivity: now() - 30*time.Second.Milliseconds()}
	if got := c.SilenceSince("c1"); got < 29*time.Second || got > 31*time.Second {
		t.Errorf("SilenceSince after a 30s gap = %v, want ~30s", got)
	}
	// A clock that stepped backwards is not a silence — otherwise the notice would print a negative
	// age, and the watchdog's comparison would invert.
	c.state["c1"] = &convState{streaming: true, lastActivity: now() + time.Hour.Milliseconds()}
	if got := c.SilenceSince("c1"); got != 0 {
		t.Errorf("SilenceSince with a future timestamp = %v, want 0", got)
	}
}
