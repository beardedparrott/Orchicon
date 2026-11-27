package providers

// antigravity.go — THE `google` PROVIDER TIER, answered WITHOUT going through EffectiveProfile.
//
// THE FAULT THIS FILE FIXES, in one sentence: the antigravity adapter kind's only provider is `google`
// (internal/adapter/providers.go: AddAdapterKind(KindAntigravity, "google")), `google` is BOTH absent from
// orchicon.BuiltinProfile's builtinBaseURLs/BuiltinProfileIDs AND absent from the vendored catalog, so
// Service.ListProviderModels' first statement — EffectiveProfile — returned db.ErrNotFound, which
// handler.mapErr turned into connect.CodeNotFound and the GUI rendered raw as `[not_found] db: not found`.
// The operator could see and select the provider pill (providersForAdapter synthesises it) but selecting it
// produced that error instead of a model list.
//
// WHY THE CLAUDE PRECEDENT DOES NOT TRANSFER: claude's provider is `anthropic`, which is a built-in profile
// AND has vendored catalog rows, so its list is only ever needed once EffectiveProfile has SUCCEEDED.
// `claudeManagedCatalogModels` is a fallback for the empty-list case inside the normal path; for `google`
// that path is never entered. So this is a branch at the TOP of ListProviderModels, not another helper
// consulted from the middle of it.
//
// WHAT IT DELIBERATELY IS NOT: a synthetic `google` orchicon.Profile. orchicon.ProfileKind has no ACP kind,
// ValidateProfile rejects unknown kinds, and opencodeProviderNPM would drop `google` from the container
// provider config — a synthetic profile would advertise an OpenAI-compatible HTTP endpoint that does not
// exist, and orchicon never speaks HTTP to agy at all: it speaks ACP over stdio to a child process.
//
// THE SEAM. internal/providers must not import internal/antigravity (that package pulls the dispatcher,
// the runtime client and the DB, so the import would be a cycle). So the live listing is a function value
// injected by the plane: internal/antigravity/modellist.go holds the real `agy acp` handshake and its TTL
// cache, and internal/server/server.go injects a closure over the bridge. An unwired lister is not an
// error — it is the plane saying "no agy here", and the answer is the vendored snapshot with a WARN.

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// ErrAntigravityAuth is the sentinel a lister returns (wrapped, so errors.Is matches) when the antigravity
// adapter cannot authenticate: no `agy` binary on the host, or agy present and signed out.
//
// IT IS A SENTINEL RATHER THAN A TYPED ERROR because the transport (internal/antigravity/acp) is on the
// other side of the seam and this package cannot import it. The lister closure in server.go is what
// classifies `*acp.AuthError` and the binary-missing failure into this, and handler.mapErr is what turns it
// into connect.CodeFailedPrecondition carrying adapter.AntigravityAuthRequiredMessage.
var ErrAntigravityAuth = errors.New(adapter.AntigravityAuthRequiredMessage)

// AgyModel is one model an antigravity listing produced. ID is agy's BARE `modelId` (e.g.
// "gemini-4-carbon-high") — never a 3-segment ref, because the picker composes `antigravity/google/<id>`
// itself and a full ref here would produce a 4-segment ref
// (frontend/src/api/providers.ts projectPickerModels, ModelPicker.tsx).
type AgyModel struct {
	// ID is the bare agy modelId.
	ID string
	// Name is agy's display name for the model (informational; the picker labels rows by id).
	Name string
}

// AntigravityModelLister lists the antigravity adapter's models for one tenant. It is the seam described
// above: the plane injects an implementation that drives the real `agy acp` handshake, and a test injects
// a fixed list. A non-auth failure (timeout, wedged child) is a plain error, NOT ErrAntigravityAuth: the
// caller falls back to the vendored snapshot for those and never for an auth failure.
type AntigravityModelLister func(ctx context.Context, tenantID string) ([]AgyModel, error)

// SetAntigravityModelLister injects the live agy model lister. Safe to call after construction (the plane
// wires it once boot has a bridge) and safe to leave unset (the vendored snapshot answers).
func (s *Service) SetAntigravityModelLister(fn AntigravityModelLister) {
	s.agyListerMu.Lock()
	s.agyLister = fn
	s.agyListerMu.Unlock()
}

// antigravityLister reads the injected lister (nil when the plane wired none).
func (s *Service) antigravityLister() AntigravityModelLister {
	s.agyListerMu.RLock()
	defer s.agyListerMu.RUnlock()
	return s.agyLister
}

// listAntigravityModels answers the `google` provider tier: the live agy catalogue when a lister is wired
// and works, the vendored snapshot (with a WARN) when it is not or fails for a non-auth reason, and an
// ErrAntigravityAuth the caller maps to CodeFailedPrecondition when the host simply is not signed in.
//
// AN AUTH FAILURE IS NEVER MASKED BY THE SNAPSHOT. A populated list on a signed-out host would tell the
// operator the tier works when every turn against it fails — the same class of lie as the error this
// change removes.
func (s *Service) listAntigravityModels(ctx context.Context, tenantID string) (ModelsResult, error) {
	if lister := s.antigravityLister(); lister != nil {
		models, err := lister(ctx, tenantID)
		switch {
		case err != nil && errors.Is(err, ErrAntigravityAuth):
			return ModelsResult{}, &antigravityAuthError{cause: err}
		case err != nil:
			s.warnAntigravityFallback(tenantID, "the live agy model listing failed", err)
		case len(models) == 0:
			s.warnAntigravityFallback(tenantID, "the live agy model listing returned no models", nil)
		default:
			return ModelsResult{Models: antigravityRowsFromLive(models), Enabled: true}, nil
		}
	} else {
		s.warnAntigravityFallback(tenantID, "no agy model lister is wired on this plane", nil)
	}

	snapshot := orchicon.AntigravityModelSnapshot()
	if len(snapshot) == 0 {
		// The embedded document failed to decode — the one case where there is nothing honest to show.
		return ModelsResult{}, errors.New("antigravity: the vendored model snapshot is unreadable and no live listing is available")
	}
	source, captured := orchicon.AntigravityModelSnapshotMeta()
	if s.log != nil {
		s.log.Warn("providers: listing the antigravity model tier from the vendored snapshot — it may be stale",
			"ref_id", antigravityProvider, "snapshot_source", source, "snapshot_captured", captured)
	}
	return ModelsResult{Models: antigravityRowsFromSnapshot(snapshot), Degraded: true, Enabled: true}, nil
}

// warnAntigravityFallback logs WHY the snapshot is being used. It is a WARN, not an Info: every picker
// open on this plane is about to show an operator a list agy did not confirm.
func (s *Service) warnAntigravityFallback(tenantID, reason string, err error) {
	if s.log == nil {
		return
	}
	attrs := []any{"ref_id", antigravityProvider, "tenant", tenantID}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	s.log.Warn("providers: "+reason+" — falling back to the vendored antigravity snapshot", attrs...)
}

// antigravityAuthError is the auth failure with an ACTIONABLE message and no transport prefix.
//
// Errors.Is(err, ErrAntigravityAuth) is true through Unwrap, which is what handler.mapErr keys on; Error()
// returns adapter.AntigravityAuthRequiredMessage verbatim because connect.NewError puts that string in the
// operator's `[code] message` — the `[not_found] db: not found` shape this change exists to remove.
type antigravityAuthError struct {
	cause error
}

func (e *antigravityAuthError) Error() string { return adapter.AntigravityAuthRequiredMessage }

func (e *antigravityAuthError) Unwrap() error { return ErrAntigravityAuth }

// antigravityRowsFromLive projects a live handshake's models into picker rows.
func antigravityRowsFromLive(models []AgyModel) []ModelRow {
	seen := make(map[string]bool, len(models))
	out := make([]ModelRow, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelRow{
			ID: id,
			// CONTEXT: agy's model list states no window, so the row carries the one window agy is
			// observed to report — `usage_update.size` (1048576 on agy 1.5.13, simple_turn.jsonl:13,17,
			// captured 2026-11). The live per-turn gauge (Mapper.ContextGauge) still overrides it at run
			// time; this is what lets the picker annotate a model and the compaction math start from a
			// real number instead of "unknown".
			Context: orchicon.AntigravityObservedContextWindow,
			// Reasoning is the EFFORT SUFFIX on the id, which is how agy encodes thinking level (there is
			// no per-model capability field in `models.availableModels`).
			Reasoning: agyEffortSuffix(id) != "",
			Source:    "live",
			// The window is known, so the picker must not warn that it is missing.
			WarnNoContext: false,
			Visible:       true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// antigravityRowsFromSnapshot projects the vendored snapshot into picker rows.
func antigravityRowsFromSnapshot(models []orchicon.ModelInfo) []ModelRow {
	out := make([]ModelRow, 0, len(models))
	for _, m := range models {
		// The snapshot carries the measured window per row (antigravity_models.go's loader fills it
		// from the file's `context`, defaulting to AntigravityObservedContextWindow), so a snapshot row
		// annotates its window exactly as a live row does and does not raise the "context unknown" warn.
		window := m.Context
		if window <= 0 {
			window = orchicon.AntigravityObservedContextWindow
		}
		out = append(out, ModelRow{
			ID:            m.ID,
			Context:       window,
			Reasoning:     m.SupportsReasoningEffort(),
			Source:        orchicon.AntigravitySnapshotProvenance,
			WarnNoContext: false,
			Visible:       true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// agyEffortSuffix reports the thinking-level suffix an agy model id carries, or "" when it carries none.
// agy spells effort as a SUFFIX on the model id (`gemini-4-carbon-high`), and the TUI renders the flag this
// drives at internal/tui/modelpick/modelpick.go.
func agyEffortSuffix(id string) string {
	lower := strings.ToLower(id)
	for _, suffix := range []string{"-xhigh", "-high", "-medium", "-low"} {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimPrefix(suffix, "-")
		}
	}
	return ""
}
