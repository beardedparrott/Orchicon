package orchicon

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// The tool_call_count ladder dimension, evaluated natively at opencode
// parity: tier crossings latch without compacting (the dimension is opted
// out of compact_dims by default — collapsing context on tool pressure
// would force yet more tool calls), and the abort tier fails the session
// with budget_abort:tool_call_count. Limits are worker budget_overrides
// over tenant default_budget_overrides (first-class key end-to-end);
// unset falls back to the ladder default (100), explicit <= 0 disables.

func toolsDimSession(t *testing.T, budgets string, toolUses int) *Session {
	t.Helper()
	return &Session{
		id:  "test-tools-dim",
		log: slog.Default(),
		cp:  DefaultCompactPolicy(),
		cs: compactState{
			budget: opencode.ParseBudgetLadder([]byte(budgets)),
			spend:  opencode.NewBudgetSpend(),
		},
		toolUses: toolUses,
	}
}

func TestNativeToolsDimAbortAtLimit(t *testing.T) {
	s := toolsDimSession(t, `{"tool_call_count":3}`, 3)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); got != "budget_abort:tool_call_count" {
		t.Fatalf("3/3 calls → %q, want budget_abort:tool_call_count", got)
	}
}

func TestNativeToolsDimBelowLimitNoAbortOrCompact(t *testing.T) {
	s := toolsDimSession(t, `{"tool_call_count":10}`, 6)
	got := s.maybeCompact(context.Background(), 5, Usage{})
	if strings.HasPrefix(got, "budget_abort:") {
		t.Fatalf("6/10 calls → %q, want no abort", got)
	}
	if strings.HasPrefix(got, "compacted:") {
		t.Fatalf("6/10 calls → %q, tools dim must never compact", got)
	}
}

func TestNativeToolsDimDisabledByZero(t *testing.T) {
	s := toolsDimSession(t, `{"tool_call_count":0}`, 500)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); got != "" {
		t.Fatalf("disabled gate at 500 calls → %q, want no action", got)
	}
}

func TestNativeToolsDimDefaultLimitPinned(t *testing.T) {
	// Unset tool_call_count falls back to the ladder default (100) —
	// pinning the current contract so a default change fails loudly.
	s := toolsDimSession(t, `{}`, 100)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); got != "budget_abort:tool_call_count" {
		t.Fatalf("unset limit at 100 calls → %q, want budget_abort:tool_call_count", got)
	}
	s = toolsDimSession(t, `{}`, 50)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); strings.HasPrefix(got, "budget_abort:") {
		t.Fatalf("unset limit at 50 calls → %q, want no abort", got)
	}
}
