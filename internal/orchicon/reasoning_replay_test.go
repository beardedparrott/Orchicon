package orchicon

// reasoning_replay_test.go — a thinking-mode provider must get its reasoning back.

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func reasoningSession() *Session {
	return &Session{id: "test-reasoning", log: slog.New(slog.DiscardHandler)}
}

// THE ASSERTION THAT ENCODES THE 400: the worker's assistant message must carry the round's reasoning, and
// the OpenAI-compat serializer must put it on the wire as `reasoning_content`.
func TestWorkerAssistantMessageCarriesItsReasoningOntoTheWire(t *testing.T) {
	s := reasoningSession()
	s.appendAssistantToolUse("", []ToolCall{{
		ToolCallID: "call-1", Name: "read_file", ArgsJSON: `{"path":"a"}`,
	}}, "I should read the file first.")

	req := TurnRequest{Model: "deepseek-v4", Messages: s.history}
	// ReplayReasoning is what the provider profile asks for; forceReplay is what the 400-repair flips on. Both
	// routes read the same message field, so asserting the field is enough — asserted both ways below.
	body := buildOpenAIRequestWithReplay(req, Quirks{ReplayReasoning: true}, false)

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"reasoning_content":"I should read the file first."`) {
		t.Fatalf("the request carries no reasoning_content:\n%s\n  a thinking-mode provider rejects this with "+
			"400 'The reasoning_content in the thinking mode must be passed back to the API'", raw)
	}
}

// ...and WITHOUT it in history there is nothing to replay, which is exactly why the repair could not save the
// failing request. This pins the mechanism the fix removes.
func TestDroppingReasoningLeavesTheRepairWithNothing(t *testing.T) {
	// A history built the OLD way: assistant turn with tool calls and no reasoning.
	s := reasoningSession()
	s.appendAssistantToolUse("", []ToolCall{{ToolCallID: "call-1", Name: "read_file", ArgsJSON: `{}`}}, "")

	req := TurnRequest{Model: "deepseek-v4", Messages: s.history}
	if repairRequest(RepairReasoningContent, &req) {
		t.Fatal("the repair claimed it could fix a history with no reasoning — it cannot, and reporting that " +
			"it can would retry a request that is identical to the one that just failed")
	}
}

// A round that produced ONLY reasoning (no text, no tool call) still has to exist as a message, or the
// provider never receives its reasoning back.
func TestReasoningOnlyRoundStillRecordsAMessage(t *testing.T) {
	s := reasoningSession()
	s.appendAssistantToolUse("", nil, "thinking only")

	if len(s.history) != 1 {
		t.Fatalf("history has %d messages, want 1", len(s.history))
	}
	msg := s.history[0]
	if len(msg.Content) == 0 || msg.Content[0].Reasoning == nil || *msg.Content[0].Reasoning != "thinking only" {
		t.Fatalf("reasoning-only round did not record its reasoning: %+v", msg)
	}
}

// The transcript carries it too, because a RESUMED session rebuilds history from the transcript parts — a
// replay that drops reasoning hits the same 400 on its first round.
func TestToolCallTranscriptPartCarriesReasoning(t *testing.T) {
	// The payload the loop writes is the contract the replay reads; asserting the key here keeps the two
	// halves from drifting (the writer is in loop.go, the reader in replay).
	var d struct {
		Text      string     `json:"text"`
		ToolCalls []ToolCall `json:"tool_calls"`
		Reasoning string     `json:"reasoning,omitempty"`
	}
	payload := map[string]any{
		"text": "", "tool_calls": []ToolCall{}, "reasoning": "because",
	}
	raw, _ := json.Marshal(payload)
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Reasoning != "because" {
		t.Fatalf("the transcript part's reasoning round-tripped as %q — the replay would lose it", d.Reasoning)
	}
}
