package scheduler

// Unit tests for resolveAdapterRowKind ( single source of truth): the adapter-ROW selection
// kind used by TaskReconciler dispatch derives from the model_ref's parsed adapter kind.

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
