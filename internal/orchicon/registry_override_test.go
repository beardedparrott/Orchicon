package orchicon

// Registry built-in override resolution tests: a built-in provider id must
// resolve built-in default ⊕ tenant overrides (EffectiveProfile), with the
// pure built-in table as the no-row fallback.

import (
	"context"
	"errors"
	"testing"
)

func newOverrideTestRegistry() *Registry {
	return NewRegistry(nil, nil, nil, nil)
}

// AC: built-in ⊕ overrides — the loader's profile wins over the built-in
// default.
func TestRegistryBuiltinOverrideProfileWins(t *testing.T) {
	r := newOverrideTestRegistry()
	r.SetBuiltinOverridesLoader(func(ctx context.Context, tenantID, providerID string) (Profile, bool, error) {
		p := mustBuiltin("ollama")
		p.BaseURL = "https://ollama.example.com"
		return p, true, nil
	})
	p, err := r.profile(context.Background(), "tnt_test", "ollama")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if p.BaseURL != "https://ollama.example.com" {
		t.Errorf("BaseURL = %q, want the override (not the built-in default)", p.BaseURL)
	}
}

// AC: no stored row → pure built-in default (loader returns found=false).
func TestRegistryBuiltinNoRowFallsBackToBuiltin(t *testing.T) {
	r := newOverrideTestRegistry()
	r.SetBuiltinOverridesLoader(func(ctx context.Context, tenantID, providerID string) (Profile, bool, error) {
		return Profile{}, false, nil
	})
	p, err := r.profile(context.Background(), "tnt_test", "ollama")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if base, _ := BuiltinProfile("ollama"); p.BaseURL != base.BaseURL {
		t.Errorf("BaseURL = %q, want built-in default %q", p.BaseURL, base.BaseURL)
	}
}

// AC: loader error → logged fallback to built-in default (availability
// first — a lookup hiccup must not fail dispatch).
func TestRegistryBuiltinLoaderErrorFallsBack(t *testing.T) {
	r := newOverrideTestRegistry()
	r.SetBuiltinOverridesLoader(func(ctx context.Context, tenantID, providerID string) (Profile, bool, error) {
		return Profile{}, false, errors.New("db hiccup")
	})
	p, err := r.profile(context.Background(), "tnt_test", "ollama")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if base, _ := BuiltinProfile("ollama"); p.BaseURL != base.BaseURL {
		t.Errorf("BaseURL = %q, want built-in default fallback", p.BaseURL)
	}
}

// AC: a non-builtin id never consults the built-in overrides loader.
func TestRegistryCustomStillUsesCustomLoader(t *testing.T) {
	r := newOverrideTestRegistry()
	called := false
	r.SetBuiltinOverridesLoader(func(ctx context.Context, tenantID, providerID string) (Profile, bool, error) {
		called = true
		return Profile{}, false, nil
	})
	_, _ = r.profile(context.Background(), "tnt_test", "my-custom-provider")
	if called {
		t.Errorf("built-in overrides loader was consulted for a custom provider id")
	}
}

// AC: without the hook (nil loader) built-ins resolve to the pure table —
// the pre-fix behavior, kept as the safety fallback.
func TestRegistryNoHookPureBuiltin(t *testing.T) {
	r := newOverrideTestRegistry()
	p, err := r.profile(context.Background(), "tnt_test", "ollama")
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if base, _ := BuiltinProfile("ollama"); p.BaseURL != base.BaseURL {
		t.Errorf("BaseURL = %q, want built-in default", p.BaseURL)
	}
}

func mustBuiltin(id string) Profile {
	p, ok := BuiltinProfile(id)
	if !ok {
		panic("missing builtin: " + id)
	}
	return p
}
