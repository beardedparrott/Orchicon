package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestClaudeLiveSmoke is the ONE permitted live Claude session: it exists to
// prove the real stream-json protocol still maps onto ExecutionCallbacks so a
// fixture drift cannot hide behind canned JSONL. It is OFF by default and
// never runs in CI — the whole mapper suite passes on fixtures alone.
//
//	ORCHICON_TEST_LIVE_CLAUDE=1 go test ./internal/claude/ -run TestClaudeLiveSmoke -v
//
// COST DISCIPLINE (the operator is on a $20/mo plan): the cheapest model, ONE
// prompt, a one-word reply, no tool loop, a 90 s deadline, and the reported
// cost is logged then asserted to be small. It also needs the `claude` CLI on
// PATH plus mounted credentials; when either is missing the test SKIPS rather
// than failing, so an offline run is never a false red.
func TestClaudeLiveSmoke(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("live Claude smoke is opt-in: set ORCHICON_TEST_LIVE_CLAUDE=1")
	}
	// Same contract as TestClaudeTaskTodoLiveSmoke: a missing CLI SKIPS, it is
	// never a false red. The gate opts a host IN; it does not assert that every
	// host (the runtime container included) has the binary and credentials.
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("the claude CLI is not installed in this environment: %v", err)
	}
	model := os.Getenv("ORCHICON_TEST_LIVE_CLAUDE_MODEL")
	if model == "" {
		// Cheapest tier, cheapest alias. Deliberately NOT a sonnet/opus.
		model = "claude-haiku-4-5"
	}
	rec := &captureCallbacks{}
	b := New(quietLogger())
	b.SetUsageRecorder(rec.recordUsage)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	manifest := scheduler.ExecutionManifest{
		ExecutionID: "exec-live-smoke",
		Goal:        "Reply with the single word: pong. Do not use any tool.",
		ModelRef:    "claude/anthropic/" + model,
		ProjectDir:  t.TempDir(),
	}
	err := b.Start(ctx, db.ExecutionRow{ID: "exec-live-smoke", TenantID: "t1"}, manifest, rec)
	if err != nil {
		t.Fatalf("live Start: %v", err)
	}
	if len(rec.results) != 1 {
		t.Fatalf("OnResult calls = %+v, want exactly one", rec.results)
	}
	if !rec.results[0].succeeded {
		t.Fatalf("live turn failed: %q", rec.results[0].errMsg)
	}
	if len(rec.usage) != 1 {
		t.Fatalf("usage records = %+v, want one", rec.usage)
	}
	u := rec.usage[0]
	t.Logf("live smoke: model=%s tokens_in=%d tokens_out=%d cache_read=%d cache_write=%d cost_usd=%.6f",
		u.Model, u.PromptTokens, u.CompletionTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD)
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		t.Error("no token usage reported — the stream mapping may have drifted")
	}
	// Attribution parity: the record must carry the pricing provider and the
	// claude adapter kind, or the resolver can never price it (no discoverer
	// needed — the offline catalog is the source on this plane).
	if u.Provider != "anthropic" {
		t.Errorf("usage Provider = %q, want anthropic", u.Provider)
	}
	if u.AdapterKind != "claude" {
		t.Errorf("usage AdapterKind = %q, want claude", u.AdapterKind)
	}
	// Cache buckets are reported distinctly (never flattened) and are never
	// negative; a fresh session usually reports cache_write > 0.
	if u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		t.Errorf("negative cache bucket: read=%d write=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.CostUSD > 0.05 {
		t.Errorf("live smoke cost %.4f USD, want a single cheap prompt", u.CostUSD)
	}
}

// TestClaudeTaskTodoLiveSmoke is the ONE permitted live session for the
// TODO-PARITY feature: it asks a real claude session for a two-item task list
// and proves the task-tracking tools actually stream and land on the todowrite
// envelope. It is gated by ORCHICON_TEST_LIVE_CLAUDE=1 AND NOTHING ELSE — the
// default suite is zero-cost — and it SKIPS (never fails) when the `claude` CLI
// is absent, so it is also the recorded opt-in verification.
//
//	ORCHICON_TEST_LIVE_CLAUDE=1 go test ./internal/claude/ -run TestClaudeTaskTodoLiveSmoke -v
//
// COST DISCIPLINE: cheapest model, ONE prompt, short reply, 120 s deadline; the
// reported cost is logged for the operator.
func TestClaudeTaskTodoLiveSmoke(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("live Claude task-todo smoke is opt-in: set ORCHICON_TEST_LIVE_CLAUDE=1")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("the claude CLI is not installed in this environment: %v", err)
	}
	model := os.Getenv("ORCHICON_TEST_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5" // cheapest tier, cheapest alias
	}
	rec := &captureCallbacks{}
	b := New(quietLogger())
	b.SetSessionStore(rec.recordParts)
	b.SetUsageRecorder(rec.recordUsage)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	manifest := scheduler.ExecutionManifest{
		ExecutionID: "exec-live-todo",
		Goal:        "Create a task list with exactly two items: 'alpha' with status in_progress, and 'beta' with status pending. Then stop.",
		ModelRef:    "claude/anthropic/" + model,
		ProjectDir:  t.TempDir(),
	}
	if err := b.Start(ctx, db.ExecutionRow{ID: "exec-live-todo", TenantID: "t1"}, manifest, rec); err != nil {
		t.Fatalf("live Start: %v", err)
	}

	s := rec.snap()
	parts := s.partsOfKind(db.SessionPartToolUse)
	found := false
	for _, p := range parts {
		body, _ := json.Marshal(p.payload)
		if strings.Contains(string(body), `"tool":"todowrite"`) && strings.Contains(string(body), `"todos"`) {
			found = true
			t.Logf("live task-todo smoke: todowrite envelope = %s", body)
		}
	}
	if !found {
		t.Errorf("no todowrite envelope part recorded across %d tool_use parts — the task tools did not stream", len(parts))
	}
	for _, u := range s.usage {
		t.Logf("live task-todo smoke: model=%s prompt=%d completion=%d cost_usd=%.6f", u.Model, u.PromptTokens, u.CompletionTokens, u.CostUSD)
	}
}
