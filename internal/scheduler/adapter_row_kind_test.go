package scheduler

// Unit tests for resolveAdapterRowKind (ADR-0003 single source of truth):
// the adapter-ROW selection kind used by TaskReconciler dispatch derives
// from the model_ref's parsed adapter kind — the same source the bridge
// Resolve at dispatch uses — then falls back to the legacy default kind
// (the empty-ref fallback that closes the dispatch black hole where
// kind "" matched zero runtime_adapters rows and the task requeued
// forever). Worker-level runtime_ref (the former first-choice kind that
// could diverge from the ref and misroute dispatch) is retired; there is
// no runtime_ref input anymore.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

func TestResolveAdapterRowKind(t *testing.T) {
	cases := []struct {
		modelRef string
		want     string
	}{
		// The model_ref's parsed adapter kind governs (the persisted
		// selection routes row selection too).
		{"orchicon/commandcode/deepseek/deepseek-v4-flash", "orchicon"},
		{"claude/anthropic/m", "claude"},
		{"opencode/anthropic/m", "opencode"},
		// Legacy 2-segment ref: the parser's inference (opencode) —
		// backward compat pinned.
		{"anthropic/claude-4", adapter.DefaultAdapterKind},
		// Empty/malformed ref: legacy default kind.
		{"", adapter.DefaultAdapterKind},
		{"opencode/", adapter.DefaultAdapterKind},
	}
	for _, c := range cases {
		if got := resolveAdapterRowKind(c.modelRef); got != c.want {
			t.Errorf("resolveAdapterRowKind(%q) = %q; want %q", c.modelRef, got, c.want)
		}
	}
}
