package orchicon

// unlimited_defaults_test.go — AN UNSET BUDGET IS NO BUDGET, proven by running the real loop.
//
// THE RUN THAT MOTIVATED IT (from the live plane, execution 01M6JJG45V00EPC5RN9WPYEK7R): a worker session
// ran 4 minutes and 30 tool rounds, compacted its own history at turn 30, and the provider then rejected the
// next request with
//
//	400 The `reasoning_content` in the thinking mode must be passed back to the API.
//
// The turn-count ceiling that fired was 30 — a built-in default the operator had never set. The tenant's
// stored budget carried no compact_max_turns, no tokens and no wall clock; the only gate it had configured was
// cost_usd. Every one of those unset ceilings is now "no gate", so this test drives a long, expensive session
// with an EMPTY budget and asserts the two things an operator cares about: it is never killed on a number
// nobody set, and its history is never collapsed behind it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A LONG, EXPENSIVE, TOOL-DRIVEN SESSION WITH NO CONFIGURED BUDGET SURVIVES.
func TestNoConfiguredBudgetLetsALongExpensiveSessionFinish(t *testing.T) {
	dir := t.TempDir()
	prov := &mockProvider{}
	// 40 tool rounds, then a clean finish. Each round is a tool call + its result.
	for i := 0; i < 40; i++ {
		prov.turns = append(prov.turns, scriptedTurn{
			events: []Event{
				ReasoningDelta{Text: "thinking about the next step"},
				ToolCall{Index: 0, ToolCallID: "call-" + fmt.Sprint(i), Name: "bash", ArgsJSON: `{"command":"true"}`},
			},
			finish: StopToolUse,
			usage:  Usage{InputTokens: 60_000, OutputTokens: 4_000, ReasoningTokens: 3_000, CostUSD: 0.40},
		})
	}
	prov.turns = append(prov.turns, scriptedTurn{
		events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: done."}},
		finish: StopStop,
	})

	m := testManifest("orchicon/mockprov/deepseek-v4-flash")
	m.Budgets = []byte(`{}`) // NOTHING configured: the operator set no ceiling at all
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_unlimited"),
		Manifest:   m,
		ProjectDir: dir,
		Provider:   prov,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}

	_, _, _, _, _, _, results := cb.snapshot()
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("a 40-round session with no configured budget failed: %+v", results)
	}
	if n := prov.requestCount(); n < 40 {
		t.Fatalf("the session stopped after %d provider turns, want all 40 — a default killed it", n)
	}

	// THE COLLAPSE THAT COST THE RUN: no offload file means no history was evicted.
	offload := filepath.Join(dir, ".orchicon", "offload", "exec_unlimited.jsonl")
	if b, err := os.ReadFile(offload); err == nil {
		t.Fatalf("the session collapsed its own history with no compact_max_turns configured "+
			"(%d bytes offloaded) — that is the turn-count default coming back", len(b))
	}

	// And it did not merely survive: the history the provider received still held the reasoning for every
	// tool-calling round, so nothing was silently dropped either.
	if prov.requestCount() == 0 {
		t.Fatal("no provider request captured")
	}
	last := prov.lastRequest()
	assistant, withReasoning := 0, 0
	for _, msg := range last.Messages {
		if msg.Role != RoleAssistant {
			continue
		}
		assistant++
		for _, c := range msg.Content {
			if c.Reasoning != nil && *c.Reasoning != "" {
				withReasoning++
				break
			}
		}
	}
	if assistant < 30 {
		t.Fatalf("the final request carried only %d assistant messages", assistant)
	}
	if withReasoning != assistant {
		t.Fatalf("%d of %d assistant messages reached the provider WITHOUT their reasoning — a thinking-mode "+
			"provider rejects that (the 400 this change exists for)", assistant-withReasoning, assistant)
	}
}

// AND A CONFIGURED CEILING STILL STOPS IT — the defaults are gone, not the feature.
func TestAConfiguredTurnGateStillCollapses(t *testing.T) {
	dir := t.TempDir()
	prov := &mockProvider{}
	for i := 0; i < 8; i++ {
		prov.turns = append(prov.turns, scriptedTurn{
			events: []Event{
				ToolCall{Index: 0, ToolCallID: "call-" + fmt.Sprint(i), Name: "bash", ArgsJSON: `{}`},
			},
			finish: StopToolUse,
			usage:  Usage{InputTokens: 1_000},
		})
	}
	prov.turns = append(prov.turns, scriptedTurn{
		events: []Event{TextDelta{Text: "ORCHICON WORKER SUMMARY: done."}}, finish: StopStop,
	})
	m := testManifest("orchicon/mockprov/deepseek-v4-flash")
	m.Budgets = []byte(`{"compact_max_turns":3,"context_compaction":{"enabled":true,"recent_turns":2}}`)
	s, err := NewSession(SessionConfig{
		ExecRow: testExecRow("exec_configured"), Manifest: m, ProjectDir: dir, Provider: prov,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".orchicon", "offload", "exec_configured.jsonl")); err != nil {
		t.Fatalf("compact_max_turns=3 was configured and the session never collapsed its history: %v", err)
	}
}
