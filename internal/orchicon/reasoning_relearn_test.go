package orchicon

// reasoning_relearn_test.go — the reasoning a thinking-mode provider demands back must be (1) learned ONCE per
// provider rather than once per request, (2) replayed byte-for-byte, and (3) reported in the error when the
// provider rejects it anyway.
//
// REPORTED (twice, a worker execution failing ~4 minutes in, 33 tool rounds deep):
//
//	provider stream failed: provider status 400 400 Bad Request: {"error":{"message":"The `reasoning_content`
//	in the thinking mode must be passed back to the API. (request_id: f8e4b9cb-…)","type":"invalid_request_error"}}
//
// The provider was the tenant's own DeepSeek entry — a CUSTOM OpenAI-compatible profile (deepseek-direct,
// https://api.deepseek.com/v1). Custom profiles start with ReplayReasoning OFF by necessity (the field is not
// part of the OpenAI chat schema), so that session sent no reasoning at all on the first attempt of EVERY
// request and survived each round only because the 400-repair replayed it on the second. The lesson lived in
// StreamTurn's local scope: 33 rounds, 33 rejections, 33 resends, and every round one unrepairable history away
// from a fatal error. These tests pin the three things that made that possible.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// requireReasoningServer is a strict thinking-mode provider: it rejects any request whose history omits the
// reasoning that accompanied an assistant tool call, with the provider's own wording.
func requireReasoningServer(t *testing.T, calls *atomic.Int32, bodies *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if bodies != nil {
			*bodies = append(*bodies, string(b))
		}
		if !strings.Contains(string(b), `"reasoning_content"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func reasoningReq(reasoning string) TurnRequest {
	r := reasoning
	return TurnRequest{
		Model: "deepseek-flash",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: strPtr("build it")}}},
			{Role: RoleAssistant, Content: []Content{
				{ToolUse: &ContentToolUse{ToolCallID: "call-1", Name: "bash", ArgsJSON: `{}`}, Reasoning: &r},
			}},
			{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call-1", Content: "ok"}}}},
		},
	}
}

// THE REGRESSION THAT COST 33 REJECTIONS: the provider's demand must survive the request that taught it.
func TestReasoningReplayDemandIsLearnedOncePerProviderNotPerRequest(t *testing.T) {
	forgetLearnedReplayReasoning()
	t.Cleanup(forgetLearnedReplayReasoning)

	var calls atomic.Int32
	var bodies []string
	srv := requireReasoningServer(t, &calls, &bodies)
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(), ProviderID: "deepseek-direct",
		Retry:  RetryPolicy{MaxAttempts: 1, Sleep: func(time.Duration) {}},
		Quirks: Quirks{}, // a CUSTOM profile: replay off, exactly as profileFromRow builds it
	}

	// Round 1: the provider rejects, the repair replays, the turn succeeds.
	if _, err := c.StreamTurn(context.Background(), reasoningReq("read the file first")); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("round 1 took %d HTTP calls, want 2 (reject + repaired resend)", n)
	}
	if !ReplayReasoningLearned("deepseek-direct") {
		t.Fatal("the provider demanded reasoning replay and the demand was not remembered")
	}

	// Round 2 — a NEW request, as the loop sends on its next tool round. It must carry the reasoning on the
	// FIRST attempt: the demand is a fact about the endpoint, not about round 1's request.
	if _, err := c.StreamTurn(context.Background(), reasoningReq("now the styles")); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("round 2 took %d HTTP calls, want 1 (total 3): the learned replay must apply to the first "+
			"attempt, or every round of a tool-using session pays a rejection", n-2)
	}
	if !strings.Contains(bodies[2], `"reasoning_content":"now the styles"`) {
		t.Fatalf("round 2's first request did not replay the reasoning:\n%s", bodies[2])
	}
}

// A WHITESPACE-ONLY ROUND IS STILL REASONING. Trimming it away made it "no reasoning to replay", which is the
// one state the repair cannot fix.
func TestReasoningIsReplayedByteForByte(t *testing.T) {
	s := &Session{id: "test-byte-exact"}
	raw := "\n\n  deliberate leading and trailing space  \n"
	s.appendAssistantToolUse("", []ToolCall{{ToolCallID: "call-1", Name: "bash", ArgsJSON: `{}`}}, raw)

	msg := s.history[len(s.history)-1]
	if len(msg.Content) == 0 || msg.Content[0].Reasoning == nil {
		t.Fatal("a round with reasoning recorded none")
	}
	if got := *msg.Content[0].Reasoning; got != raw {
		t.Fatalf("replayed reasoning was altered: got %q, want %q", got, raw)
	}

	body := buildOpenAIRequest(TurnRequest{Model: "deepseek-flash", Messages: s.history}, Quirks{ReplayReasoning: true})
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var want string
	wb, _ := json.Marshal(raw)
	want = `"reasoning_content":` + string(wb)
	if !strings.Contains(string(wire), want) {
		t.Fatalf("the wire did not carry the reasoning verbatim (want %s):\n%s", want, wire)
	}
}

// COMPACTION MUST NOT STRIP IT. The reasoning rides one content block; evicting that block while the message
// survives is how a history can silently lose the field the provider requires.
func TestCompactionCarriesReasoningOntoASurvivingBlock(t *testing.T) {
	s, _, _, _ := compactTestSession(t, `{"context_compaction":{"enabled":true,"pressure_frac":0.8,"recent_turns":2}}`, 1000000)
	s.appendUser(TransUserMessage, "goal", "goal")

	// A middle round with TWO calls whose results are separate messages (the shape a resumed session's replay
	// produces): the first result is evictable, the second is pinned in the tail.
	r := "planned two writes"
	s.history = append(s.history, Message{Role: RoleAssistant, Content: []Content{
		{ToolUse: &ContentToolUse{ToolCallID: "call-a", Name: "write", ArgsJSON: `{}`}, Reasoning: &r},
		{ToolUse: &ContentToolUse{ToolCallID: "call-b", Name: "write", ArgsJSON: `{}`}},
	}})
	s.history = append(s.history, Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call-a", Content: "ok a"}}}})
	// Tail: enough messages that call-b's result is pinned.
	s.history = append(s.history, Message{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call-b", Content: "ok b"}}}})
	s.history = append(s.history, Message{Role: RoleUser, Content: []Content{{Text: strPtr("carry on")}}})

	_, replacement := s.planMiddleEviction()
	if replacement == nil {
		t.Fatal("expected a replacement history")
	}
	var survivor *Message
	for i := range replacement {
		for _, c := range replacement[i].Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == "call-b" {
				survivor = &replacement[i]
			}
		}
	}
	if survivor == nil {
		t.Fatalf("the surviving round was evicted entirely; replacement: %+v", replacement)
	}
	if got := contentReasoning(survivor.Content); got == nil || *got != r {
		t.Fatalf("the surviving assistant message lost its reasoning (%v) — a thinking-mode provider rejects "+
			"a history that omits it", got)
	}
}

// AND WHEN IT FAILS ANYWAY, THE ERROR SAYS WHAT WAS SENT. Without this the operator sees the provider's 400 and
// cannot tell "replay was on", "there was nothing to replay", and "we never resent" apart.
func TestAnUnrepairableReasoningErrorReportsWhatWasSent(t *testing.T) {
	forgetLearnedReplayReasoning()
	t.Cleanup(forgetLearnedReplayReasoning)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`))
	}))
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(), ProviderID: "deepseek-direct",
		Retry: RetryPolicy{MaxAttempts: 1, Sleep: func(time.Duration) {}},
	}
	// The history HAS reasoning, so the repair runs — and the provider still refuses.
	_, err := c.StreamTurn(context.Background(), reasoningReq("I should read the file"))
	if err == nil {
		t.Fatal("a 400 that survives the repair budget must surface")
	}
	msg := err.Error()
	if !strings.Contains(msg, "reasoning replay on") {
		t.Errorf("the error does not say whether replay was on: %s", msg)
	}
	if !strings.Contains(msg, "1 of 1 assistant messages carried reasoning_content") {
		t.Errorf("the error does not say how much reasoning the request carried: %s", msg)
	}
	if !strings.Contains(msg, "2 repair resend(s)") {
		t.Errorf("the error does not say how often the repair resent: %s", msg)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("got %d HTTP calls, want 3 (initial + %d repairs)", n, repairAttempts)
	}

	// AND WITH NOTHING TO REPLAY the report must say so, plus zero resends — the other half of the ambiguity.
	forgetLearnedReplayReasoning()
	calls.Store(0)
	_, err = c.StreamTurn(context.Background(), TurnRequest{
		Model:    "deepseek-flash",
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hello")}}}},
	})
	if err == nil {
		t.Fatal("an unrepairable 400 must surface")
	}
	msg = err.Error()
	if !strings.Contains(msg, "0 of 0 assistant messages") || !strings.Contains(msg, "0 repair resend(s)") {
		t.Errorf("the error does not distinguish 'nothing to replay' from 'replay off': %s", msg)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("got %d HTTP calls, want 1 — a repair that changes nothing must not be resent", n)
	}
}
