package orchicon

// QA acceptance-criterion tests for the session engine agent loop
// (ADR-0007). All tests run against the scripted mock provider — no live
// API. Several tests intentionally FAIL, demonstrating defects found in
// review (the assertion comments name the bug).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func qaSession(t *testing.T, prov Provider, tools ToolRegistry) *Session {
	t.Helper()
	dir := t.TempDir()
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_test"),
		Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
		ProjectDir: dir,
		Provider:   prov,
		Tools:      tools,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return s
}

// noToolsProvider honestly reports no tool capability. Guards the
// no-tools-wire defense: the loop must refuse to send a tool-less request
// (a tool-trained model would improvise its native token-format tool calls
// as plain text — the silent-instant-finish bug class).
type noToolsProvider struct {
	turns []scriptedTurn
	sent  []TurnRequest
}

func (p *noToolsProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	p.sent = append(p.sent, req)
	return &mockStream{events: p.turns[0].events, finish: p.turns[0].finish}, nil
}
func (p *noToolsProvider) ListModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *noToolsProvider) Capabilities() Capabilities                          { return Capabilities{Streaming: true} }

// AC: a provider without tool capability fails the execution FAST with an
// actionable message — never a silently tool-less wire request.
func TestQANoToolsCapabilityFailsFast(t *testing.T) {
	prov := &noToolsProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "I would call a tool now…"}}, finish: StopStop},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Fatalf("OnResult = %+v, want exactly one failed result", results)
	}
	for _, frag := range []string{"no tool-call capability", "tool-driven", "Settings → Adapters → Providers"} {
		if !strings.Contains(results[0].errMsg, frag) {
			t.Errorf("OnResult error missing %q: %q", frag, results[0].errMsg)
		}
	}
	// The provider must never have been called — the refusal is pre-stream.
	if len(prov.sent) != 0 {
		t.Errorf("StreamTurn called %d times, want 0 (refusal must be pre-stream)", len(prov.sent))
	}
	// Transcript is marked failed (resumable, replayable).
	evs, err := Load(s.TranscriptPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	failed := false
	for _, e := range evs {
		if e.Type == TransState {
			var d struct {
				State string `json:"state"`
			}
			_ = jsonUnmarshal(e.Data, &d)
			if d.State == "failed" {
				failed = true
			}
		}
	}
	if !failed {
		t.Error("transcript not marked failed after no-tools refusal")
	}
}

// AC1: text-only run → OnStarted / OnText (chunked) / OnResult(success)
// with full accumulated output.
func TestQATextOnlyCallbackParity(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Hello, "}, TextDelta{Text: "world!"}}, finish: StopStop, usage: Usage{InputTokens: 100, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	started, text, _, _, stalls, _, results := cb.snapshot()
	if started != 1 {
		t.Errorf("OnStarted = %d, want 1", started)
	}
	if joined := strings.Join(text, ""); !strings.Contains(joined, "Hello, world!") {
		t.Errorf("OnText = %q, want %q", joined, "Hello, world!")
	}
	if len(stalls) != 0 {
		t.Errorf("stalls = %v, want none", stalls)
	}
	if len(results) != 1 || !results[0].succeeded || !strings.Contains(results[0].output, "Hello, world!") {
		t.Errorf("OnResult = %+v", results)
	}
	evs, err := Load(s.TranscriptPath())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) == 0 || evs[0].Type != TransSession {
		t.Errorf("first event = %+v, want session header", evs[0])
	}
}

// AC1: tool round → OnToolCall + tool result back in history + loop
// continues. Verifies BUG-1 fix (assistant tool_use precedes tool results
// in history).
func TestQAToolRoundHistory(t *testing.T) {
	tools := newMockTools()
	tools.results["read"] = "file contents"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{
			ToolCallStart{Index: 0, ToolCallID: "tc1", Name: "read"},
			ToolCallDelta{Index: 0, ArgsJSONDelta: `{"path":"`},
			ToolCallDelta{Index: 0, ArgsJSONDelta: `a.txt"}`},
			ToolCallEnd{Index: 0},
		}, finish: StopToolUse, usage: Usage{InputTokens: 200, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "Done."}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, toolCalls, _, _, _, results := cb.snapshot()
	if len(toolCalls) != 1 || !strings.Contains(toolCalls[0], `read({"path":"a.txt"})`) {
		t.Errorf("OnToolCall = %v", toolCalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v", results)
	}
	req := prov.lastRequest()
	if len(req.Messages) < 3 {
		t.Fatalf("BUG-1: turn-2 history has %d messages, want >= 3 (goal, assistant tool_use, tool result) — got %+v", len(req.Messages), req.Messages)
	}
	// Contract: assistant message with ToolUse must precede the tool result.
	asst := req.Messages[len(req.Messages)-2]
	if asst.Role != RoleAssistant || len(asst.Content) == 0 || asst.Content[0].ToolUse == nil {
		t.Errorf("BUG-1: message before tool result is role=%q, want assistant tool_use (got %+v)", asst.Role, req.Messages)
	}
}

// AC1: OnWrittenFiles parity for write/edit tools (args carry the path).
func TestQAOnWrittenFilesParity(t *testing.T) {
	tools := newMockTools()
	tools.results["write"] = "wrote"
	tools.results["edit"] = "edited"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{
			ToolCallStart{Index: 0, ToolCallID: "t1", Name: "write"},
			ToolCallDelta{Index: 0, ArgsJSONDelta: `{"path":"a.md","content":"x"}`},
			ToolCallEnd{Index: 0},
			ToolCallStart{Index: 1, ToolCallID: "t2", Name: "edit"},
			ToolCallDelta{Index: 1, ArgsJSONDelta: `{"path":"b.go"}`},
			ToolCallEnd{Index: 1},
		}, finish: StopToolUse, usage: Usage{InputTokens: 200, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "wrote"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, written, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v", results)
	}
	if len(written) != 2 {
		t.Errorf("OnWrittenFiles = %v, want 2 (a.md, b.go)", written)
	}
}

// AC: model bound at session start; no per-turn switch.
func TestQAModelBoundAtStart(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "a"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	s := qaSession(t, prov, nil)
	if s.identity.Model != "deepseek-v4-flash" || s.identity.ProviderID != "mockprov" {
		t.Errorf("identity = %q/%q", s.identity.ProviderID, s.identity.Model)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if req := prov.lastRequest(); req.Model != "deepseek-v4-flash" {
		t.Errorf("turn model = %q", req.Model)
	}
}

// AC: manifest fields flow into identity + cached system prefix; the goal
// is the first user message (no re-render).
func TestQAManifestIdentityInput(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "ok"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	s := qaSession(t, prov, nil)
	id := s.Identity()
	if id.SystemPrompt != "You are the QA test worker." || id.Goal != "Write a test." || id.AcceptanceCriteria != "Tests pass." {
		t.Errorf("identity = %+v", id)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := prov.lastRequest()
	// ADR-0009 two-zone layout: block 0 is the cached static prefix —
	// composite prompt verbatim, then the thin native layer + env facts.
	// No mutable state yet → the single cached block only.
	if len(req.System) != 1 || !req.System[0].Cache {
		t.Errorf("system = %+v, want one cache-flagged block", req.System)
	}
	if !strings.HasPrefix(req.System[0].Text, "You are the QA test worker.") {
		t.Errorf("static prefix must start with the composite prompt verbatim: %q", req.System[0].Text[:min(80, len(req.System[0].Text))])
	}
	if !strings.Contains(req.System[0].Text, NativeStaticLayer) {
		t.Error("static prefix must carry the thin native layer")
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != RoleUser {
		t.Errorf("first message = %+v", req.Messages[0])
	}
}

// AC: mid-run injection — queued user turn drains between tool rounds and
// the reply streams back in the SAME session.
func TestQAInjectionMidRun(t *testing.T) {
	tools := newMockTools()
	tools.results["noop"] = "ok"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 200, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "reply"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
	}}
	s := qaSession(t, prov, tools)
	s.queueInjected("please focus")
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range prov.lastRequest().Messages {
		if m.Role == RoleUser && len(m.Content) > 0 && m.Content[0].Text != nil && *m.Content[0].Text == "please focus" {
			found = true
		}
	}
	if !found {
		t.Errorf("injected message missing from turn-2 history: %+v", prov.lastRequest().Messages)
	}
}

// AC: cancellation — cancelling the context stops the loop gracefully
// (no new provider call), marks the transcript cancelled, and leaves the
// session resumable.
func TestQACancellation(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "never"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	s := qaSession(t, prov, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the loop must stop before ANY provider call
	cb := &recordedCallback{}
	if err := s.Run(ctx, cb); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if prov.requestCount() != 0 {
		t.Errorf("provider turns = %d, want 0 (cancelled before any call)", prov.requestCount())
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded || results[0].errMsg != "cancelled" {
		t.Errorf("OnResult = %+v, want failure 'cancelled'", results)
	}
	evs, lerr := Load(s.TranscriptPath())
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	cancelled := false
	for _, e := range evs {
		if e.Type == TransState {
			var d struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.State == "cancelled" {
				cancelled = true
			}
		}
	}
	if !cancelled {
		t.Errorf("transcript not marked cancelled")
	}
}

// AC2: no-progress guard requires a REPEATED tool signature (zero token
// growth on one unique-signature round is healthy). Verifies BUG-2 fix.
func TestQANoProgressGuardNeedsRepeat(t *testing.T) {
	tools := newMockTools()
	tools.results["noop"] = "ok"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t2", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, stalls, _, results := cb.snapshot()
	if len(stalls) > 0 {
		t.Errorf("BUG-2: stall fired %v on two UNIQUE-signature tool rounds (want no stall — repetition requires the same signature 2x)", stalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("BUG-2: OnResult = %+v, want success (guard must not kill a healthy multi-round run)", results)
	}
}

// AC: runaway tool use is stopped by the tool-call budget ladder (not a
// step guillotine): the tool_call_count abort tier fails the session with
// budget_abort:tool_call_count, terminal-direct with no stall.
func TestQAToolsBudgetAbort(t *testing.T) {
	tools := newMockTools()
	tools.results["noop"] = "ok"
	var turns []scriptedTurn
	for i := 0; i < 6; i++ {
		turns = append(turns, scriptedTurn{
			events: []Event{ToolCallStart{Index: 0, ToolCallID: fmt.Sprintf("t%d", i), Name: "noop"}, ToolCallEnd{Index: 0}},
			finish: StopToolUse, usage: Usage{InputTokens: int64(100 + i*10), OutputTokens: 10},
		})
	}
	prov := &mockProvider{turns: turns}
	s := qaSession(t, prov, tools)
	s.cs.budget = opencode.ParseBudgetLadder([]byte(`{"tool_call_count":3}`))
	s.cs.spend = opencode.NewBudgetSpend()
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, stalls, _, results := cb.snapshot()
	if prov.requestCount() != 3 {
		t.Errorf("provider turns = %d, want 3 (abort on the 3rd tool call)", prov.requestCount())
	}
	if len(results) != 1 || results[0].succeeded || !strings.Contains(results[0].errMsg, "budget_abort:tool_call_count") {
		t.Errorf("OnResult = %+v, want failure budget_abort:tool_call_count", results)
	}
	if len(stalls) != 0 {
		t.Errorf("stalls = %v, want none (ladder abort is terminal-direct)", stalls)
	}
}

// AC: mid-stream error surfaces (no silent gap).
func TestQAMidStreamError(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "partial"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}, streamErr: fmt.Errorf("mid-stream failure")},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, text, _, _, _, _, results := cb.snapshot()
	if strings.Join(text, "") != "partial" {
		t.Errorf("text = %q", strings.Join(text, ""))
	}
	if len(results) != 1 || results[0].succeeded || !strings.Contains(results[0].errMsg, "mid-stream failure") {
		t.Errorf("OnResult = %+v", results)
	}
}

// AC: panic containment — a panicking tool is recovered per-call, the
// execution survives and the result is an error.
func TestQAPanicContainmentTool(t *testing.T) {
	tools := newMockTools()
	tools.panics["noop"] = true
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "after"}}, finish: StopStop, usage: Usage{InputTokens: 200, OutputTokens: 10}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v (panic must be contained)", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success (tool panic → error result)", results)
	}
	req := prov.lastRequest()
	last := req.Messages[len(req.Messages)-1]
	if len(last.Content) != 1 || last.Content[0].ToolResult == nil || !last.Content[0].ToolResult.IsError {
		t.Errorf("tool result not error-marked: %+v", last.Content)
	}
}

// AC: panic containment at the SESSION boundary — a provider callback
// panic fails ONLY this execution, transcript marked failed.
func TestQAPanicContainmentProvider(t *testing.T) {
	prov := &mockProvider{}
	wrapped := &panicProvider{stream: &panickingStream{panicMsg: "provider callback panicked"}}
	s := qaSession(t, prov, nil)
	s.provider = wrapped
	cb := &recordedCallback{}
	err := s.Run(context.Background(), cb)
	if err == nil {
		t.Fatal("Run returned nil, want ErrPanic")
	}
	if !strings.Contains(err.Error(), "session panic recovered") && !strings.Contains(err.Error(), "provider callback panicked") {
		t.Errorf("err = %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("OnResult = %+v, want failure with panic", results)
	}
	evs, _ := Load(s.TranscriptPath())
	failed := false
	for _, e := range evs {
		if e.Type == TransState {
			var d struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.State == "failed" {
				failed = true
			}
		}
	}
	if !failed {
		t.Errorf("transcript not marked failed")
	}
}

// AC: crash-safe transcript — torn final line tolerated on replay, seq
// continues after the last durable line.
func TestQATranscriptCrashSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec_crash.jsonl")
	content := `{"seq":1,"type":"session","ts":"t","data":{}}` + "\n" +
		`{"seq":2,"type":"text","ts":"t","data":{"text":"durable"}}` + "\n" +
		`{"seq":3,"type":"text","ts":"t` // torn
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	evs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(evs) != 2 {
		t.Errorf("replayed = %d, want 2 (torn tolerated)", len(evs))
	}
	tr, err := openTranscript(path)
	if err != nil {
		t.Fatalf("openTranscript: %v", err)
	}
	defer tr.Close()
	if tr.Seq() != 2 {
		t.Errorf("seq after reopen = %d, want 2", tr.Seq())
	}
}

// AC: identity isolation — per-execution sessions, header carries the
// worker identity.
func TestQAIdentityIsolation(t *testing.T) {
	provA := &mockProvider{turns: []scriptedTurn{{events: []Event{TextDelta{Text: "a"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}}}}
	provB := &mockProvider{turns: []scriptedTurn{{events: []Event{TextDelta{Text: "b"}}, finish: StopStop, usage: Usage{InputTokens: 1, OutputTokens: 1}}}}
	sA := qaSession(t, provA, nil)
	sB := qaSession(t, provB, nil)
	if sA.TranscriptPath() == sB.TranscriptPath() {
		t.Fatal("sessions share transcript path (isolation broken)")
	}
	cbA := &recordedCallback{}
	if err := sA.Run(context.Background(), cbA); err != nil {
		t.Fatalf("Run A: %v", err)
	}
	evs, _ := Load(sA.TranscriptPath())
	var hdr struct {
		Identity Identity `json:"identity"`
	}
	if err := json.Unmarshal(evs[0].Data, &hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if hdr.Identity.WorkerName != "qa-worker" || hdr.Identity.WorkerID != "worker_test" {
		t.Errorf("header identity = %+v", hdr.Identity)
	}
}

// AC: bridge capability surface + safe failure modes.
func TestQABridgeCapabilities(t *testing.T) {
	dir := t.TempDir()
	b := NewBridge(nil, dir, nil)
	var _ scheduler.MessageInjector = b
	var _ scheduler.Aborter = b
	var _ scheduler.LivenessReporter = b
	var _ scheduler.SessionContinuer = b
	var _ scheduler.ConfigurableBridge = b
	if b.IsExecutionActive("nope") {
		t.Error("unknown exec active")
	}
	if err := b.AbortExecution(context.Background(), "unknown", "r"); err != nil {
		t.Errorf("abort unknown: %v", err)
	}
	if err := b.SendExecutionMessage(context.Background(), "unknown", "hi"); err == nil {
		t.Error("inject unknown must error")
	}
}

// AC: ContinueSession verifies identity; identity-less transcript refused.
func TestQAContinueSessionIdentityVerified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	tr, err := openTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(TransSession, map[string]any{"identity": Identity{ExecutionID: "exec_prior", WorkerID: "worker_test", WorkerName: "qa-worker", TenantID: "tnt_test"}}); err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
	b := NewBridge(nil, dir, nil)
	ack, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{SessionID: "exec_prior", ExecutionID: "exec_prior"})
	if err != nil || !strings.Contains(ack, "exec_prior") {
		t.Errorf("ContinueSession = %q, %v", ack, err)
	}
	// No-identity transcript refused.
	p2 := filepath.Join(dir, "noid.jsonl")
	tr2, _ := openTranscript(p2)
	_ = tr2.Append(TransText, map[string]any{"text": "x"})
	_ = tr2.Close()
	if _, err := b.ContinueSession(context.Background(), scheduler.ContinueSessionOpts{SessionID: "noid"}); err == nil {
		t.Error("identity-less continue must be refused")
	}
}

// panickingStream panics on Next.
type panickingStream struct{ panicMsg string }

func (p *panickingStream) Next(ctx context.Context) (Event, bool, error) { panic(p.panicMsg) }
func (p *panickingStream) Close() error                                  { return nil }

type panicProvider struct{ stream TurnStream }

func (p *panicProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	return p.stream, nil
}
func (p *panicProvider) ListModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }
func (p *panicProvider) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true}
}

// AC: sequence continuation (opt-in, DEFAULT OFF) — a new session seeded
// from a prior session's transcript replays the full prior conversation
// into history (the provider sees the prior goal + assistant tool_use +
// tool result), the new goal arrives as the LAST user message, and the
// new session's own header identity is its own execution id (identity
// isolation preserved — no worker sees another worker's transcript).
func TestQASequenceContinuation(t *testing.T) {
	dir := t.TempDir()

	// Prior session: a tool round + a finishing text turn.
	priorPath := filepath.Join(dir, ".orchicon", "sessions", "exec_prior.jsonl")
	if err := os.MkdirAll(filepath.Dir(priorPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := &mockProvider{turns: []scriptedTurn{
		{events: []Event{
			ToolCallStart{Index: 0, ToolCallID: "tc1", Name: "read"},
			ToolCallDelta{Index: 0, ArgsJSONDelta: `{"path":"`},
			ToolCallDelta{Index: 0, ArgsJSONDelta: `a.txt"}`},
			ToolCallEnd{Index: 0},
		}, finish: StopToolUse, usage: Usage{InputTokens: 200, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "Prior done."}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
	}}
	sp := qaSession(t, prior, newMockTools())
	if err := sp.Run(context.Background(), &recordedCallback{}); err != nil {
		t.Fatalf("prior Run: %v", err)
	}
	// Move the prior transcript to the sessions dir under the expected name.
	priorTmp := sp.TranscriptPath()
	if err := os.Rename(priorTmp, priorPath); err != nil {
		t.Fatalf("rename prior transcript: %v", err)
	}

	// Continuing session: SAME worker (identity isolation), new execution
	// id, new goal (a chain's next step), seeded from the prior
	// transcript.
	tools := newMockTools()
	tools.results["read"] = "file contents"
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Continuing."}}, finish: StopStop, usage: Usage{InputTokens: 400, OutputTokens: 12}},
	}}
	sdir := t.TempDir()
	man := testManifest("orchicon/mockprov/deepseek-v4-flash")
	man.Goal = "Continue the chain: finish the next step."
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_new"),
		Manifest:   man,
		ProjectDir: sdir,
		Provider:   prov,
		Tools:      tools,
	})
	if err != nil {
		t.Fatalf("NewSession continuation: %v", err)
	}
	s.SetContinuation(priorPath)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("continuation Run: %v", err)
	}

	req := prov.lastRequest()
	if len(req.Messages) < 4 {
		t.Fatalf("continuation history has %d messages, want >= 4 (prior goal, prior assistant tool_use, prior tool result, new goal) — got %+v", len(req.Messages), req.Messages)
	}
	// Prior goal exactly once (seeded), new goal exactly once as LAST.
	priorGoalCount := 0
	for _, m := range req.Messages {
		if m.Role == RoleUser && len(m.Content) > 0 && m.Content[0].Text != nil && strings.Contains(*m.Content[0].Text, "Write a test.") {
			priorGoalCount++
		}
	}
	if priorGoalCount != 1 {
		t.Errorf("prior goal count = %d, want 1", priorGoalCount)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != RoleUser || last.Content[0].Text == nil || !strings.Contains(*last.Content[0].Text, "Continue the chain") {
		t.Errorf("last message = role %q, want the new chain goal user message", last.Role)
	}
	// Seeded assistant tool_use present (BUG-1 parity survives replay).
	foundAsst := false
	for _, m := range req.Messages {
		if m.Role == RoleAssistant && len(m.Content) > 0 && m.Content[0].ToolUse != nil && m.Content[0].ToolUse.Name == "read" {
			foundAsst = true
		}
	}
	if !foundAsst {
		t.Errorf("seeded assistant tool_use missing from continuation history")
	}
	// New session's header carries ITS OWN identity (exec_test, not exec_prior).
	evs, err := Load(s.TranscriptPath())
	if err != nil {
		t.Fatalf("Load continuation transcript: %v", err)
	}
	var hdr struct {
		Identity Identity `json:"identity"`
	}
	if err := json.Unmarshal(evs[0].Data, &hdr); err != nil {
		t.Fatalf("continuation header: %v", err)
	}
	if hdr.Identity.ExecutionID == "exec_prior" {
		t.Errorf("continuation header carries the PRIOR execution id (isolation broken)")
	}
}

// AC: sequence continuation refuses cross-worker continuation (identity
// isolation — no worker ever resumes another worker's transcript).
func TestQASetContinuationRefusesCrossWorker(t *testing.T) {
	// verifyContinuationIdentity is bridge-level; exercise it directly.
	dir := t.TempDir()
	path := filepath.Join(dir, "prior.jsonl")
	tr, err := openTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(TransSession, map[string]any{"identity": Identity{ExecutionID: "exec_prior", WorkerID: "worker_A", WorkerName: "worker-a", TenantID: "tnt_test"}}); err != nil {
		t.Fatal(err)
	}
	_ = tr.Close()
	// Same worker → ok.
	execSame := testExecRow("exec_new")
	execSame.WorkerID = "worker_A"
	if err := verifyContinuationIdentity(path, execSame); err != nil {
		t.Errorf("same-worker continuation refused: %v", err)
	}
	// Different worker → refused.
	execDiff := testExecRow("exec_new")
	execDiff.WorkerID = "worker_B"
	if err := verifyContinuationIdentity(path, execDiff); err == nil {
		t.Error("cross-worker continuation must be refused")
	}
}
