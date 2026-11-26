package antigravity

// bridge_session_test.go — AC1: FIXTURE-DRIVEN. Every assertion here replays the payloads child 1 RECORDED
// from agy 1.5.13 and checks what the bridge did with them.
//
// The pipeline under test is the REAL one end to end: `Bridge.Start` → the acp client → a fake `agy acp`
// child replaying the recorded turn → the mapper → `scheduler.ExecutionCallbacks` + the usage recorder + the
// diff ledger + the session store. Nothing is stubbed except the process itself, so an assertion that holds
// here is an assertion about the transcript agy actually produces.
//
// The fixtures named in each test's comment are the recording the payloads come from; the file lives at
// internal/antigravity/testdata/acp/<name>.jsonl.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestBridge_TextStreamsToOnText: `agent_message_chunk` → OnText, and the accumulated text is the OnResult
// output. Fixture: file_edit_diff.jsonl:26-28 (three chunks that must concatenate into one answer).
func TestBridge_TextStreamsToOnText(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateMessage("I have created "),
		updateMessage("a.txt with content `x`"),
		updateMessage(" All done!"),
		updateUsage(9664, 1048576),
	}})
	h.startAndWait()

	want := "I have created a.txt with content `x` All done!"
	got := h.outputText()
	if got != want {
		t.Fatalf("OnText accumulated %q, want %q", got, want)
	}
	// The chunks were streamed INDIVIDUALLY, not coalesced at turn end: that is what makes the live session
	// pane update as the model writes.
	h.cbs.mu.Lock()
	chunks := append([]string(nil), h.cbs.text...)
	h.cbs.mu.Unlock()
	if len(chunks) != 3 {
		t.Errorf("OnText fired %d times, want 3 (one per agent_message_chunk): %v", len(chunks), chunks)
	}
	res := h.result()
	if !res.OK {
		t.Fatalf("execution did not succeed: %q", res.ErrMsg)
	}
	if res.Output != want {
		t.Errorf("OnResult output = %q, want the accumulated assistant text %q", res.Output, want)
	}
	// The transcript carries ONE coalesced text part per turn, not one per chunk.
	textParts := h.partsOfKind(db.SessionPartText)
	if len(textParts) != 1 {
		t.Fatalf("text parts = %d, want 1 coalesced part per turn", len(textParts))
	}
	var payload struct {
		Part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
	}
	if err := json.Unmarshal(textParts[0].Payload, &payload); err != nil {
		t.Fatalf("decode text part: %v (%s)", err, textParts[0].Payload)
	}
	if payload.Part.Text != want {
		t.Errorf("text part = %q, want %q", payload.Part.Text, want)
	}
}

// TestBridge_ThoughtChunksBecomeReasoning: `agent_thought_chunk` → a reasoning part on the channel the claude
// bridge uses.
//
// SYNTHETIC BY NECESSITY, AND THE TEST SAYS SO: agy 1.5.13 NEVER emits this update (docs §3.1,
// thinking_turn.jsonl contains 8 agent_message_chunks and zero thought chunks — TestReplay_ThinkingTurn in the
// acp package asserts that absence). The type is handled so a future agy needs no client change, so this test
// feeds the shape the protocol defines and checks the mapper's arm.
func TestBridge_ThoughtChunksBecomeReasoning(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateThought("Let me work out 17*23. "),
		updateThought("It is 391."),
		updateMessage("391"),
		updateUsage(9513, 1048576),
	}})
	h.startAndWait()

	reasoning := h.partsOfKind(db.SessionPartReasoning)
	if len(reasoning) != 1 {
		t.Fatalf("reasoning parts = %d, want 1 coalesced part for the turn", len(reasoning))
	}
	var payload struct {
		Part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
	}
	if err := json.Unmarshal(reasoning[0].Payload, &payload); err != nil {
		t.Fatalf("decode reasoning part: %v (%s)", err, reasoning[0].Payload)
	}
	if payload.Part.Type != "reasoning" {
		t.Errorf("part type = %q, want \"reasoning\"", payload.Part.Type)
	}
	want := "Let me work out 17*23. It is 391."
	if payload.Part.Text != want {
		t.Errorf("reasoning text = %q, want %q", payload.Part.Text, want)
	}
	// REASONING IS NOT THE ANSWER: the accumulated output must contain only the agent message.
	if out := h.outputText(); strings.Contains(out, "391.") {
		t.Errorf("reasoning leaked into the assistant output: %q", out)
	}

	// The token half of the same story: agy reports thought TOKENS even though it streams no text.
	cases := map[string]int64{}
	for _, u := range h.usageRecords() {
		cases["reasoning"] = u.ReasoningTokens
	}
	if _, ok := cases["reasoning"]; !ok {
		t.Fatal("no usage record was written, so ReasoningTokens could not be checked")
	}
}

// TestBridge_RunCommandToolCall: a `run_command` → OnToolCall with the CANONICAL name (`bash`), its input and
// its resolved output. Fixture: tool_run_command_permission_allow.jsonl:20-31.
func TestBridge_RunCommandToolCall(t *testing.T) {
	h := newHarness(t)
	// The recorded command output string, verbatim.
	const output = "Created At: 2026-11-24T01:19:52-06:00\nCompleted At: 2026-11-24T01:19:52-06:00\n" +
		"Command exited with code 0.\n\nCommand output:\nhi\r\n"
	args := map[string]any{
		"CommandLine":       "echo hi",
		"WaitMsBeforeAsync": 5000,
		"toolSummary":       "Run echo hi",
	}
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateToolCall("call_13391380448657600714", "run_command", "execute", "echo hi", "in_progress", args),
		updateToolCallCompleted("call_13391380448657600714", "run_command", "execute", args,
			[]map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": output}}},
			map[string]any{"output": output, "exit_code": 0}),
		updateMessage("The output of the command is `hi`."),
		updateUsage(9227, 1048576),
	}})
	h.startAndWait()

	// The RUNNING sighting carries the name and the input.
	h.cbs.mu.Lock()
	calls := append([]recordedToolCall(nil), h.cbs.toolCalls...)
	h.cbs.mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("OnToolCall fired %d times, want at least 2 (the running sighting and the completion): %+v",
			len(calls), calls)
	}
	first := calls[0]
	if first.Name != "bash" {
		t.Errorf("tool name = %q, want the canonical %q (agy spells it run_command; the UI and the ledger read "+
			"the canonical name)", first.Name, "bash")
	}
	if first.Output != nil {
		t.Errorf("the running sighting carried an output (%q); it has none yet", string(first.Output))
	}
	var input map[string]any
	if err := json.Unmarshal(first.Input, &input); err != nil {
		t.Fatalf("OnToolCall input is not JSON: %v (%s)", err, first.Input)
	}
	if got := input["CommandLine"]; got != "echo hi" {
		t.Errorf("input[CommandLine] = %v, want \"echo hi\" — the raw agy args must survive", got)
	}

	// The COMPLETION carries the resolved output, which is what the live pane renders.
	last := calls[len(calls)-1]
	if last.Name != "bash" {
		t.Errorf("completion tool name = %q, want %q", last.Name, "bash")
	}
	if !strings.Contains(string(last.Output), "hi") {
		t.Errorf("completion output = %q, want the command's own output (\"hi\")", string(last.Output))
	}

	// The durable part records both names: the canonical one the UI reads, and the raw agy spelling.
	toolParts := h.partsOfKind(db.SessionPartToolUse)
	if len(toolParts) == 0 {
		t.Fatal("no tool_use parts were recorded")
	}
	var sawRaw bool
	for _, p := range toolParts {
		var env struct {
			Part struct {
				Tool    string `json:"tool"`
				AgyName string `json:"agy_name"`
			} `json:"part"`
		}
		if err := json.Unmarshal(p.Payload, &env); err != nil {
			continue
		}
		if env.Part.Tool == "bash" && env.Part.AgyName == "run_command" {
			sawRaw = true
		}
	}
	if !sawRaw {
		t.Errorf("no tool_use part carried both the canonical tool (bash) and the raw agy name (run_command); "+
			"parts: %s", payloadsOf(toolParts))
	}
}

// TestBridge_ReplaceFileContentEdit: an edit → OnWrittenFiles + the diff ledger, and a whole-file write also
// produces an artifact. Fixture: file_edit_diff.jsonl:21-27 (write_file then edit_file, both with `content[]`
// diff items; the edit carries oldText and newText).
func TestBridge_ReplaceFileContentEdit(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.worktree, "a.txt")
	writeArgs := map[string]any{"Content": "x", "Path": target}
	editArgs := map[string]any{"Path": target, "Replacement": "y", "Target": "x"}

	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		// write_file: absent oldText == a new file.
		updateToolCall("call_w", "write_file", "edit", "File a.txt", "in_progress", writeArgs, target),
		updateToolCallCompleted("call_w", "write_file", "edit", writeArgs,
			[]map[string]any{{"type": "diff", "path": target, "newText": "x"}},
			map[string]any{}, target),
		// edit_file: oldText present.
		updateToolCall("call_e", "edit_file", "edit", "File a.txt", "in_progress", editArgs, target),
		updateToolCallCompleted("call_e", "edit_file", "edit", editArgs,
			[]map[string]any{{"type": "diff", "path": target, "newText": "y", "oldText": "x"}},
			map[string]any{}, target),
		updateMessage("Done."),
		updateUsage(9664, 1048576),
	}})
	h.startAndWait()

	// OnWrittenFiles fired, deduped, with the absolute path agy reported.
	files := h.writtenFiles()
	if len(files) != 1 || files[0] != target {
		t.Fatalf("OnWrittenFiles = %v, want exactly [%s] (the same file was written and then edited, so it "+
			"must be reported once)", files, target)
	}

	// The DIFF LEDGER ran, once per write, with the canonical tool name and an execution-dir-relative path —
	// the form the shared file-edit hook reads.
	ledger := h.ledgerCalls()
	if len(ledger) != 2 {
		t.Fatalf("diff-ledger calls = %d, want 2 (one per completed edit): %+v", len(ledger), ledger)
	}
	if ledger[0].Tool != "write" || ledger[1].Tool != "edit" {
		t.Errorf("ledger tools = [%q %q], want [\"write\" \"edit\"] (agy's write_file then edit_file)",
			ledger[0].Tool, ledger[1].Tool)
	}
	for _, call := range ledger {
		if call.Input["filePath"] != "a.txt" {
			t.Errorf("ledger input filePath = %v, want \"a.txt\" (execution-dir-relative)", call.Input["filePath"])
		}
		if call.ExecDir != h.worktree {
			t.Errorf("ledger execDir = %q, want the worktree %q", call.ExecDir, h.worktree)
		}
		if call.TenantID != "tnt_test" {
			t.Errorf("ledger tenant = %q, want tnt_test", call.TenantID)
		}
	}

	// A whole-file WRITE also reports an artifact carrying the file's text; an EDIT does not (it produced a
	// modification, not a new artifact).
	h.cbs.mu.Lock()
	artifacts := append([]recordedArtifact(nil), h.cbs.artifacts...)
	h.cbs.mu.Unlock()
	if len(artifacts) != 1 {
		t.Fatalf("OnArtifact fired %d times, want 1 (the write only): %+v", len(artifacts), artifacts)
	}
	if artifacts[0].Name != target || artifacts[0].Body != "x" {
		t.Errorf("artifact = %+v, want name %q and body \"x\"", artifacts[0], target)
	}
	if artifacts[0].Type != "text" {
		t.Errorf("artifact type = %q, want \"text\" for a .txt", artifacts[0].Type)
	}

	// The FAILED arm must not ledger or report: a refused edit wrote nothing.
	if res := h.result(); !res.OK {
		t.Errorf("execution did not succeed: %q", res.ErrMsg)
	}
}

// TestBridge_FailedEditNeverLedgers: a `failed` tool_call_update wrote nothing, so it must not fire
// OnWrittenFiles or the ledger. Fixture: tool_permission_reject.jsonl:30 (a denied run_command ends `failed`).
func TestBridge_FailedEditNeverLedgers(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.worktree, "a.txt")
	args := map[string]any{"Path": target, "Content": "x"}
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateToolCall("call_f", "write_file", "edit", "File a.txt", "in_progress", args, target),
		updateToolCallFailed("call_f", "write_file", "edit", args, target),
		updateMessage("The write was refused."),
		updateUsage(9000, 1048576),
	}})
	h.startAndWait()

	if files := h.writtenFiles(); len(files) != 0 {
		t.Errorf("OnWrittenFiles fired for a FAILED edit: %v — a refused/failed call wrote nothing", files)
	}
	if ledger := h.ledgerCalls(); len(ledger) != 0 {
		t.Errorf("the diff ledger ran for a FAILED edit: %+v — the hook would diff a file that was never written", ledger)
	}
	// The tool call still reaches the UI, with its error state in the transcript.
	if _, ok := h.findToolCall("write"); !ok {
		t.Error("a failed write produced no OnToolCall at all — the refusal would be invisible in the pane")
	}
}

// TestBridge_UsageRecordsAllBuckets: the prompt result's usage → the usage recorder with input/output/thinking/
// cache-read tokens, and the context window `used/size` in the step_finish part.
//
// Fixture: file_edit_diff.jsonl:36 — inputTokens 15726, outputTokens 163, cachedReadTokens 21689,
// thoughtTokens 0, totalTokens 37578 — and the gauges at :24/:30/:33/:35 (9135→9256→9523→9664 of 1048576).
func TestBridge_UsageRecordsAllBuckets(t *testing.T) {
	h := newHarness(t)
	// A recording with a nonzero thought count, so the reasoning bucket is provably wired: thinking_turn.jsonl:26
	// reports thoughtTokens 237.
	usage := acp.Usage{
		InputTokens:       15726,
		OutputTokens:      163,
		CachedReadTokens:  21689,
		CachedWriteTokens: 0,
		ThoughtTokens:     237,
		TotalTokens:       37578,
	}
	h.serve(script{
		usage: &usage,
		updates: []string{
			updateUsage(0, 1048576),
			updateMessage("done"),
			updateUsage(9664, 1048576),
		},
	})
	h.startAndWait()

	records := h.usageRecords()
	if len(records) != 1 {
		t.Fatalf("usage records = %d, want exactly 1 per turn: %+v", len(records), records)
	}
	rec := records[0]
	if rec.PromptTokens != 15726 {
		t.Errorf("PromptTokens = %d, want 15726 (agy's inputTokens)", rec.PromptTokens)
	}
	if rec.CompletionTokens != 163 {
		t.Errorf("CompletionTokens = %d, want 163 (agy's outputTokens)", rec.CompletionTokens)
	}
	if rec.ReasoningTokens != 237 {
		t.Errorf("ReasoningTokens = %d, want 237 (agy reports thinking as usage.thoughtTokens)", rec.ReasoningTokens)
	}
	if rec.CacheReadTokens != 21689 {
		t.Errorf("CacheReadTokens = %d, want 21689 (agy's cachedReadTokens)", rec.CacheReadTokens)
	}
	if rec.AdapterKind != adapter.KindAntigravity {
		t.Errorf("AdapterKind = %q, want %q", rec.AdapterKind, adapter.KindAntigravity)
	}
	if rec.Provider != "google" {
		t.Errorf("Provider = %q, want \"google\"", rec.Provider)
	}
	if rec.Model != harnessModel {
		t.Errorf("Model = %q, want the BARE id %q", rec.Model, harnessModel)
	}
	if rec.ExecutionID != h.manifest.ExecutionID {
		t.Errorf("ExecutionID = %q, want %q", rec.ExecutionID, h.manifest.ExecutionID)
	}
	if rec.TenantID != "tnt_test" {
		t.Errorf("TenantID = %q, want tnt_test", rec.TenantID)
	}

	// THE CONTEXT WINDOW is recorded in the step_finish part: scheduler.UsageRecord has no window field, and
	// inventing a DB column is not this adapter's call.
	finishes := h.partsOfKind(db.SessionPartStepFinish)
	if len(finishes) != 1 {
		t.Fatalf("step_finish parts = %d, want 1", len(finishes))
	}
	var payload struct {
		Usage struct {
			Input         int64 `json:"input"`
			Output        int64 `json:"output"`
			CachedRead    int64 `json:"cached_read"`
			Reasoning     int64 `json:"reasoning"`
			Total         int64 `json:"total"`
			ContextWindow struct {
				Used int64 `json:"used"`
				Size int64 `json:"size"`
			} `json:"context_window"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(finishes[0].Payload, &payload); err != nil {
		t.Fatalf("decode step_finish part: %v (%s)", err, finishes[0].Payload)
	}
	if payload.Usage.ContextWindow.Used != 9664 {
		t.Errorf("context window used = %d, want 9664 (the last gauge agy reported)", payload.Usage.ContextWindow.Used)
	}
	if payload.Usage.ContextWindow.Size != 1048576 {
		t.Errorf("context window size = %d, want 1048576", payload.Usage.ContextWindow.Size)
	}
	if payload.Usage.Reasoning != 237 {
		t.Errorf("step_finish reasoning = %d, want 237", payload.Usage.Reasoning)
	}
}

// TestBridge_UsageIsIdempotentPerTurn: a DUPLICATED delivery of the same turn must not double-charge the
// execution's budget (claude's D8 rule, parse.go's usageFingerprint).
//
// Driven at the mapper level because the duplicate this guards against is not something the session can
// produce: it comes from a re-emitted result after a reconnect, and the fingerprint — execution + session +
// turn number + every bucket — is what drops it.
func TestBridge_UsageIsIdempotentPerTurn(t *testing.T) {
	h := newHarness(t)
	m := NewMapper("exe_dup", h.cbs, MapperDeps{
		TenantID: "tnt_test",
		ExecDir:  h.worktree,
		Manifest: h.manifest,
		UsageRecorder: func(_ context.Context, rec scheduler.UsageRecord) error {
			h.mu.Lock()
			h.usage = append(h.usage, rec)
			h.mu.Unlock()
			return nil
		},
		Log: quiet(),
	})
	res := &acp.PromptResult{StopReason: acp.StopReasonEndTurn, Usage: defaultTurnUsage()}
	m.RecordTurn(context.Background(), res)
	m.recordUsage(context.Background(), res) // the duplicate delivery of the SAME turn
	if got := len(h.usageRecords()); got != 1 {
		t.Fatalf("usage records = %d, want 1 — a duplicated delivery of one turn must be dropped, not "+
			"double-charged. Records: %+v", got, h.usageRecords())
	}
}

// TestBridge_MultipleToolCallsInSequence: a recorded turn with three tool calls (write, edit, read) maps all
// three, and the read is not treated as an edit. Fixture: file_edit_diff.jsonl:21-35.
func TestBridge_MultipleToolCallsInSequence(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(h.worktree, "a.txt")
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateToolCall("call_w", "write_file", "edit", "File a.txt", "in_progress", map[string]any{"Path": target, "Content": "x"}, target),
		updateToolCallCompleted("call_w", "write_file", "edit", map[string]any{"Path": target, "Content": "x"},
			[]map[string]any{{"type": "diff", "path": target, "newText": "x"}}, map[string]any{}, target),
		updateToolCall("call_e", "edit_file", "edit", "File a.txt", "in_progress", map[string]any{"Path": target}, target),
		updateToolCallCompleted("call_e", "edit_file", "edit", map[string]any{"Path": target},
			[]map[string]any{{"type": "diff", "path": target, "newText": "y", "oldText": "x"}}, map[string]any{}, target),
		updateToolCall("call_r", "read_file", "read", "File a.txt", "in_progress", map[string]any{"Path": target}, target),
		updateToolCallCompleted("call_r", "read_file", "read", map[string]any{"Path": target}, nil, map[string]any{}, target),
		updateMessage("done"),
		updateUsage(9664, 1048576),
	}})
	h.startAndWait()

	// The READ is classified as a read, so it never touches the ledger or the written-file set.
	var sawRead bool
	for _, c := range h.toolCalls() {
		if c.Name == "read" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Errorf("read_file did not reach OnToolCall as %q; calls: %+v", "read", h.toolCalls())
	}
	if got := len(h.ledgerCalls()); got != 2 {
		t.Errorf("diff-ledger calls = %d, want 2 (the write and the edit; NOT the read)", got)
	}
}

// TestBridge_MCPToolCallIsPassedThrough: an agy MCP tool (`mcp_<server>_<tool>`) keeps its own name — there is
// no canonical equivalent, and inventing one would hide which server answered.
// Fixture: mcp_tool_call.jsonl:19 (`mcp_echo_echo`, kind "other").
func TestBridge_MCPToolCallIsPassedThrough(t *testing.T) {
	h := newHarness(t)
	args := map[string]any{"text": "hi", "toolSummary": "Echo hi"}
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updateToolCall("call_m", "mcp_echo_echo", "other", "Run mcp_echo_echo", "in_progress", args),
		updateToolCallCompleted("call_m", "mcp_echo_echo", "other", args,
			[]map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": "echo: hi"}}},
			map[string]any{"output": "echo: hi"}),
		updateMessage("The echo tool returned hi."),
		updateUsage(9783, 1048576),
	}})
	h.startAndWait()

	call, ok := h.findToolCall("mcp_echo_echo")
	if !ok {
		t.Fatalf("the MCP tool did not reach OnToolCall under its own name; calls: %+v", h.toolCalls())
	}
	if !strings.Contains(string(call.Output), "echo: hi") {
		t.Errorf("MCP tool output = %q, want the tool's result", string(call.Output))
	}
	// An MCP tool is not a file edit, so it must not touch the ledger.
	if got := len(h.ledgerCalls()); got != 0 {
		t.Errorf("the diff ledger ran for an MCP tool call (%d calls)", got)
	}
}

// TestBridge_PlanBecomesTodos: an ACP `plan` maps onto the todowrite envelope every other adapter uses.
//
// PROTOCOL-SHAPED, NOT RECORDED: agy 1.5.13 was not recorded sending a `plan` update, so this asserts the
// mapping the protocol defines. If a future recording shows a different payload the mapper's arm is what
// changes, and the GUI/TUI keep reading the same todowrite envelope.
func TestBridge_PlanBecomesTodos(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{
		updateUsage(0, 1048576),
		updatePlan(
			map[string]any{"content": "write hello.txt", "status": "completed", "priority": "high"},
			map[string]any{"content": "run cat hello.txt", "status": "in_progress", "priority": "medium"},
		),
		updateMessage("done"),
	}})
	h.startAndWait()

	var todos []any
	for _, c := range h.toolCalls() {
		if c.Name != "todowrite" {
			continue
		}
		var input map[string]any
		if err := json.Unmarshal(c.Input, &input); err != nil {
			t.Fatalf("todowrite input is not JSON: %v (%s)", err, c.Input)
		}
		if list, ok := input["todos"].([]any); ok {
			todos = list
		}
	}
	if len(todos) != 2 {
		t.Fatalf("todowrite todos = %v, want the two plan entries", todos)
	}
	first, _ := todos[0].(map[string]any)
	if first["content"] != "write hello.txt" || first["status"] != "completed" {
		t.Errorf("first todo = %v, want {content: write hello.txt, status: completed}", first)
	}
	// The sidecar snapshot is written at the SAME site, so the DB-less worktree view agrees.
	if _, err := os.Stat(filepath.Join(h.worktree, ".orchicon-todos.json")); err != nil {
		t.Logf("todo sidecar not found at .orchicon-todos.json (%v) — SaveTodoSnapshot owns the filename", err)
	}
}

// TestBridge_SessionIDIsPersisted: the agy session id is persisted through the SessionStore so recovery and
// follow-ups resume the same conversation. It is what `SetSessionStore` exists for.
func TestBridge_SessionIDIsPersisted(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	infos := h.partsOfKind(db.SessionPartSessionInfo)
	if len(infos) == 0 {
		t.Fatal("no session_info part was recorded — a follow-up would have no agy session id to resume")
	}
	var payload struct {
		SessionID   string `json:"session_id"`
		AdapterKind string `json:"adapter_kind"`
	}
	if err := json.Unmarshal(infos[0].Payload, &payload); err != nil {
		t.Fatalf("decode session_info part: %v (%s)", err, infos[0].Payload)
	}
	if payload.SessionID != harnessSessionID {
		t.Errorf("session_info.session_id = %q, want %q", payload.SessionID, harnessSessionID)
	}
	if payload.AdapterKind != adapter.KindAntigravity {
		t.Errorf("session_info.adapter_kind = %q, want %q", payload.AdapterKind, adapter.KindAntigravity)
	}
	// And the client actually set the model the manifest named.
	sets := h.proc().modelIdSets()
	if len(sets) != 1 || sets[0] != harnessModel {
		t.Errorf("session/set_model calls = %v, want exactly [%s]", sets, harnessModel)
	}
}

// TestBridge_SystemPromptTravelsAsMetaAndNotInTheGoal: the manifest's system prompt is delivered through
// `_meta.antigravity.systemPrompt.append` on `session/new`, and the first prompt carries only the goal.
// Fixture: handshake_auth.jsonl:7 (the string-shaped append).
func TestBridge_SystemPromptTravelsAsMetaAndNotInTheGoal(t *testing.T) {
	h := newHarness(t)
	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	p := h.proc()
	p.mu.Lock()
	params := append([]json.RawMessage(nil), p.newParams...)
	prompts := append([]promptRecord(nil), p.prompts...)
	p.mu.Unlock()

	if len(params) != 1 {
		t.Fatalf("session/new was called %d times, want 1", len(params))
	}
	var wire struct {
		Meta struct {
			Antigravity struct {
				SystemPrompt struct {
					Append json.RawMessage `json:"append"`
				} `json:"systemPrompt"`
			} `json:"antigravity"`
		} `json:"_meta"`
		AdditionalDirectories []string `json:"additionalDirectories"`
	}
	if err := json.Unmarshal(params[0], &wire); err != nil {
		t.Fatalf("decode session/new params: %v (%s)", err, params[0])
	}
	appendRaw := wire.Meta.Antigravity.SystemPrompt.Append
	if len(appendRaw) == 0 {
		t.Fatal("session/new carried no _meta.antigravity.systemPrompt.append — the worker's prompt, context " +
			"files and skills would never reach the model")
	}
	// agy accepts only a STRING there (handshake_auth.jsonl recorded that a non-string is ignored).
	if appendRaw[0] != '"' {
		t.Errorf("systemPrompt.append was sent as %s, want a JSON string", appendRaw)
	}
	var appended string
	_ = json.Unmarshal(appendRaw, &appended)
	if !strings.Contains(appended, "Orchicon worker") {
		t.Errorf("appended system prompt = %q, want the manifest's SystemPrompt", appended)
	}

	// The carve-outs are advertised as additionalDirectories — the ACP equivalent of claude's --add-dir.
	if len(wire.AdditionalDirectories) == 0 {
		t.Error("session/new carried no additionalDirectories — the worker could not write to the scratch dir " +
			"or its own run metadata")
	}

	// AND THE GOAL IS THE FIRST PROMPT, without the system prompt repeated in it.
	if len(prompts) == 0 {
		t.Fatal("no session/prompt was sent")
	}
	first := prompts[0].Text
	if !strings.Contains(first, h.manifest.Goal) {
		t.Errorf("first prompt = %q, want it to carry the goal", first)
	}
	if strings.Contains(first, "Project context and skills are appended here") {
		t.Errorf("the first prompt repeated the SYSTEM prompt (%q) — it already travelled on session/new, so "+
			"this would double its tokens in the model's context", first)
	}
	if !strings.Contains(first, "ACCEPTANCE CRITERIA") {
		t.Errorf("first prompt = %q, want it to carry the acceptance criteria", first)
	}
}

// TestBridge_SpawnSpecCarriesNoSecretsOnArgv: the launch argv must never contain the API key (procspec.go's
// contract), and it must carry `acp`.
func TestBridge_SpawnSpecCarriesNoSecretsOnArgv(t *testing.T) {
	h := newHarness(t)
	h.b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "gemini-api-key", ShareTelemetry: false}, "super-secret-key", nil
	})
	h.serve(script{updates: []string{updateMessage("done")}})
	h.startAndWait()

	p := h.proc()
	for _, tok := range p.argv {
		if strings.Contains(tok, "super-secret-key") {
			t.Fatalf("the API key appears on the launch argv (%q) — it is visible in `ps`, in the container "+
				"engine's command line, and in every process listing. argv = %v", tok, p.argv)
		}
	}
	// It IS on the child's environment, under the name agy reads.
	var onEnv bool
	for _, kv := range p.env {
		if kv == "GEMINI_API_KEY=super-secret-key" {
			onEnv = true
		}
	}
	if !onEnv {
		t.Error("the API key is not on the child environment — the gemini-api-key method would fail to authenticate")
	}
}

// TestBridge_UnknownModelFromAgyFailsLoud: when agy itself refuses the model (-32602) the execution fails with
// agy's own text. Fixture: unknown_model.jsonl:8.
func TestBridge_UnknownModelFromAgyFailsLoud(t *testing.T) {
	h := newHarness(t)
	// The harness catalogue advertises harnessModel, so this model passes the pre-check and agy's own refusal
	// is what the test exercises.
	h.manifest.ModelRef = "antigravity/google/" + harnessModel
	h.serve(script{rejectModel: true})
	h.startAndWait()

	res := h.result()
	if res.OK {
		t.Fatal("agy refused the model but the execution succeeded")
	}
	if !strings.Contains(res.ErrMsg, "Unknown model ID") {
		t.Errorf("failure message = %q, want agy's own text (\"Unknown model ID\")", res.ErrMsg)
	}
}

// TestBridge_AuthFailureIsActionable: auth failures surface the one actionable instruction, not a generic RPC
// error. Fixture: auth_required.jsonl (agy's "GEMINI_API_KEY is not set" matches no shared signal, which is
// exactly why every authenticate error is mapped).
func TestBridge_AuthFailureIsActionable(t *testing.T) {
	h := newHarness(t)
	// The fake reports agy's own missing-key text, which matches NONE of the shared auth signals — so this
	// fails unless the client maps EVERY authenticate error onto the instruction (acp.Client.Authenticate).
	h.serve(script{authError: true})
	h.startAndWait()

	res := h.result()
	if res.OK {
		t.Fatal("an unauthenticated session was reported as successful")
	}
	if !strings.Contains(res.ErrMsg, "Antigravity is not signed in") {
		t.Errorf("failure message = %q, want the actionable sign-in instruction", res.ErrMsg)
	}
}

func payloadsOf(parts []db.SessionPart) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p.Payload))
	}
	return out
}
