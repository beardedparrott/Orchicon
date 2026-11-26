package antigravity

// session_ops_test.go — AC6: mid-run injection, abort, compaction and a follow-up continuation, each against
// a LIVE session (a fake child holding a turn open), not against a handle.
//
// WHY THE FAKE MUST HANG FOR THESE: an injection that arrives after the turn already ended proves nothing —
// agy would have no running turn to queue behind. So `script.hang` makes the child withhold its prompt answer
// until it receives `session/cancel` or the client tears the process down, which is exactly the state the real
// bridge is in during a long model turn.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestBridge_SendExecutionMessageIsQueuedUntilTheTurnEnds is AC6's injection half, and it pins the ONE
// deliberate deviation from claude: agy has no channel for injecting into a running turn, so the message is
// QUEUED and delivered as the next `session/prompt` after the current `stopReason`.
func TestBridge_SendExecutionMessageIsQueuedUntilTheTurnEnds(t *testing.T) {
	h := newHarness(t)
	h.serveHanging()
	h.start()

	// Wait until the FIRST prompt is genuinely in flight.
	proc := h.awaitProc(t, 1)
	h.awaitPrompts(t, 0, 1)

	// Inject while the turn is running.
	if err := h.b.SendExecutionMessage(context.Background(), h.manifest.ExecutionID,
		"also create a second file"); err != nil {
		t.Fatalf("SendExecutionMessage on a live session: %v", err)
	}

	// A SECOND PROMPT MUST NOT EXIST YET: agy defines nothing for two overlapping prompts, so writing one
	// would corrupt the wire.
	if got := len(proc.promptsSeen()); got != 1 {
		t.Fatalf("session/prompt count = %d while the first turn is still running — the injection must be "+
			"queued, not written concurrently (agy defines no behaviour for a second in-flight prompt)", got)
	}

	// The queued message is delivered once the turn's stopReason arrives. Release the turn the way agy does when
	// the model simply finishes — NOT with a cancel, which would end the session before the queue drained.
	proc.release()
	prompts := h.awaitPrompts(t, 0, 2)
	if !strings.Contains(prompts[1].Text, "also create a second file") {
		t.Errorf("second prompt = %q, want the injected message", prompts[1].Text)
	}
	// The queued message is also a durable user part, so the transcript shows the human's message.
	var sawUser bool
	for _, p := range h.partsOfKind(db.SessionPartUserMessage) {
		if strings.Contains(string(p.Payload), "also create a second file") {
			sawUser = true
		}
	}
	if !sawUser {
		t.Error("the injected message produced no user_message session part")
	}
}

// TestBridge_AbortExecutionCancelsThenCloses is AC6's abort half: `session/cancel` goes out, the turn ends
// `cancelled`, and the execution is reported as a FAILURE that names the reason.
func TestBridge_AbortExecutionCancelsThenCloses(t *testing.T) {
	// Shorten the escalation so the test does not sit on the production grace window.
	prev := abortGrace
	abortGrace = 250 * time.Millisecond
	t.Cleanup(func() { abortGrace = prev })

	h := newHarness(t)
	h.serveHanging()
	h.start()
	h.awaitProc(t, 1)
	h.awaitPrompts(t, 0, 1)
	proc := h.proc()

	if err := h.b.AbortExecution(context.Background(), h.manifest.ExecutionID, "operator cancelled the run"); err != nil {
		t.Fatalf("AbortExecution: %v", err)
	}
	res := h.awaitResult()

	// `session/cancel` is the graceful half and must have been sent BEFORE anything was killed: it is what lets
	// agy end the turn with a stopReason and report the turn's usage.
	waitFor(t, "session/cancel to be observed", func() bool { return proc.cancelCount() >= 1 })

	if res.OK {
		t.Fatalf("a cancelled execution reported success (output %q)", res.Output)
	}
	if !strings.Contains(res.ErrMsg, "operator cancelled the run") {
		t.Errorf("failure message = %q, want the reason the caller supplied", res.ErrMsg)
	}
	// The cancellation is durable, so an operator reading the execution afterwards sees why it stopped.
	var sawError bool
	for _, p := range h.partsOfKind(db.SessionPartError) {
		if strings.Contains(string(p.Payload), "operator cancelled the run") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no error session part carried the abort reason; parts: %v", h.partsOfKind(db.SessionPartError))
	}
}

// TestBridge_AbortExecutionIsANoOpForAnUnknownExecution: claude's Aborter is a safe no-op, and the execution
// RPCs call it optimistically.
func TestBridge_AbortExecutionIsANoOpForAnUnknownExecution(t *testing.T) {
	b := New(quiet())
	for _, fn := range []func() error{
		func() error { return b.AbortExecution(context.Background(), "exe_never_existed", "why not") },
	} {
		if err := fn(); err != nil {
			t.Errorf("AbortExecution on an unknown execution returned %v, want nil (a safe no-op)", err)
		}
	}
	if b.IsExecutionActive("exe_never_existed") {
		t.Error("IsExecutionActive reported an unknown execution as live")
	}
}

// TestBridge_CompactExecutionPromptsTheSlashCommand is AC6's compaction half: `/compact` goes on the wire as an
// ORDINARY prompt, because that is how agy's own available_commands work (docs §3.10, compact.jsonl:22).
func TestBridge_CompactExecutionPromptsTheSlashCommand(t *testing.T) {
	h := newHarness(t)
	h.serveHanging()
	h.start()
	h.awaitProc(t, 1)
	h.awaitPrompts(t, 0, 1)
	proc := h.proc()

	if err := h.b.CompactExecution(context.Background(), h.manifest.ExecutionID,
		"google", harnessModel, "keep the goal and the acceptance criteria"); err != nil {
		t.Fatalf("CompactExecution: %v", err)
	}
	// Release the in-flight turn the way agy does when the model finishes, so the queued /compact is delivered.
	proc.release()
	waitFor(t, "the /compact prompt", func() bool {
		for _, p := range proc.promptsSeen() {
			if strings.TrimSpace(p.Text) == compactPrompt {
				return true
			}
		}
		return false
	})

	// The compaction is recorded, with a note that remainingScope is informational for this adapter.
	compacted := h.partsOfKind(db.SessionPartCompacted)
	if len(compacted) != 1 {
		t.Fatalf("compacted parts = %d, want 1", len(compacted))
	}
	if !strings.Contains(string(compacted[0].Payload), "antigravity_compact_command") {
		t.Errorf("compacted part = %s, want the agy /compact source tag", compacted[0].Payload)
	}
}

// TestBridge_ContinueSessionResumesAFinishedSession is AC6's follow-up half. The live half is covered by the
// injection test (ContinueSession on a live session IS SendTurn, and the same passthrough is asserted above).
func TestBridge_ContinueSessionResumesWithTheStoredSessionID(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{updateMessage("first run done")}})
	h.startAndWait()

	// The finished session's agy id came from the recorded session_info part, exactly as recovery reads it.
	infos := h.partsOfKind(db.SessionPartSessionInfo)
	if len(infos) == 0 {
		t.Fatal("no session_info part — a follow-up would have no agy session id")
	}
	stored := harnessSessionID

	// Reset the recorded results so the follow-up's OnResult is unambiguous.
	h.cbs.mu.Lock()
	h.cbs.results = nil
	h.cbs.mu.Unlock()

	sid, err := h.b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID:  h.manifest.ExecutionID,
		TenantID:     "tnt_test",
		SystemPrompt: h.manifest.SystemPrompt,
		ModelRef:     h.manifest.ModelRef,
		ProjectDir:   h.project,
		WorktreePath: h.worktree,
		ProjectID:    h.manifest.ProjectID,
		TaskID:       h.manifest.TaskID,
		WorkerID:     h.manifest.WorkerID,
		SessionID:    stored,
		Message:      "what did you just do?",
	})
	if err != nil {
		t.Fatalf("ContinueSession: %v", err)
	}
	if sid != stored {
		t.Errorf("ContinueSession returned session id %q, want the stored %q — a follow-up must resume the "+
			"SAME agy conversation", sid, stored)
	}

	// A SECOND CHILD is spawned for the follow-up (this one re-attaches), and it must use `session/resume`
	// with the stored id rather than opening a fresh conversation.
	h.awaitProc(t, 2)
	var resumed []string
	for i := 0; i < 2; i++ {
		resumed = append(resumed, h.procAt(i).resumedIDs()...)
	}
	if len(resumed) == 0 || resumed[0] != stored {
		t.Fatalf("session/resume calls = %v, want the stored id %q — opening a FRESH conversation would lose "+
			"the whole context of the original run", resumed, stored)
	}
	// And the follow-up's message became the first prompt of the resumed session.
	followUp := h.awaitPrompts(t, 1, 1)
	if got := followUp[0].Text; !strings.Contains(got, "what did you just do?") {
		t.Errorf("follow-up prompt = %q, want the follow-up message", got)
	}
}

// TestBridge_ContinueSessionWithoutALiveSessionOrIDRefuses: with neither a live session nor a stored id there
// is nothing to resume, and returning an error is what tells the RPC layer to say so.
func TestBridge_ContinueSessionWithoutASessionRefuses(t *testing.T) {
	b := New(quiet())
	_, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{
		ExecutionID: "exe_never_existed",
		Message:     "hello?",
	})
	if err == nil {
		t.Fatal("ContinueSession with no live session and no stored id succeeded")
	}
	if !strings.Contains(err.Error(), "no live agy session") {
		t.Errorf("ContinueSession error = %q, want the no-live-session refusal", err)
	}
}

// TestBridge_SendExecutionMessageWithoutALiveSessionRefuses: the injector must not silently accept a message
// nobody will deliver.
func TestBridge_SendExecutionMessageWithoutALiveSessionRefuses(t *testing.T) {
	b := New(quiet())
	err := b.SendExecutionMessage(context.Background(), "exe_never_existed", "hi")
	if err == nil {
		t.Fatal("SendExecutionMessage with no live session succeeded — the message would be dropped silently")
	}
	if !strings.Contains(err.Error(), "no live agy session") {
		t.Errorf("error = %q, want the no-live-session refusal", err)
	}
	if err := b.CompactExecution(context.Background(), "exe_never_existed", "google", "m", ""); err == nil {
		t.Fatal("CompactExecution with no live session succeeded")
	}
}

// TestBridge_IsExecutionActiveTracksTheLiveSession pins the liveness reporter the reaper reads.
func TestBridge_IsExecutionActiveTracksTheLiveSession(t *testing.T) {
	h := newHarness(t)
	h.serveHanging()
	h.start()
	waitFor(t, "the session to register", func() bool {
		return h.b.IsExecutionActive(h.manifest.ExecutionID)
	})
	if err := h.b.AbortExecution(context.Background(), h.manifest.ExecutionID, "test over"); err != nil {
		t.Fatalf("AbortExecution: %v", err)
	}
	h.awaitResult()
	waitFor(t, "the session to deregister", func() bool {
		return !h.b.IsExecutionActive(h.manifest.ExecutionID)
	})
}
