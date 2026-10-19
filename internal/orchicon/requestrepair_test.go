package orchicon

// requestrepair_test.go — A REPAIRABLE REQUEST IS RESHAPED AND RESENT; A TRANSIENT ONE IS RETRIED; A FATAL ONE
// IS SURFACED. All three, because the value of the distinction is that it does not blur.
//
// The operator: "if we run into an error about prompts/requests then we should reshape them automatically to
// fix the underlying issue and resend." The risk of doing that carelessly is rewriting requests that were
// fine, so every case below pins a REFUSAL as well as a repair.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── classification: what is worth repairing, and what is not ──────────────────────────────────────────────

func TestClassifyRepairableRequiresTheProvidersOwnWords(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   RepairKind
	}{
		{
			// THE REAL ONE. Verbatim from the provider.
			name: "deepseek reasoning_content", status: 400,
			body: `{"error":{"type":"invalid_request_error","message":"Upstream request failed: [invalid_request_error] The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`,
			want: RepairReasoningContent,
		},
		{
			// A 400 that says nothing recognisable must NOT be "repaired": this is a genuinely bad request,
			// and rewriting it would silently change what the caller asked for.
			name: "a bare 400 is not a licence to rewrite", status: 400,
			body: `{"error":{"message":"bad request"}}`,
			want: RepairNone,
		},
		{
			// A 400 about something else entirely.
			name: "context length is not repairable here", status: 400,
			body: `{"error":{"message":"This model's maximum context length is 65536 tokens"}}`,
			want: RepairNone,
		},
		{
			name: "unknown field", status: 400,
			body: `{"error":{"message":"Unrecognized field: reasoning_effort"}}`,
			want: RepairUnknownField,
		},
		{
			// Not a 400 at all — a transient status is the RETRY layer's business, never a repair.
			name: "429 is transient, not repairable", status: 429,
			body: `{"error":{"message":"reasoning_content must be passed back"}}`,
			want: RepairNone,
		},
		{
			name: "500 is transient, not repairable", status: 500,
			body: `{"error":{"message":"reasoning_content must be passed back"}}`,
			want: RepairNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyRepairable(c.status, c.body); got != c.want {
				t.Errorf("classifyRepairable(%d, %q) = %q, want %q", c.status, c.body, got, c.want)
			}
		})
	}
}

// ── the repair must CHANGE something, or it must refuse ───────────────────────────────────────────────────

// A REPAIR THAT CHANGES NOTHING MUST REFUSE. Resending identical bytes collects the identical error, so the
// caller would burn its budget on a problem that cannot resolve — this is the property that keeps the loop
// from being a spin.
func TestRepairRefusesWhenItWouldChangeNothing(t *testing.T) {
	// reasoning_content demanded, but the request carries no reasoning to replay.
	req := TurnRequest{Messages: []Message{
		{Role: RoleAssistant, Content: []Content{{Text: strPtrEmpty()}}},
	}}
	if changed := repairRequest(RepairReasoningContent, &req); changed {
		t.Error("a request with no reasoning must NOT report a change — resending it would fail identically")
	}
}

func TestRepairReasoningReportsAChangeWhenThereIsReasoningToReplay(t *testing.T) {
	r := "I should call the tool."
	req := TurnRequest{Messages: []Message{
		{Role: RoleAssistant, Content: []Content{{Reasoning: &r}}},
	}}
	if changed := repairRequest(RepairReasoningContent, &req); !changed {
		t.Error("a request WITH reasoning must report a change — that is what makes the resend worth doing")
	}
}

func TestRepairToolMessageOrderDropsOrphansAndReportsTheChange(t *testing.T) {
	req := TurnRequest{Messages: []Message{
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call-1", Name: "read"}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call-1", Content: "ok"}}}},
		// An assistant call with NO result, and a result with NO call: the two orphans providers reject.
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call-orphan", Name: "read"}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "result-orphan", Content: "x"}}}},
	}}
	if changed := repairRequest(RepairToolMessageOrder, &req); !changed {
		t.Fatal("dropping orphaned tool messages must report a change")
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == "call-orphan" {
				t.Error("an assistant tool call with no result survived the repair")
			}
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "result-orphan" {
				t.Error("a tool result with no call survived the repair")
			}
		}
	}
	// And the VALID pair is untouched — a repair that removes good history is worse than the error.
	var sawCall, sawResult bool
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == "call-1" {
				sawCall = true
			}
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "call-1" {
				sawResult = true
			}
		}
	}
	if !sawCall || !sawResult {
		t.Error("the repair removed a VALID tool call/result pair")
	}
}

// ── the serialiser: reasoning reaches the wire only when asked for ────────────────────────────────────────

func TestReasoningIsReplayedOnlyWhenTheProviderWantsIt(t *testing.T) {
	r := "thinking..."
	req := TurnRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{{Text: strPtrEmpty(), Reasoning: &r}}},
		},
	}

	// OFF (the default): the field must NOT be sent. It is not part of the OpenAI chat schema, and a provider
	// that does not expect it may reject the unknown field — trading one broken turn for another.
	off := buildOpenAIRequest(req, Quirks{})
	if got := off.Messages[0].ReasoningContent; got != "" {
		t.Errorf("reasoning was sent to a provider that does not ask for it: %q", got)
	}

	// ON: it must be sent, because this is the provider that REJECTS a request without it.
	on := buildOpenAIRequest(req, Quirks{ReplayReasoning: true})
	if got := on.Messages[0].ReasoningContent; got != r {
		t.Errorf("reasoning was not replayed to a provider that requires it: got %q, want %q", got, r)
	}
}

// ── end to end: a 400 that names reasoning_content is repaired and the turn SUCCEEDS ──────────────────────

func TestATurnRejectedForMissingReasoningIsRepairedAndResent(t *testing.T) {
	var calls atomic.Int32
	var sawReasoningOnResend atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if n == 1 {
			// First attempt: reject exactly as the provider does, and REQUIRE the field on the retry.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`))
			return
		}
		if strings.Contains(string(b), "reasoning_content") {
			sawReasoningOnResend.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	r := "I need to read the file first."
	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(),
		Retry: RetryPolicy{MaxAttempts: 1, Sleep: func(time.Duration) {}},
		Quirks: Quirks{},
	}
	_, err := c.StreamTurn(context.Background(), TurnRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{{Reasoning: &r}}},
			{Role: RoleUser, Content: []Content{{Text: strPtr("go on")}}},
		},
	})
	if err != nil {
		t.Fatalf("the turn failed despite a repairable error and reasoning to replay: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("the request was never resent (calls=%d)", calls.Load())
	}
	if !sawReasoningOnResend.Load() {
		t.Error("the resend did NOT carry reasoning_content — the repair did not reach the wire")
	}
}

// AND WHEN THERE IS NOTHING TO REPAIR, THE ERROR SURFACES — no infinite loop, no swallowed failure.
func TestARepairableErrorWithNothingToFixSurfaces(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`))
	}))
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(),
		Retry: RetryPolicy{MaxAttempts: 1, Sleep: func(time.Duration) {}},
	}
	// No reasoning anywhere in the request, so no repair can apply.
	_, err := c.StreamTurn(context.Background(), TurnRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: strPtr("hello")}}},
		},
	})
	if err == nil {
		t.Fatal("an unrepairable 400 must surface as an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the request was sent %d times; a repair that changes nothing must not be resent", n)
	}
}

// A TRANSIENT FAILURE IS STILL RETRIED, UNCHANGED — the repair loop must not have displaced that.
func TestTransientFailuresAreStillRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream busy"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(),
		Retry: RetryPolicy{MaxAttempts: 3, Sleep: func(time.Duration) {}},
	}
	if _, err := c.StreamTurn(context.Background(), TurnRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}}},
	}); err != nil {
		t.Fatalf("a 503 must be retried and succeed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one failure + one retry)", calls.Load())
	}
}

func TestRepairAttemptsAreBounded(t *testing.T) {
	// A provider that keeps returning a repairable error with something to fix must not spin forever: the
	// budget is repairAttempts, and the error surfaces after it.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized field: reasoning_effort"}}`))
	}))
	defer srv.Close()

	r := "some reasoning"
	c := &OpenAICompatClient{
		BaseURL: srv.URL, HTTP: srv.Client(),
		Retry: RetryPolicy{MaxAttempts: 1, Sleep: func(time.Duration) {}},
	}
	_, err := c.StreamTurn(context.Background(), TurnRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{{Reasoning: &r}}},
		},
	})
	if err == nil {
		t.Fatal("want an error once the repair budget is spent")
	}
	if n := calls.Load(); n > repairAttempts+1 {
		t.Errorf("the send loop ran %d times, want at most %d", n, repairAttempts+1)
	}
}

var _ = fmt.Sprintf
