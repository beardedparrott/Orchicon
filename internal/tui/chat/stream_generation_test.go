package chat

// stream_generation_test.go — ONE STREAM'S END MUST NOT END ANOTHER STREAM'S TURN.
//
// Two operator reports, one mechanism:
//
//	"if I interject on an ongoing message in conversations in the TUI, it duplicates my message"
//	"When I leave an chat and go back into it, it loses the 'orchicon is thinking...' and watchdog"
//
// An interjection SUPERSEDES the running turn, which closes the old stream. That close arrived as a
// StreamDoneMsg carrying only the conversation id, and the shell cleared the slot unconditionally — i.e. it
// cleared the state of the NEW turn the operator had just started. The thinking notice, the watchdog and the
// pending-reply id all went with it, and the poll fell to the non-streaming REPLACE path.
//
// The fix is a generation per send: a stream carries the generation it started under, and only a matching
// generation may touch the slot. These tests pin that, and the re-attach that fills the slot back in when this
// client never had one.

import (
	"testing"
)

// A SUPERSEDED STREAM'S END IS IGNORED. This is the interjection case: the old stream closes, the new turn
// must survive it.
func TestASupersededStreamEndDoesNotClearTheNewTurn(t *testing.T) {
	c := NewController(nil)

	// Turn 1 is streaming (generation 1).
	c.mu.Lock()
	c.state["c1"] = &convState{streaming: true, pendingReplyID: "a1", gen: 1}
	c.mu.Unlock()
	oldGen := c.CurrentGen("c1")

	// The operator INTERJECTS: the slot moves to generation 2 with a fresh ack.
	c.mu.Lock()
	c.state["c1"].gen++
	c.state["c1"].pendingReplyID = "a2"
	c.mu.Unlock()

	// Now the SUPERSEDED stream closes.
	c.EndStream("c1", oldGen)

	st := c.state["c1"]
	if !st.streaming {
		t.Error("the superseded stream's close stopped the slot — the interjection's turn is running, so the " +
			"operator loses the thinking indicator and the watchdog the moment they interject")
	}
	if st.pendingReplyID != "a2" {
		t.Errorf("pendingReplyID = %q after the old stream closed, want a2 — the re-attach target for the "+
			"interjection's turn was cleared by the turn it replaced", st.pendingReplyID)
	}
}

// AND A DROP FROM A SUPERSEDED STREAM IS IGNORED TOO — the stalled-watchdog and socket-teardown paths take
// the same route, and a stale one would tear down the live turn.
func TestASupersededStreamDropDoesNotTearDownTheNewTurn(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true, pendingReplyID: "a1", gen: 1}
	oldGen := c.CurrentGen("c1")

	c.mu.Lock()
	c.state["c1"].gen++
	c.state["c1"].pendingReplyID = "a2"
	c.mu.Unlock()

	c.dropStream("c1", oldGen, errStreamStalled)

	st := c.state["c1"]
	if !st.streaming {
		t.Error("a stale drop tore the slot down — the interjection's turn is still running")
	}
	if st.pendingReplyID != "a2" {
		t.Errorf("pendingReplyID = %q, want a2 (a stale drop must not rewrite the live turn's state)",
			st.pendingReplyID)
	}
}

// THE CURRENT STREAM'S END STILL CLEARS THE SLOT — the guard must not be so strict that a finished turn leaves
// the composer advertising a Stop key for a reply that is over.
func TestTheCurrentStreamEndStillClearsTheSlot(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true, pendingReplyID: "a1", gen: 3}

	c.EndStream("c1", 3)

	st := c.state["c1"]
	if st.streaming || st.pendingReplyID != "" {
		t.Errorf("the current stream's end left the slot open (streaming=%v pending=%q) — the stop affordance "+
			"would advertise a dead key", st.streaming, st.pendingReplyID)
	}
}

// A WATCH ARMED FOR ONE TURN STOPS WHEN THE SLOT MOVES ON, rather than judging the replacement turn's slot.
// Without this the superseded turn's watchdog would eventually see a quiet NEW turn and tear it down — the
// watchdog causing the very failure it exists to prevent.
func TestAWatchStopsWhenItsGenerationIsSuperseded(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{
		streaming:    true,
		gen:          1,
		lastActivity: now() - askStreamStallTimeout.Milliseconds() - 1, // long silent
	}

	stop, stale := c.livenessCheckFor("c1", 1)
	if stop || !stale {
		t.Fatalf("fixture: the current generation must read as stalled (stop=%v stale=%v)", stop, stale)
	}

	// The slot moves to a new turn: this watch has nothing left to say about it, even though the activity
	// stamp still looks silent.
	c.mu.Lock()
	c.state["c1"].gen = 2
	c.mu.Unlock()

	stop, stale = c.livenessCheckFor("c1", 1)
	if !stop {
		t.Error("a watch whose generation was superseded kept running — it would report on the turn that " +
			"replaced it")
	}
	if stale {
		t.Error("a superseded watch still judged the new turn's slot stale, so it would tear down a turn it " +
			"does not own")
	}
}

// --- re-attach (the leave-and-return bug) ---------------------------------------------------------------

// RE-ATTACH RESTORES THE SLOT for a turn the server reports as running. This is the operator's "leave a chat
// and go back into it" — the pane's thinking notice and watchdog are derived from the slot, and nothing
// restored it.
func TestReattachRestoresTheSlotForAServerRunningTurn(t *testing.T) {
	c := NewController(nil)

	if cmd := c.Reattach("c1", "a1"); cmd == nil {
		t.Fatal("re-attaching to a running turn produced no Watch command — the live stream would never be " +
			"picked up again, leaving only the poll")
	}

	st := c.state["c1"]
	if !st.streaming {
		t.Fatal("the slot is not streaming after re-attach, so the pane shows an idle conversation")
	}
	if st.pendingReplyID != "a1" {
		t.Errorf("pendingReplyID = %q, want a1", st.pendingReplyID)
	}
	if !st.reconnecting {
		t.Error("the re-attached slot is not marked reconnecting — the pane cannot tell the operator that the " +
			"reply continues from elsewhere")
	}
}

// A LIVE LOCAL STREAM ALWAYS WINS. Server state fills a gap; it must never replace a working stream with a
// second one, which is the GUI's rule too.
func TestReattachLeavesALiveLocalStreamAlone(t *testing.T) {
	c := NewController(nil)
	c.state["c1"] = &convState{streaming: true, pendingReplyID: "mine", gen: 7}

	if cmd := c.Reattach("c1", "server"); cmd != nil {
		t.Error("re-attach produced a Watch for a conversation this client is already streaming")
	}
	st := c.state["c1"]
	if st.pendingReplyID != "mine" {
		t.Errorf("re-attach overwrote the live turn's pending id with the server's (%q) — the local stream is "+
			"authoritative while it runs", st.pendingReplyID)
	}
	if st.gen != 7 {
		t.Errorf("re-attach bumped the generation to %d; it adopts the turn it re-attaches to, so a re-attach "+
			"must not make a live stream look superseded", st.gen)
	}
}

// NOTHING TO RE-ATTACH WITHOUT AN ID: turn_in_flight alone is not enough, because the Watch RPC needs the
// assistant message id to address THIS turn rather than a stale one.
func TestReattachNeedsAPendingReplyID(t *testing.T) {
	c := NewController(nil)
	if cmd := c.Reattach("c1", ""); cmd != nil {
		t.Error("re-attach ran without a pending assistant id")
	}
	if cmd := c.Reattach("", "a1"); cmd != nil {
		t.Error("re-attach ran without a conversation")
	}
	if st := c.state["c1"]; st != nil && st.streaming {
		t.Error("a no-op re-attach started streaming")
	}
}
