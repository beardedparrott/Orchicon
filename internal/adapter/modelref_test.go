package adapter

import (
	"strings"
	"testing"
)

// testRegistry is a registry with the built-in opencode providers plus a
// tenant-created custom provider (as the provider-layer task will surface
// through Settings → Adapters).
func testRegistry() ProviderRegistry {
	c := NewBuiltinProviderCatalog()
	c.AddAdapterKind(DefaultAdapterKind, "local-models")
	return c
}

func TestParseModelRef(t *testing.T) {
	reg := testRegistry()
	cases := []struct {
		ref      string
		adapter  string
		provider string
		model    string
	}{
		// Legacy 2-segment (built-in first segment) — adapter inferred.
		{"opencode/deepseek-v4-flash-free", "opencode", "opencode", "deepseek-v4-flash-free"},
		{"anthropic/claude-sonnet-4", "opencode", "anthropic", "claude-sonnet-4"},
		{"opencode-go/deepseek-v4-flash", "opencode", "opencode-go", "deepseek-v4-flash"},
		// Legacy 2-segment (tenant-custom first segment) — adapter inferred
		// once the operator defines the provider in Settings → Adapters.
		{"local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL", "opencode", "local-models", "Qwen3.6-35B-A3B-UD-Q4_K_XL"},
		// 3-segment.
		{"claude/anthropic/claude-sonnet-5", "claude", "anthropic", "claude-sonnet-5"},
		{"opencode/opencode-go/deepseek-v4-flash", "opencode", "opencode-go", "deepseek-v4-flash"},
		{"orchicon/local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL", "orchicon", "local-models", "Qwen3.6-35B-A3B-UD-Q4_K_XL"},
		// Slashed model ids (4+ segments): the model field is VERBATIM.
		{"orchicon/command-code/deepseek/deepseek-v4-flash", "orchicon", "command-code", "deepseek/deepseek-v4-flash"},
		{"opencode/provider/a/b/c", "opencode", "provider", "a/b/c"},
		{"opencode/command-code/deepseek/deepseek-v4-flash", "opencode", "command-code", "deepseek/deepseek-v4-flash"},
		// 1-segment legacy bare model id.
		{"deepseek-v4-flash", "opencode", "", "deepseek-v4-flash"},
		{"test-model", "opencode", "", "test-model"},
	}
	for _, c := range cases {
		got, err := ParseModelRef(c.ref, reg)
		if err != nil {
			t.Errorf("ParseModelRef(%q) error = %v, want nil", c.ref, err)
			continue
		}
		if got.Adapter != c.adapter {
			t.Errorf("ParseModelRef(%q).Adapter = %q, want %q", c.ref, got.Adapter, c.adapter)
		}
		if got.Provider != c.provider {
			t.Errorf("ParseModelRef(%q).Provider = %q, want %q", c.ref, got.Provider, c.provider)
		}
		if got.Model != c.model {
			t.Errorf("ParseModelRef(%q).Model = %q, want %q", c.ref, got.Model, c.model)
		}
	}
}

func TestParseModelRefUnknownProvider(t *testing.T) {
	reg := testRegistry()
	_, err := ParseModelRef("mystery-provider/claude-sonnet-5", reg)
	if err == nil {
		t.Fatal("ParseModelRef(unknown 2-seg first segment) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "mystery-provider") {
		t.Errorf("error %q does not name the unknown provider", err.Error())
	}
	if !strings.Contains(err.Error(), "Settings → Adapters") {
		t.Errorf("error %q does not point at Settings → Adapters", err.Error())
	}
}

func TestParseModelRefUnknownAdapter(t *testing.T) {
	reg := testRegistry()
	_, err := ParseModelRef("foo/anthropic/claude-sonnet-5", reg)
	if err == nil {
		t.Fatal("ParseModelRef(unknown adapter segment) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "foo") {
		t.Errorf("error %q does not name the unknown adapter", err.Error())
	}
	if !strings.Contains(err.Error(), "register an adapter") {
		t.Errorf("error %q does not point at adapter registration", err.Error())
	}
}

func TestParseModelRefKnownAdapterTwoSeg(t *testing.T) {
	// Implementer note (Design Approver): a 2-segment ref whose first
	// segment is a KNOWN adapter kind is malformed — it should be written
	// adapter/provider/model.
	reg := testRegistry()
	_, err := ParseModelRef("claude/anthropic", reg)
	if err == nil {
		t.Fatal("ParseModelRef(2-seg with known-adapter first segment) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "adapter kind") {
		t.Errorf("error %q does not explain the adapter-kind confusion", err.Error())
	}
}

func TestParseModelRefMalformed(t *testing.T) {
	reg := testRegistry()
	for _, ref := range []string{"", "   ", "/", "//", "opencode/", "/claude-sonnet-5"} {
		if _, err := ParseModelRef(ref, reg); err == nil {
			t.Errorf("ParseModelRef(%q) = nil error, want malformed rejection", ref)
		}
	}
}

func TestParseModelRefNilRegistry(t *testing.T) {
	// A nil registry (deep adapter layer) parses structurally: legacy
	// inference without known-provider validation, 3+ segments left-greedy.
	got, err := ParseModelRef("opencode/deepseek-v4-flash-free", nil)
	if err != nil {
		t.Fatalf("ParseModelRef(nil reg) error = %v", err)
	}
	if got.Adapter != "opencode" || got.Provider != "opencode" || got.Model != "deepseek-v4-flash-free" {
		t.Errorf("ParseModelRef(nil reg) = %+v, want {opencode opencode deepseek-v4-flash-free}", got)
	}
	got, err = ParseModelRef("orchicon/command-code/deepseek/deepseek-v4-flash", nil)
	if err != nil {
		t.Fatalf("ParseModelRef(nil reg, slashed) error = %v", err)
	}
	if got.Adapter != "orchicon" || got.Provider != "command-code" || got.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("ParseModelRef(nil reg, slashed) = %+v, want model verbatim", got)
	}
}

func TestAdapterKind(t *testing.T) {
	cases := []struct {
		ref  string
		kind string
	}{
		{"opencode/opencode-go/deepseek-v4-flash", "opencode"},
		{"claude/anthropic/claude-sonnet-5", "claude"},
		{"orchicon/command-code/deepseek/deepseek-v4-flash", "orchicon"},
		{"opencode/deepseek-v4-flash-free", "opencode"}, // legacy 2-seg
		{"anthropic/claude-sonnet-4", "opencode"},       // legacy 2-seg
		{"deepseek-v4-flash", "opencode"},               // legacy 1-seg
		{"mystery/provider/model", "mystery"},           // unknown kind surfaces at Resolve
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := AdapterKind(c.ref); got != c.kind {
			t.Errorf("AdapterKind(%q) = %q, want %q", c.ref, got, c.kind)
		}
	}
}

func TestSplitForServe(t *testing.T) {
	cases := []struct {
		ref      string
		provider string
		model    string
		ok       bool
	}{
		{"opencode/deepseek-v4-flash-free", "opencode", "deepseek-v4-flash-free", true},
		{"anthropic/claude-sonnet-4", "anthropic", "claude-sonnet-4", true},
		{"opencode/opencode-go/deepseek-v4-flash", "opencode-go", "deepseek-v4-flash", true},
		{"orchicon/command-code/deepseek/deepseek-v4-flash", "command-code", "deepseek/deepseek-v4-flash", true},
		{"claude/anthropic/claude-sonnet-5", "anthropic", "claude-sonnet-5", true},
		{"deepseek-v4-flash", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		provider, model, ok := SplitForServe(c.ref)
		if ok != c.ok || provider != c.provider || model != c.model {
			t.Errorf("SplitForServe(%q) = (%q, %q, %v), want (%q, %q, %v)", c.ref, provider, model, ok, c.provider, c.model, c.ok)
		}
	}
}
