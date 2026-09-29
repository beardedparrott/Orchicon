package claude

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// parse_usage_test.go drives the claude USAGE path on canned stream-json
// fixtures ONLY (no live session, no spend): the four Anthropic wire buckets
// must be recorded DISTINCTLY through the SHARED aigateway.UsageFromAnthropic
// transform, attributed to the right execution, and a replayed terminal result
// must not double-charge.

const (
	// Terminal `result` carrying the authoritative per-turn aggregate.
	claudeResultUsage = `{"type":"result","subtype":"success","session_id":"sess-9","num_turns":2,"result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":1000,"output_tokens":800,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}}`
	// Terminal `result` with NO usage object (the per-message fallback case).
	claudeResultNoUsage = `{"type":"result","subtype":"success","session_id":"sess-9","result":"ok","total_cost_usd":0.03}`
	// A per-message usage sample on the `assistant` message.
	claudeAssistantUsage = `{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":33,"cache_creation_input_tokens":44}}}`
	// The partial-message stream's opening sample for the SAME message id.
	claudeMessageStartUsage = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":33,"cache_creation_input_tokens":44}}}}`
)

// (a) A terminal `result` records ONE usage record with the four buckets
// DISTINCT, the anthropic provider, the claude adapter kind, and THIS
// execution's id.
func TestClaudeTerminalResultRecordsFourDistinctBuckets(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	if !feed(t, m, claudeResultUsage) {
		t.Fatal("terminal result did not return terminal=true")
	}
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want exactly one", rec.usage)
	}
	u := rec.usage[0]
	if u.PromptTokens != 1000 || u.CacheReadTokens != 5000 || u.CacheWriteTokens != 200 || u.CompletionTokens != 800 {
		t.Fatalf("buckets = %+v, want prompt=1000 cache_read=5000 cache_write=200 completion=800 (never flattened)", u)
	}
	if u.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic (without it the catalog can never price the record)", u.Provider)
	}
	if u.AdapterKind != "claude" {
		t.Fatalf("AdapterKind = %q, want claude", u.AdapterKind)
	}
	if u.Model != "claude-sonnet-5" {
		t.Fatalf("Model = %q, want the bare model id", u.Model)
	}
	if u.ExecutionID != "exec-t" {
		t.Fatalf("ExecutionID = %q, want exec-t (per-execution attribution)", u.ExecutionID)
	}
	if u.CostUSD != 0.02 {
		t.Fatalf("CostUSD = %v, want the adapter-reported 0.02 (repricing is the gateway's job)", u.CostUSD)
	}
}

// (b) A duplicated/replayed terminal result charges the execution ONCE.
func TestClaudeDuplicateResultChargesOnce(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeResultUsage, claudeResultUsage, claudeResultUsage)
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want exactly one (a replay must not double-charge)", rec.usage)
	}
}

// (c) usage arriving only on the `assistant` message (the terminal result
// carries none) records the accumulated per-message buckets.
func TestClaudeUsageOnlyOnAssistantFallsBackToPerMessageBuckets(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeAssistantUsage, claudeResultNoUsage)
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want exactly one", rec.usage)
	}
	u := rec.usage[0]
	if u.PromptTokens != 11 || u.CacheReadTokens != 33 || u.CacheWriteTokens != 44 || u.CompletionTokens != 22 {
		t.Fatalf("buckets = %+v, want the per-message sample 11/33/44/22", u)
	}
}

// (c2) when BOTH a per-message sample and the terminal aggregate are present,
// the aggregate is recorded ALONE — the per-message sample is never summed in.
func TestClaudeAggregateWinsAndIsNeverSummedWithPerMessage(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeAssistantUsage, claudeResultUsage)
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want exactly one", rec.usage)
	}
	u := rec.usage[0]
	if u.PromptTokens != 1000 || u.CacheReadTokens != 5000 || u.CacheWriteTokens != 200 || u.CompletionTokens != 800 {
		t.Fatalf("buckets = %+v, want the result AGGREGATE only (no double count)", u)
	}
}

// (c3) the partial-message stream emits the SAME message's usage twice
// (message_start, then the assistant message). They collapse to ONE sample.
func TestClaudePartialStreamMessageSamplesCollapseOnce(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeMessageStartUsage, claudeAssistantUsage, claudeResultNoUsage)
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want exactly one", rec.usage)
	}
	u := rec.usage[0]
	if u.PromptTokens != 11 || u.CacheReadTokens != 33 || u.CacheWriteTokens != 44 || u.CompletionTokens != 22 {
		t.Fatalf("buckets = %+v, want the message's usage ONCE (element-wise max, not summed)", u)
	}
}

// (c4) the per-turn accumulator resets at the turn boundary — turn 1's
// per-message usage never leaks into turn 2's fallback.
func TestClaudeUsageDoesNotLeakAcrossTurns(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeAssistantUsage, claudeResultNoUsage) // turn 1: fallback records 11/33/44/22
	feed(t, m, claudeResultNoUsage)                       // turn 2: nothing accumulated, nothing recorded
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want turn 1 only (accumulator reset at the boundary)", rec.usage)
	}
}

// (d) the recorded buckets must EQUAL the SHARED transform's canonical mapping
// of the same wire usage — a claude-only mapping creeping back in fails here.
func TestClaudeBucketsRouteThroughSharedTransform(t *testing.T) {
	want := aigateway.UsageFromAnthropic(aigateway.AnthropicUsage{
		InputTokens:              1000,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 200,
		OutputTokens:             800,
	}, aigateway.UsageInput{})

	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, claudeResultUsage)
	u := rec.usage[0]
	if u.PromptTokens != want.PromptTokens || u.CacheReadTokens != want.CacheReadTokens ||
		u.CacheWriteTokens != want.CacheWriteTokens || u.CompletionTokens != want.CompletionTokens {
		t.Fatalf("recorded buckets %d/%d/%d/%d != the SHARED transform's %d/%d/%d/%d",
			u.PromptTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CompletionTokens,
			want.PromptTokens, want.CacheReadTokens, want.CacheWriteTokens, want.CompletionTokens)
	}
}

// The provider/model attribution: bare when deps.Model is set, and resolved by
// stripping the adapter segment off the 3-segment model_ref when it is not.
func TestClaudeProviderAndModelAttribution(t *testing.T) {
	rec2 := &captureCallbacks{}
	m2 := NewMapper("exec-t2", rec2, MapperDeps{
		TenantID:      "t1",
		Manifest:      scheduler.ExecutionManifest{ModelRef: "claude/anthropic/claude-sonnet-4"},
		UsageRecorder: rec2.recordUsage,
		Log:           quietLogger(),
	})
	feed(t, m2, claudeResultUsage)
	if len(rec2.usage) != 1 {
		t.Fatalf("usage records = %+v, want one", rec2.usage)
	}
	if rec2.usage[0].Provider != "anthropic" || rec2.usage[0].Model != "claude-sonnet-4" {
		t.Fatalf("provider/model = %q/%q, want anthropic/claude-sonnet-4 (adapter segment stripped)",
			rec2.usage[0].Provider, rec2.usage[0].Model)
	}
}
