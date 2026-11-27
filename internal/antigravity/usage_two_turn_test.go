package antigravity

// usage_two_turn_test.go — AC4: after a TWO-TURN conversation, the usage layer holds exactly two rows whose
// token sums equal the two turns' accounting, with no double count.
//
// THE REGIME IS THE POINT. agy's headless mode reports usage CUMULATIVELY over the session
// (https://antigravity.google/docs/cli/headless), but the ACP `session/prompt` RESULT — the only usage
// source this adapter reads — is PER-TURN: each result carries that turn's own buckets
// (docs/adapters/antigravity-acp.md:157-159, testdata/acp/simple_turn.jsonl:18). So NO delta conversion is
// applied anywhere in the mapper, and this test pins that: if someone "fixed" a perceived double count by
// subtracting the previous turn, the sum here would come out short.

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestMapper_TwoTurnsRecordTwoRowsWithTheTurnsOwnTotals drives the mapper the way a real two-turn
// conversation does — one `session/prompt` result per turn, each carrying its OWN per-turn usage — and
// asserts the rows the usage layer would report.
func TestMapper_TwoTurnsRecordTwoRowsWithTheTurnsOwnTotals(t *testing.T) {
	h := newHarness(t)
	var recorded []scheduler.UsageRecord
	m := NewMapper("exe_two_turn", h.cbs, MapperDeps{
		TenantID: "tnt_two_turn",
		ExecDir:  h.worktree,
		Model:    "gemini-4-carbon-high",
		Manifest: h.manifest,
		UsageRecorder: func(_ context.Context, rec scheduler.UsageRecord) error {
			recorded = append(recorded, rec)
			return nil
		},
		// The session's auth method decides the billing regime; oauth-personal is plan quota. It is
		// asserted below so the two-turn proof also pins that EVERY turn of the session carries it.
		AuthMethod: "oauth-personal",
		Log:        quiet(),
	})

	// Turn 1 is the RECORDED turn: testdata/acp/simple_turn.jsonl:18's prompt result, with the buckets the
	// recording carried (input 9064, output 1, no cache, no thought).
	turn1 := acp.PromptResult{
		StopReason: acp.StopReasonEndTurn,
		Usage: acp.Usage{
			InputTokens: 9064, OutputTokens: 1, TotalTokens: 9065,
		},
	}
	// Turn 2 is a second, DISTINCT turn: its input includes turn 1's output (a real continuation grows),
	// and it reports thinking tokens, which is how agy bills reasoning (there is no reasoning text).
	turn2 := acp.PromptResult{
		StopReason: acp.StopReasonEndTurn,
		Usage: acp.Usage{
			InputTokens:       9130,
			OutputTokens:      240,
			CachedReadTokens:  9065,
			CachedWriteTokens: 0,
			ThoughtTokens:     137,
			TotalTokens:       18572,
		},
	}

	m.RecordTurn(context.Background(), &turn1)
	m.RecordTurn(context.Background(), &turn2)

	if len(recorded) != 2 {
		t.Fatalf("usage rows = %d, want 2 (one per turn): %+v", len(recorded), recorded)
	}

	// The sum of the two per-turn results IS the session total, because ACP usage is per-turn. The
	// cumulative figure agy would report in headless mode is exactly this — 9065 + 9507 — so a delta
	// conversion applied on top would halve it.
	wantPrompt := turn1.Usage.InputTokens + turn2.Usage.InputTokens
	wantCompletion := turn1.Usage.OutputTokens + turn2.Usage.OutputTokens
	wantCacheRead := turn1.Usage.CachedReadTokens + turn2.Usage.CachedReadTokens
	wantReasoning := turn1.Usage.ThoughtTokens + turn2.Usage.ThoughtTokens

	var gotPrompt, gotCompletion, gotCacheRead, gotReasoning int64
	for _, r := range recorded {
		gotPrompt += r.PromptTokens
		gotCompletion += r.CompletionTokens
		gotCacheRead += r.CacheReadTokens
		gotReasoning += r.ReasoningTokens
		if r.Provider != "google" {
			t.Errorf("row provider = %q, want google", r.Provider)
		}
		if r.Model != "gemini-4-carbon-high" {
			t.Errorf("row model = %q, want the BARE agy model id", r.Model)
		}
		if r.Billing != db.BillingPlanQuota {
			t.Errorf("row billing = %q, want %q — oauth-personal is plan quota, and EVERY turn of the "+
				"session must carry the marker or the cost explorer would price one of them", r.Billing, db.BillingPlanQuota)
		}
	}
	if gotPrompt != wantPrompt {
		t.Errorf("prompt tokens across rows = %d, want %d (the two per-turn inputs)", gotPrompt, wantPrompt)
	}
	if gotCompletion != wantCompletion {
		t.Errorf("completion tokens across rows = %d, want %d", gotCompletion, wantCompletion)
	}
	if gotCacheRead != wantCacheRead {
		t.Errorf("cache-read tokens across rows = %d, want %d", gotCacheRead, wantCacheRead)
	}
	// agy's thoughtTokens is billed reasoning; it lands on the existing reasoning_tokens column.
	if gotReasoning != wantReasoning {
		t.Errorf("reasoning tokens across rows = %d, want %d (agy's thoughtTokens)", gotReasoning, wantReasoning)
	}
	if wantReasoning == 0 {
		t.Fatal("the fixture carries no thought tokens, so the reasoning assertion proves nothing")
	}

	// NO DOUBLE COUNT: the per-turn rows must sum to the session total, not to twice it.
	sessionTotal := int64(0)
	for _, u := range []acp.Usage{turn1.Usage, turn2.Usage} {
		sessionTotal += u.InputTokens + u.OutputTokens + u.CachedReadTokens + u.CachedWriteTokens + u.ThoughtTokens
	}
	var rowTotal int64
	for _, r := range recorded {
		rowTotal += r.PromptTokens + r.CompletionTokens + r.CacheReadTokens + r.CacheWriteTokens + r.ReasoningTokens
	}
	if rowTotal != sessionTotal {
		t.Fatalf("sum of row tokens = %d, want %d (the session total) — a mismatch means either a double "+
			"count or a delta conversion was applied to already-per-turn figures", rowTotal, sessionTotal)
	}
}
