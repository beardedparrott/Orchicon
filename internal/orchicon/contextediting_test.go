package orchicon

import (
	"strings"
	"testing"
)

// These pin the simple contract: keep the last N tool results verbatim, stub the rest, and let the agent
// still SEE that each stubbed call happened and what it was.

func ceAssistantMsg(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Text: &text}}}
}

// ceToolPair builds an assistant tool_use + a tool result, the shape the loop actually produces, so the
// stub can recover the tool's NAME and ARGS from the request side.
func ceToolPair(id, name, args, output string, isErr bool) []Message {
	return []Message{
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{
			ToolCallID: id, Name: name, ArgsJSON: args,
		}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
			ToolCallID: id, Content: output, IsError: isErr,
		}}}},
	}
}

// ceHistoryOf builds n tool calls with distinct ids/outputs.
func ceHistoryOf(n int) []Message {
	h := make([]Message, 0, n*2)
	for i := 0; i < n; i++ {
		h = append(h, ceToolPair(
			"call-"+string(rune('a'+i%26))+string(rune('0'+i/26)),
			"bash",
			`{"command":"go test ./...","timeout":60}`,
			strings.Repeat("x", 1000),
			false,
		)...)
	}
	return h
}

func ceResultContents(h []Message) []string {
	var out []string
	for _, m := range h {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				out = append(out, c.ToolResult.Content)
			}
		}
	}
	return out
}

// TestKeepRecentToolResults pins the whole rule: exactly the newest N survive verbatim.
func TestKeepRecentToolResults(t *testing.T) {
	const keep = 20
	h := ceHistoryOf(50)
	cleared, reclaimed := keepRecentToolResults(h, keep)

	contents := ceResultContents(h)
	if len(contents) != 50 {
		t.Fatalf("history should still contain 50 tool results, got %d", len(contents))
	}
	if cleared != 30 {
		t.Errorf("cleared %d results, want 30 (50 calls - keep 20)", cleared)
	}
	if reclaimed <= 0 {
		t.Errorf("reclaimed bytes should be positive, got %d", reclaimed)
	}
	// The newest `keep` are verbatim, the older ones are stubs.
	for i, c := range contents {
		isStub := strings.HasPrefix(c, clearedStubPrefix)
		if i < len(contents)-keep && !isStub {
			t.Errorf("result %d should be stubbed (it is older than the newest %d)", i, keep)
		}
		if i >= len(contents)-keep && isStub {
			t.Errorf("result %d must stay verbatim — it is within the newest %d", i, keep)
		}
	}
}

// TestStubKeepsToolIdentity is the requirement that makes this safe: the agent must still know a call
// happened and what it was, or it will re-run work to recover its own reasoning.
func TestStubKeepsToolIdentity(t *testing.T) {
	h := ceHistoryOf(3)
	keepRecentToolResults(h, 1)

	contents := ceResultContents(h)
	stub := contents[0]
	if !strings.HasPrefix(stub, clearedStubPrefix) {
		t.Fatalf("oldest result should be a stub, got %q", stub)
	}
	if !strings.Contains(stub, "bash") {
		t.Errorf("the stub must name the TOOL that ran, got %q", stub)
	}
	if !strings.Contains(stub, "go test ./...") {
		t.Errorf("the stub must keep the argument identity, got %q", stub)
	}
	if !strings.Contains(stub, "durable transcript") {
		t.Errorf("the stub should say the output still exists, got %q", stub)
	}
}

// TestStubIsCheap: a stub must be far smaller than the payload, or clearing achieves nothing.
func TestStubIsCheap(t *testing.T) {
	h := ceHistoryOf(2)
	keepRecentToolResults(h, 1)
	stub := ceResultContents(h)[0]
	if len(stub) > 200 {
		t.Fatalf("stub is %d bytes — too large to be worth clearing", len(stub))
	}
}

// TestErrorsAreNeverStubbed: an error is why the worker changed approach.
func TestErrorsAreNeverStubbed(t *testing.T) {
	h := []Message{}
	h = append(h, ceToolPair("c1", "bash", `{"command":"make"}`, "ERROR: build failed: undefined: Foo", true)...)
	h = append(h, ceToolPair("c2", "bash", `{"command":"make2"}`, strings.Repeat("y", 500), false)...)
	h = append(h, ceToolPair("c3", "bash", `{"command":"make3"}`, strings.Repeat("z", 500), false)...)

	keepRecentToolResults(h, 1)
	contents := ceResultContents(h)
	if !strings.Contains(contents[0], "undefined: Foo") {
		t.Fatalf("an ERROR result must never be stubbed, got %q", contents[0])
	}
}

// TestKeepZeroDisables: 0 means disabled, so an unconfigured deployment keeps its behaviour.
func TestKeepZeroDisables(t *testing.T) {
	h := ceHistoryOf(10)
	before := ceResultContents(h)
	cleared, _ := keepRecentToolResults(h, 0)
	if cleared != 0 {
		t.Fatalf("keep=0 must clear nothing, cleared %d", cleared)
	}
	after := ceResultContents(h)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("keep=0 altered result %d", i)
		}
	}
}

// TestFewerCallsThanKeepIsUntouched: the common early-session case must be a no-op.
func TestFewerCallsThanKeepIsUntouched(t *testing.T) {
	h := ceHistoryOf(5)
	before := ceResultContents(h)
	if cleared, _ := keepRecentToolResults(h, 20); cleared != 0 {
		t.Fatalf("5 calls with keep=20 must clear nothing, cleared %d", cleared)
	}
	after := ceResultContents(h)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("result %d was altered", i)
		}
	}
}

// TestClearingIsIdempotent: repeated rounds must not re-stub or double-count.
func TestClearingIsIdempotent(t *testing.T) {
	h := ceHistoryOf(30)
	first, firstBytes := keepRecentToolResults(h, 20)
	if first == 0 {
		t.Fatal("expected the first pass to stub")
	}
	second, secondBytes := keepRecentToolResults(h, 20)
	if second != 0 || secondBytes != 0 {
		t.Fatalf("second pass re-stubbed %d results (%d bytes); it must be idempotent", second, secondBytes)
	}
	if firstBytes <= 0 {
		t.Errorf("first pass should report reclaimed bytes")
	}
}

// TestClearingPolicyFromSettings pins the single-knob shape.
func TestClearingPolicyFromSettings(t *testing.T) {
	// Absent / empty = OFF.
	for _, raw := range [][]byte{nil, []byte("{}"), []byte(`{"tokens":0}`)} {
		if got := contextEditPolicyFromSettings(raw).KeepToolCalls; got != 0 {
			t.Errorf("clearing must default OFF for %q, got keep=%d", raw, got)
		}
	}
	got := contextEditPolicyFromSettings([]byte(`{"context_editing":{"keep_tool_calls":20}}`))
	if got.KeepToolCalls != 20 {
		t.Errorf("keep_tool_calls = %d, want 20", got.KeepToolCalls)
	}
	// The old multi-knob block must not resurrect the old behaviour; an unrecognised block is simply off.
	old := contextEditPolicyFromSettings([]byte(`{"context_editing":{"enabled":true,"trigger_frac":0.5,"keep_recent":8}}`))
	if old.KeepToolCalls != 0 {
		t.Errorf("a legacy block with no keep_tool_calls must read as OFF, got keep=%d", old.KeepToolCalls)
	}
}

// TestSummarizeToolArgs covers the identity extraction for the common tool shapes.
func TestSummarizeToolArgs(t *testing.T) {
	cases := map[string]string{
		`{"filePath":"internal/foo.go"}`: "(internal/foo.go)",
		`{"path":"/tmp/x"}`:              "(/tmp/x)",
		`{"command":"go build ./..."}`:   "(go build ./...)",
		`{"pattern":"TODO"}`:             "(TODO)",
		`{"query":"how does X work"}`:    "(how does X work)",
		`{"unrelated":"value"}`:          "",
		`not json`:                       "(not json)",
		`{"command":"line1\nline2"}`:     "(line1 line2)",
	}
	for in, want := range cases {
		if got := summarizeToolArgs(in); got != want {
			t.Errorf("summarizeToolArgs(%q) = %q, want %q", in, got, want)
		}
	}
	// A long argument is truncated so the stub stays cheap.
	long := `{"command":"` + strings.Repeat("a", 500) + `"}`
	if got := summarizeToolArgs(long); len(got) > 140 {
		t.Errorf("a long argument must be truncated, got %d chars", len(got))
	}
}
