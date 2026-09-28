package claude

import "testing"

// Canned stream-json fixtures — no I/O, no subprocess, no Anthropic spend.

func TestParseLineSystemInit(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"sess-abc","cwd":"/w"}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if ev.Type != "system" || ev.Subtype != "init" {
		t.Fatalf("got type=%q subtype=%q", ev.Type, ev.Subtype)
	}
	if ev.SessionID != "sess-abc" {
		t.Fatalf("session id = %q, want sess-abc", ev.SessionID)
	}
}

func TestParseLineTextDelta(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"stream_event","session_id":"s","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if ev.Type != "text_delta" || ev.Text != "hello " {
		t.Fatalf("got type=%q text=%q", ev.Type, ev.Text)
	}
}

func TestParseLineAssistantToolUseAndText(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"writing"},{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"/w/a.go"}}]}}`
	ev, err := ParseLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if ev.Text != "writing" {
		t.Fatalf("text = %q", ev.Text)
	}
	if len(ev.ToolUses) != 1 || ev.ToolUses[0].Name != "Write" || ev.ToolUses[0].ID != "tu-1" {
		t.Fatalf("tool uses = %+v", ev.ToolUses)
	}
	if got := writtenFilesFromTool(ev.ToolUses[0].Name, ev.ToolUses[0].Input); len(got) != 1 || got[0] != "/w/a.go" {
		t.Fatalf("written files = %v", got)
	}
}

func TestParseLineUserToolResult(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"ok","is_error":false}]}}`
	ev, err := ParseLine([]byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if len(ev.ToolResults) != 1 || ev.ToolResults[0].ToolUseID != "tu-1" || ev.ToolResults[0].Content != "ok" {
		t.Fatalf("tool results = %+v", ev.ToolResults)
	}
}

func TestParseLineResults(t *testing.T) {
	cases := []struct {
		line    string
		subtype string
		ok      bool
	}{
		{`{"type":"result","subtype":"success","session_id":"s","result":"done","total_cost_usd":0.01,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`, ResultSuccess, true},
		{`{"type":"result","subtype":"error_max_turns","session_id":"s"}`, ResultErrorMaxTurns, false},
		{`{"type":"result","subtype":"error_max_budget_usd","session_id":"s"}`, ResultErrorMaxBudget, false},
	}
	for _, c := range cases {
		ev, err := ParseLine([]byte(c.line))
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", c.subtype, err)
		}
		if !ev.IsTerminalResult() {
			t.Fatalf("%s: not terminal", c.subtype)
		}
		if ev.TurnSucceeded() != c.ok {
			t.Fatalf("%s: TurnSucceeded = %v, want %v", c.subtype, ev.TurnSucceeded(), c.ok)
		}
		if ev.Subtype != c.subtype {
			t.Fatalf("subtype = %q, want %q", ev.Subtype, c.subtype)
		}
	}
	// A non-terminal line is never a terminal result.
	ev, _ := ParseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`))
	if ev.IsTerminalResult() {
		t.Fatal("assistant message reported as terminal result")
	}
	// Usage is captured.
	ev, _ = ParseLine([]byte(`{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":33,"cache_creation_input_tokens":44}}`))
	if ev.Usage.InputTokens != 11 || ev.Usage.OutputTokens != 22 || ev.Usage.CacheReadTokens != 33 || ev.Usage.CacheCreationTokens != 44 || ev.TotalCostUSD != 0.25 {
		t.Fatalf("usage = %+v cost=%v", ev.Usage, ev.TotalCostUSD)
	}
}

func TestParseLineMalformedErrors(t *testing.T) {
	if _, err := ParseLine([]byte("not json")); err == nil {
		t.Fatal("malformed line should error")
	}
	// An unknown but well-formed type is tolerated (no error, empty event).
	ev, err := ParseLine([]byte(`{"type":"future_event_kind"}`))
	if err != nil {
		t.Fatalf("unknown type should not error: %v", err)
	}
	if ev.Type != "future_event_kind" {
		t.Fatalf("type = %q", ev.Type)
	}
}

func TestWrittenFilesFromTool(t *testing.T) {
	if got := writtenFilesFromTool("Bash", map[string]any{"command": "ls"}); got != nil {
		t.Fatalf("Bash produced files: %v", got)
	}
	if got := writtenFilesFromTool("Edit", map[string]any{"file_path": "/a/b.go"}); len(got) != 1 || got[0] != "/a/b.go" {
		t.Fatalf("Edit = %v", got)
	}
	if got := writtenFilesFromTool("Write", nil); got != nil {
		t.Fatalf("nil input = %v", got)
	}
}

func TestModelForRef(t *testing.T) {
	cases := map[string]string{
		"claude/anthropic/claude-sonnet-5": "claude-sonnet-5",
		"anthropic/claude-sonnet-4":        "claude-sonnet-4",
		"claude-sonnet-5":                  "claude-sonnet-5",
		"":                                 "",
	}
	for in, want := range cases {
		if got := modelForRef(in); got != want {
			t.Errorf("modelForRef(%q) = %q, want %q", in, got, want)
		}
	}
}
