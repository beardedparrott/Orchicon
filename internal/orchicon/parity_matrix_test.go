package orchicon

// Parity matrix (consolidated) — one subtest per acceptance-criterion
// surface of the native adapter session engine, run entirely against the
// scripted mock provider (no live API). It mirrors the individual
// qa_loop_test cases but asserts every AC surface in one named matrix so
// the reviewer / QA can point at a single place. Every subtest reuses the
// shared mock provider + recording callbacks from qa_harness_test.go.
//
// NOTE on the recovery surface: the plan's "drive bridge Start with a
// RecoveryTrigger mock" seam does not exist on NativeBridge — recovery is
// the platform-level internal/recovery.Engine.TriggerOnFailure, invoked by
// the recovery service when an execution fails, not by the session loop or
// the bridge. The parity assertion here therefore exercises the session's
// crash-safe resume path: a fatal stall surfaces OnStall(fatal) + OnResult
// failure, and a recovered execution (new session over the SAME durably
// persisted transcript) replays history and continues to success.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParityMatrix(t *testing.T) {
	// execution-view telemetry: OnStarted → OnText(chunked) → OnResult(success).
	t.Run("execution_view", func(t *testing.T) {
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
		if joined := strings.Join(text, ""); joined != "Hello, world!" {
			t.Errorf("OnText = %q, want %q", joined, "Hello, world!")
		}
		if len(stalls) != 0 {
			t.Errorf("stalls = %v, want none", stalls)
		}
		if len(results) != 1 || !results[0].succeeded || results[0].output != "Hello, world!" {
			t.Errorf("OnResult = %+v", results)
		}
	})

	// todo panel: a todowrite tool round is recorded (OnToolCall) and the
	// todo payload is durable in the transcript (the UI's todo sidecar).
	t.Run("todo_panel", func(t *testing.T) {
		tools := newMockTools()
		prov := &mockProvider{turns: []scriptedTurn{
			{events: []Event{
				ToolCallStart{Index: 0, ToolCallID: "td1", Name: "todowrite"},
				ToolCallDelta{Index: 0, ArgsJSONDelta: `{"todos":[{"content":"step 1","status":"in_progress"}]}`},
				ToolCallEnd{Index: 0},
			}, finish: StopToolUse, usage: Usage{InputTokens: 200, OutputTokens: 10}},
			{events: []Event{TextDelta{Text: "ok"}}, finish: StopStop, usage: Usage{InputTokens: 300, OutputTokens: 15}},
		}}
		s := qaSession(t, prov, tools)
		cb := &recordedCallback{}
		if err := s.Run(context.Background(), cb); err != nil {
			t.Fatalf("Run: %v", err)
		}
		_, _, toolCalls, _, _, _, results := cb.snapshot()
		if len(results) != 1 || !results[0].succeeded {
			t.Errorf("OnResult = %+v", results)
		}
		found := false
		for _, tc := range toolCalls {
			if strings.Contains(tc, "todowrite(") && strings.Contains(tc, "step 1") {
				found = true
			}
		}
		if !found {
			t.Errorf("OnToolCall missing todowrite payload: %v", toolCalls)
		}
		evs, err := Load(s.TranscriptPath())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		dur := false
		for _, e := range evs {
			if e.Type != TransToolCall {
				continue
			}
			if strings.Contains(string(e.Data), "todowrite") && strings.Contains(string(e.Data), "step 1") {
				dur = true
			}
		}
		if !dur {
			t.Errorf("transcript missing durable todo sidecar (TransToolCall todowrite)")
		}
	})

	// file diffs: write + edit tool calls → OnWrittenFiles carries both paths.
	t.Run("file_diffs", func(t *testing.T) {
		tools := newMockTools()
		tools.results["write"] = "wrote"
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
	})

	// callbacks: terminal OnResult(success) with no stalls on a clean run.
	t.Run("callbacks", func(t *testing.T) {
		prov := &mockProvider{turns: []scriptedTurn{
			{events: []Event{TextDelta{Text: "done"}}, finish: StopStop, usage: Usage{InputTokens: 5, OutputTokens: 2}},
		}}
		s := qaSession(t, prov, nil)
		cb := &recordedCallback{}
		if err := s.Run(context.Background(), cb); err != nil {
			t.Fatalf("Run: %v", err)
		}
		_, _, _, _, stalls, stallFatal, results := cb.snapshot()
		if len(results) != 1 || !results[0].succeeded {
			t.Errorf("OnResult = %+v, want terminal success", results)
		}
		if len(stalls) != 0 || len(stallFatal) != 0 {
			t.Errorf("stalls = %v (fatal=%v), want none on a clean run", stalls, stallFatal)
		}
	})

	// cancel: a cancelled context stops the loop before any provider call
	// and surfaces OnResult(false, "cancelled").
	t.Run("cancel", func(t *testing.T) {
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
	})

	// injection: a queued mid-run human turn drains between tool rounds and
	// appears in the next turn's history.
	t.Run("injection", func(t *testing.T) {
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
	})

	// recovery round trip: a FATAL stall surfaces OnStall(fatal) + OnResult
	// failure; a recovered execution (new session over the same durable
	// transcript) replays history and continues to success.
	t.Run("recovery_round_trip", func(t *testing.T) {
		t.Setenv("ORCHICON_SESSION_MAX_STEPS", "3")
		dir := t.TempDir()

		// Phase 1 — drive the session into the max-steps FATAL stall.
		toolsA := newMockTools()
		toolsA.results["noop"] = "ok"
		var turns []scriptedTurn
		for i := 0; i < 6; i++ {
			turns = append(turns, scriptedTurn{
				events: []Event{ToolCallStart{Index: 0, ToolCallID: fmt.Sprintf("t%d", i), Name: "noop"}, ToolCallEnd{Index: 0}},
				finish: StopToolUse, usage: Usage{InputTokens: int64(100 + i*10), OutputTokens: 10},
			})
		}
		provA := &mockProvider{turns: turns}
		sA, err := NewSession(SessionConfig{
			ExecRow:    testExecRow("exec_rec"),
			Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
			ProjectDir: dir,
			Provider:   provA,
			Tools:      toolsA,
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		cbA := &recordedCallback{}
		if err := sA.Run(context.Background(), cbA); err != nil {
			t.Fatalf("phase1 Run: %v", err)
		}
		_, _, _, _, stalls, stallFatal, resultsA := cbA.snapshot()
		if len(resultsA) != 1 || resultsA[0].succeeded || !strings.Contains(resultsA[0].errMsg, "max_steps") {
			t.Errorf("phase1 OnResult = %+v, want failure max_steps", resultsA)
		}
		if len(stalls) != 1 || len(stallFatal) != 1 || !stallFatal[0] {
			t.Errorf("phase1 OnStall = %v (fatal=%v), want one FATAL stall", stalls, stallFatal)
		}
		if provA.requestCount() != 3 {
			t.Errorf("phase1 provider turns = %d, want 3 (max steps)", provA.requestCount())
		}

		// Phase 2 — recovery resume: a new execution over the SAME durable
		// transcript (reuse ProjectDir + execution id) replays the prior
		// session and continues to success. This is the crash-safe recovery
		// round trip the platform's recovery engine re-dispatches.
		provB := &mockProvider{turns: []scriptedTurn{
			{events: []Event{TextDelta{Text: "recovered"}}, finish: StopStop, usage: Usage{InputTokens: 50, OutputTokens: 12}},
		}}
		sB, err := NewSession(SessionConfig{
			ExecRow:    testExecRow("exec_rec"),
			Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
			ProjectDir: dir,
			Provider:   provB,
			Tools:      newMockTools(),
		})
		if err != nil {
			t.Fatalf("NewSession recovery: %v", err)
		}
		cbB := &recordedCallback{}
		if err := sB.Run(context.Background(), cbB); err != nil {
			t.Fatalf("phase2 Run: %v", err)
		}
		_, _, _, _, _, _, resultsB := cbB.snapshot()
		if len(resultsB) != 1 || !resultsB[0].succeeded {
			t.Errorf("phase2 OnResult = %+v, want recovered success", resultsB)
		}
		req := provB.lastRequest()
		if len(req.Messages) == 0 {
			t.Fatalf("phase2 first request has no messages (transcript not replayed)")
		}
		replayedGoal := false
		for _, m := range req.Messages {
			if m.Role == RoleUser && len(m.Content) > 0 && m.Content[0].Text != nil && strings.Contains(*m.Content[0].Text, "Write a test.") {
				replayedGoal = true
			}
		}
		if !replayedGoal {
			t.Errorf("phase2 recovered session did not replay the durable goal: %+v", req.Messages)
		}
	})
}
