package claude

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// bridge_usage_integration_test.go is QA's end-to-end usage check: a CANNED
// claude stream-json turn is driven through the REAL Bridge / session / Mapper
// path (fake proc, no live session, no spend), and the sampled usage must reach
// the bridge's UsageRecorder as ONE record with the four Anthropic buckets
// DISTINCT, attributed to THIS execution, through the SHARED transform.

const (
	// The partial-message stream's opening sample for msg_1.
	integMessageStart = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1000,"output_tokens":1,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}}}}`
	// The same message's full report (must COLLAPSE with the sample above).
	integAssistantUsage = `{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1000,"output_tokens":800,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}}}`
	// The authoritative terminal turn aggregate.
	integResult = `{"type":"result","subtype":"success","session_id":"sess-i","num_turns":1,"result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":1000,"output_tokens":800,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}}`
)

// TestBridgeUsageRecordEndToEndCannedStream asserts the whole bridge wiring:
// ParseLine → Mapper.Handle → MapperDeps.UsageRecorder (the bridge sink).
func TestBridgeUsageRecordEndToEndCannedStream(t *testing.T) {
	fp := newFakeProc()
	rec := &captureCallbacks{}
	done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)

	fp.push(initLine)
	fp.push(integMessageStart)
	fp.push(integAssistantUsage)
	fp.push(integResult)
	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %d (%+v), want exactly one (partial samples collapse, no double count)", len(rec.usage), rec.usage)
	}
	u := rec.usage[0]
	// Four DISTINCT buckets — never a flattened total.
	if u.PromptTokens != 1000 || u.CacheReadTokens != 5000 || u.CacheWriteTokens != 200 || u.CompletionTokens != 800 {
		t.Fatalf("buckets = %+v, want prompt=1000 cache_read=5000 cache_write=200 completion=800", u)
	}
	// Attribution through the REAL bridge: this execution, anthropic provider,
	// claude adapter — without Provider the catalog can never price the row.
	if u.ExecutionID != "exec-pm" {
		t.Fatalf("ExecutionID = %q, want exec-pm", u.ExecutionID)
	}
	if u.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic", u.Provider)
	}
	if u.AdapterKind != "claude" {
		t.Fatalf("AdapterKind = %q, want claude", u.AdapterKind)
	}
	if u.Model != "claude-sonnet-5" {
		t.Fatalf("Model = %q, want the bare model id", u.Model)
	}
	if u.TenantID != "t1" {
		t.Fatalf("TenantID = %q, want t1", u.TenantID)
	}
	if u.CostUSD != 0.02 {
		t.Fatalf("CostUSD = %v, want the adapter-reported 0.02 (repricing is the gateway's job)", u.CostUSD)
	}
}
