package scheduler

// Unit tests for resolveAdapterRowKind (ADR-0005 D6): the adapter-ROW
// selection kind used by TaskReconciler dispatch. runtime_ref wins when
// set (pre-existing behavior); an empty runtime_ref falls back to the
// model_ref's parsed adapter kind — closing the dispatch black hole where
// kind "" matched zero runtime_adapters rows and the task requeued
// forever — then to the legacy default kind.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

func TestResolveAdapterRowKind(t *testing.T) {
	cases := []struct {
		runtimeRef string
		modelRef   string
		want       string
	}{
		// runtime_ref set: always wins — even divergent from the ref's kind
		// (the terminal failed_to_start semantics for divergent refs is
		// preserved; never silently repointed).
		{"opencode", "claude/anthropic/m", "opencode"},
		{"claude", "opencode/anthropic/m", "claude"},
		// Empty runtime_ref: the model_ref's parsed adapter kind governs
		// (the persisted selection routes row selection too).
		{"", "orchicon/commandcode/deepseek/deepseek-v4-flash", "orchicon"},
		{"", "claude/anthropic/m", "claude"},
		// Empty runtime_ref + legacy 2-segment ref: the parser's inference
		// (opencode) — backward compat pinned.
		{"", "anthropic/claude-4", adapter.DefaultAdapterKind},
		// Empty runtime_ref + empty/malformed ref: legacy default kind.
		{"", "", adapter.DefaultAdapterKind},
		{"", "opencode/", adapter.DefaultAdapterKind},
	}
	for _, c := range cases {
		if got := resolveAdapterRowKind(c.runtimeRef, c.modelRef); got != c.want {
			t.Errorf("resolveAdapterRowKind(%q, %q) = %q; want %q", c.runtimeRef, c.modelRef, got, c.want)
		}
	}
}
