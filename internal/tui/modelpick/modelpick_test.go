package modelpick

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// SplitRef uses the PINNED grammar, so the picker's tiers are seeded exactly as
// the plane would read the stored ref.
func TestSplitRefSeedsTheTiersFromTheGrammar(t *testing.T) {
	cases := []struct {
		ref                   string
		kind, provider, model string
	}{
		// Canonical 3-segment.
		{"opencode/anthropic/claude-sonnet-4", "opencode", "anthropic", "claude-sonnet-4"},
		// The model segment is the VERBATIM remainder (ADR-0003).
		{"orchicon/commandcode/deepseek/deepseek-v4-flash", "orchicon", "commandcode", "deepseek/deepseek-v4-flash"},
		// Legacy 2-segment: the adapter is inferred.
		{"ollama/llama3", "opencode", "ollama", "llama3"},
		// 1-segment: a bare model id.
		{"llama3", "opencode", "", "llama3"},
		// Empty and malformed seed nothing (the operator re-chooses).
		{"", "", "", ""},
		{"bad//ref/x", "", "", ""},
	}
	for _, c := range cases {
		kind, provider, model := SplitRef(c.ref)
		if kind != c.kind || provider != c.provider || model != c.model {
			t.Errorf("SplitRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.ref, kind, provider, model, c.kind, c.provider, c.model)
		}
	}
}

// The native provider tier drops DISABLED providers (not selectable) and badges
// tenant customs — the merged view the GUI consumes.
func TestProviderOptionsNativeDropsDisabledAndBadgesCustom(t *testing.T) {
	opts := ProviderOptionsNative([]*apiv1.ProviderEntry{
		{Id: "anthropic", DisplayName: "Anthropic", Enabled: true},
		{Id: "off", DisplayName: "Off", Enabled: false},
		{Id: "local-models", Enabled: true, IsCustom: true},
	})
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2 (the disabled provider must be dropped): %+v", len(opts), opts)
	}
	if opts[0].Value != "anthropic" || opts[0].Label != "Anthropic" {
		t.Fatalf("option 0 = %+v, want the display name", opts[0])
	}
	// A provider with no display name falls back to its id, and a custom one is
	// badged so the operator knows where to manage it.
	if opts[1].Value != "local-models" || opts[1].Label != "local-models" || opts[1].Meta != "custom" {
		t.Fatalf("option 1 = %+v, want the id with a custom badge", opts[1])
	}
}

// The gateway (legacy adapter) provider tier keeps the gateway's own labelling
// and custom flag.
func TestProviderOptionsGateway(t *testing.T) {
	opts := ProviderOptionsGateway([]*apiv1.AIProvider{
		{Id: "anthropic", Name: "Anthropic", Enabled: true},
		{Id: "bare"},
		{Id: "mine", Name: "Mine", Custom: true},
	})
	if len(opts) != 3 {
		t.Fatalf("got %d options, want all 3 (this tier does not filter)", len(opts))
	}
	if opts[0].Label != "Anthropic" || opts[0].Meta != "" {
		t.Fatalf("option 0 = %+v, want the gateway name and no badge", opts[0])
	}
	if opts[1].Label != "bare" {
		t.Fatalf("option 1 = %+v, want the id fallback", opts[1])
	}
	if opts[2].Meta != "custom" {
		t.Fatalf("option 2 = %+v, want a custom badge", opts[2])
	}
}

// The native model tier drops HIDDEN models and ANNOTATES a missing context hint
// rather than dropping it (ADR-0006 D8).
func TestModelOptionsNativeDropsHiddenAndAnnotatesMissingContext(t *testing.T) {
	opts := ModelOptionsNative([]*apiv1.ProviderModel{
		{Id: "claude-sonnet-4", Context: 200000, Reasoning: true, Visible: true},
		{Id: "hidden", Visible: false},
		{Id: "mystery", Visible: true, WarnNoContext: true},
	})
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2 (the hidden model must be dropped): %+v", len(opts), opts)
	}
	if opts[0].Value != "claude-sonnet-4" || !strings.Contains(opts[0].Meta, "200K ctx") {
		t.Fatalf("option 0 = %+v, want the context window in the meta", opts[0])
	}
	if !strings.Contains(opts[0].Meta, "reasoning") {
		t.Fatalf("option 0 meta = %q, want the reasoning flag", opts[0].Meta)
	}
	if opts[1].Value != "mystery" || !strings.Contains(opts[1].Meta, "no ctx hint") {
		t.Fatalf("option 1 = %+v, want a missing-context annotation (not a drop)", opts[1])
	}
}

// The discovery tier commits the BARE model id — never the legacy 2-segment
// model_ref, which would make the picker join a bogus 4-segment ref.
func TestModelOptionsDiscoveryUsesTheBareModelID(t *testing.T) {
	opts := ModelOptionsDiscovery([]*apiv1.OpenCodeModel{
		{
			Id: "claude-sonnet-4", ProviderId: "anthropic", Name: "Claude Sonnet 4",
			ModelRef: "anthropic/claude-sonnet-4",
			Limits:   &apiv1.ModelLimits{Context: 200000},
		},
		{Id: "bare"},
	})
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2", len(opts))
	}
	if opts[0].Value != "claude-sonnet-4" {
		t.Fatalf("value = %q, want the bare id (not %q)", opts[0].Value, "anthropic/claude-sonnet-4")
	}
	if opts[0].Label != "Claude Sonnet 4" {
		t.Fatalf("label = %q, want the display name", opts[0].Label)
	}
	if !strings.Contains(opts[0].Meta, "200K ctx") {
		t.Fatalf("meta = %q, want the context window", opts[0].Meta)
	}
	if opts[1].Label != "bare" {
		t.Fatalf("a model with no name must fall back to its id, got %q", opts[1].Label)
	}
}

func TestModelMetaAndFmtTokens(t *testing.T) {
	cases := []struct {
		ctx      int64
		reason   bool
		warn     bool
		contains []string
		empty    bool
	}{
		{ctx: 200000, contains: []string{"200K ctx"}},
		{ctx: 1000000, reason: true, contains: []string{"1.0M ctx", "reasoning"}},
		{ctx: 500, contains: []string{"500 ctx"}},
		{warn: true, contains: []string{"no ctx hint"}},
		{empty: true},
	}
	for _, c := range cases {
		got := ModelMeta(c.ctx, c.reason, c.warn)
		if c.empty {
			if got != "" {
				t.Errorf("ModelMeta(zero) = %q, want empty (nothing to annotate)", got)
			}
			continue
		}
		for _, want := range c.contains {
			if !strings.Contains(got, want) {
				t.Errorf("ModelMeta(%d, %v, %v) = %q, want it to contain %q", c.ctx, c.reason, c.warn, got, want)
			}
		}
	}
}

// An unconfigured discoverer is translated into something an operator can act on
// instead of leaking "unimplemented" at them.
func TestFriendlyErrExplainsAnUnconfiguredDiscoverer(t *testing.T) {
	if got := FriendlyErr(nil); got != "" {
		t.Errorf("FriendlyErr(nil) = %q, want empty", got)
	}
	if got := FriendlyErr(errText("rpc error: code = Unimplemented desc = boom")); !strings.Contains(got, "not configured") {
		t.Errorf("FriendlyErr(unimplemented) = %q, want a configuration hint", got)
	}
	if got := FriendlyErr(errText("connection refused")); got != "connection refused" {
		t.Errorf("FriendlyErr(other) = %q, want the raw text passed through", got)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
