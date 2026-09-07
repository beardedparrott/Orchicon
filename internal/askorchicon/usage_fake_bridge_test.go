package askorchicon

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/opencode"
)

// spyUsageRecorder captures every UsageInput passed to Record so an Ask
// turn's usage capture can be asserted field-by-field. It satisfies the
// Service's usageRecorder interface.
type spyUsageRecorder struct {
	mu  sync.Mutex
	in  []aigateway.UsageInput
	err error
}

func (s *spyUsageRecorder) Record(_ context.Context, in aigateway.UsageInput) (db.UsageRecordRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = append(s.in, in)
	return db.UsageRecordRow{}, s.err
}

func (s *spyUsageRecorder) inputs() []aigateway.UsageInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]aigateway.UsageInput, len(s.in))
	copy(out, s.in)
	return out
}

// busStepFinish builds the step-finish bus event the serve emits on step
// completion (the opencode tokens/cost shape LegacyEventFromBus maps to
// {type: "step_finish", part: {...}}). The tokens map mirrors the wire:
// {"input","output","reasoning"} plus a nested {"cache":{"read","write"}}.
func busStepFinish(sessionID string, tokens map[string]any, cost float64) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type":   "step-finish",
				"tokens": tokens,
				"cost":   cost,
				"time":   map[string]any{"start": 1.0, "end": 2.0},
			},
		},
	}
}

// collectTurnWithRecorder drives collectConversationReply over the fake
// session client with a usage recorder wired into the Service. It returns the
// collector's reply/error so tests can assert usage capture around a live turn.
func collectTurnWithRecorder(t *testing.T, client *fakeSessionClient, opts turnCollectOpts, spy *spyUsageRecorder) (string, error) {
	t.Helper()
	s := &Service{log: slog.Default(), turns: newTurnRegistry(), usageRecorder: spy}
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	done := make(chan struct{})
	var reply string
	var err error
	go func() {
		defer close(done)
		reply, _, _, err = s.collectConversationReply(context.Background(), opts)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("collectConversationReply did not return within 10s")
	}
	return reply, err
}

// TestAskUsageCaptureNonOpencodeAdapter is the fake-bridge usage aggregation
// test: an Ask session driven on a NON-opencode adapter (via the fake session
// client acting as that adapter's bridge) records a live usage sample with the
// correct adapter kind, provider/model attribution, token/cost buckets, and
// Ask-session (conversation) attribution. No claude claim — the non-opencode
// kind is a fake until a real non-opencode adapter lands.
func TestAskUsageCaptureNonOpencodeAdapter(t *testing.T) {
	client := &fakeSessionClient{}
	spy := &spyUsageRecorder{}
	// A non-opencode adapter kind (segment 1 ≠ "opencode"): a fake bridge
	// kind standing in until a real non-opencode adapter lands. The serve
	// grammar is adapter/provider/model.
	ref := "orbital/mockprov/deepseek-v4-flash"
	opts := turnCollectOpts{
		client: client, tenantID: "tnt_ask", convID: "conv_usage", sessionID: "ses_1",
		reuseSystem: "REUSE_SYSTEM", modelRef: ref, userMsg: "hello",
	}
	go func() {
		waitForSend(t, client, 1)
		client.sub.feed(busStepFinish("ses_1", map[string]any{
			"input":      float64(120),
			"output":     float64(45),
			"reasoning":  float64(10),
			"cache":      map[string]any{"read": float64(500), "write": float64(80)},
		}, 0.037))
		client.sub.feed(busIdle("ses_1"))
	}()
	if _, err := collectTurnWithRecorder(t, client, opts, spy); err != nil {
		t.Fatalf("collect error: %v", err)
	}

	got := spy.inputs()
	if len(got) != 1 {
		t.Fatalf("usage samples = %d, want 1 (one step_finish)", len(got))
	}
	in := got[0]
	// Adapter tagging: the resolved adapter kind from segment 1.
	if in.AdapterKind != "orbital" {
		t.Errorf("adapter_kind = %q, want %q", in.AdapterKind, "orbital")
	}
	// Attribution: provider/model split from the ref, adapter-agnostic.
	if in.Provider != "mockprov" || in.Model != "deepseek-v4-flash" {
		t.Errorf("attribution = %s/%s, want mockprov/deepseek-v4-flash", in.Provider, in.Model)
	}
	// Ask-session attribution: conversation id in SessionID (no execution/
	// task/project for a chat turn).
	if in.SessionID != "conv_usage" {
		t.Errorf("session_id = %q, want %q (Ask conversation)", in.SessionID, "conv_usage")
	}
	if in.TenantID != "tnt_ask" {
		t.Errorf("tenant_id = %q, want %q", in.TenantID, "tnt_ask")
	}
	if in.ExecutionID != "" || in.TaskID != "" || in.ProjectID != "" {
		t.Errorf("execution/task/project must stay empty for an Ask turn (got %q/%q/%q)", in.ExecutionID, in.TaskID, in.ProjectID)
	}
	// Token buckets mirror the live step_finish payload (live-usage-only).
	if in.PromptTokens != 120 || in.CacheReadTokens != 500 || in.CacheWriteTokens != 80 ||
		in.CompletionTokens != 45 || in.ReasoningTokens != 10 {
		t.Errorf("token buckets = %d/%d/%d/%d/%d, want 120/500/80/45/10",
			in.PromptTokens, in.CacheReadTokens, in.CacheWriteTokens, in.CompletionTokens, in.ReasoningTokens)
	}
	if in.CostUSD != 0.037 {
		t.Errorf("cost = %v, want 0.037", in.CostUSD)
	}
}

// TestAskUsageCaptureEmptySampleDropped verifies live-usage-only parity with
// the worker recorder: a step_finish with no tokens and no cost is dropped,
// never synthesized into a usage record.
func TestAskUsageCaptureEmptySampleDropped(t *testing.T) {
	client := &fakeSessionClient{}
	spy := &spyUsageRecorder{}
	opts := turnCollectOpts{
		client: client, tenantID: "tnt_ask", convID: "conv_empty", sessionID: "ses_1",
		reuseSystem: "REUSE_SYSTEM", modelRef: "orbital/mockprov/deepseek-v4-flash", userMsg: "hi",
	}
	go func() {
		waitForSend(t, client, 1)
		client.sub.feed(busStepFinish("ses_1", map[string]any{}, 0))
		client.sub.feed(busIdle("ses_1"))
	}()
	if _, err := collectTurnWithRecorder(t, client, opts, spy); err != nil {
		t.Fatalf("collect error: %v", err)
	}
	if got := spy.inputs(); len(got) != 0 {
		t.Errorf("usage samples = %d, want 0 (empty step_finish must be dropped)", len(got))
	}
}

// TestAskUsageCaptureNilRecorderNoUsage verifies that without a wired recorder
// Ask continues to record zero usage (the unchanged no-recorder state).
func TestAskUsageCaptureNilRecorderNoUsage(t *testing.T) {
	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, tenantID: "tnt_ask", convID: "conv_nil", sessionID: "ses_1",
		reuseSystem: "REUSE_SYSTEM", modelRef: "orbital/mockprov/deepseek-v4-flash", userMsg: "hi",
	}
	go func() {
		waitForSend(t, client, 1)
		client.sub.feed(busStepFinish("ses_1", map[string]any{"input": float64(10)}, 0.001))
		client.sub.feed(busIdle("ses_1"))
	}()
	// Nil recorder passed through — collectTurnWithRecorder wires the spy, so
	// build the Service directly to simulate the no-recorder configuration.
	s := &Service{log: slog.Default(), turns: newTurnRegistry()}
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, _, err = s.collectConversationReply(context.Background(), opts)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("collectConversationReply did not return within 10s")
	}
	if err != nil {
		t.Fatalf("collect error: %v", err)
	}
	// No recorder ⇒ no record path to hit; the test simply must not panic.
}
