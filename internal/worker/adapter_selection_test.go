package worker

// Unit tests for the ADR-0005 adapter-selection helpers in validate.go:
// registered-kinds validation of the explicit adapter input, the
// adapter/ref agreement contract, the adapter-change contract, and the
// computed adapter exposure. No DB — the helpers are pure functions over
// injected seams (SetAdapterKinds / SetModelRefRegistry).

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

func mustKinds(kinds ...string) func() []string {
	return func() []string { return kinds }
}

func TestValidateAdapterInput(t *testing.T) {
	orig := adapterKindsFn
	origReg := modelRefRegistry
	defer func() { adapterKindsFn = orig; modelRefRegistry = origReg }()

	adapterKindsFn = mustKinds("opencode")
	modelRefRegistry = adapter.NewBuiltinProviderCatalog()

	// Empty input is valid (no explicit selection).
	got, err := validateAdapterInput("")
	if err != nil || got != "" {
		t.Fatalf("validateAdapterInput(empty) = %q, %v; want empty, nil", got, err)
	}
	// Registered kind passes (trimmed).
	got, err = validateAdapterInput(" opencode ")
	if err != nil || got != "opencode" {
		t.Fatalf("validateAdapterInput(opencode) = %q, %v; want opencode, nil", got, err)
	}
	// Catalog-known but UNREGISTERED kind is rejected — the explicit input
	// is a routing request and validates against the Dispatcher.
	_, err = validateAdapterInput("claude")
	if err == nil || !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("validateAdapterInput(claude) err = %v; want actionable error naming claude + registered kinds", err)
	}
	// Unwired kinds fn falls back to the catalog's kinds (headless/tests).
	adapterKindsFn = nil
	if _, err := validateAdapterInput("claude"); err != nil {
		t.Fatalf("unwired kinds: validateAdapterInput(claude) err = %v; want catalog fallback to accept", err)
	}
	if _, err := validateAdapterInput("nonexistent"); err == nil {
		t.Fatal("unwired kinds: validateAdapterInput(nonexistent) succeeded; want error")
	}
}

func TestValidateAdapterRefAgreement(t *testing.T) {
	// No input — no-op.
	if err := validateAdapterRefAgreement("", "opencode/anthropic/m"); err != nil {
		t.Fatalf("agreement(empty, ref) = %v; want nil", err)
	}
	// Lone adapter with no ref is rejected — the ref is the only store.
	err := validateAdapterRefAgreement("opencode", "")
	if err == nil || !strings.Contains(err.Error(), "without a model_ref") {
		t.Fatalf("agreement(opencode, empty) = %v; want lone-adapter rejection", err)
	}
	// Agreeing pair passes.
	if err := validateAdapterRefAgreement("opencode", "opencode/anthropic/m"); err != nil {
		t.Fatalf("agreement(opencode, opencode/anthropic/m) = %v; want nil", err)
	}
	// Mismatch is rejected.
	err = validateAdapterRefAgreement("claude", "opencode/anthropic/m")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("agreement(claude, opencode/...) = %v; want mismatch rejection", err)
	}
	// Legacy 2-segment ref parses to opencode — agreement with opencode passes.
	if err := validateAdapterRefAgreement("opencode", "anthropic/claude-4"); err != nil {
		t.Fatalf("agreement(opencode, anthropic/claude-4) = %v; want nil (legacy inference)", err)
	}
}

func TestValidateAdapterChange(t *testing.T) {
	origReg := modelRefRegistry
	defer func() { modelRefRegistry = origReg }()
	modelRefRegistry = adapter.NewBuiltinProviderCatalog()

	// Unchanged adapter re-save: even a catalog-known-but-deleted provider
	// keeps the ADR-0004 D5 re-save semantics (no adapter-change gate).
	if err := validateAdapterChange(context.Background(), "", "opencode/ghost/m", "opencode/ghost/m2"); err != nil {
		t.Fatalf("unchanged-adapter re-save = %v; want nil (D5 preserved)", err)
	}
	// Same parsed adapter (legacy vs 3-seg) is not a change.
	if err := validateAdapterChange(context.Background(), "", "anthropic/claude-4", "opencode/anthropic/m"); err != nil {
		t.Fatalf("legacy to 3-seg same adapter = %v; want nil", err)
	}
	// Adapter change to a valid provider/model pair passes.
	if err := validateAdapterChange(context.Background(), "", "opencode/anthropic/m", "orchicon/commandcode/deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("opencode to orchicon valid pair = %v; want nil", err)
	}
	// Adapter change keeping a provider unknown for the new kind is
	// rejected with an actionable error naming the new kind's valid
	// providers (the native bridge serves every built-in profile, so an
	// unknown id like "mystery" is the invalid-pair case).
	err := validateAdapterChange(context.Background(), "", "opencode/anthropic/m", "orchicon/mystery/m")
	if err == nil || !strings.Contains(err.Error(), "orchicon") || !strings.Contains(err.Error(), "commandcode") {
		t.Fatalf("opencode to orchicon with unknown provider = %v; want error naming orchicon providers", err)
	}
	// 1-segment legacy ref: parse-only, nothing further to validate.
	if err := validateAdapterChange(context.Background(), "", "opencode/anthropic/m", "bare-model"); err != nil {
		t.Fatalf("1-segment change = %v; want nil", err)
	}
	// Malformed new ref surfaces the parser error.
	if err := validateAdapterChange(context.Background(), "", "opencode/anthropic/m", "opencode/"); err == nil {
		t.Fatal("malformed new ref succeeded; want parser error")
	}
}

func TestAdapterKindOf(t *testing.T) {
	cases := []struct {
		ref  string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"bare-model", "opencode"},           // 1-seg legacy inference
		{"anthropic/claude-4", "opencode"},   // 2-seg legacy inference
		{"opencode/anthropic/m", "opencode"}, // explicit
		// Slashed model id stays verbatim; adapter is segment 1.
		{"orchicon/commandcode/deepseek/deepseek-v4-flash", "orchicon"},
	}
	for _, c := range cases {
		if got := adapterKindOf(c.ref); got != c.want {
			t.Errorf("adapterKindOf(%q) = %q; want %q", c.ref, got, c.want)
		}
	}
}

// TestValidateAdapterChangePhantomKindIsLegacy: a current ref whose parsed
// head is NOT a registered adapter kind (pre-namespace legacy data, e.g.
// "commandcode/deepseek/x") never expressed an adapter selection — the
// adapter-change gate is skipped entirely and any validated new ref saves.
// Regression for the 2026-09 QA save-brick (phantom "commandcode" kind).
func TestValidateAdapterChangePhantomKindIsLegacy(t *testing.T) {
	origReg := modelRefRegistry
	defer func() { modelRefRegistry = origReg }()
	modelRefRegistry = adapter.NewBuiltinProviderCatalog()

	if err := validateAdapterChange(context.Background(), "", "commandcode/deepseek/deepseek-v4-flash", "opencode/commandcode/deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("phantom-kind current ref blocked a valid new ref: %v", err)
	}
	if err := validateAdapterChange(context.Background(), "", "commandcode/deepseek/deepseek-v4-flash", "orchicon/commandcode/deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("phantom-kind current ref blocked a native ref: %v", err)
	}
	// An EMPTY current ref is a FIRST selection, not phantom data: the
	// full provider/model pair gate still applies (the existing service
	// tests pin InvalidArgument for an invalid pair from empty).
	if err := validateAdapterChange(context.Background(), "", "", "orchicon/mystery/m"); err == nil {
		t.Fatal("first selection with an invalid pair passed; want gate applied")
	}
}

// TestValidateAdapterChangeIdenticalResaveNoOp: re-saving the EXACT current
// ref is a pure no-op — even when the stored ref would fail fresh grammar
// validation (legacy data). Only a DIFFERENT ref triggers the gate.
func TestValidateAdapterChangeIdenticalResaveNoOp(t *testing.T) {
	origReg := modelRefRegistry
	defer func() { modelRefRegistry = origReg }()
	modelRefRegistry = adapter.NewBuiltinProviderCatalog()

	// Identical legacy ref: no-op despite the phantom head.
	if err := validateAdapterChange(context.Background(), "", "commandcode/deepseek/deepseek-v4-flash", "commandcode/deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("identical legacy re-save rejected: %v", err)
	}
	// Identical known-but-deleted provider re-save stays a no-op (D5).
	if err := validateAdapterChange(context.Background(), "", "opencode/ghost/m", "opencode/ghost/m"); err != nil {
		t.Fatalf("identical re-save rejected: %v", err)
	}
	// Whitespace-only differences are still identical.
	if err := validateAdapterChange(context.Background(), "", "opencode/ghost/m", " opencode/ghost/m "); err != nil {
		t.Fatalf("whitespace-identical re-save rejected: %v", err)
	}
}

// TestValidateModelRefForUpdateNoOp: the update-aware ref validator passes
// an identical legacy ref through WITHOUT grammar re-validation, and
// validates any different ref fully.
func TestValidateModelRefForUpdateNoOp(t *testing.T) {
	origReg := modelRefRegistry
	defer func() { modelRefRegistry = origReg }()
	modelRefRegistry = adapter.NewBuiltinProviderCatalog()

	// Identical legacy ref: no-op despite failing fresh grammar parse
	// ("commandcode" is not a registered adapter kind).
	got, err := validateModelRefForUpdate(context.Background(), "", "commandcode/deepseek/deepseek-v4-flash", "commandcode/deepseek/deepseek-v4-flash")
	if err != nil || got != "commandcode/deepseek/deepseek-v4-flash" {
		t.Fatalf("identical legacy re-save = (%q, %v); want verbatim, nil", got, err)
	}
	// A different ref is fully validated (this one is valid).
	if _, err := validateModelRefForUpdate(context.Background(), "", "commandcode/deepseek/x", "opencode/commandcode/deepseek/deepseek-v4-flash"); err != nil {
		t.Fatalf("changed valid ref rejected: %v", err)
	}
	// A different ref that fails the grammar is rejected.
	if _, err := validateModelRefForUpdate(context.Background(), "", "commandcode/deepseek/x", "commandcode/other/y"); err == nil {
		t.Fatal("changed legacy-shaped ref accepted; want parse error")
	}
}
