package orchicon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- Profile table (D4) -------------------------------------------------------

func TestBuiltinProfiles(t *testing.T) {
	want := map[string]struct{ base, env string }{
		"anthropic":   {"https://api.anthropic.com", "ANTHROPIC_API_KEY"},
		"openai":      {"https://api.openai.com/v1", "OPENAI_API_KEY"},
		"openrouter":  {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
		"opencode":    {"https://opencode.ai/zen/v1", "OPENCODE_API_KEY"},
		"opencode-go": {"https://opencode.ai/zen/go/v1", "OPENCODE_API_KEY"}, // distinct base, same auth env as Zen
		"commandcode": {"https://api.commandcode.ai", "COMMANDCODE_API_KEY"},
		"ollama":      {"http://localhost:11434", ""},
	}
	for id, w := range want {
		p, ok := BuiltinProfile(id)
		if !ok {
			t.Fatalf("builtin %s missing", id)
		}
		if p.BaseURL != w.base {
			t.Fatalf("%s base = %q, want %q", id, p.BaseURL, w.base)
		}
		if p.AuthEnv != w.env {
			t.Fatalf("%s auth env = %q, want %q", id, p.AuthEnv, w.env)
		}
		if !p.Visible {
			t.Fatalf("%s must be visible", id)
		}
	}
	if _, ok := BuiltinProfile("nonexistent"); ok {
		t.Fatal("unknown id must not resolve")
	}
	// opencode (Zen /zen/v1) and opencode-go (/zen/go/v1) are distinct profiles.
	zen, _ := BuiltinProfile("opencode")
	zenGo, _ := BuiltinProfile("opencode-go")
	if zen.BaseURL == zenGo.BaseURL {
		t.Fatal("Zen and Go profiles must have distinct base URLs")
	}
	// ollama profile must be the no-auth kind.
	oll, _ := BuiltinProfile("ollama")
	if oll.Kind != ProfileKindOllama {
		t.Fatal("ollama kind")
	}
	// commandcode must be its own kind (dual-transport wrapper).
	cc, _ := BuiltinProfile("commandcode")
	if cc.Kind != ProfileKindCommandCode {
		t.Fatal("commandcode kind")
	}
}

func TestValidateProfile(t *testing.T) {
	ok := Profile{ID: "my-vllm", Kind: ProfileKindCustom, BaseURL: "http://localhost:8000/v1", Custom: true}
	if err := ValidateProfile(ok); err != nil {
		t.Fatalf("valid custom: %v", err)
	}
	if err := ValidateProfile(Profile{ID: "", BaseURL: "x"}); err == nil {
		t.Fatal("empty id must fail")
	}
	if err := ValidateProfile(Profile{ID: "x", Kind: ProfileKindCustom, BaseURL: ""}); err == nil {
		t.Fatal("empty base url must fail")
	}
	// Built-in collision: a tenant-created custom profile may not shadow a built-in.
	shadow := Profile{ID: "openai", Kind: ProfileKindCustom, BaseURL: "http://evil", Custom: true}
	if err := ValidateProfile(shadow); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("builtin collision must fail: %v", err)
	}
	// commandcode kind is built-in only.
	cc := Profile{ID: "mycc", Kind: ProfileKindCommandCode, BaseURL: "x", Custom: true}
	if err := ValidateProfile(cc); err == nil {
		t.Fatal("custom commandcode kind must fail")
	}
}

func TestCustomProfileLoader(t *testing.T) {
	if _, ok := BuiltinProfile("acme-llm"); ok {
		t.Fatal("precondition")
	}
	SetCustomProfileLoader(func(ctx context.Context, tenantID string) ([]Profile, error) {
		return []Profile{{ID: "acme-llm", Kind: ProfileKindCustom, BaseURL: "http://acme/v1", Custom: true, Visible: true}}, nil
	})
	t.Cleanup(func() { SetCustomProfileLoader(nil) })

	r := NewRegistry(NewCredentialResolver(nil, nil), NewSourcingService(nil, nil), nil, nil)
	p, err := r.Get(context.Background(), "tenant-1", "acme-llm")
	if err != nil {
		t.Fatalf("custom profile through registry: %v", err)
	}
	if p == nil {
		t.Fatal("nil provider")
	}
	// No credential required (no AuthEnv/AuthSecretRef) → auth-less client.
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatalf("list models: %v", err)
	}
}

// --- Vendored catalog (D8) ----------------------------------------------------

func TestCatalogShape(t *testing.T) {
	m, ok := GetModel("anthropic/claude-sonnet-4")
	if !ok {
		t.Fatal("catalog entry missing")
	}
	if m.Context != 200000 || m.MaxOutput != 64000 {
		t.Fatalf("context/output = %d/%d", m.Context, m.MaxOutput)
	}
	if !m.SupportsTools() {
		t.Fatal("tools flag")
	}
	if m.Pricing == nil || m.Pricing.InputPerM != 3.0 || m.Pricing.OutputPerM != 15.0 {
		t.Fatalf("pricing = %+v", m.Pricing)
	}
	if m.Pricing.Currency != "USD" {
		t.Fatal("currency")
	}
}

func TestCatalogMissingPricingMeansBillingApplies(t *testing.T) {
	m, ok := GetModel("commandcode/qwen/qwen3-coder-plus")
	if !ok {
		t.Fatal("entry missing")
	}
	if m.HasPricing() {
		t.Fatal("qwen3-coder-plus must have no pricing")
	}
	// Zero cost but an explicit billing-applies disclaimer, never "free".
	if m.CostFor(Usage{InputTokens: 1000, OutputTokens: 1000}) != 0 {
		t.Fatal("no pricing must compute zero cost")
	}
	if !strings.Contains(BillingDisclaimer, "billing applies") {
		t.Fatal("disclaimer text")
	}
}

func TestCatalogCostFor(t *testing.T) {
	m, _ := GetModel("anthropic/claude-sonnet-4")
	u := Usage{InputTokens: 1e6, OutputTokens: 1e6, CacheReadTokens: 1e6, CacheWriteTokens: 1e6}
	// 3.0 + 15.0 + 0.3 + 3.75
	if got := m.CostFor(u); got < 22.04 || got > 22.06 {
		t.Fatalf("cost = %f, want ~22.05", got)
	}
	// Reasoning tokens are an output SUB-BUCKET — costFor must not double-count
	// (Usage.TotalTokens excludes them too).
	u2 := Usage{OutputTokens: 100}
	if got := m.CostFor(u2); got != 15.0*100/1e6 {
		t.Fatalf("reasoning double-count: %f", got)
	}
}

func TestCatalogAliasLookup(t *testing.T) {
	m, ok := GetModelForProvider("anthropic", "sonnet-4") // alias
	if !ok || m.ID != "claude-sonnet-4" {
		t.Fatalf("alias = %#v ok=%v", m, ok)
	}
}

func TestCatalogPerProvider(t *testing.T) {
	counts := CatalogProviders()
	for _, p := range []string{"anthropic", "openai", "openrouter", "opencode", "opencode-go", "commandcode", "ollama"} {
		if counts[p] == 0 {
			t.Fatalf("provider %s has no catalog models", p)
		}
	}
	// Hidden entries are excluded.
	if m, ok := GetModel("ollama/llama3.2"); !ok || !m.Visible {
		t.Fatalf("llama3.2 = %#v ok=%v", m, ok)
	}
}

var _ = errors.New
