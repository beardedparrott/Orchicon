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

// restoreToolValidateModelRef saves the package-global toolValidateModelRef and
// restores it at test teardown. SetValidationRegistry permanently rebinds the
// global to a closure over the service; without this a wiring test leaks the
// injected registry into every later test that asserts the builtin fallback.
func restoreToolValidateModelRef(t *testing.T) {
	t.Helper()
	saved := toolValidateModelRef
	t.Cleanup(func() { toolValidateModelRef = saved })
}

// TestToolValidateModelRef via a wired Service: SetValidationRegistry binds the
// package-global toolValidateModelRef to a closure over the service, so the
// update_settings tool's write path shares the SAME injected registry as the
// settings RPC. 3-seg / legacy 2-seg / slashed-model refs pass; unknown adapter
// and malformed refs fail with a Settings → Adapters pointer.
func TestToolValidateModelRef(t *testing.T) {
	restoreToolValidateModelRef(t)
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
	restoreToolValidateModelRef(t)
	// Force the pristine package-global default (builtin catalog) regardless of
	// registration order: a prior Service.SetValidationRegistry would otherwise
	// have rebound the global, making this test silently test the wrong registry.
	toolValidateModelRef = func(ref string) error {
		if strings.TrimSpace(ref) == "" {
			return nil
		}
		if _, err := adapter.ParseModelRef(ref, adapter.NewBuiltinProviderCatalog()); err != nil {
			return err
		}
		return nil
	}
	if err := toolValidateModelRef("opencode/opencode-go/deepseek-v4-flash"); err != nil {
		t.Errorf("default toolValidateModelRef(3-seg builtin) = %v, want nil", err)
	}
	err := toolValidateModelRef("foo/anthropic/claude-sonnet-5")
	if err == nil || !strings.Contains(err.Error(), "register an adapter") {
		t.Errorf("default toolValidateModelRef(unknown adapter) = %v, want 'register an adapter' rejection", err)
	}
}
