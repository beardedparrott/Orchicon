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
		// Turn 1: a tool-call round — the executed tool call ARMS the
		// decision-signal gate (probe-startup guard). StopToolUse never
		// settles; the loop continues.
		{events: []Event{TextDelta{Text: "Work done."}, ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
		// Turn 2: markerless StopStop settle AFTER work began → the gate
		// fires the completion probe.
		{events: []Event{TextDelta{Text: "Summarizing next."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 110, OutputTokens: 10}},
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
	// Three provider turns: the tool round (arms the gate) + the
	// markerless settle + the probe turn. The probe interjection itself
	// does not consume a provider call.
	if got := prov.requestCount(); got != 3 {
		t.Errorf("StreamTurn calls = %d, want 3 (tool round + settle + probe turn)", got)
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

// AC: a model that goes SILENT through the full probe budget — probe turns
// that produce NOTHING (no deltas, no tokens: consecutive UNANSWERED
// probes) — fails honestly (stalled:missing_decision_signal:…), never
// succeeds. 2026-09-09 liveness-kill fix: the budget counts consecutive
// UNANSWERED probes; a session that keeps replying keeps getting probes
// (bounded by the wall-clock budget ladder instead).
func TestQADecisionGateFailsAfterProbeBudget(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: a tool-call round — ARMS the gate (probe-startup guard).
		{events: []Event{TextDelta{Text: "Work done."}, ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 25}},
		// Turn 2: markerless StopStop settle AFTER work began → probe #1.
		{events: []Event{TextDelta{Text: "Summarizing next."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 110, OutputTokens: 10}},
		// Probe 1: EMPTY reply (no deltas, no tokens) — unanswered, and the
		// defer path re-queues (no provider call consumed).
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 120, OutputTokens: 0}},
		// Probe 1 (re-queued): STILL empty → deferral bound spent → the
		// gate falls through, probe #1's slot stays spent, probe #2 fires.
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 130, OutputTokens: 0}},
		// Probe 2: EMPTY again → probe budget exhausted → honest failure.
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 140, OutputTokens: 0}},
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
	// 4 provider calls: tool round (arms the gate) + markerless settle
	// (fires probe 1) + probe-1's empty reply + the re-queued probe-1's
	// second empty reply → the deferral bound (2) fails the session
	// HONESTLY at the defer path, before probe #2 ever fires. This is the
	// empty-provider bound: never an infinite defer loop, never an
	// unbounded probe cycle.
	if got := prov.requestCount(); got != 4 {
		t.Errorf("StreamTurn calls = %d, want 4 (tool round + settle + 2 empty probe replies)", got)
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
	// StopLength recovery parity: the turn continues via a bounded
	// continuation (the mock provider exhausts its scripted turns → the
	// continuation turn's pre-stream failure fails the execution). The
	// failure carries the length-continuation message, never a hollow
	// success.
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "Maybe also handle `"}}, finish: StopLength, usage: Usage{InputTokens: 100, OutputTokens: 4096}, bare: true},
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
	if len(results) == 1 && !strings.Contains(results[0].errMsg, "length") && !strings.Contains(results[0].errMsg, "provider stream failed") {
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

// AC (2026-09-09 liveness-kill regression, operator-transcript parity): a
// probe that gets a REPLY must not spend the next budget slot or fail the
// session outright. Tonight's kill: probe #1 → model answers with a status
// line ("Still here — resuming…") → the next markerless settle burned
// probe #2 → instant missing_decision_signal failure inside ~11s on a
// session that was actively replying. With the fix the budget counts
// CONSECUTIVE UNANSWERED probes: every content reply resets it, so a
// session that keeps replying keeps streaming (the wall-clock budget
// ladder, not this gate, bounds a never-delivering session). The mock
// running OUT OF TURNS while the session is still alive is the proof.

// AC (parity): a probe whose provider turn returns EMPTY (zero deltas,
// immediate StopStop) must not loop forever on the defer path — one
// deferral retry, then the honest completion_probe_no_response failure.
func TestQADecisionGateEmptyProbeTurnFailsHonestly(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: a tool-call round — ARMS the gate (probe-startup guard).
		{events: []Event{TextDelta{Text: "Starting."}, ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 20}},
		// Turn 2: markerless StopStop settle AFTER work began → probe #1.
		{events: []Event{TextDelta{Text: "Summarizing next."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 110, OutputTokens: 10}},
		// Turn 3: probe #1's reply is EMPTY (deltas dropped, immediate
		// StopStop) — the defer path re-queues the probe (no new provider
		// turn is consumed by the defer itself).
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 0}},
		// Turn 4: the re-queued probe's turn — STILL empty → deferral bound
		// spent → honest failure. (The bound fires when the settle re-enters
		// with awaiting still set and deferrals > 1.)
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 0}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 {
		t.Fatalf("OnResult count = %d, want 1", len(results))
	}
	if results[0].succeeded {
		t.Error("OnResult succeeded — an empty probe turn must fail honestly, never a hollow success")
	}
	if !strings.Contains(results[0].errMsg, "missing_decision_signal") {
		t.Errorf("errMsg = %q, want missing_decision_signal", results[0].errMsg)
	}
}

// AC (2026-09-09 probe-loop fix): a probe reply that is a BARE STATUS LINE
// (no marker, no WORKING token) is NOT an answer — it must NOT reset the
// probe budget. The budget counts LIFETIME probes now: intro + probe +
// status reply + probe + status reply = budget spent → honest
// completion_probe_no_response failure after exactly 2 probes. This is the
// regression for the 15-probe / zero-tool-call loop
// (transcript 01M23C2MTF1ZYYHE8ACK17PMKV): the worker must never be trapped
// answering probes instead of working.
func TestQADecisionGateStatusLineReplyDoesNotResetBudget(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: a tool-call round — ARMS the gate (probe-startup guard).
		{events: []Event{TextDelta{Text: "Starting the work."}, ToolCallStart{Index: 0, ToolCallID: "t1", Name: "noop"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 20}},
		// Turn 2: markerless StopStop settle AFTER work began → probe #1.
		{events: []Event{TextDelta{Text: "Summarizing next."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 110, OutputTokens: 10}},
		// Turn 3: probe #1's reply — a bare status line, no marker, no
		// WORKING token. NOT an answer: the budget stays spent.
		{events: []Event{TextDelta{Text: "I'll re-sync state before continuing."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 130, OutputTokens: 15}},
		// Turn 4: probe #2 fires (budget 2/2) → its reply is another status
		// line → budget exhausted → honest failure (never an infinite
		// probe loop).
		{events: []Event{TextDelta{Text: "I need to re-sync my state."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 150, OutputTokens: 12}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	// Bounded: call1 = tool round (arms), call2 = markerless settle →
	// probe #1, call2's reply = status line (spends the slot), probe #2 →
	// call4 = status-line reply → budget exhausted → honest failure. The
	// probe interjections themselves consume no provider calls. NEVER an
	// unbounded probe loop.
	if got := prov.requestCount(); got != 4 {
		t.Errorf("StreamTurn calls = %d, want 4 (tool round + settle + 2 probe-reply cycles, then fail)", got)
	}
	if len(results) != 1 || results[0].succeeded {
		t.Errorf("OnResult = %+v, want honest failure (status-line replies never answer the probe)", results)
	}
	if len(results) == 1 && !strings.Contains(results[0].errMsg, "missing_decision_signal") {
		t.Errorf("errMsg = %q, want missing_decision_signal", results[0].errMsg)
	}
}

// AC (2026-09-09 probe-loop fix): the probe's WORKING token IS a valid
// answer — it disarms the gate (budget reset + awaiting cleared) so the
// model continues its real work and delivers the marker on a later settle.
// No kill, no probe spam.
func TestQADecisionGateWorkingTokenDisarmsGate(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: markerless intro → probe #1.
		{events: []Event{TextDelta{Text: "Starting the work."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 20}},
		// Turn 2: probe #1's reply is the WORKING token → gate disarmed
		// (awaiting cleared, budget reset).
		{events: []Event{TextDelta{Text: "WORKING"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 130, OutputTokens: 2}},
		// Turn 3: the model continues real work and settles markerless →
		// a fresh probe fires (budget was reset, this is probe #1 again).
		{events: []Event{TextDelta{Text: "work segment done"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 150, OutputTokens: 10}},
		// Turn 4: probe's reply carries the REAL marker → success.
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — did the thing"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 170, OutputTokens: 12}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success (WORKING disarmed the gate; marker on the final settle)", results)
	}
	for _, r := range results {
		if strings.Contains(r.errMsg, "missing_decision_signal") {
			t.Errorf("missing_decision_signal after a WORKING answer: %q", r.errMsg)
		}
	}
}

// Unit pins for completionProbeReply classification (probe-loop fix).
func TestCompletionProbeReplyClassification(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{"marker", "text ORCHICON WORKER SUMMARY: success — did it", probeReplySummary},
		{"working token", "WORKING", probeReplyWorking},
		{"working token lowercase", "working", probeReplyWorking},
		{"working with punctuation", "*WORKING*", probeReplyWorking},
		{"status line is NOT an answer", "I'll re-sync state and continue.", probeReplyNone},
		{"working embedded in a word", "I am networking with the team.", probeReplyNone},
		{"empty", "", probeReplyNone},
		{"placeholder echo is not a real marker", "ORCHICON WORKER SUMMARY: success — <summary>", probeReplyNone},
	}
	for _, tc := range cases {
		got, idx := completionProbeReply(tc.output)
		if got != tc.want {
			t.Errorf("completionProbeReply(%q) = kind %d idx %d, want kind %d", tc.output, got, idx, tc.want)
		}
		if tc.want == probeReplyNone && idx != -1 {
			t.Errorf("probeReplyNone must carry idx -1, got %d", idx)
		}
	}
}

// AC (2026-09-09 probe-startup guard): a markerless settle BEFORE the
// session has executed any tool call must NOT fire the completion probe.
// The incident: the probe fired 6s after dispatch — before the model had
// streamed a single token — and demanded summary-or-WORKING from a worker
// that never got to start. Now the gate only arms from the first executed
// tool call; a pre-work markerless settle just settles the turn and the
// loop continues (the model gets another plain turn to actually begin).
func TestQADecisionGateStartupGuardNoProbeBeforeWork(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: markerless intro (no tool calls) — the gate must NOT
		// probe here; the loop continues and consumes turn 2.
		{events: []Event{TextDelta{Text: "Planning the approach."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 20}},
		// Turn 2: the model actually begins — a tool call round.
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "bash"}, ToolCallDelta{Index: 0, ArgsJSONDelta: "{}"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 120, OutputTokens: 10}},
		// Turn 3: markerless text settle AFTER work began — the gate is
		// armed now, so the completion probe fires.
		{events: []Event{TextDelta{Text: "Still verifying."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 140, OutputTokens: 8}},
		// Probe turn: delivers the marker → success.
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — completed the task"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 160, OutputTokens: 12}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	// The run must SUCCEED — the startup guard let the model begin (turn 2
	// tool call) and the marker arrived on the later settle.
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success (startup guard never probed the pre-work settle)", results)
	}
	for _, r := range results {
		if strings.Contains(r.errMsg, "missing_decision_signal") {
			t.Errorf("missing_decision_signal with the startup guard active: %q", r.errMsg)
		}
	}
	// 4 provider calls: intro + tool turn + post-work settle + probe turn.
	if got := prov.requestCount(); got != 4 {
		t.Errorf("StreamTurn calls = %d, want 4 (intro + tool turn + post-work settle + probe turn)", got)
	}
}

// AC (2026-09-09 probe-loop fix, startup guard): the exact incident shape —
// a worker whose FIRST turn is empty/instant (zero deltas, immediate
// StopStop) and never executes a tool call must NOT be probed. The old
// gate read the empty first turn as a "cut-off summary" and fired the
// probe 6s after dispatch. Now the markerless settle before any work
// simply continues; the session stays alive and gets its next turn.
func TestQADecisionGateEmptyFirstTurnNotProbed(t *testing.T) {
	prov := &mockProvider{turns: []scriptedTurn{
		// Turn 1: EMPTY instant turn (the incident's first turn).
		{events: []Event{}, finish: StopStop, bare: true, usage: Usage{InputTokens: 50, OutputTokens: 0}},
		// Turn 2: the model now actually begins — tool call.
		{events: []Event{ToolCallStart{Index: 0, ToolCallID: "t1", Name: "bash"}, ToolCallEnd{Index: 0}}, finish: StopToolUse, bare: true, usage: Usage{InputTokens: 70, OutputTokens: 5}},
		// Turn 3: settles markerless after work → probe fires.
		{events: []Event{TextDelta{Text: "Continuing."}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 90, OutputTokens: 6}},
		// Probe turn: marker → success.
		{events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: success — done"}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 110, OutputTokens: 10}},
	}}
	s := qaSession(t, prov, nil)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Errorf("OnResult = %+v, want success (empty first turn must not trigger the probe)", results)
	}
	for _, r := range results {
		if strings.Contains(r.errMsg, "missing_decision_signal") {
			t.Errorf("missing_decision_signal on the incident shape: %q", r.errMsg)
		}
	}
}
