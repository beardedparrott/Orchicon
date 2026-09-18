package chat

// turn_poll_test.go — THE MID-TURN POLL IS ARMED WITH THE TURN AND STOPS WITH IT.
//
// The merge half of the live-streaming fix lives in internal/tui (it owns chatStore); this file covers
// the SCHEDULING, which is here because it reads the controller's own per-conversation state.
//
// Why the poll exists at all is worth restating where the scheduling lives: the live socket delivered
// heartbeats and nothing else, so the pane showed a thinking notice whose age kept resetting and never
// a reply. The server already mirrors the running turn's text, reasoning and tool ledger into the
// acked assistant message every 250ms — for exactly this audience, "a client that lost the live stream
// (refresh, another tab/device) polls ListMessages and watches the reply grow" — so the TUI polls, and
// liveness stops depending on a long-lived connection.

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// THE POLL RUNS WHILE THE TURN STREAMS, and stops as soon as it does not.
//
// Asserted through the real command channel the shell drains, because that is the wiring that matters:
// a poll that fired into nothing would be indistinguishable from no poll at all. The interval is a
// constant (askTurnPollInterval), so this waits on real time — once, for the behaviour that makes the
// feature exist.
func TestTheTurnPollPushesCommandsWhileStreamingAndStopsAfter(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 8)
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	c.mu.Lock()
	c.state["c1"] = &convState{streaming: true}
	c.mu.Unlock()

	go c.runTurnPoll("c1")

	select {
	case <-cmds:
	case <-time.After(4 * time.Second):
		t.Fatal("no poll was pushed while the turn was streaming — the mid-turn poll IS the live-streaming " +
			"fix: without it nothing refreshes the transcript from the durable store")
	}

	// The turn ends: the poll must stop rather than leave a goroutine per turn behind.
	c.mu.Lock()
	c.state["c1"].streaming = false
	c.mu.Unlock()

	// Drain anything already queued, then confirm nothing new arrives across several intervals.
	deadline := time.After(3 * askTurnPollInterval)
drain:
	for {
		select {
		case <-cmds:
		case <-deadline:
			break drain
		}
	}
	if n := len(cmds); n != 0 {
		t.Errorf("%d poll command(s) queued after the turn stopped streaming — the goroutine must not "+
			"outlive the turn it serves", n)
	}
}

// A POLL FOR A CONVERSATION WITH NO SLOT STOPS IMMEDIATELY, so a turn torn down between the arm and
// the first tick does not leave a poll running against nothing.
func TestTheTurnPollReturnsWithoutASlot(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 8)
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	done := make(chan struct{})
	go func() {
		c.runTurnPoll("never-existed")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * askTurnPollInterval):
		t.Fatal("runTurnPoll kept running for a conversation with no state slot")
	}
	if n := len(cmds); n != 0 {
		t.Errorf("%d poll(s) pushed for an unknown conversation", n)
	}
}

// AND A FINISHED TURN STOPS IT, which is the same check from the other direction: `streaming` is
// cleared by EndStream on the completion poll, and the poll must notice rather than run forever.
func TestTheTurnPollStopsWhenTheTurnCompletes(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 8)
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	c.mu.Lock()
	c.state["c1"] = &convState{streaming: true}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.runTurnPoll("c1")
		close(done)
	}()

	// Let it tick at least once, then complete the turn.
	time.Sleep(askTurnPollInterval + 200*time.Millisecond)
	c.mu.Lock()
	c.state["c1"].streaming = false
	c.mu.Unlock()

	select {
	case <-done:
	case <-time.After(3 * askTurnPollInterval):
		t.Fatal("the poll did not stop when the turn completed — one goroutine per turn would accumulate " +
			"for the life of the session")
	}
}

// ⚠️ THE POLL IS ARMED BY THE SEND, NOT BY THE STREAM OPENING. THIS IS THE BUG.
//
// The operator's discovery, which cracked it: "When I click into a conversation and send a message, I
// see the 'Orchicon is thinking...' and the watchdog notification, but NO other messages show up.
// However, when I click away from the conversation and go back into it, reasoning shows up and the
// stream is now streaming live!" — and their hypothesis, "is it possible the 'Orchicon is thinking...'
// message is what was preventing the live streaming?"
//
// The notice was the SYMPTOM, not the cause. Both liveness goroutines used to be armed inside
// startStream's command, AFTER `ChatStream` returned. That call blocks until the server's response
// headers arrive — so when a proxy buffers the streaming response, it blocks FOREVER: `consume` never
// runs, neither goroutine is ever started, and `streaming` (set true synchronously before the call) stays
// true, which is exactly why the pane showed the thinking notice and its watchdog age and nothing else.
//
// EVERY SAFETY NET WAS GATED BEHIND THE FAILURE IT EXISTED TO COVER — including the durable poll I added
// for it. Armed at the send instead, the poll runs whatever the transport does.
//
// A nil client is the cleanest stand-in for "the stream never opens": ChatStream cannot succeed, so
// startStream's command never arms anything. If the poll pushes a command anyway, it was armed by the
// send — which is the property.
func TestThePollIsArmedEvenWhenTheStreamNeverOpens(t *testing.T) {
	c := NewController(nil)
	cmds := make(chan tea.Cmd, 8)
	c.Bind(&recorder{conn: map[string]bool{}}, cmds)

	c.SendWithAttachments("c1", "hello", "", nil)
	// Deliberately do NOT run the returned stream command: it is the thing that blocks.

	select {
	case <-cmds:
	case <-time.After(4 * time.Second):
		t.Fatal("no poll was armed by the send. The poll must NOT depend on the stream opening — that " +
			"dependency is the whole bug: a handshake that blocks forever left every safety net unstarted " +
			"while the pane showed a thinking notice and nothing else")
	}
}

// AND THE WATCHDOG'S CLOCK IS STAMPED BY THE SEND, so a handshake that never completes is caught as
// SILENCE rather than waiting forever for a first event that will never arrive.
//
// Without this stamp `lastActivity` stays zero, which livenessCheck deliberately treats as "not started
// yet" — correct for a stream still opening, and exactly wrong here: it is the case where the stream
// never opens at all, so the watchdog would have waited for ever alongside the poll.
func TestTheSendStampsTheWatchdogsClock(t *testing.T) {
	c := NewController(nil)
	c.SendWithAttachments("c1", "hello", "", nil)

	c.mu.Lock()
	st := c.state["c1"]
	c.mu.Unlock()
	if st == nil {
		t.Fatal("no conversation slot was created by the send")
	}
	if st.lastActivity == 0 {
		t.Fatal("the send did not stamp lastActivity, so a stream that never opens is indistinguishable " +
			"from one that has not started yet and the watchdog can never fire")
	}
	if !st.streaming {
		t.Error("the send did not mark the turn streaming, so the pane would show no indicator at all")
	}
	// With the stamp in place, silence past the timeout IS detected — the case the watchdog exists for.
	c.mu.Lock()
	c.state["c1"].lastActivity = now() - askStreamStallTimeout.Milliseconds() - 1
	c.mu.Unlock()
	if !c.streamIsStale("c1") {
		t.Error("a send that has gone silent past the timeout is not reported stale")
	}
}
