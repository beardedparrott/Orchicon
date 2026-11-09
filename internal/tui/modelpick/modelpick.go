// Package modelpick holds the screen-independent plumbing for the three-tier MODEL picker
// (kit2.ModelPicker).
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

// NativeAdapterKind is the adapter kind whose models resolve from the providers service rather than
// CLI discovery (ADR-0004 D1).
const NativeAdapterKind = "orchicon"

// catalogSourcedKinds is the set of adapter kinds whose MODEL tier resolves from the providers
// SOURCING view instead of opencode-CLI discovery.
var catalogSourcedKinds = func() map[string]bool {
	set := make(map[string]bool)
	for _, k := range adapter.CatalogSourcedAdapterKinds() {
		set[k] = true
	}
	return set
}()

// UsesCatalogSourcing reports whether an adapter kind's model tier comes from the providers
// sourcing view (vendored catalog ⊕ probe ⊕ manual) rather than opencode-CLI discovery.
func UsesCatalogSourcing(kind string) bool { return catalogSourcedKinds[kind] }

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

// SplitRef parses a stored ref into the picker's three segments using the SAME grammar authority
// the server validates with (internal/adapter).
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

// FetchModels loads one provider's model list for the chosen adapter.
func FetchModels(ctx context.Context, cl *client.Clients, kind, provider string) ([]kit2.PickerOption, bool, error) {
	if UsesCatalogSourcing(kind) {
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

// ModelOptionsNative projects the providers SOURCING view.
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

// ModelOptionsDiscovery projects opencode-CLI discovery models.
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

// ContextWindow resolves a model ref's context-window size (in tokens), reading the source the
// ref's OWN adapter specifies: The adapter segment is NEVER crossed.
func ContextWindow(ctx context.Context, cl *client.Clients, ref string) (int64, error) {
	kind, provider, model := SplitRef(ref)
	if provider == "" || model == "" {
		return 0, nil // a partial/legacy ref has no resolvable window
	}
	if UsesCatalogSourcing(kind) {
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

// ModelMeta is a model row's dim right-hand context: the context window the compaction math depends
// on, the reasoning flag, the missing-context warning (ADR-0006 D8.
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

// shortSource renders a provenance for a picker row.
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
		// THE PLANE'S OWN HINT IS THE ACTIONABLE PART, and this used to THROW IT AWAY.
		hint := planeHint(txt)
		if hint == "" {
			hint = "install opencode on the plane's PATH, or set ORCHICON_OPENCODE_BIN"
		}
		return "model discovery is not configured on this plane — " + hint + " — then try again"
	}
	return txt
}

// planeHint extracts the trailing parenthetical from a plane error, which is where every
// not-configured message this client shows puts its remedy.
func planeHint(txt string) string {
	i := strings.LastIndex(txt, "(")
	j := strings.LastIndex(txt, ")")
	if i < 0 || j < i {
		return ""
	}
	return strings.TrimSpace(strings.Trim(txt[i:j+1], "()"))
}
