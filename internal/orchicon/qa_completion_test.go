package orchicon

// Decision-signal gate + completion probe tests (opencode parity): a
// native session that settles WITHOUT a real ORCHICON WORKER SUMMARY must
// never record success. Covers the gate (bare StopStop → probe → marker →
// success), the honest-failure path (probe budget exhausted), and
// StopLength/StopOther never settling as success.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)
// AC: a bare StopStop turn (no marker) is NOT settled as success — the
// completion probe fires (one extra provider turn), the probe turn
// delivers the marker, and the session settles with the marker in output.
func TestQADecisionGateProbesForMissingMarker(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Work done."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
		// Probe turn: the model delivers the sign-off.
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — completed the QA scenario"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 120, OutputTokens: 30}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, text, _, _, stalls, _, results := cb.snapshot()
	if len(stalls) != 0 {
		t.Errorf("stalls = %v, want none (probe path, not a stall path)", stalls)
	}
	if got := strings.Join(text, ""); !strings.Contains(got, "Work done.") {
		t.Errorf("OnText = %q, want the model's turn text", got)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success after the probe delivered the marker", results)
	}
	if len(results) == 1 && !strings.Contains(results[0].output, "ORCHICON WORKER SUMMARY: success") {
		t.Errorf("OnResult output missing marker: %q", results[0].output)
	}
	// Two provider turns: the model turn + the probe turn.
	if got := prov.requestCount(); got != 2 {
		t.Errorf("StreamTurn calls = %d, turn+probe turn", got)
	}
	// The probe turn's history must contain the probe text as a user message.
	req := prov.lastRequest()
	found := false
	for _, m := range req.Messages {
		if m.Role == RoleUser {
			for _, c := range m.Content {
				if c.Text != nil && strings.Contains(*c.Text, "cut off before your final ORCHICON WORKER SUMMARY") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("probe text missing from probe-turn history: %+v", req.Messages)
	}
}

// AC: a model that never delivers the marker — even after the probe budget
// — fails honestly (stalled:missing_decision_signal:…), never succeeds.
func TestQADecisionGateFailsAfterProbeBudget(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Work done."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
		// Probe 1: still no marker.
		{events: []Event{TextDelta{Text: "Hmm, more monologue."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 120, OutputTokens: 25}},
		// Probe 2: still no marker → budget exhausted → honest failure.
		{events: []Event{TextDelta{Text: "More monologue."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 140, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, stalls, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("OnResult = %+v, want honest failure (probe budget exhausted without marker)", results)
	}
	if len(results) == 1 && !strings.Contains(results[0].errMsg, "missing_decision_signal") {
		t.Errorf("errMsg = %q, want missing_decision_signal", results[0].errMsg)
	}
	// Probe budget: exactly 2 probe turns (completionProbeMaxTurns) after
	// the model turn → 3 StreamTurn calls total.
	if got := prov.requestCount(); got != 3 {
		t.Errorf("StreamTurn calls = %d, want 3 (turn + 2 probes)", got)
	}
	_ = stalls
}

// AC: a REAL marker delivered on the bare StopStop turn itself settles
// immediately — no probe.
func TestQADecisionGateRealMarkerSettles(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — all acceptance criteria met"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want immediate success (real marker present)", results)
	}
	if got := prov.requestCount(); got != 1 {
		t.Errorf("StreamTurn calls = %d, want 1 (no probe needed)", got)
	}
}

// AC: a template-echo marker ("success — <summary>") is NOT a real sign-off
// — the gate probes rather than settling on the placeholder.
func TestQADecisionGatePlaceholderMarkerProbes(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Plan: reply with ORCHICON WORKER SUMMARY: success — <summary> at the end."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
		// Probe turn delivers a real marker.
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — done"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 120, OutputTokens: 25}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success (probe delivered a real marker)", results)
	}
	if got := prov.requestCount(); got != 2 {
		t.Errorf("StreamTurn calls = %d, want 2 (placeholder did not settle; probe fired)", got)
	}
}

// AC: StopLength never settles as success — the 4096-cap mid-monologue
// truncation shape (the reported hollow successes) must fail.
func TestQADecisionGateStopLengthFails(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Maybe also handle `"}}, finish: StopLength, usage: Usage{InputTokens: 100, OutputTokens: 4096}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("StopLength without marker = %+v, want failure", results)
	}
	if len(results) == 1 && !strings.Contains(results[0].errMsg, `"length"`) {
		t.Errorf("errMsg = %q, want the stop-reason failure", results[0].errMsg)
	}
}

// AC: StopOther never settles as success — the honest terminal for a
// stream that ended without any provider stop signal.
func TestQADecisionGateStopOtherFails(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "partial"}}, finish: StopOther, usage: Usage{InputTokens: 10, OutputTokens: 5}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("StopOther = %+v, want failure (no provider stop signal)", results)
	}
}

// AC: a truncated marker (mid-marker cut, StopLength) in output is not a
// settle signal — the gate fails the turn rather than trusting a partial
// sign-off.
func TestQADecisionGateTruncatedMarkerFails(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — all"}}, finish: StopLength, usage: Usage{InputTokens: 100, OutputTokens: 4096}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("Truncated marker = %+v, want failure (partial sign-off is not a completed turn)", results)
	}
}
// --- provider-stream regression tests (all families) ------------------------

// AC: openaicompat stream ends WITHOUT finish_reason/[DONE] (proxy drop /
// connection close): flush() now reports StopOther — the honest terminal —
// instead of the synthesized StopStop that recorded hollow successes.
func TestQAOpenAICompatNoStopReasonYieldsStopOther(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial response\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL + "/v1", AuthStyle: "none",
		Quirks: Quirks{SupportsToolCalls: true, UsageInFinalChunk: true},
	}
	stream, err := c.StreamTurn(context.Background(), TurnRequest{
		Model:    "test-model",
		System:   []SystemBlock{{Text: "sys"}},
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}}},
	})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	defer stream.Close()

	var finish *Finish
	for {
		ev, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !more {
			break
		}
		if f, ok := ev.(Finish); ok {
			finish = &f
		}
	}
	if finish == nil {
		t.Fatalf("no Finish event")
	}
	if finish.StopReason != StopOther {
		t.Errorf("StopReason = %q, want StopOther (stream ended without a provider stop signal)", finish.StopReason)
	}
}

// AC: a REAL finish_reason "stop" still maps to StopStop end-to-end.
func TestQAOpenAICompatRealStopYieldsStopStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &OpenAICompatClient{
		BaseURL: srv.URL + "/v1", AuthStyle: "none",
		Quirks: Quirks{SupportsToolCalls: true, UsageInFinalChunk: true},
	}
	stream, err := c.StreamTurn(context.Background(), TurnRequest{
		Model:    "test-model",
		System:   []SystemBlock{{Text: "sys"}},
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}}},
	})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	defer stream.Close()

	var finish *Finish
	for {
		ev, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !more {
			break
		}
		if f, ok := ev.(Finish); ok {
			finish = &f
		}
	}
	if finish == nil {
		t.Fatalf("no Finish event")
	}
	if finish.StopReason != StopStop {
		t.Errorf("StopReason = %q, want StopStop", finish.StopReason)
	}
}

// AC: ollama stop-reason mapping — "" (stream cut before the done
// envelope) is StopOther; real reasons are preserved.
func TestQAOllamaDoneReasonMapping(t *testing.T) {
	if got := mapOllamaDone(""); got != StopOther {
		t.Errorf("mapOllamaDone(\"\") = %q, want StopOther", got)
	}
	if got := mapOllamaDone("stop"); got != StopStop {
		t.Errorf("mapOllamaDone(\"stop\") = %q, want StopStop", got)
	}
	if got := mapOllamaDone("length"); got != StopLength {
		t.Errorf("mapOllamaDone(\"length\") = %q, want StopLength", got)
	}
	if got := mapOllamaDone("load"); got != StopOther {
		t.Errorf("mapOllamaDone(\"load\") = %q, want StopOther", got)
	}
}

// AC: anthropic message_stop WITHOUT a prior message_delta stop_reason is
// StopOther (truncation shape), while end_turn remains StopStop.
func TestQAAnthropicMessageStopWithoutReasonYieldsStopOther(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// message_start → text delta → message_stop with NO message_delta
		// stop_reason (the truncation shape).
		sseData := func(ev, data string) {
			_, _ = w.Write([]byte("event: " + ev + "\ndata: " + data + "\n\n"))
			w.(http.Flusher).Flush()
		}
		sseData("message_start", `{"type":"message_start","message":{"id":"m1","role":"assistant"}}`)
		sseData("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		sseData("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)
		sseData("message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	c := &AnthropicClient{BaseURL: srv.URL, AuthStyle: "none", APIKey: "test"}
	stream, err := c.StreamTurn(context.Background(), TurnRequest{
		Model:    "claude-test",
		System:   []SystemBlock{{Text: "sys"}},
		Messages: []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}}},
	})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	defer stream.Close()

	var finish *Finish
	for {
		ev, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !more {
			break
		}
		if f, ok := ev.(Finish); ok {
			finish = &f
		}
	}
	if finish == nil {
		t.Fatalf("no Finish event")
	}
	if finish.StopReason != StopOther {
		t.Errorf("StopReason = %q, want StopOther (message_stop without stop_reason)", finish.StopReason)
	}
	if got := mapAnthropicStop("end_turn"); got != StopStop {
		t.Errorf("mapAnthropicStop(\"end_turn\") = %q, want StopStop", got)
	}
	if got := mapAnthropicStop(""); got != StopOther {
		t.Errorf("mapAnthropicStop(\"\") = %q, want StopOther", got)
	}
}

// AC: legacycc stream ends without a finish event → flush reports
// StopOther (honest terminal).
func TestQALegacyCCFlushWithoutFinishYieldsStopOther(t *testing.T) {
	if got := mapLegacyStop("stop"); got != StopStop {
		t.Errorf("mapLegacyStop(\"stop\") = %q, want StopStop", got)
	}
	if got := mapLegacyStop(""); got != StopOther {
		t.Errorf("mapLegacyStop(\"\") = %q, want StopOther", got)
	}
}
