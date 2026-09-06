package askorchicon

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// validationTestRegistry mirrors the merged CLI-aware registry the server
// injects into the Service (builtin profiles ∪ tenant-custom local-models ∪
// CLI-discovered deepseek provider id), the way settings-level tests build it.
func validationTestRegistry() adapter.ProviderRegistry {
	c := adapter.NewBuiltinProviderCatalog()
	c.AddAdapterKind(adapter.DefaultAdapterKind, "local-models", "deepseek")
	return c
}

// TestToolValidateModelRef via a wired Service: SetValidationRegistry binds the
// package-global toolValidateModelRef to a closure over the service, so the
// update_settings tool's write path shares the SAME injected registry as the
// settings RPC. 3-seg / legacy 2-seg / slashed-model refs pass; unknown adapter
// and malformed refs fail with a Settings → Adapters pointer.
func TestToolValidateModelRef(t *testing.T) {
	s := &Service{}
	s.SetValidationRegistry(validationTestRegistry())

	pass := []string{
		"claude/anthropic/claude-sonnet-5",
		"opencode/opencode-go/deepseek-v4-flash",
		"orchicon/local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL",
		"opencode-go/deepseek-v4-flash",                                   // legacy 2-seg (built-in)
		"local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL",                         // legacy 2-seg (tenant-custom)
		"orchicon/commandcode/deepseek/deepseek-v4-flash",                 // slashed model id, verbatim
		"",                                                                // unset = valid
		"   ",                                                             // unset/blank = valid
	}
	for _, ref := range pass {
		if err := toolValidateModelRef(ref); err != nil {
			t.Errorf("toolValidateModelRef(%q) error = %v, want nil", ref, err)
		}
	}

	fail := []struct {
		ref   string
		point string
	}{
		{"foo/anthropic/claude-sonnet-5", "register an adapter"}, // unknown adapter
		{"mystery-provider/claude-sonnet-5", "Settings → Adapters"}, // unknown 2-seg provider
		{"/", "adapter/provider/model"}, // malformed
		{"claude/anthropic", "adapter kind"}, // 2-seg known-adapter first segment
	}
	for _, c := range fail {
		err := toolValidateModelRef(c.ref)
		if err == nil {
			t.Errorf("toolValidateModelRef(%q) = nil error, want rejection", c.ref)
			continue
		}
		if !strings.Contains(err.Error(), c.point) {
			t.Errorf("toolValidateModelRef(%q) error %q does not contain %q", c.ref, err.Error(), c.point)
		}
	}
}

// TestToolValidateModelRefDefaultBuiltin pins the package-global default: when
// NO Service wiring has run, toolValidateModelRef degrades to the static builtin
// catalog, so a built-in 3-seg ref passes and an unknown adapter fails.
func TestToolValidateModelRefDefaultBuiltin(t *testing.T) {
	if err := toolValidateModelRef("opencode/opencode-go/deepseek-v4-flash"); err != nil {
		t.Errorf("default toolValidateModelRef(3-seg builtin) = %v, want nil", err)
	}
	err := toolValidateModelRef("foo/anthropic/claude-sonnet-5")
	if err == nil || !strings.Contains(err.Error(), "register an adapter") {
		t.Errorf("default toolValidateModelRef(unknown adapter) = %v, want 'register an adapter' rejection", err)
	}
}
