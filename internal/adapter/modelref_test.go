package adapter

import "testing"

func TestParseModelRef(t *testing.T) {
	cases := []struct {
		ref     string
		adapter string
		model   string
	}{
		// 3-segment: explicit adapter kind.
		{"opencode/anthropic/claude-sonnet-4", "opencode", "claude-sonnet-4"},
		{"claude/anthropic/claude-sonnet-4", "claude", "claude-sonnet-4"},
		{"opencode/deepseek/deepseek-v4-flash", "opencode", "deepseek-v4-flash"},
		// 2-segment legacy: adapter defaults to opencode.
		{"opencode/deepseek-v4-flash", "opencode", "deepseek-v4-flash"},
		{"anthropic/claude-sonnet-4", "opencode", "claude-sonnet-4"},
		{"deepseek/deepseek-v4-flash", "opencode", "deepseek-v4-flash"},
		// 1-segment legacy test/dev refs: adapter defaults to opencode.
		{"deepseek-v4-flash", "opencode", "deepseek-v4-flash"},
		{"test-model", "opencode", "test-model"},
		// Whitespace-tolerant.
		{"  opencode / anthropic / claude-sonnet-4 ", "opencode", "claude-sonnet-4"},
		// Empty separators collapse.
		{"opencode//claude-sonnet-4", "opencode", "claude-sonnet-4"},
	}
	for _, c := range cases {
		got := ParseModelRef(c.ref)
		if got.Adapter != c.adapter {
			t.Errorf("ParseModelRef(%q).Adapter = %q, want %q", c.ref, got.Adapter, c.adapter)
		}
		if got.Model != c.model {
			t.Errorf("ParseModelRef(%q).Model = %q, want %q", c.ref, got.Model, c.model)
		}
	}
}

func TestParseModelRefMalformed(t *testing.T) {
	for _, ref := range []string{"", "   ", "/", "//"} {
		got := ParseModelRef(ref)
		if got.Adapter != "" {
			t.Errorf("ParseModelRef(%q).Adapter = %q, want empty (malformed)", ref, got.Adapter)
		}
	}
}
