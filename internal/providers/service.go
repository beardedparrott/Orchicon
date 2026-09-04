// Package providers implements the tenant-facing Providers management
// surface behind Settings → Adapters (ADR-0006): the merged provider view
// (built-in profiles ⊕ stored overrides ⊕ tenant custom providers), custom
// provider CRUD, token auto-write into the tenant secrets store, and the
// deletion guard.
//
// It CONSUMES the provider-layer substrate (internal/orchicon) — profile
// validation, sourcing, credential resolution, registry — and never
// re-implements it. Plaintext tokens are write-only: they never appear in
// any response (only has_token_stored + the expected secret name).
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// refRE is the custom provider ref-id grammar (ADR-0006 D2):
// slug-style, 2–64 chars. Segment 2 of the model_ref namespace.
const refPattern = `^[a-z0-9][a-z0-9_-]{1,63}$`

var refRE = regexp.MustCompile(refPattern)

// Auth modes (custom providers only).
const (
	AuthModeNone  = "none"
	AuthModeToken = "token"
)

const secretDescription = "Provider token (auto-written from Settings → Adapters)"

// maxTokenLen bounds provider tokens (security standard: input size bounds).
const maxTokenLen = 4096

// maxDisplayNameLen bounds custom provider display names.
const maxDisplayNameLen = 200

// maxManualModels bounds the manual model list per provider.
const maxManualModels = 500

// errInvalidArgument marks validation failures; the handler maps it to
// connect.CodeInvalidArgument (no error-string sniffing).
var errInvalidArgument = errors.New("invalid argument")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errInvalidArgument, fmt.Sprintf(format, a...))
}

// builtinSecretNames is the standard-name auto-write map (ADR-0006 D5) —
// the single source of truth, matching the credential resolver's canonical
// naming (secret names coincide with the env var names). Ollama is
// deliberately absent: the local server needs no authentication.
var builtinSecretNames = map[string]string{
	"anthropic":   "ANTHROPIC_API_KEY",
	"openai":      "OPENAI_API_KEY",
	"openrouter":  "OPENROUTER_API_KEY",
	"opencode":    "OPENCODE_API_KEY",
	"opencode-go": "OPENCODE_API_KEY",
	"commandcode": "COMMANDCODE_API_KEY",
}

// CustomSecretName derives the standard tenant-secret name for a custom
// provider ref (ADR-0006 D2): CUSTOM_<REF uppercased, - → _>_API_KEY. The
// secrets name regex forbids hyphens, so `local-models` maps to
// CUSTOM_LOCAL_MODELS_API_KEY.
func CustomSecretName(ref string) string {
	return "CUSTOM_" + strings.ToUpper(strings.ReplaceAll(ref, "-", "_")) + "_API_KEY"
}

// ValidateRefID enforces the ref grammar and the derived-secret-name
// contract (must be storable under the secrets name regex).
func ValidateRefID(ref string) error {
	if !refRE.MatchString(ref) {
		return invalidf("ref_id %q must match %s", ref, refPattern)
	}
	if _, builtin := orchicon.BuiltinProfile(ref); builtin {
		return invalidf("ref_id %q collides with a built-in provider", ref)
	}
	if err := secrets.ValidateName(CustomSecretName(ref)); err != nil {
		return invalidf("ref_id %q: derived secret name %q is not storable: %v", ref, CustomSecretName(ref), err)
	}
	return nil
}

// validateBaseURL bounds and shape-checks a base URL.
func validateBaseURL(raw string) error {
	if raw == "" {
		return invalidf("base_url must not be empty")
	}
	if len(raw) > 2048 {
		return invalidf("base_url too long (max 2048)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalidf("base_url must be an absolute http(s) URL")
	}
	return nil
}

func validateAuthMode(mode string) error {
	if mode != AuthModeNone && mode != AuthModeToken {
		return invalidf("auth_mode must be %q or %q", AuthModeNone, AuthModeToken)
	}
	return nil
}

// ManualModel is one operator-added model entry (ADR-0006 D1/D4): the
// storage shape of manual_models JSONB, mirroring orchicon.ModelInfo hints.
type ManualModel struct {
	ID        string `json:"id"`
	Context   int64  `json:"context,omitempty"`
	MaxOutput int64  `json:"maxOutput,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
}

func marshalManualModels(models []ManualModel) ([]byte, error) {
	if models == nil {
		models = []ManualModel{}
	}
	b, err := json.Marshal(models)
	if err != nil {
		return nil, fmt.Errorf("providers: marshal manual models: %w", err)
	}
	return b, nil
}

func unmarshalManualModels(raw []byte) []ManualModel {
	if len(raw) == 0 {
		return nil
	}
	var out []ManualModel
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil // corrupt jsonb is treated as empty; the list stays editable
	}
	return out
}

// mergeManualModels merges operator manual-model edits into the stored
// list. replace=true swaps the whole list; otherwise entries merge by id
// (the submitted entry wins), preserving stored order first. Bounded by
// maxManualModels.
func mergeManualModels(current, updates []ManualModel, replace bool) ([]ManualModel, error) {
	var out []ManualModel
	if replace {
		out = append(out, updates...)
	} else {
		out = append(out, current...)
		idx := make(map[string]int, len(out))
		for i, m := range out {
			idx[m.ID] = i
		}
		for _, u := range updates {
			if i, ok := idx[u.ID]; ok {
				out[i] = u
				continue
			}
			idx[u.ID] = len(out)
			out = append(out, u)
		}
	}
	if len(out) > maxManualModels {
		return nil, invalidf("too many manual models: max %d, got %d", maxManualModels, len(out))
	}
	for _, m := range out {
		if strings.TrimSpace(m.ID) == "" || len(m.ID) > maxDisplayNameLen {
			return nil, invalidf("manual model id must be 1–%d characters", maxDisplayNameLen)
		}
		if m.Context < 0 || m.MaxOutput < 0 {
			return nil, invalidf("manual model %q: context/max_output must be non-negative", m.ID)
		}
	}
	// Dedup defensively (a corrupted store could carry repeats).
	seen := map[string]bool{}
	dedup := out[:0]
	for _, m := range out {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		dedup = append(dedup, m)
	}
	return dedup, nil
}

// Entry is one merged provider row (ADR-0006 D4). Built-ins carry
// ReadOnly=true; plaintext tokens never appear here.
type Entry struct {
	ID              string
	DisplayName     string
	Kind            string
	BaseURL         string // effective (override wins over built-in default)
	BaseURLOverride string
	Enabled         bool
	AuthMode        string
	IsCustom        bool
	ReadOnly        bool
	HasTokenStored  bool
	TokenSecretName string
	NumCtxDefault   int64
	HiddenModels    []string
	ManualModels    []ManualModel
}

// ReferencingWorker is one live reference to a provider (deletion guard).
type ReferencingWorker struct {
	WorkerID   string
	WorkerName string
	ModelRef   string
}

// ErrReferencedByWorkers is the deletion-guard sentinel; use
// errors.As to extract the *ReferencedError.
var ErrReferencedByWorkers = errors.New("provider referenced by workers")

// ReferencedError carries the referencing workers out of DeleteCustom.
type ReferencedError struct {
	Workers []ReferencingWorker
}

func (e *ReferencedError) Error() string {
	names := make([]string, 0, len(e.Workers))
	for _, w := range e.Workers {
		names = append(names, w.WorkerName)
	}
	return fmt.Sprintf("%s: %s", ErrReferencedByWorkers, strings.Join(names, ", "))
}

func (e *ReferencedError) Is(target error) bool { return target == ErrReferencedByWorkers }

// Service is the provider settings core: storage over provider_settings,
// token auto-write into the tenant secrets store, custom CRUD, and the
// substrate loader/invalidation wiring.
type Service struct {
	pool *db.Pool
	kek  []byte
	log  *slog.Logger

	substrateOnce sync.Once
	registry      *orchicon.Registry
	sourcing      *orchicon.SourcingService
	resolver      *orchicon.CredentialResolver
}

// New constructs the service. kek may be nil/short — the store is then
// disabled (fail-closed on token writes), mirroring internal/secrets.
func New(pool *db.Pool, kek []byte, log *slog.Logger) *Service {
	return &Service{pool: pool, kek: kek, log: log}
}

// substrate lazily wires the orchicon substrate trio (credential resolver →
// sourcing service → registry). Built once per process; per-tenant client
// caches live inside the registry and are dropped via Registry.Invalidate.
func (s *Service) substrate() (*orchicon.Registry, *orchicon.SourcingService, *orchicon.CredentialResolver) {
	s.substrateOnce.Do(func() {
		resolver := orchicon.NewCredentialResolver(s.pool, s.kek)
		src := orchicon.NewSourcingService(s.log, nil)
		warn := func(format string, a ...any) {
			if s.log != nil {
				s.log.Warn(fmt.Sprintf(format, a...))
			}
		}
		s.registry = orchicon.NewRegistry(resolver, src, nil, warn)
		// BUILT-IN provider overrides (ADR-0006 D6 parity): registry.Get for
		// a built-in id resolves built-in default ⊕ tenant provider-settings
		// overrides via EffectiveProfile — the SAME mapping the RPC views
		// use. Without this hook every built-in chat client used the
		// built-in defaults (observed: an ollama CLOUD BaseURLOverride in
		// Settings while chat kept dialing http://localhost:11434).
		s.registry.SetBuiltinOverridesLoader(s.EffectiveProfile)
		s.sourcing = src
		s.resolver = resolver
	})
	return s.registry, s.sourcing, s.resolver
}

// Registry exposes the substrate registry (future consumers, tests).
func (s *Service) Registry() *orchicon.Registry { r, _, _ := s.substrate(); return r }

// SourcingService exposes the sourcing service (future consumers, tests).
func (s *Service) SourcingService() *orchicon.SourcingService { _, src, _ := s.substrate(); return src }

// CredentialResolver exposes the resolver (end-to-end credential tests).
func (s *Service) CredentialResolver() *orchicon.CredentialResolver {
	_, _, r := s.substrate()
	return r
}

// RegisterSubstrateLoader installs this service as the orchicon
// custom-profile loader (the substrate's designated hook, ADR-0006 D6).
// From then on orchicon.Registry.Get resolves tenant custom provider ids
// end-to-end; DISABLED providers never load (they must never resolve to
// clients). Called once at daemon wiring (internal/api.Mount).
func (s *Service) RegisterSubstrateLoader() {
	orchicon.SetCustomProfileLoader(s.loadEnabledCustomProfiles)
}

// loadEnabledCustomProfiles implements the substrate profileLoader
// signature: every ENABLED tenant custom provider as an orchicon.Profile.
func (s *Service) loadEnabledCustomProfiles(ctx context.Context, tenantID string) ([]orchicon.Profile, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("providers: load custom profiles: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListProviderSettings(ctx, tx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	var out []orchicon.Profile
	for _, r := range rows {
		if !r.IsCustom || !r.Enabled {
			continue
		}
		if p, ok := profileFromRow(r); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// profileFromRow maps a stored row to an orchicon.Profile (pure; no I/O).
// ok=false when the row references an unknown provider (deleted built-in).
func profileFromRow(r db.ProviderSettingsRow) (orchicon.Profile, bool) {
	if !r.IsCustom {
		base, ok := orchicon.BuiltinProfile(r.ProviderID)
		if !ok {
			return orchicon.Profile{}, false
		}
		if r.BaseURLOverride != "" {
			base.BaseURL = r.BaseURLOverride
		}
		if r.NumCtxDefault > 0 {
			base.NumCtxDefault = r.NumCtxDefault
		}
		// Bind the standard secret name so the credential resolver finds the
		// tab-written token (AuthSecretRef-first, env fallback) — without
		// this the E2E flow fails ErrAuthMissing after a token save.
		if ref := builtinSecretNames[r.ProviderID]; ref != "" {
			base.AuthSecretRef = ref
		}
		base.HiddenModels = r.HiddenModels
		return base, true
	}
	p := orchicon.Profile{
		ID:            r.ProviderID,
		Kind:          orchicon.ProfileKindCustom,
		BaseURL:       r.BaseURL,
		Custom:        true,
		Visible:       true,
		HiddenModels:  r.HiddenModels,
		NumCtxDefault: r.NumCtxDefault,
		// Custom providers are OpenAI-compatible by definition (Settings →
		// Adapters → Providers only accepts compat endpoints), and Orchicon
		// native sessions are inherently tool-driven (every turn requests
		// tools). Zero-value Quirks here silently dropped the tools array
		// from every wire request — a tool-trained model then improvises
		// its native token-format tool calls as PLAIN TEXT, the loop sees a
		// text-only finish, and executions "succeed" in 1-2 seconds with
		// markup garbage ("<｜DSML｜tool_calls>…") as the final message.
		Quirks: orchicon.Quirks{SupportsToolCalls: true},
	}
	if r.AuthMode == AuthModeToken {
		p.AuthSecretRef = CustomSecretName(r.ProviderID)
	}
	for _, m := range unmarshalManualModels(r.ManualModels) {
		p.ManualModels = append(p.ManualModels, orchicon.ModelInfo{
			ID: m.ID, Context: m.Context, MaxOutput: m.MaxOutput, Visible: true,
		})
	}
	return p, true
}

// EffectiveProfile resolves the effective orchicon.Profile for a provider
// (built-in ⊕ overrides, or custom) plus its enabled flag. The ONE mapping
// function behind the substrate loader and the RPC views.
func (s *Service) EffectiveProfile(ctx context.Context, tenantID, providerID string) (orchicon.Profile, bool, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return orchicon.Profile{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, providerID)
	if errors.Is(err, db.ErrNotFound) {
		// No stored row: pure built-in default (never a custom). Still bind
		// the standard AuthSecretRef — a tab-written token lives in the
		// secrets store alone (SetProviderToken writes no settings row), so
		// the resolver must look it up even without an overrides row.
		if base, ok := orchicon.BuiltinProfile(providerID); ok {
			if ref := builtinSecretNames[providerID]; ref != "" {
				base.AuthSecretRef = ref
			}
			return base, true, nil
		}
		return orchicon.Profile{}, false, db.ErrNotFound
	}
	if err != nil {
		return orchicon.Profile{}, false, err
	}
	p, ok := profileFromRow(row)
	if !ok {
		return orchicon.Profile{}, false, db.ErrNotFound
	}
	return p, row.Enabled, nil
}

// ListForTenant returns the merged provider view: every built-in profile
// (read-only, ⊕ stored overrides) plus the tenant's custom entries.
func (s *Service) ListForTenant(ctx context.Context, tenantID string) ([]Entry, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListProviderSettings(ctx, tx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]db.ProviderSettingsRow, len(rows))
	var customs []db.ProviderSettingsRow
	for _, r := range rows {
		if r.IsCustom {
			customs = append(customs, r)
			continue
		}
		byID[r.ProviderID] = r
	}

	out := make([]Entry, 0, len(orchicon.BuiltinProfileIDs())+len(customs))
	for _, id := range orchicon.BuiltinProfileIDs() {
		entry := builtinEntry(id, byID[id])
		if entry.TokenSecretName != "" {
			entry.HasTokenStored, err = secretExists(ctx, tx.Tx, tenantID, entry.TokenSecretName)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, entry)
	}
	for _, r := range customs {
		entry := customEntry(r)
		if entry.TokenSecretName != "" {
			entry.HasTokenStored, err = secretExists(ctx, tx.Tx, tenantID, entry.TokenSecretName)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// secretExists reports whether a tenant secret with that name is stored.
func secretExists(ctx context.Context, tx pgx.Tx, tenantID, name string) (bool, error) {
	_, err := db.GetSecretByName(ctx, tx, tenantID, name)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func builtinEntry(id string, r db.ProviderSettingsRow) Entry {
	base, ok := orchicon.BuiltinProfile(id)
	if !ok {
		return Entry{ID: id}
	}
	e := Entry{
		ID:              id,
		DisplayName:     id,
		Kind:            string(base.Kind),
		BaseURL:         base.BaseURL,
		Enabled:         true,
		ReadOnly:        true,
		TokenSecretName: builtinSecretNames[id],
		NumCtxDefault:   base.NumCtxDefault,
		HiddenModels:    []string{},
	}
	if r.ProviderID == id {
		e.BaseURLOverride = r.BaseURLOverride
		if e.BaseURLOverride != "" {
			e.BaseURL = e.BaseURLOverride
		}
		e.Enabled = r.Enabled
		e.HiddenModels = r.HiddenModels
		if r.NumCtxDefault > 0 {
			e.NumCtxDefault = r.NumCtxDefault
		}
	}
	return e
}

func customEntry(r db.ProviderSettingsRow) Entry {
	e := Entry{
		ID:           r.ProviderID,
		DisplayName:  r.DisplayName,
		Kind:         string(orchicon.ProfileKindCustom),
		BaseURL:      r.BaseURL,
		Enabled:      r.Enabled,
		AuthMode:     r.AuthMode,
		IsCustom:     true,
		HiddenModels: r.HiddenModels,
	}
	if e.DisplayName == "" {
		e.DisplayName = r.ProviderID
	}
	if r.AuthMode == AuthModeToken {
		e.TokenSecretName = CustomSecretName(r.ProviderID)
	}
	e.ManualModels = unmarshalManualModels(r.ManualModels)
	return e
}

// entryFromRow rebuilds an Entry after a mutation (secret-existence
// lookup rides the caller's transaction when provided).
func (s *Service) entryFromRow(ctx context.Context, tx pgx.Tx, tenantID string, r db.ProviderSettingsRow) (Entry, error) {
	var (
		e   Entry
		err error
	)
	if r.IsCustom {
		e = customEntry(r)
	} else {
		e = builtinEntry(r.ProviderID, r)
	}
	if e.TokenSecretName != "" {
		e.HasTokenStored, err = secretExists(ctx, tx, tenantID, e.TokenSecretName)
		if err != nil {
			return Entry{}, err
		}
	}
	return e, nil
}

// UpdateSettingsInput is the partial-update merge for provider settings
// (ADR-0006 D4): only set fields apply; valid for built-ins AND customs.
type UpdateSettingsInput struct {
	ProviderID      string
	Enabled         *bool
	BaseURLOverride *string
	NumCtxDefault   *int64
	HiddenModels    []string
	HiddenModelsSet bool
	ManualModels    []ManualModel
	ManualReplace   bool
}

// UpdateSettings applies a partial settings update. Built-ins persist an
// override row on first change; customs merge on top of their row.
// Registry invalidation fires on EVERY mutation path.
func (s *Service) UpdateSettings(ctx context.Context, tenantID string, in UpdateSettingsInput) (Entry, error) {
	in.ProviderID = strings.TrimSpace(in.ProviderID)
	if in.ProviderID == "" {
		return Entry{}, invalidf("provider_id must not be empty")
	}
	if in.BaseURLOverride != nil && *in.BaseURLOverride != "" {
		if err := validateBaseURL(*in.BaseURLOverride); err != nil {
			return Entry{}, err
		}
	}
	var hiddenArg []string
	if in.HiddenModelsSet {
		for _, m := range in.HiddenModels {
			if m = strings.TrimSpace(m); m != "" {
				hiddenArg = append(hiddenArg, m)
			}
		}
	}

	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var row db.ProviderSettingsRow
	existing, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, in.ProviderID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		if _, builtin := orchicon.BuiltinProfile(in.ProviderID); !builtin {
			return Entry{}, db.ErrNotFound
		}
		row, err = db.UpdateProviderSettingsBuiltIn(ctx, tx.Tx, tenantID, in.ProviderID,
			in.Enabled, in.BaseURLOverride, in.NumCtxDefault, hiddenArg, in.HiddenModelsSet)
	case err != nil:
		return Entry{}, err
	case existing.IsCustom:
		merged := existing
		if in.Enabled != nil {
			merged.Enabled = *in.Enabled
		}
		// Custom entries carry their base URL in base_url (their definition),
		// not base_url_override (built-in-only) — keeps UpdateCustom and
		// UpdateSettings uniform.
		if in.BaseURLOverride != nil {
			merged.BaseURL = *in.BaseURLOverride
		}
		if in.NumCtxDefault != nil {
			merged.NumCtxDefault = *in.NumCtxDefault
		}
		if in.HiddenModelsSet {
			if merged.HiddenModels == nil {
				merged.HiddenModels = []string{}
			}
			merged.HiddenModels = hiddenArg
		}
		if len(in.ManualModels) > 0 || in.ManualReplace {
			cur := unmarshalManualModels(existing.ManualModels)
			mergedList, merr := mergeManualModels(cur, in.ManualModels, in.ManualReplace)
			if merr != nil {
				return Entry{}, merr
			}
			if merged.ManualModels, err = marshalManualModels(mergedList); err != nil {
				return Entry{}, err
			}
		}
		if profile, ok := profileFromRow(merged); ok {
			if verr := orchicon.ValidateProfile(profile); verr != nil {
				return Entry{}, invalidf("%v", verr)
			}
		}
		row, err = db.UpsertProviderSettings(ctx, tx.Tx, merged)
	default:
		row, err = db.UpdateProviderSettingsBuiltIn(ctx, tx.Tx, tenantID, in.ProviderID,
			in.Enabled, in.BaseURLOverride, in.NumCtxDefault, hiddenArg, in.HiddenModelsSet)
	}
	if err != nil {
		return Entry{}, err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.settings_updated", TargetType: "provider", TargetID: in.ProviderID,
		After: audit.Snapshot(map[string]any{"provider": in.ProviderID})}); err != nil {
		return Entry{}, fmt.Errorf("providers: audit settings update: %w", err)
	}
	// Build the response entry INSIDE the open transaction — after Commit
	// the tx is closed and any further query fails (closed-tx read bug).
	e, err := s.entryFromRow(ctx, tx.Tx, tenantID, row)
	if err != nil {
		return Entry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, err
	}
	s.invalidate(tenantID, in.ProviderID)
	return e, nil
}

// CreateCustomInput creates a tenant-scoped custom provider entry.
type CreateCustomInput struct {
	DisplayName string
	RefID       string
	BaseURL     string
	AuthMode    string
}

// CreateCustom creates a custom OpenAI-compatible provider entry. Ref ids
// join the model_ref grammar (segment 2); built-in collisions are rejected.
func (s *Service) CreateCustom(ctx context.Context, tenantID string, in CreateCustomInput) (Entry, error) {
	in.RefID = strings.TrimSpace(in.RefID)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.AuthMode = strings.TrimSpace(in.AuthMode)
	if in.DisplayName == "" {
		in.DisplayName = in.RefID
	}
	if err := ValidateRefID(in.RefID); err != nil {
		return Entry{}, err
	}
	if len(in.DisplayName) > maxDisplayNameLen {
		return Entry{}, invalidf("display_name too long (max %d)", maxDisplayNameLen)
	}
	if err := validateBaseURL(in.BaseURL); err != nil {
		return Entry{}, err
	}
	// Plane-aware loopback resolution: inside the control-plane container,
	// "localhost:port" points at the container, not the user's machine —
	// rewrite to the docker bridge gateway and say so in plain language.
	if resolved, note, changed := resolveForPlane(in.BaseURL); changed {
		in.BaseURL = resolved
		if s.log != nil {
			s.log.Info("providers: custom provider base URL auto-resolved for the control-plane container", "ref_id", in.RefID, "note", note)
		}
	}
	if in.AuthMode == "" {
		in.AuthMode = AuthModeNone
	}
	if err := validateAuthMode(in.AuthMode); err != nil {
		return Entry{}, err
	}

	profile := orchicon.Profile{
		ID: in.RefID, Kind: orchicon.ProfileKindCustom, BaseURL: in.BaseURL,
		Custom: true, Visible: true,
	}
	if in.AuthMode == AuthModeToken {
		profile.AuthSecretRef = CustomSecretName(in.RefID)
	}
	if err := orchicon.ValidateProfile(profile); err != nil {
		return Entry{}, invalidf("%v", err)
	}

	manual, err := marshalManualModels(nil)
	if err != nil {
		return Entry{}, err
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, in.RefID); err == nil {
		return Entry{}, fmt.Errorf("provider %q already exists", in.RefID)
	} else if !errors.Is(err, db.ErrNotFound) {
		return Entry{}, err
	}
	row, err := db.UpsertProviderSettings(ctx, tx.Tx, db.ProviderSettingsRow{
		ID: uuid.NewString(), TenantID: tenantID, ProviderID: in.RefID,
		Enabled: true, BaseURL: in.BaseURL, AuthMode: in.AuthMode,
		DisplayName: in.DisplayName, IsCustom: true,
		HiddenModels: []string{}, ManualModels: manual,
	})
	if err != nil {
		return Entry{}, err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.created", TargetType: "provider", TargetID: in.RefID,
		After: audit.Snapshot(map[string]any{"provider": in.RefID, "auth_mode": in.AuthMode})}); err != nil {
		return Entry{}, fmt.Errorf("providers: audit create: %w", err)
	}
	// Build the response entry INSIDE the open transaction — after Commit
	// the tx is closed and any further query fails (closed-tx read bug).
	e, err := s.entryFromRow(ctx, tx.Tx, tenantID, row)
	if err != nil {
		return Entry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, err
	}
	s.invalidate(tenantID, in.RefID)
	return e, nil
}

// UpdateCustomInput updates a custom provider; the ref id is immutable.
type UpdateCustomInput struct {
	RefID       string
	DisplayName *string
	BaseURL     *string
	AuthMode    *string
}

// UpdateCustom edits display name / baseURL / auth mode of a custom entry.
func (s *Service) UpdateCustom(ctx context.Context, tenantID string, in UpdateCustomInput) (Entry, error) {
	in.RefID = strings.TrimSpace(in.RefID)
	if in.RefID == "" {
		return Entry{}, invalidf("ref_id must not be empty")
	}
	if in.DisplayName != nil {
		d := strings.TrimSpace(*in.DisplayName)
		if d == "" || len(d) > maxDisplayNameLen {
			return Entry{}, invalidf("display_name must be 1–%d characters", maxDisplayNameLen)
		}
	}
	if in.BaseURL != nil {
		if err := validateBaseURL(strings.TrimSpace(*in.BaseURL)); err != nil {
			return Entry{}, err
		}
		// Same plane-aware loopback resolution as CreateCustom.
		if resolved, note, changed := resolveForPlane(*in.BaseURL); changed {
			*in.BaseURL = resolved
			if s.log != nil {
				s.log.Info("providers: custom provider base URL auto-resolved for the control-plane container", "ref_id", in.RefID, "note", note)
			}
		}
	}
	if in.AuthMode != nil {
		if err := validateAuthMode(strings.TrimSpace(*in.AuthMode)); err != nil {
			return Entry{}, err
		}
	}

	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, in.RefID)
	if err != nil {
		return Entry{}, err
	}
	if !existing.IsCustom {
		return Entry{}, invalidf("provider %q is built-in and read-only", in.RefID)
	}
	merged := existing
	if in.DisplayName != nil {
		merged.DisplayName = strings.TrimSpace(*in.DisplayName)
	}
	if in.BaseURL != nil {
		merged.BaseURL = strings.TrimSpace(*in.BaseURL)
	}
	if in.AuthMode != nil {
		merged.AuthMode = strings.TrimSpace(*in.AuthMode)
	}
	if profile, ok := profileFromRow(merged); ok {
		if verr := orchicon.ValidateProfile(profile); verr != nil {
			return Entry{}, invalidf("%v", verr)
		}
	}
	row, err := db.UpsertProviderSettings(ctx, tx.Tx, merged)
	if err != nil {
		return Entry{}, err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.updated", TargetType: "provider", TargetID: in.RefID,
		After: audit.Snapshot(map[string]any{"provider": in.RefID})}); err != nil {
		return Entry{}, fmt.Errorf("providers: audit update: %w", err)
	}
	e, err := s.entryFromRow(ctx, tx.Tx, tenantID, row)
	if err != nil {
		return Entry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, err
	}
	s.invalidate(tenantID, in.RefID)
	return e, nil
}

// DeleteCustom deletes a tenant custom provider. The deletion guard blocks
// the delete while any worker / tenant-default model_ref still references
// the provider (ADR-0006 D3): a *ReferencedError lists every referencing
// worker and the row is untouched.
func (s *Service) DeleteCustom(ctx context.Context, tenantID, refID string) error {
	refID = strings.TrimSpace(refID)
	if refID == "" {
		return invalidf("ref_id must not be empty")
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, refID)
	if err != nil {
		return err
	}
	if !existing.IsCustom {
		return invalidf("provider %q is built-in and cannot be deleted", refID)
	}
	refs, err := s.scanModelRefs(ctx, tx.Tx, tenantID, refID)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ReferencedError{Workers: refs}
	}
	if err := db.DeleteProviderSettings(ctx, tx.Tx, tenantID, refID); err != nil {
		return err
	}
	// Purge the auto-written secret: a deleted provider must not orphan its
	// CUSTOM_<REF>_API_KEY entry in the secrets section (the token auto-write
	// contract, delete half). Idempotent — nothing stored is fine.
	if existing.AuthMode == AuthModeToken {
		if sec, serr := db.GetSecretByName(ctx, tx.Tx, tenantID, CustomSecretName(refID)); serr == nil {
			if derr := db.DeleteSecret(ctx, tx.Tx, tenantID, sec.ID); derr != nil {
				return derr
			}
		} else if !errors.Is(serr, db.ErrNotFound) {
			return serr
		}
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.deleted", TargetType: "provider", TargetID: refID}); err != nil {
		return fmt.Errorf("providers: audit delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.invalidate(tenantID, refID)
	return nil
}

// refsProvider reports whether a model_ref references the provider as its
// segment-2 value (pure). The structural parse (nil registry) resolves
// legacy 2-segment and explicit 3-segment refs; refs the parser rejects
// structurally fall back to a prefix match on "<provider>/".
func refsProvider(ref, providerID string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if parsed, err := adapter.ParseModelRef(ref, nil); err == nil {
		return parsed.Provider == providerID
	}
	return strings.HasPrefix(ref, providerID+"/")
}

// scanModelRefs collects every live reference to providerID: the tenant's
// worker/version model_refs plus the tenant default worker and Ask-Orchicon
// model refs. Deduped by (worker, model_ref).
func (s *Service) scanModelRefs(ctx context.Context, tx pgx.Tx, tenantID, providerID string) ([]ReferencingWorker, error) {
	rows, err := db.ListWorkerModelRefs(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	defaultWorker, defaultAsk, err := db.GetTenantDefaultModelRefs(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	type key struct{ name, ref string }
	seen := map[key]bool{}
	out := make([]ReferencingWorker, 0, len(rows))
	add := func(workerID, workerName, ref string) {
		k := key{workerName, ref}
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, ReferencingWorker{WorkerID: workerID, WorkerName: workerName, ModelRef: ref})
	}
	for _, r := range rows {
		if refsProvider(r.ModelRef, providerID) {
			add(r.WorkerID, r.WorkerName, r.ModelRef)
		}
	}
	for _, pair := range []struct{ ref, label string }{{defaultWorker, "default worker model"}, {defaultAsk, "Ask Orchicon default model"}} {
		if refsProvider(pair.ref, providerID) {
			add("", pair.label, pair.ref)
		}
	}
	return out, nil
}

// SetProviderToken auto-writes/updates the provider's tenant secret under
// the standard name (ADR-0006 D5). Idempotent: an unchanged value performs
// no write (no ciphertext churn, updated_at untouched). Plaintext is never
// returned. Ollama rejects token saves (it needs no authentication).
func (s *Service) SetProviderToken(ctx context.Context, tenantID, providerID, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", invalidf("token must not be empty")
	}
	if len(token) > maxTokenLen {
		return "", invalidf("token too long (max %d)", maxTokenLen)
	}
	if err := s.requireKEK(); err != nil {
		return "", err
	}
	name, err := s.secretNameFor(ctx, tenantID, providerID)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetSecretByName(ctx, tx.Tx, tenantID, name)
	switch {
	case err == nil:
		if pt, derr := secretcrypto.Decrypt(existing.Ciphertext, s.kek); derr == nil && string(pt) == token {
			return name, nil // idempotent no-op — no write, no audit churn
		}
		ct, eerr := secretcrypto.Encrypt([]byte(token), s.kek)
		if eerr != nil {
			return "", eerr
		}
		kv := int16(1)
		if _, uerr := db.UpdateSecret(ctx, tx.Tx, tenantID, existing.ID, nil, &ct, &kv); uerr != nil {
			return "", uerr
		}
		if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.token_updated", TargetType: "provider", TargetID: providerID,
			After: audit.Snapshot(map[string]any{"provider": providerID, "secret": name})}); err != nil {
			return "", fmt.Errorf("providers: audit token update: %w", err)
		}
	case errors.Is(err, db.ErrNotFound):
		ct, eerr := secretcrypto.Encrypt([]byte(token), s.kek)
		if eerr != nil {
			return "", eerr
		}
		if _, cerr := db.CreateSecret(ctx, tx.Tx, db.SecretRow{ID: uuid.NewString(), TenantID: tenantID, Name: name, Description: secretDescription, Ciphertext: ct, KeyVersion: 1}); cerr != nil {
			return "", cerr
		}
		if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.token_set", TargetType: "provider", TargetID: providerID,
			After: audit.Snapshot(map[string]any{"provider": providerID, "secret": name})}); err != nil {
			return "", fmt.Errorf("providers: audit token set: %w", err)
		}
	default:
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	s.invalidate(tenantID, providerID)
	return name, nil
}

// ClearProviderToken deletes the provider's stored token secret (opt-in
// "remove credential"); the resolver's env fallback then applies. No-op
// (nil) when nothing is stored.
func (s *Service) ClearProviderToken(ctx context.Context, tenantID, providerID string) error {
	name, err := s.secretNameFor(ctx, tenantID, providerID)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	existing, err := db.GetSecretByName(ctx, tx.Tx, tenantID, name)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := db.DeleteSecret(ctx, tx.Tx, tenantID, existing.ID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.token_cleared", TargetType: "provider", TargetID: providerID,
		After: audit.Snapshot(map[string]any{"provider": providerID, "secret": name})}); err != nil {
		return fmt.Errorf("providers: audit token clear: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.invalidate(tenantID, providerID)
	return nil
}

// requireKEK fail-closes token operations when the store is disabled.
func (s *Service) requireKEK() error {
	if len(s.kek) != 32 {
		return fmt.Errorf("secrets store unavailable: KEK not configured")
	}
	return nil
}

// secretNameFor resolves the standard secret name for a provider and
// validates the operation is legal (provider exists; ollama takes none).
func (s *Service) secretNameFor(ctx context.Context, tenantID, providerID string) (string, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return "", invalidf("provider_id must not be empty")
	}
	if _, builtin := orchicon.BuiltinProfile(providerID); builtin {
		name, ok := builtinSecretNames[providerID]
		if !ok || name == "" {
			return "", invalidf("provider %q requires no authentication — nothing to store", providerID)
		}
		return name, nil
	}
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, providerID)
	if err != nil {
		return "", err
	}
	if !row.IsCustom {
		return "", invalidf("provider %q is built-in", providerID)
	}
	if row.AuthMode != AuthModeToken {
		return "", invalidf("provider %q has auth mode %q — set auth mode to %q before storing a token", providerID, row.AuthMode, AuthModeToken)
	}
	return CustomSecretName(providerID), nil
}

// invalidate drops the substrate caches for one (tenant, provider) pair —
// called on EVERY mutation path (enable/disable, baseURL, tokens,
// visibility, manual models, delete).
func (s *Service) invalidate(tenantID, providerID string) {
	reg, _, _ := s.substrate()
	reg.Invalidate(tenantID, providerID)
}

// applyBaseURLRepair persists a self-healed base URL (custom providers
// only — a built-in's override must go through the operator, since it
// changes which endpoint the built-in wire profile targets). Audit-logged
// with before/after; best-effort: a persist failure leaves the runtime
// result usable for THIS listing, and the next probe retries the repair.
func (s *Service) applyBaseURLRepair(ctx context.Context, tenantID, providerID, from, to string) error {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := db.GetProviderSettings(ctx, tx.Tx, tenantID, providerID)
	if err != nil {
		return err
	}
	if !row.IsCustom {
		return fmt.Errorf("provider %q is built-in; base-URL repair is custom-only", providerID)
	}
	if row.BaseURL != from {
		// Concurrently changed (e.g. the operator edited it mid-probe):
		// do not clobber; the next probe re-evaluates.
		return fmt.Errorf("provider %q base URL changed during repair", providerID)
	}
	row.BaseURL = to
	if _, err := db.UpsertProviderSettings(ctx, tx.Tx, row); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx.Tx, audit.Entry{TenantID: tenantID, Action: "provider.base_url_self_healed", TargetType: "provider", TargetID: providerID,
		Before: audit.Snapshot(map[string]any{"base_url": from}),
		After:  audit.Snapshot(map[string]any{"base_url": to})}); err != nil {
		return fmt.Errorf("providers: audit self-heal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.invalidate(tenantID, providerID)
	return nil
}

// EnabledCustomProviderIDs lists the tenant's ENABLED custom provider ids
// (the model-ref validation merge source, ADR-0006 D6; also the substrate
// loader's filter). Served to internal/worker via the API wiring.
func (s *Service) EnabledCustomProviderIDs(ctx context.Context, tenantID string) ([]string, error) {
	tx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListProviderSettings(ctx, tx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.IsCustom && r.Enabled {
			out = append(out, r.ProviderID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ModelRow is one sourcing-view model (ADR-0006 D4): the merged catalog →
// probe → manual list (substrate SourcingService), annotated with the
// visibility toggle state and the WARN flag for selectable models lacking
// a context hint.
type ModelRow struct {
	ID            string
	Context       int64
	MaxOutput     int64
	Reasoning     bool
	Source        string
	WarnNoContext bool
	Visible       bool
}

// ModelsResult is one ListProviderModels result.
type ModelsResult struct {
	Models   []ModelRow
	Degraded bool
	Enabled  bool
}

// ListProviderModels surfaces the sourcing state for one provider. Hidden
// models are INCLUDED with Visible=false (the operator must be able to
// re-check them); probe failure is non-fatal (Degraded=true — the UI
// renders visibly degraded, never a blank list).
func (s *Service) ListProviderModels(ctx context.Context, tenantID, providerID string) (ModelsResult, error) {
	profile, enabled, err := s.EffectiveProfile(ctx, tenantID, providerID)
	if err != nil {
		return ModelsResult{}, err
	}
	if !enabled {
		return ModelsResult{Models: []ModelRow{}, Enabled: false}, nil
	}
	_, sourcing, resolver := s.substrate()
	// Resolve the credential (tenant secret → env) for the probe: an
	// auth-requiring endpoint 401s an unauthenticated probe, which today
	// degrades silently into a "probe failed" state. Resolve mirrors the
	// real client path; ErrAuthMissing resolves to "" (the probe proceeds
	// unauthenticated — non-fatal by design, and public/local endpoints
	// succeed without a credential).
	bearer, _ := resolver.Resolve(ctx, tenantID, profile) //nolint:errcheck // probe stays non-fatal without a credential
	// Ask the substrate for the UNFILTERED merged list (hidden set cleared),
	// then annotate visibility here — re-hiding is only possible when hidden
	// entries stay visible to the operator.
	unfiltered := profile
	unfiltered.HiddenModels = nil
	res := sourcing.ListModels(ctx, unfiltered, bearer)
	// SELF-HEALING probe: a stored URL that was saved before plane-aware
	// resolution existed (or that was entered in a form the container can't
	// reach — loopback, missing /v1) fails here forever. Try the most
	// likely repaired candidates; if one works, PERSIST it (chat uses the
	// same base URL — the fix must apply to turns, not just the listing)
	// and log the repair in plain language.
	if res.Degraded && len(res.Models) == 0 {
		for _, cand := range repairCandidates(profile.BaseURL) {
			cp := profile
			cp.BaseURL = cand
			if r2 := sourcing.ListModels(ctx, cp, bearer); !r2.Degraded && len(r2.Models) > 0 {
				if err := s.applyBaseURLRepair(ctx, tenantID, providerID, profile.BaseURL, cand); err != nil {
					break
				}
				if s.log != nil {
					s.log.Info("providers: probe self-healed the base URL", "ref_id", providerID, "from", profile.BaseURL, "to", cand)
				}
				profile.BaseURL = cand
				res = r2
				break
			}
		}
	}
	hidden := make(map[string]bool, len(profile.HiddenModels))
	for _, id := range profile.HiddenModels {
		hidden[id] = true
	}
	out := make([]ModelRow, 0, len(res.Models))
	for _, m := range res.Models {
		out = append(out, ModelRow{
			ID: m.ID, Context: m.Context, MaxOutput: m.MaxOutput,
			Reasoning: m.SupportsReasoningEffort(),
			Source:    m.Provenance, WarnNoContext: m.Context <= 0,
			Visible: !hidden[m.ID],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return ModelsResult{Models: out, Degraded: res.Degraded, Enabled: true}, nil
}
