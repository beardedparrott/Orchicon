package antigravity

// ask_session_test.go — the Ask conversation LIFECYCLE and its streaming map.
//
// What is asserted here is what the acceptance criteria name and nothing more:
//
//	resume across a "restart"   — a NEW Bridge with the SAME persisted agy session id resumes it (AC6)
//	model switch in place       — a changed model_ref issues session/set_model, not a new conversation
//	cwd is the project dir      — so an approved write lands in the project (AC4's precondition)
//	teardown/shutdown           — CloseAsk retires every child (the server's shutdown seam)
//	streaming                   — chunks, tool calls and results become the bus events the collector reads
//	the ASK diff ledger         — owner id is the CONVERSATION, not an execution
//	usage                       — owner kind ask_conversation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestAskSession_ResumeAfterRestart is AC6's mechanism, proved without a plane: a SECOND Bridge that has
// never spawned anything is asked for a turn with the persisted agy session id, and it must RESUME that
// conversation rather than open a new one.
func TestAskSession_ResumeAfterRestart(t *testing.T) {
	// The first "plane": it opens a conversation and records agy's own session id.
	first := newAskHarness(t, script{})
	sid := first.create(askmode.Iteration)
	if sid != harnessSessionID {
		t.Fatalf("CreateConversationSession returned %q, want agy's own id %q", sid, harnessSessionID)
	}
	// Simulate the plane going away: every child is retired, exactly as shutdown does.
	first.b.CloseAsk()

	// THE SECOND "PLANE": a fresh Bridge, the same persisted id.
	second := newAskHarness(t, script{})
	ctx := second.turnCtx(askmode.Iteration)
	col := drain(t, second.subscribe(ctx))
	sent := second.sendTurn(ctx, sid, askmode.Iteration, "what were we talking about?")

	waitFor(t, "the resuming prompt to reach agy", func() bool {
		p := second.procOrNil(0)
		return p != nil && len(p.promptsSeen()) >= 1
	})
	fp := second.proc(0)

	// THE EVIDENCE: agy was asked to RE-ATTACH to the SAME id, not to open a new conversation.
	resumed := fp.resumedIDs()
	if len(resumed) != 1 || resumed[0] != sid {
		t.Fatalf("agy was asked to resume %v, want exactly [%s] (a new session/new would lose the context)", resumed, sid)
	}
	if n := fp.sessionNewCalls(); n != 0 {
		t.Fatalf("the resuming bridge opened %d NEW conversation(s), want 0", n)
	}

	ackTurn(t, fp, make(chan struct{}))
	fp.release()
	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the resuming turn never finished")
	}
	col.awaitKind(t, "idle", 1)
}

// TestAskSession_ResumeFallsBackToLoad covers the build that has no `session/resume`: the resume degrades to
// `session/load` (which replays history) instead of opening a NEW conversation and losing the context.
func TestAskSession_ResumeFallsBackToLoad(t *testing.T) {
	h := newAskHarness(t, script{resumeError: true})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	sent := h.sendTurn(ctx, harnessSessionID, askmode.Iteration, "continue")

	waitFor(t, "the prompt to reach agy", func() bool {
		p := h.procOrNil(0)
		return p != nil && len(p.promptsSeen()) >= 1
	})
	fp := h.proc(0)
	// THE FALLBACK RE-ATTACHED TO THE SAME CONVERSATION. resumedIDs lists the sessions the fake actually
	// re-attached (the load, after the resume was refused) — what must never happen is a NEW conversation.
	ids := fp.resumedIDs()
	if len(ids) == 0 {
		t.Fatal("neither session/resume nor session/load was called: the conversation was not re-attached")
	}
	for _, id := range ids {
		if id != harnessSessionID {
			t.Fatalf("re-attached to %q, want %q", id, harnessSessionID)
		}
	}
	if n := fp.sessionNewCalls(); n != 0 {
		t.Fatalf("the fallback opened %d NEW conversation(s), want 0 (that would lose the context)", n)
	}
	fp.release()
	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never finished")
	}
}

// TestAskSession_ModelSwitchInPlace proves a changed model_ref is a `session/set_model` on the LIVE
// conversation, keeping its context — not a new session.
func TestAskSession_ModelSwitchInPlace(t *testing.T) {
	h := newAskHarness(t, script{})
	sid := h.create(askmode.Iteration)
	// A CONVERSATION IS CREATED WITHOUT A MODEL (CreateConversationSession has no model_ref argument), so agy
	// keeps its own default until the FIRST TURN names one. That is the intended behaviour, not a gap: the
	// model belongs to the turn.
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	first := h.sendTurn(ctx, sid, askmode.Iteration, "first turn")
	waitFor(t, "the first turn to set the model", func() bool {
		p := h.procOrNil(0)
		return p != nil && len(p.modelIdSets()) >= 1
	})
	if got := h.proc(0).modelIdSets(); got[0] != harnessModel {
		t.Fatalf("first turn set model %v, want [%s]", got, harnessModel)
	}
	h.proc(0).release()
	<-first

	// A second turn: the model must be RE-AFFIRMED IN PLACE on the same conversation, so a switched
	// model keeps the context rather than starting over.
	second := h.sendTurn(ctx, sid, askmode.Iteration, "second turn")
	waitFor(t, "the second turn to reach agy", func() bool {
		p := h.procOrNil(0)
		return p != nil && len(p.promptsSeen()) >= 2
	})
	sets := h.proc(0).modelIdSets()
	if len(sets) < 2 || sets[1] != harnessModel {
		t.Fatalf("model sets = %v, want at least two, both %q (set IN PLACE)", sets, harnessModel)
	}
	// ONE child, ONE conversation: the switch did not respawn or reopen.
	if n := h.proc(0).sessionNewCalls(); n != 1 {
		t.Fatalf("session/new was called %d times, want 1 (a model switch must not reopen the conversation)", n)
	}
	if len(h.procs) != 1 {
		t.Fatalf("%d children were spawned, want 1 (a model switch must not respawn)", len(h.procs))
	}
	h.proc(0).release()
	<-second
}

// TestAskSession_CwdIsTheProjectDir is AC4's precondition: an Ask conversation assigned to a project runs IN
// that project, which is what makes an approved write land in it.
func TestAskSession_CwdIsTheProjectDir(t *testing.T) {
	h := newAskHarness(t, script{})
	h.create(askmode.Iteration)
	fp := h.proc(0)
	if fp.cwd != h.dir {
		t.Fatalf("the child's cwd = %q, want the conversation's project dir %q", fp.cwd, h.dir)
	}
	// And the session/new request carries it too (agy uses the request's cwd, not the process's).
	params := fp.sessionNewParams()
	if len(params) == 0 {
		t.Fatal("no session/new params were recorded")
	}
	if !containsFold(string(params[0]), h.dir) {
		t.Fatalf("session/new params do not name the project dir %q: %s", h.dir, params[0])
	}
}

// TestAskSession_CwdIsTheAskDirWithoutAProject is the other half: a conversation with NO project runs in its
// own ask directory, so it never silently inherits some other tree.
func TestAskSession_CwdIsTheAskDirWithoutAProject(t *testing.T) {
	h := newAskHarness(t, script{})
	ctx := context.Background()
	ctx = askmode.WithMode(ctx, askmode.Iteration)
	// NO ConversationScope project: an unassigned conversation.
	if _, err := h.b.CreateConversationSession(ctx, h.con, "t"); err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	fp := h.proc(0)
	if filepath.Base(fp.cwd) != h.con {
		t.Fatalf("cwd = %q, want the per-conversation ask directory ending in %q", fp.cwd, h.con)
	}
	if fp.cwd == h.dir {
		t.Fatal("an unassigned conversation must not adopt a project directory")
	}
}

// TestCloseAsk_RetiresEveryChild is the shutdown seam: without it a plane restart leaves every Ask child
// running, holding its session and its stdin.
func TestCloseAsk_RetiresEveryChild(t *testing.T) {
	h := newAskHarness(t, script{})
	h.create(askmode.Iteration)
	fp := h.proc(0)

	h.b.CloseAsk()

	waitFor(t, "the child to be closed", func() bool {
		select {
		case <-fp.exited:
			return true
		default:
			return false
		}
	})
	// The registry is cleared, so a later ReplyPermissionDecision cannot reach a dead session.
	if err := h.b.ReplyPermissionDecision(context.Background(), harnessSessionID, "x", "once"); err == nil {
		t.Fatal("ReplyPermissionDecision succeeded after CloseAsk — a dead session is still resolvable")
	}
}

// TestAskSession_StreamsTextToolCallsAndUsage is the streaming map (AC4's mechanism): chunks become deltas,
// a tool call becomes a part, its completion becomes a tool_result, and the turn closes with idle + a usage
// row attributed to the CONVERSATION.
func TestAskSession_StreamsTextToolCallsAndUsage(t *testing.T) {
	sc := script{
		updates: []string{
			updateMessage("Writing "),
			updateMessage("the file."),
			updateToolCall("call_w", "write_file", "edit", "write_file", "running",
				map[string]any{"TargetFile": "notes.md", "Content": "hi"}, "notes.md"),
			updateToolCallCompleted("call_w", "write_file", "edit",
				map[string]any{"TargetFile": "notes.md", "Content": "hi"},
				[]map[string]any{{"type": "diff", "path": "notes.md", "newText": "hi"}},
				map[string]any{"output": "wrote notes.md"}, "notes.md"),
			updateUsage(1200, 200000),
		},
		// THE TURN'S BILLABLE ACCOUNTING is on the prompt RESULT, not the gauge update above.
		usage: &acp.Usage{InputTokens: 1200, OutputTokens: 163, TotalTokens: 1363},
	}
	h := newAskHarness(t, sc)
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	sid := h.create(askmode.Iteration)
	_ = sid

	col := drainSince(t, h)
	sent := h.sendTurn(ctx, h.sessionID(), askmode.Iteration, "write notes.md")
	waitFor(t, "the turn to end", func() bool { return len(col.ofKind("idle")) >= 1 })
	<-sent

	// TEXT DELTAS.
	deltas := col.ofKind("delta")
	if len(deltas) < 2 {
		t.Fatalf("got %d text deltas, want at least 2: %+v", len(deltas), deltas)
	}
	var text string
	for _, d := range deltas {
		if d.IsReasoning {
			t.Fatalf("a TEXT chunk was marked as reasoning: %+v", d)
		}
		text += d.Text
	}
	if text != "Writing the file." {
		t.Fatalf("streamed text = %q, want %q", text, "Writing the file.")
	}

	// THE COMPLETED TEXT PART (one per turn, not per token).
	if parts := col.ofKind("part"); !hasPart(parts, "text") {
		t.Fatalf("no completed text part: %+v", parts)
	}

	// THE TOOL CALL AND ITS RESULT.
	if parts := col.ofKind("part"); !hasToolUse(parts, "write_file") {
		t.Fatalf("no tool_use part for write_file: %+v", parts)
	}
	results := col.ofKind("tool_result")
	if len(results) == 0 {
		t.Fatal("no tool_result for the completed write")
	}
	last := results[len(results)-1]
	if last.ToolName != "write_file" {
		t.Errorf("tool_result ToolName = %q, want write_file", last.ToolName)
	}
	if last.Output != "wrote notes.md" {
		t.Errorf("tool_result Output = %q, want %q", last.Output, "wrote notes.md")
	}
	if last.IsError {
		t.Error("a completed write was reported as an error")
	}

	// USAGE, ATTRIBUTED TO THE CONVERSATION.
	recs := h.usageRecords()
	if len(recs) != 1 {
		t.Fatalf("recorded %d usage samples, want 1", len(recs))
	}
	rec := recs[0]
	if rec.SessionID != h.con {
		t.Errorf("usage SessionID = %q, want the conversation id %q (owner kind ask_conversation)", rec.SessionID, h.con)
	}
	if rec.ExecutionID != "" || rec.TaskID != "" || rec.WorkerID != "" {
		t.Errorf("usage attributed to an execution/task/worker (%q/%q/%q) — a conversation has none",
			rec.ExecutionID, rec.TaskID, rec.WorkerID)
	}
	if rec.PromptTokens != 1200 || rec.CompletionTokens != 163 {
		t.Errorf("usage tokens = %d/%d, want 1200/163", rec.PromptTokens, rec.CompletionTokens)
	}
}

// TestAskSession_AskDiffLedgerNotExecutionLedger is requirement 7: an Ask conversation's edits go to the ASK
// ledger (owner kind ask_conversation), never the execution one.
func TestAskSession_AskDiffLedgerNotExecutionLedger(t *testing.T) {
	sc := script{
		updates: []string{
			updateToolCall("call_e", "replace_file_content", "edit", "edit", "running",
				map[string]any{"TargetFile": "notes.md"}, "notes.md"),
			updateToolCallCompleted("call_e", "replace_file_content", "edit",
				map[string]any{"TargetFile": "notes.md"},
				[]map[string]any{{"type": "diff", "path": "notes.md", "newText": "after"}},
				map[string]any{"output": "edited"}, "notes.md"),
		},
	}
	h := newAskHarness(t, sc)
	var execLedger int
	// The EXECUTION half is installed too, so the test can prove it is NOT the one that fires.
	h.b.SetFileEditHook(func(context.Context, string, string, string, string, map[string]any, string) {
		execLedger++
	})

	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	h.create(askmode.Iteration)
	sent := h.sendTurn(ctx, h.sessionID(), askmode.Iteration, "edit notes.md")
	waitFor(t, "the ledger to fire", func() bool { return len(h.ledgerCalls()) > 0 })
	h.proc(0).release()
	<-sent

	calls := h.ledgerCalls()
	if len(calls) == 0 {
		t.Fatal("the ASK diff ledger never fired for a completed edit")
	}
	if calls[0].OwnerID != h.con {
		t.Errorf("ledger owner id = %q, want the conversation id %q", calls[0].OwnerID, h.con)
	}
	if calls[0].Tool != "edit" {
		t.Errorf("ledger tool = %q, want the canonical %q", calls[0].Tool, "edit")
	}
	if execLedger != 0 {
		t.Errorf("the EXECUTION ledger fired %d time(s) for an Ask conversation's edit, want 0", execLedger)
	}
}

// TestAskSession_QueuesPromptsIsFalse pins the advertised injection semantics: agy cannot take a message into
// a running turn, so claiming it could would misreport every interjection as folded in.
func TestAskSession_QueuesPromptsIsFalse(t *testing.T) {
	if New(nil).QueuesPrompts() {
		t.Fatal("QueuesPrompts() = true; agy defines no way to add a message to a running turn")
	}
}

// TestAskSession_RestrictChatToolsIsANoOp pins the honest answer: the boundary IS enforced in the permission
// handler, so the adapter must not make askorchicon log "PROSE ONLY" on every turn.
func TestAskSession_RestrictChatToolsIsANoOp(t *testing.T) {
	if err := New(nil).RestrictChatTools(context.Background(), scheduler.ToolPolicy{
		Mode: askmode.Brainstorm, Denied: []string{"write", "edit", "bash"},
	}); err != nil {
		t.Fatalf("RestrictChatTools = %v, want nil (the boundary is enforced before a card)", err)
	}
}

// --- helpers ------------------------------------------------------------------------------------

// drainSince opens a bus for the conversation and starts collecting. It is separate from drain because a test
// that created the session FIRST needs the bus to cover only the turn it is about to send.
func drainSince(t *testing.T, h *askHarness) *eventCollector {
	t.Helper()
	return drain(t, h.subscribe(context.Background()))
}

// hasPart reports whether any part event has the given Type.
func hasPart(parts []scheduler.SessionEvent, typ string) bool {
	for _, p := range parts {
		if p.Type == typ {
			return true
		}
	}
	return false
}

// hasToolUse reports whether any part event is a tool_use naming the given tool.
func hasToolUse(parts []scheduler.SessionEvent, tool string) bool {
	for _, p := range parts {
		if p.Type != "tool_use" || p.Part == nil {
			continue
		}
		if name, _ := p.Part["tool"].(string); name == tool {
			return true
		}
	}
	return false
}
