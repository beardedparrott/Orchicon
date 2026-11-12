package orchicon

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// The tool_call_count ladder dimension, evaluated natively at opencode parity: tier crossings latch
// without compacting (the dimension is opted out of compact_dims by default.

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

func TestNativeToolsDimUnsetMeansNoAbort(t *testing.T) {
	// An UNSET tool_call_count is no gate at all. It used to fall back to 100,
	// which hard-aborted honest tool-heavy runs on a number nobody had set.
	for _, calls := range []int{100, 250, 5000} {
		s := toolsDimSession(t, `{}`, calls)
		if got := s.maybeCompact(context.Background(), 5, Usage{}); strings.HasPrefix(got, "budget_abort:") {
			t.Fatalf("unset tool_call_count at %d calls → %q, want no abort", calls, got)
		}
	}
	// A CONFIGURED limit still aborts — that is the half that must keep working.
	s := toolsDimSession(t, `{"tool_call_count":100}`, 100)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); got != "budget_abort:tool_call_count" {
		t.Fatalf("configured limit 100 at 100 calls → %q, want budget_abort:tool_call_count", got)
	}
	s = toolsDimSession(t, `{"tool_call_count":100}`, 50)
	if got := s.maybeCompact(context.Background(), 5, Usage{}); strings.HasPrefix(got, "budget_abort:") {
		t.Fatalf("configured limit 100 at 50 calls → %q, want no abort", got)
	}
}
