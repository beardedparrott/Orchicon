// Package modelpick holds the screen-independent plumbing for the three-tier
// MODEL picker (kit2.ModelPicker): the per-adapter data sources, the ref-grammar
// handoff, and the option projections.
//
// It exists so ONE cascade serves every screen that chooses a model_ref
// (Control's tenant defaults, Execution's per-worker model) instead of each
// screen re-deriving the RPC mapping and the grammar handoff.
//
// Data sources (ADR-0004 D1 — mirroring the GUI's ModelPicker per adapter):
//   - adapter tier: AIGatewayService.ListAdapterKinds (registered kinds plus the
//     Ask-capable subset).
//   - provider tier: the NATIVE kind reads the merged Providers view
//     (ProviderService.ListProviders — built-ins ⊕ stored overrides ⊕ tenant
//     customs, ENABLED only, exactly what Settings → Adapters edits); every other
//     kind reads its adapter-scoped gateway set (AIGatewayService.ListProviders).
//   - model tier: the NATIVE kind reads the sourcing view
//     (ProviderService.ListProviderModels — vendored catalog ⊕ probe ⊕ manual);
//     every other kind uses opencode-CLI discovery
//     (AIGatewayService.ListOpenCodeModels).
//
// The two branches exist because the ref's own adapter segment decides which
// registry can resolve a model list at all: the native bridge does not share the
// opencode-CLI provider namespace, and vice versa.
package modelpick

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// NativeAdapterKind is the adapter kind whose models resolve from the providers
// service rather than CLI discovery (ADR-0004 D1). The picker lists it FIRST and
// defaults a fresh selection to it (ADR-0005 D5) — the operator's "select
// orchicon (first in the list) or opencode".
const NativeAdapterKind = "orchicon"

// KindsMsg carries the registered adapter kinds.
type KindsMsg struct {
	Kinds      []string
	AskCapable []string
	Err        error
}

// ProvidersMsg carries one adapter's provider list.
type ProvidersMsg struct {
	Adapter string
	Opts    []kit2.PickerOption
	Err     error
}

// ModelsMsg carries one provider's model list.
type ModelsMsg struct {
	Adapter  string
	Provider string
	Opts     []kit2.PickerOption
	Degraded bool
	Err      error
}

// SplitRef parses a stored ref into the picker's three segments using the SAME
// grammar authority the server validates with (internal/adapter), so the picker
// can never disagree with what the plane accepts.
//
// A nil registry is deliberate: this is a structural read of an ALREADY-STORED
// value, and the registry's known-provider checks would reject a legacy ref the
// plane still legitimately holds. An empty or malformed ref seeds an empty
// selection, so the operator re-chooses instead of being handed a guess.
func SplitRef(ref string) (kind, provider, model string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", ""
	}
	parsed, err := adapter.ParseModelRef(ref, nil)
	if err != nil {
		return "", "", ""
	}
	return parsed.Adapter, parsed.Provider, parsed.Model
}

// --- fetches ----------------------------------------------------------------

// FetchKinds loads the registered adapter kinds (and the Ask-capable subset).
func FetchKinds(ctx context.Context, cl *client.Clients) ([]string, []string, error) {
	if cl == nil || cl.AIGateway == nil {
		return nil, nil, errors.New("no AI gateway client")
	}
	resp, err := cl.AIGateway.ListAdapterKinds(ctx, connect.NewRequest(&apiv1.ListAdapterKindsRequest{}))
	if err != nil {
		return nil, nil, err
	}
	return resp.Msg.GetAdapterKinds(), resp.Msg.GetAskCapableKinds(), nil
}

// FetchProviders loads one adapter's provider tier.
func FetchProviders(ctx context.Context, cl *client.Clients, kind string) ([]kit2.PickerOption, error) {
	if kind == NativeAdapterKind {
		// The merged Providers view: the tenant's authoritative provider set,
		// with tenant customs included.
		if cl == nil || cl.Providers == nil {
			return nil, errors.New("no provider client")
		}
		resp, err := cl.Providers.ListProviders(ctx, connect.NewRequest(&apiv1.ProviderListRequest{}))
		if err != nil {
			return nil, err
		}
		return ProviderOptionsNative(resp.Msg.GetProviders()), nil
	}
	// Any other kind: its adapter-scoped provider set from the registry.
	if cl == nil || cl.AIGateway == nil {
		return nil, errors.New("no AI gateway client")
	}
	adapterKind := kind
	resp, err := cl.AIGateway.ListProviders(ctx, connect.NewRequest(&apiv1.ListProvidersRequest{Adapter: &adapterKind}))
	if err != nil {
		return nil, err
	}
	return ProviderOptionsGateway(resp.Msg.GetProviders()), nil
}

// FetchModels loads one provider's model list for the chosen adapter. The model
// VALUE is always the bare model id — never the legacy 2-segment model_ref — so
// the picker's join produces exactly adapter/provider/model.
func FetchModels(ctx context.Context, cl *client.Clients, kind, provider string) ([]kit2.PickerOption, bool, error) {
	if kind == NativeAdapterKind {
		if cl == nil || cl.Providers == nil {
			return nil, false, errors.New("no provider client")
		}
		resp, err := cl.Providers.ListProviderModels(ctx, connect.NewRequest(&apiv1.ProviderModelsRequest{ProviderId: provider}))
		if err != nil {
			return nil, false, err
		}
		return ModelOptionsNative(resp.Msg.GetModels()), resp.Msg.GetDegraded(), nil
	}
	if cl == nil || cl.AIGateway == nil {
		return nil, false, errors.New("no AI gateway client")
	}
	providerFilter, adapterKind := provider, kind
	resp, err := cl.AIGateway.ListOpenCodeModels(ctx, connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{
		Provider: &providerFilter,
		Adapter:  &adapterKind,
	}))
	if err != nil {
		return nil, false, err
	}
	return ModelOptionsDiscovery(resp.Msg.GetModels()), false, nil
}

// --- option projections (pure — unit-testable without a plane) --------------

// ProviderOptionsNative projects the merged Providers view into picker options.
// DISABLED providers are dropped: they are not selectable, matching the GUI.
func ProviderOptionsNative(entries []*apiv1.ProviderEntry) []kit2.PickerOption {
	out := make([]kit2.PickerOption, 0, len(entries))
	for _, p := range entries {
		if !p.GetEnabled() {
			continue
		}
		label := p.GetDisplayName()
		if label == "" {
			label = p.GetId()
		}
		meta := ""
		if p.GetIsCustom() {
			meta = "custom"
		}
		out = append(out, kit2.PickerOption{Value: p.GetId(), Label: label, Meta: meta})
	}
	return out
}

// ProviderOptionsGateway projects the adapter-scoped gateway provider set.
func ProviderOptionsGateway(provs []*apiv1.AIProvider) []kit2.PickerOption {
	out := make([]kit2.PickerOption, 0, len(provs))
	for _, p := range provs {
		label := p.GetName()
		if label == "" {
			label = p.GetId()
		}
		meta := ""
		if p.GetCustom() {
			meta = "custom"
		}
		out = append(out, kit2.PickerOption{Value: p.GetId(), Label: label, Meta: meta})
	}
	return out
}

// ModelOptionsNative projects the providers SOURCING view. A HIDDEN model is
// dropped (the operator re-enables it in Settings → Adapters), and a model with
// no context hint is ANNOTATED rather than dropped.
func ModelOptionsNative(models []*apiv1.ProviderModel) []kit2.PickerOption {
	out := make([]kit2.PickerOption, 0, len(models))
	for _, mo := range models {
		if !mo.GetVisible() {
			continue
		}
		out = append(out, kit2.PickerOption{
			Value: mo.GetId(),
			Label: mo.GetId(),
			Meta:  ModelMeta(mo.GetContext(), mo.GetReasoning(), mo.GetWarnNoContext(), mo.GetSource()),
		})
	}
	return out
}

// ModelOptionsDiscovery projects opencode-CLI discovery models. The VALUE is the
// bare model id — never the legacy 2-segment model_ref, which would produce a
// bogus 4-segment ref when the picker joins the segments.
func ModelOptionsDiscovery(models []*apiv1.OpenCodeModel) []kit2.PickerOption {
	out := make([]kit2.PickerOption, 0, len(models))
	for _, mo := range models {
		label := mo.GetName()
		if label == "" {
			label = mo.GetId()
		}
		ctxTokens := mo.GetLimits().GetContext() // nil-safe getter
		out = append(out, kit2.PickerOption{
			Value: mo.GetId(),
			Label: label,
			Meta:  ModelMeta(ctxTokens, mo.GetCapabilities().GetReasoning(), ctxTokens <= 0, ""),
		})
	}
	return out
}

// ContextWindow resolves a model ref's context-window size (in tokens),
// reading the source the ref's OWN adapter specifies:
//
//   - the native `orchicon` kind reads the providers sourcing view
//     (ProviderService.ListProviderModels → the native registry/sourcing
//     substrate, which shells out to nothing);
//   - every other kind reads opencode-CLI discovery.
//
// The adapter segment is NEVER crossed. In particular an `orchicon` ref must NOT
// fall back to the opencode CLI: the whole point of the native adapter is that
// the product works WITHOUT opencode installed, so a cross-source fallback here
// would make selecting `orchicon` silently invoke the opencode binary
// (ListOpenCodeModels returns Unimplemented when no discoverer is configured) —
// the exact dependency the native adapter exists to remove. An adapter whose
// model list legitimately IS the CLI (a legacy `opencode/...` ref) still reads
// the CLI, because for THAT adapter the CLI is the authority.
//
// Returns 0 with a nil error when the window is genuinely unknown from that
// source (a model with no context hint, a provider that cannot enumerate models,
// or a ref with no resolvable segments): callers render the bare occupancy
// rather than inventing a denominator, and must NOT cache the 0 as though it
// were a resolved window.
func ContextWindow(ctx context.Context, cl *client.Clients, ref string) (int64, error) {
	kind, provider, model := SplitRef(ref)
	if provider == "" || model == "" {
		return 0, nil // a partial/legacy ref has no resolvable window
	}
	if kind == NativeAdapterKind {
		return nativeModelContext(ctx, cl, provider, model)
	}
	return cliModelContext(ctx, cl, kind, provider, model)
}

// nativeModelContext reads the window from the providers sourcing view (the
// native substrate — no opencode binary involved).
func nativeModelContext(ctx context.Context, cl *client.Clients, provider, model string) (int64, error) {
	if cl == nil || cl.Providers == nil {
		return 0, errors.New("no provider client")
	}
	resp, err := cl.Providers.ListProviderModels(ctx, connect.NewRequest(&apiv1.ProviderModelsRequest{ProviderId: provider}))
	if err != nil {
		return 0, err
	}
	for _, m := range resp.Msg.GetModels() {
		if m.GetId() == model {
			return m.GetContext(), nil
		}
	}
	return 0, nil
}

// cliModelContext reads the window from opencode-CLI discovery.
func cliModelContext(ctx context.Context, cl *client.Clients, kind, provider, model string) (int64, error) {
	if cl == nil || cl.AIGateway == nil {
		return 0, errors.New("no AI gateway client")
	}
	providerFilter, adapterKind := provider, kind
	resp, err := cl.AIGateway.ListOpenCodeModels(ctx, connect.NewRequest(&apiv1.ListOpenCodeModelsRequest{
		Provider: &providerFilter,
		Adapter:  &adapterKind,
	}))
	if err != nil {
		return 0, err
	}
	for _, m := range resp.Msg.GetModels() {
		if m.GetId() == model {
			return m.GetLimits().GetContext(), nil
		}
	}
	return 0, nil
}

// --- display helpers --------------------------------------------------------

// ModelMeta is a model row's dim right-hand context: the context window the
// compaction math depends on, the reasoning flag, the missing-context warning
// (ADR-0006 D8: a model without a context hint stays SELECTABLE but is
// annotated, never silently dropped), and the ORIGIN of those numbers.
//
// Naming the origin is the point. A hand-authored snapshot and a maintained
// registry both render as "1.0M ctx", which is exactly how a wrong value goes
// unnoticed — the operator cannot tell a verified number from a typed one. With
// the source shown, the claim is checkable.
func ModelMeta(contextTokens int64, reasoning, warnNoContext bool, source string) string {
	parts := make([]string, 0, 4)
	switch {
	case contextTokens > 0:
		parts = append(parts, FmtTokens(contextTokens)+" ctx")
	case warnNoContext:
		parts = append(parts, "⚠ no ctx hint")
	}
	if reasoning {
		parts = append(parts, "reasoning")
	}
	if short := shortSource(source); short != "" {
		parts = append(parts, short)
	}
	return strings.Join(parts, " · ")
}

// shortSource renders a provenance for a picker row. The provider's own probe is
// the unremarkable default (the provider answering for its own model) and gets no
// annotation, so the row only grows when the number came from somewhere else.
func shortSource(source string) string {
	switch {
	case source == "", source == "probe":
		return ""
	case strings.HasPrefix(source, "registry:"):
		return strings.TrimPrefix(source, "registry:")
	}
	return source
}

// FmtTokens renders a token count compactly (200K, 1.0M).
func FmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatInt(n/1_000, 10) + "K"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// FriendlyErr turns a raw load failure into something an operator can act on. An
// unconfigured discoverer is the common case for the CLI-backed tier.
func FriendlyErr(err error) string {
	if err == nil {
		return ""
	}
	txt := err.Error()
	low := strings.ToLower(txt)
	if strings.Contains(low, "unimplemented") || strings.Contains(low, "not configured") {
		return "model discovery is not configured on this plane"
	}
	return txt
}
