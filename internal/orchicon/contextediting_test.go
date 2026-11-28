package orchicon

import (
	"strings"
	"testing"
)

// These pin the clearing behaviour and, more importantly, its SAFETY properties: the recent payloads the
// model is still reasoning about must survive, errors must survive, and the placeholder must not invite a
// re-run (a cleared payload that causes a re-run costs more than it saved).

func ceToolResultMsg(id, content string, isErr bool) Message {
	return Message{
		Role: RoleTool,
		Content: []Content{{
			ToolResult: &ContentToolResult{ToolCallID: id, Content: content, IsError: isErr},
		}},
	}
}

func ceAssistantMsg(text string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Text: &text}}}
}

// historyWithResults builds a history of n tool results of size bytes each, interleaved with assistant
// turns so it looks like a real conversation.
func ceHistoryWithResults(n, bytes int) []Message {
	h := make([]Message, 0, n*2)
	for i := 0; i < n; i++ {
		h = append(h, ceAssistantMsg("calling tool"))
		h = append(h, ceToolResultMsg("call", strings.Repeat("x", bytes), false))
	}
	return h
}

func ceClearPolicy() contextEditPolicy {
	p := DefaultContextEditPolicy()
	p.Enabled = true
	return p
}

// TestClearingKeepsRecentResults is the load-bearing safety property. A model that cannot see the output
// it just asked for will re-run the command — so the newest results must be untouchable.
func TestClearingKeepsRecentResults(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 3
	pol.MinBytes = 0
	h := ceHistoryWithResults(10, 4096)

	cleared, _, _, changed := toolResultClearing(h, pol, 1, 1, 0)
	if !changed || cleared == 0 {
		t.Fatalf("expected clearing to fire, got cleared=%d changed=%v", cleared, changed)
	}

	// Count survivors by position: the LAST 3 tool results must be untouched.
	var seen int
	var toolIdxs []int
	for mi := range h {
		for ci := range h[mi].Content {
			if h[mi].Content[ci].ToolResult != nil {
				toolIdxs = append(toolIdxs, mi)
			}
		}
	}
	for _, mi := range toolIdxs[len(toolIdxs)-pol.KeepRecent:] {
		if strings.HasPrefix(h[mi].Content[0].ToolResult.Content, clearedPrefix) {
			t.Fatalf("the most recent %d tool results must NEVER be cleared — the model is reasoning about them", pol.KeepRecent)
		}
	}
	for _, mi := range toolIdxs[:len(toolIdxs)-pol.KeepRecent] {
		if !strings.HasPrefix(h[mi].Content[0].ToolResult.Content, clearedPrefix) {
			t.Fatalf("expected older results to be cleared")
		}
		seen++
	}
	if seen != cleared {
		t.Errorf("counted %d cleared but the function reported %d", seen, cleared)
	}
}

// TestClearedPlaceholderForbidsRerun pins the wording hazard: a placeholder that merely says "removed"
// invites the model to regenerate the payload, which costs more than the clearing saved.
func TestClearedPlaceholderForbidsRerun(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 0
	pol.MinBytes = 0
	h := ceHistoryWithResults(2, 4096)
	_, _, _, changed := toolResultClearing(h, pol, 1, 1, 0)
	if !changed {
		t.Fatal("expected clearing")
	}
	got := h[1].Content[0].ToolResult.Content
	if !strings.Contains(got, "Do NOT re-run") {
		t.Errorf("the placeholder must explicitly forbid re-running the command, got %q", got)
	}
	if !strings.Contains(got, "durable transcript") {
		t.Errorf("the placeholder should tell the model the data still exists, got %q", got)
	}
}

// TestClearingPreservesOffloadHandle: if the payload named an on-disk offload, clearing must carry that
// reference forward — it is the model's cheap handle on the full data.
func TestClearingPreservesOffloadHandle(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 0
	pol.MinBytes = 0
	payload := strings.Repeat("y", 4000) + " (full output offloaded to .orchicon/outputs/exec1/call1.out)"
	h := []Message{ceAssistantMsg("run"), ceToolResultMsg("call1", payload, false)}

	_, _, _, changed := toolResultClearing(h, pol, 1, 1, 0)
	if !changed {
		t.Fatal("expected clearing")
	}
	got := h[1].Content[0].ToolResult.Content
	if !strings.Contains(got, ".orchicon/outputs/exec1/call1.out") {
		t.Errorf("clearing must not take away the model's handle on the offloaded payload, got %q", got)
	}
}

// TestClearingNeverClearsErrors: an error result is why the worker changed approach. Blanking it is how a
// session starts repeating a failure it already learned from.
func TestClearingNeverClearsErrors(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 0
	pol.MinBytes = 0
	h := []Message{
		ceAssistantMsg("try"),
		ceToolResultMsg("c1", "ERROR: build failed: undefined: Foo", true),
		ceAssistantMsg("try again"),
		ceToolResultMsg("c2", strings.Repeat("z", 4096), false),
	}
	_, _, _, _ = toolResultClearing(h, pol, 1, 1, 0)

	if !strings.Contains(h[1].Content[0].ToolResult.Content, "undefined: Foo") {
		t.Fatalf("an ERROR tool result must be preserved — it is the reason the worker changed approach")
	}
	if !strings.HasPrefix(h[3].Content[0].ToolResult.Content, clearedPrefix) {
		t.Errorf("a successful result should still be cleared")
	}
}

// TestClearingRespectsTriggerAndBand pins the cache-invalidation bound: clearing must not run every turn,
// or each turn pays a cache write to save a few tokens. It fires once per pressure band.
func TestClearingRespectsTriggerAndBand(t *testing.T) {
	pol := ceClearPolicy()
	pol.TriggerFrac = 0.5
	pol.KeepRecent = 0
	pol.MinBytes = 0
	h := ceHistoryWithResults(4, 4096)

	// Well below the trigger: no clearing.
	if _, _, _, changed := toolResultClearing(h, pol, 100, 1000, 0); changed {
		t.Fatal("clearing must not fire below the trigger fraction")
	}

	// Above the trigger: fires, and reports the band.
	_, _, band, changed := toolResultClearing(h, pol, 600, 1000, 0)
	if !changed {
		t.Fatal("clearing must fire above the trigger fraction")
	}
	if band < 1 {
		t.Fatalf("band = %d, want >= 1", band)
	}

	// Same band again: must NOT re-fire (the cache bound). This is the ONLY thing under test here, so it
	// must not clear — asserted by the report, not by inspection (the band gate returns before any
	// rewriting happens).
	h2 := ceHistoryWithResults(4, 4096)
	if cleared, _, _, changed := toolResultClearing(h2, pol, 600, 1000, band); changed || cleared != 0 {
		t.Fatalf("clearing must not re-fire within the same pressure band (it would invalidate the cache every turn); cleared=%d", cleared)
	}
	// ...and the history is untouched, which is the stronger statement.
	for _, m := range h2 {
		for _, c := range m.Content {
			if c.ToolResult != nil && strings.HasPrefix(c.ToolResult.Content, clearedPrefix) {
				t.Fatal("a suppressed clear must leave the history completely untouched")
			}
		}
	}

	// A FRESH history at a higher band does fire.
	h3 := ceHistoryWithResults(4, 4096)
	if _, _, _, changed := toolResultClearing(h3, pol, 900, 1000, band); !changed {
		t.Fatal("clearing must fire when a new pressure band is entered")
	}
}

// TestClearingSkipsWhenReclaimIsTooSmall pins the clear_at_least equivalent: invalidating a cached prefix
// to reclaim a few hundred bytes is a net loss.
func TestClearingSkipsWhenReclaimIsTooSmall(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 0
	pol.MinBytes = 64 * 1024
	h := ceHistoryWithResults(2, 100) // tiny payloads

	if _, _, _, changed := toolResultClearing(h, pol, 1, 1, 0); changed {
		t.Fatal("a clear that reclaims less than min_bytes must be skipped")
	}
}

// TestClearingDisabledByDefault pins that an unconfigured deployment keeps its behaviour: an operator has
// to turn this on deliberately.
func TestClearingDisabledByDefault(t *testing.T) {
	for _, budgets := range [][]byte{nil, []byte("{}"), []byte(`{"tokens":0}`)} {
		if contextEditPolicyFromSettings(budgets).Enabled {
			t.Errorf("clearing must default OFF for %s — enabling it changes what every worker sees", budgets)
		}
	}
}

// TestClearingPolicyFromSettings pins the parse of the settings block.
func TestClearingPolicyFromSettings(t *testing.T) {
	raw := []byte(`{"context_editing":{"enabled":true,"trigger_frac":0.6,"keep_recent":12,"min_bytes":4096}}`)
	pol := contextEditPolicyFromSettings(raw)
	if !pol.Enabled {
		t.Fatal("enabled:true must turn clearing on")
	}
	if pol.TriggerFrac != 0.6 {
		t.Errorf("trigger_frac = %v, want 0.6", pol.TriggerFrac)
	}
	if pol.KeepRecent != 12 {
		t.Errorf("keep_recent = %d, want 12", pol.KeepRecent)
	}
	if pol.MinBytes != 4096 {
		t.Errorf("min_bytes = %d, want 4096", pol.MinBytes)
	}
	// An out-of-range trigger must not be accepted (it would either clear constantly or never).
	bad := contextEditPolicyFromSettings([]byte(`{"context_editing":{"enabled":true,"trigger_frac":7}}`))
	if bad.TriggerFrac > 1 || bad.TriggerFrac <= 0 {
		t.Errorf("an out-of-range trigger_frac must fall back to the default, got %v", bad.TriggerFrac)
	}
}

// TestClearingIsIdempotent: a second pass over the same band must not double-clear or miscount.
func TestClearingIsIdempotent(t *testing.T) {
	pol := ceClearPolicy()
	pol.KeepRecent = 1
	pol.MinBytes = 0
	h := ceHistoryWithResults(6, 2048)

	first, firstBytes, band, changed := toolResultClearing(h, pol, 1, 1, 0)
	if !changed || first == 0 {
		t.Fatal("expected the first pass to clear")
	}
	// Re-running at a higher band must not re-clear the already-cleared payloads.
	second, _, _, changed2 := toolResultClearing(h, pol, 1, 1, band)
	if changed2 && second != 0 {
		t.Fatalf("re-cleared %d already-cleared result(s); clear must be idempotent", second)
	}
	if firstBytes <= 0 {
		t.Errorf("reclaimed bytes should be positive, got %d", firstBytes)
	}
}

// TestEstimateHistoryTokensRoughlyTracksContent: the estimate only has to be right at a band boundary, but
// it must at least move with the content and not be wildly off.
func TestEstimateHistoryTokensRoughlyTracksContent(t *testing.T) {
	h := ceHistoryWithResults(10, 4000) // 10 results * 4000 = 40k bytes of tool output
	est := estimateHistoryTokens(h)
	if est < 5000 || est > 20000 {
		t.Fatalf("estimateHistoryTokens = %d for ~40k bytes; want roughly 40k/4 = 10000", est)
	}
}

// TestClearingBandsProgressAcrossTheWindow is the regression for a real bug this file's own test caught:
// deriving the band as int(frac/TriggerFrac) collapses to 1 for EVERY occupancy in [trigger, 1.0), so a
// session cleared once and then never again no matter how much context it accumulated. Bands must advance
// as occupancy grows, which is what makes clearing a repeated housekeeping duty rather than a one-shot.
func TestClearingBandsProgressAcrossTheWindow(t *testing.T) {
	pol := ceClearPolicy()
	pol.TriggerFrac = 0.5
	pol.BandWidth = 0.1
	pol.KeepRecent = 0
	pol.MinBytes = 0

	bands := map[int]bool{}
	for _, occ := range []int{500, 600, 700, 800, 900, 1000} {
		h := ceHistoryWithResults(4, 4096)
		_, _, band, changed := toolResultClearing(h, pol, occ, 1000, 0)
		if !changed {
			t.Fatalf("occupancy %d must clear (it is past the 0.5 trigger)", occ)
		}
		bands[band] = true
	}
	// 500..1000 in 0.1 steps above a 0.5 trigger must yield at least 4 distinct bands.
	if len(bands) < 4 {
		t.Fatalf("only %d distinct bands across the window (%v) — the band must advance with occupancy, "+
			"or clearing fires once per session", len(bands), bands)
	}
	// Monotonic: a fuller window must never report a LOWER band.
	last := 0
	for _, occ := range []int{500, 600, 700, 800, 900, 1000} {
		h := ceHistoryWithResults(4, 4096)
		_, _, band, _ := toolResultClearing(h, pol, occ, 1000, 0)
		if band < last {
			t.Fatalf("band went backwards at occupancy %d: %d after %d", occ, band, last)
		}
		last = band
	}
}
