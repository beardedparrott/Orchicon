package aigateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestAnthropicUsageMapsToCanonical pins the canonical usage-sample
// contract (docs canonical-usage-sample-contract §6): an Anthropic / Claude
// Code usage object maps to the canonical UsageInput with the expected
// bucket semantics, the four-bucket total rule holds (reasoning NOT added),
// and the gateway's row-build is source-agnostic — it applies the same
// TotalTokens formula to the canonical input with no provider branch.
func TestAnthropicUsageMapsToCanonical(t *testing.T) {
	// 1. Anthropic-shaped sample (docs §3, §6): input_tokens=1000,
	//    cache_read_input_tokens=5000, cache_creation_input_tokens=200,
	//    output_tokens=800, reasoning_tokens=300.
	usage := AnthropicUsage{
		InputTokens:              1000,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 200,
		OutputTokens:             800,
		ReasoningTokens:          300,
	}
	base := UsageInput{
		TenantID:      "tnt_dev",
		ProjectID:     "prj_dev",
		TaskID:        "task_dev",
		ExecutionID:   "exec_dev",
		WorkerID:      "worker_dev",
		Provider:      "anthropic",
		Model:         "claude-sonnet-4",
		WorkflowRunID: "wr_dev",
	}

	// 2. Pure mapping to the canonical UsageInput.
	in := UsageFromAnthropic(usage, base)

	// Canonical buckets (docs §1). Identity/context fields carry through
	// untouched; cost/total are not set here (Record owns the total).
	if in.PromptTokens != 1000 || in.CacheReadTokens != 5000 ||
		in.CacheWriteTokens != 200 || in.CompletionTokens != 800 ||
		in.ReasoningTokens != 300 {
		t.Fatalf("UsageFromAnthropic bucket mismatch: %+v", in)
	}
	if in.TenantID != base.TenantID || in.ProjectID != base.ProjectID ||
		in.TaskID != base.TaskID || in.ExecutionID != base.ExecutionID ||
		in.WorkerID != base.WorkerID || in.Provider != base.Provider ||
		in.Model != base.Model || in.WorkflowRunID != base.WorkflowRunID {
		t.Fatalf("UsageFromAnthropic dropped identity fields: %+v", in)
	}

	// 3. Total rule: TotalTokens = Prompt + CacheRead + CacheWrite +
	//    Completion. Reasoning (300) is a sub-bucket of output and is NOT
	//    added — providers that report it separately bill it as output, so
	//    adding it would double-count. Assert the invariant on the canonical
	//    sample (the sum the gateway must derive).
	if got := in.PromptTokens + in.CacheReadTokens + in.CacheWriteTokens + in.CompletionTokens; got != 7000 {
		t.Fatalf("total rule: expected 7000, got %d", got)
	}

	// 4. MANDATORY: the gateway is source-agnostic. Feeding the same
	//    canonical input through UsageRecorder.Record must produce a row
	//    with identical token buckets and TotalTokens=7000 (the four-bucket
	//    sum, reasoning excluded). This enforces the "zero gateway logic
	//    change" property: with no provider branch, the gateway's single
	//    TotalTokens formula yields the canonical total.
	rec := NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if row.PromptTokens != 1000 || row.CacheReadTokens != 5000 ||
		row.CacheWriteTokens != 200 || row.CompletionTokens != 800 ||
		row.ReasoningTokens != 300 {
		t.Fatalf("row bucket mismatch: %+v", row)
	}
	if row.TotalTokens != 7000 {
		t.Fatalf("row total: expected 7000 (Prompt+CacheRead+CacheWrite+Completion, reasoning excluded), got %d", row.TotalTokens)
	}
}
