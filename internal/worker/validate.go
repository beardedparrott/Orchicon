// Package worker implements the WorkerService Connect handler
// (docs/07_API_Specification.md §3.3, docs/05_Worker_Specification.md).
//
// It is the API-layer boundary between the generated Connect handlers
// and the data-access layer. Responsibilities:
//   - validate and sanitize all inputs (the security boundary),
//   - resolve the tenant from the request context,
//   - perform the mutation + outbox enqueue in one transaction,
//   - enforce the Worker lifecycle (draft → published → deprecated →
//     retired) and versioning model (docs/05 §4, §5),
//   - manage edit locks for the visual Worker editor (docs/07 §3.3).
//
// No business logic lives here beyond input validation and lifecycle
// transitions; reconcilers and policy engines govern deeper behavior
// (AGENTS.md invariant #1).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Input size bounds (AGENTS.md security standards: size bounds on all
// inputs to prevent memory-exhaustion abuse).
const (
	maxNameLen        = 500
	maxSlugLen        = 63
	maxDescLen        = 1 << 14 // 16 KiB
	maxPurposeLen     = 1 << 14
	maxPromptLen      = 1 << 20 // 1 MiB — system prompts can be large
	maxJSONFieldLen  = 1 << 20 // 1 MiB for permissions/budgets/labels/etc.
	maxVersionNoteLen = 1 << 14
	maxActorLen       = 200
)

// slugRE defines the canonical slug character set: lowercase alphanumerics
// and hyphens, must start and end alphanumeric. This is the security
// gate that rejects path-traversal or injection-laden slugs before they
// reach the database (mirrors internal/project/validate.go).
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// modelRefRegistry is the provider/kind catalog used to validate model_ref
// values at the API boundary. It defaults to the built-in catalog (opencode
// providers plus the known adapter kinds) and can be swapped for tests via
// SetModelRefRegistry. Tenant-created custom providers are merged in at the
// service layer (they are not part of this static catalog).
var modelRefRegistry adapter.ProviderRegistry = adapter.NewBuiltinProviderCatalog()

// SetModelRefRegistry replaces the validation catalog (test seam).
func SetModelRefRegistry(reg adapter.ProviderRegistry) {
	if reg != nil {
		modelRefRegistry = reg
	}
}

// validateModelRef trims and bounds-checks a model_ref, then validates it
// against the adapter/provider/model grammar (ADR-0003). An empty value is
// returned unchanged (empty model_ref is valid — the system default applies).
// The registry is a static built-in catalog; the settings/worker service
// layers additionally check tenant custom providers when one is configured.
func validateModelRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if utf8.RuneCountInString(ref) > maxNameLen {
		return "", fmt.Errorf("model_ref must be at most %d characters", maxNameLen)
	}
	if _, err := adapter.ParseModelRef(ref, modelRefRegistry); err != nil {
		return "", err
	}
	return ref, nil
}

// validateName trims and bounds-checks a worker name.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return name, nil
}

// normalizeSlug validates the slug if provided; otherwise derives one
// from the name (mirrors internal/project/validate.go).
func normalizeSlug(slug, name string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		slug = deriveSlug(name)
	}
	if !slugRE.MatchString(slug) {
		return "", fmt.Errorf("slug must match %s", slugRE.String())
	}
	if len(slug) > maxSlugLen {
		return "", fmt.Errorf("slug must be at most %d characters", maxSlugLen)
	}
	return slug, nil
}

// deriveSlug produces a best-effort slug from a name (mirrors
// internal/project/validate.go).
func deriveSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "worker"
	}
	return out
}

// validateTextField trims and bounds-checks a generic text field. Empty
// is allowed (defaults are applied at the DB layer).
func validateTextField(s string, max int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return s, nil
}

// validateJSONField validates a JSON-encoded string field: trims, checks
// size, and verifies valid JSON. Returns the canonical empty value for
// empty input so the DB always stores well-formed JSON (AGENTS.md
// security standards: JSON fields are validated).
func validateJSONField(s, empty, field string, max int) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return []byte(empty), nil
	}
	if len(s) > max {
		return nil, fmt.Errorf("%s must be at most %d bytes", field, max)
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("%s must be valid JSON", field)
	}
	return []byte(s), nil
}

// validateActor trims and bounds-checks the actor field for edit locks.
func validateActor(actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", errors.New("actor must not be empty")
	}
	if utf8.RuneCountInString(actor) > maxActorLen {
		return "", fmt.Errorf("actor must be at most %d characters", maxActorLen)
	}
	return actor, nil
}

// requireTenant resolves the tenant id from the context (mirrors
// internal/project/validate.go).
func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

// --- adapter selection (ADR-0005) ------------------------------------------

// adapterKindsFn returns the adapter kinds currently REGISTERED with the
// Dispatcher (ADR-0005 D2). It is injected by the server layer via
// SetAdapterKinds (a func to avoid an api → scheduler import cycle, the
// same seam Dependencies.AdapterKinds uses). nil falls back to the
// validation catalog's kinds (headless/tests).
var adapterKindsFn func() []string

// SetAdapterKinds wires the Dispatcher's registered adapter kinds into the
// worker service's adapter-input validation (ADR-0005 D2). Explicit
// adapter selections are deliberate routing requests, so they validate
// against what can actually dispatch — not merely what the catalog knows.
func SetAdapterKinds(fn func() []string) {
	adapterKindsFn = fn
}

// registeredAdapterKinds returns the adapter kinds an explicit selection
// may name: the Dispatcher's registered kinds when wired, else the
// validation catalog's kinds (headless/tests), else the legacy default
// kind so validation never accepts an empty allowlist silently.
func registeredAdapterKinds() []string {
	if adapterKindsFn != nil {
		if kinds := adapterKindsFn(); len(kinds) > 0 {
			return kinds
		}
	}
	if c, ok := modelRefRegistry.(interface{ AdapterKinds() []string }); ok {
		if kinds := c.AdapterKinds(); len(kinds) > 0 {
			return kinds
		}
	}
	return []string{adapter.DefaultAdapterKind}
}

// validateAdapterInput validates an explicit adapter selection against the
// Dispatcher's registered kinds (ADR-0005 D2/D3). An empty value is valid
// (no explicit selection was made); anything else must name a registered
// kind — catalog-known-but-unregistered kinds are rejected here with an
// actionable error, because an explicit input is a routing request.
func validateAdapterInput(kind string) (string, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", nil
	}
	if utf8.RuneCountInString(kind) > maxNameLen {
		return "", fmt.Errorf("adapter must be at most %d characters", maxNameLen)
	}
	for _, k := range registeredAdapterKinds() {
		if k == kind {
			return kind, nil
		}
	}
	return "", fmt.Errorf("unknown or unregistered adapter %q — registered adapter kinds: %s", kind, strings.Join(registeredAdapterKinds(), ", "))
}

// validateAdapterRefAgreement enforces ADR-0005 D2 for the optional
// explicit adapter input: it must travel WITH a model_ref (the ref is the
// only store — a lone adapter has nowhere to persist) and must AGREE with
// the ref's parsed adapter segment.
func validateAdapterRefAgreement(adapterSel, modelRef string) error {
	if adapterSel == "" {
		return nil
	}
	if strings.TrimSpace(modelRef) == "" {
		return fmt.Errorf("adapter %q was set without a model_ref — the model_ref's adapter segment is the persisted selection; send adapter/provider/model, or omit adapter", adapterSel)
	}
	parsed, err := adapter.ParseModelRef(modelRef, nil)
	if err != nil {
		return err
	}
	if parsed.Adapter != adapterSel {
		return fmt.Errorf("adapter %q does not match model_ref %q (parsed adapter %q) — the explicit selection must agree with the ref's adapter segment", adapterSel, modelRef, parsed.Adapter)
	}
	return nil
}

// validateAdapterChange enforces the adapter-change contract (ADR-0005 D4):
// when a version's model_ref changes to one whose parsed adapter kind
// differs from its CURRENT ref's parsed adapter kind, the full new pair
// must be valid FOR THE NEW adapter — the provider segment must be a known
// provider of that kind (built-in profile ∪ tenant custom via the same
// registry seam validateModelRef uses). Unchanged-adapter re-saves keep
// the ADR-0004 D5 semantics verbatim (catalog-known-but-deleted providers
// re-save flagged) and pass through here untouched.
func validateAdapterChange(currentRef, newRef string) error {
	current := adapterKindOf(currentRef)
	next := adapterKindOf(newRef)
	if current == next {
		return nil
	}
	parsed, err := adapter.ParseModelRef(newRef, modelRefRegistry)
	if err != nil {
		return err
	}
	if parsed.Provider == "" {
		// Legacy 1-segment ref: nothing to validate beyond the parse.
		return nil
	}
	provs := modelRefRegistry.Providers(parsed.Adapter)
	for _, p := range provs {
		if p == parsed.Provider {
			return nil
		}
	}
	if len(provs) == 0 {
		return fmt.Errorf("model_ref %q changes the worker's adapter from %q to %q, but %q has no known providers — switch to a provider of the new adapter (adapter/provider/model)", newRef, current, next, next)
	}
	return fmt.Errorf("model_ref %q changes the worker's adapter from %q to %q, but provider %q is not a known provider of %q — valid providers: %s", newRef, current, next, parsed.Provider, next, strings.Join(provs, ", "))
}

// adapterKindOf reports the worker-facing adapter selection for a
// model_ref (ADR-0005 D2, computed): the parsed adapter segment; legacy
// 1/2-segment refs report the inferred default kind ("opencode") — never
// a stored value; an empty ref reports empty.
func adapterKindOf(modelRef string) string {
	ref := strings.TrimSpace(modelRef)
	if ref == "" {
		return ""
	}
	return adapter.AdapterKind(ref)
}
