package orchicon

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// ProfileKind discriminates how the registry drives a profile.
type ProfileKind string

const (
	ProfileKindAnthropic    ProfileKind = "anthropic"   // native Anthropic Messages client
	ProfileKindOpenAICompat ProfileKind = "openai"      // Chat Completions client
	ProfileKindCommandCode  ProfileKind = "commandcode" // dual-transport wrapper
	ProfileKindOllama       ProfileKind = "ollama"      // compat turns + native metadata
	ProfileKindCustom       ProfileKind = "custom"      // tenant-created OpenAI-compatible entry
)

// Quirks are per-profile wire quirks (D4) — first-class fields, not string
// flags. Defaults are conservative; built-in profiles set what their backend
// actually accepts.
type Quirks struct {
	// StreamOptionsIncludeUsage sends stream_options:{include_usage:true}
	// (OpenAI, OpenRouter, OpenCode Zen; several local servers reject it).
	StreamOptionsIncludeUsage bool

	// SupportsToolCalls gates the tools parameter entirely.
	SupportsToolCalls bool

	// SupportsReasoningEffort gates the reasoning_effort param.
	SupportsReasoningEffort bool

	// ReasoningField selects "reasoning_effort" (OpenAI) or "reasoning"
	// (some compat servers); empty = "reasoning_effort".
	ReasoningField string

	// SupportsDeveloperRole sends "developer" instead of "system" for the
	// system prompt (newer OpenAI wire); most compat servers want "system".
	SupportsDeveloperRole bool

	// CacheReadFromPromptTokensDetails reads prompt_tokens_details.
	// cached_tokens into Usage.CacheReadTokens (OpenAI-compatible). Servers
	// that don't return it simply contribute 0.
	CacheReadFromPromptTokensDetails bool

	// MergeSystemIntoUser converts the system prompt into a leading user
	// message (servers that reject the system role).
	MergeSystemIntoUser bool

	// UsageInFinalChunk asserts usage arrives on/before the final chunk
	// (OpenAI-style trailing usage-only chunk). When false the usage from
	// any chunk is still honored — this only documents the expected shape.
	UsageInFinalChunk bool
}

// Profile is one provider configuration row (built-in or tenant custom).
// NOTE: Visible lives here (not just on ModelInfo) as the provider-level
// default for sourcing filters.
type Profile struct {
	// ID is the provider id ("openai", "openrouter", "opencode",
	// "opencode-go", "commandcode", "ollama", or tenant custom slug).
	ID string

	Kind          ProfileKind
	BaseURL       string
	AuthEnv       string // env var carrying the credential ("" = none)
	AuthSecretRef string // tenant secret NAME ("" = none); wins over env
	Quirks        Quirks

	// Visible marks the provider selectable in pickers (built-ins: true).
	Visible bool

	// Custom marks a tenant-created entry (settings task manages CRUD).
	Custom bool

	// HiddenModels are per-provider visibility toggles (model ids the
	// operator hid from pickers; sourcing filters them).
	HiddenModels []string

	// ManualModels are operator-added model entries for custom providers
	// (optional context/output/reasoning hints) — the expected path for
	// local models whose servers omit metadata (D9).
	ManualModels []ModelInfo

	// NumCtxDefault is the Ollama profile's default context window
	// (options.num_ctx sent per request); 0 = server default (~4096, which
	// this provider warns against — silent truncation breaks compaction).
	NumCtxDefault int64
}

// baseURL env overrides (documented per profile).
const (
	envCommandCodeBase = "COMMANDCODE_API_BASE"
	envOllamaHost      = "OLLAMA_HOST"
	envOllamaNumCtx    = "OLLAMA_NUM_CTX"
)

// builtinBaseURLs are the ADR D4 table values.
func builtinBaseURLs() map[string]string {
	return map[string]string{
		"anthropic":   "https://api.anthropic.com",
		"openai":      "https://api.openai.com/v1",
		"openrouter":  "https://openrouter.ai/api/v1",
		"opencode":    "https://opencode.ai/zen/v1",
		"opencode-go": "https://opencode.ai/zen/go/v1",
		"commandcode": "https://api.commandcode.ai",
		"ollama":      "http://localhost:11434",
	}
}

// builtinAuthEnvs are the ADR D4 table values ("" = no auth, e.g. Ollama).
func builtinAuthEnvs() map[string]string {
	return map[string]string{
		"anthropic":   "ANTHROPIC_API_KEY",
		"openai":      "OPENAI_API_KEY",
		"openrouter":  "OPENROUTER_API_KEY",
		"opencode":    "OPENCODE_API_KEY",
		"opencode-go": "OPENCODE_API_KEY",
		"commandcode": "COMMANDCODE_API_KEY",
		"ollama":      "",
	}
}

// builtinQuirks capture per-provider wire behavior (D4).
func builtinQuirks() map[string]Quirks {
	return map[string]Quirks{
		"openai": {
			StreamOptionsIncludeUsage:        true,
			SupportsToolCalls:                true,
			SupportsReasoningEffort:          true,
			SupportsDeveloperRole:            true,
			CacheReadFromPromptTokensDetails: true,
			UsageInFinalChunk:                true,
		},
		"openrouter": {
			StreamOptionsIncludeUsage:        true,
			SupportsToolCalls:                true,
			SupportsReasoningEffort:          true,
			CacheReadFromPromptTokensDetails: true,
			UsageInFinalChunk:                true,
		},
		"opencode": { // OpenCode Zen
			StreamOptionsIncludeUsage:        true,
			SupportsToolCalls:                true,
			CacheReadFromPromptTokensDetails: true,
			UsageInFinalChunk:                true,
		},
		"opencode-go": { // OpenCode Go — distinct baseURL, same auth env as Zen
			StreamOptionsIncludeUsage:        true,
			SupportsToolCalls:                true,
			CacheReadFromPromptTokensDetails: true,
			UsageInFinalChunk:                true,
		},
		"ollama": {
			// local server: no stream_options (rejected by compat endpoint),
			// no auth; tools pass through for capable models.
			SupportsToolCalls: true,
		},
	}
}

// BuiltinProfileIDs lists the built-in profile ids in table order.
func BuiltinProfileIDs() []string {
	return []string{"anthropic", "openai", "openrouter", "opencode", "opencode-go", "commandcode", "ollama"}
}

// BuiltinProfile returns the built-in profile for id (ok=false when id is
// not built-in). baseURL env overrides: COMMANDCODE_API_BASE, OLLAMA_HOST.
func BuiltinProfile(id string) (Profile, bool) {
	base, ok := builtinBaseURLs()[id]
	if !ok {
		return Profile{}, false
	}
	p := Profile{
		ID:      id,
		BaseURL: base,
		AuthEnv: builtinAuthEnvs()[id],
		Quirks:  builtinQuirks()[id],
		Visible: true,
	}
	switch id {
	case "anthropic":
		p.Kind = ProfileKindAnthropic
	case "commandcode":
		p.Kind = ProfileKindCommandCode
		if v := os.Getenv(envCommandCodeBase); v != "" {
			p.BaseURL = v
		}
	case "ollama":
		p.Kind = ProfileKindOllama
		if v := os.Getenv(envOllamaHost); v != "" {
			p.BaseURL = v
		}
		if v := os.Getenv(envOllamaNumCtx); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				p.NumCtxDefault = n
			}
		}
	default:
		p.Kind = ProfileKindOpenAICompat
	}
	return p, true
}

// ValidateProfile checks a profile (built-in or tenant custom) before use.
// It is the service-half validation the sibling settings-UI task calls
// before storing rows.
func ValidateProfile(p Profile) error {
	if p.ID == "" {
		return fmt.Errorf("profile id must not be empty")
	}
	if len(p.ID) > 64 {
		return fmt.Errorf("profile id too long (max 64)")
	}
	if _, builtin := builtinBaseURLs()[p.ID]; builtin && p.Custom {
		return fmt.Errorf("profile id %q collides with a built-in provider", p.ID)
	}
	if p.BaseURL == "" {
		return fmt.Errorf("profile %q: base_url is required", p.ID)
	}
	if p.Kind == "" {
		p.Kind = ProfileKindCustom
	}
	switch p.Kind {
	case ProfileKindOpenAICompat, ProfileKindCustom, ProfileKindAnthropic, ProfileKindOllama:
		// fine
	case ProfileKindCommandCode:
		if p.Custom {
			return fmt.Errorf("profile %q: kind commandcode is built-in only", p.ID)
		}
		// built-in commandcode — fine
	default:
		return fmt.Errorf("profile %q: unknown kind %q", p.ID, p.Kind)
	}
	return nil
}

// hiddenSet returns the HiddenModels membership set.
func (p Profile) hiddenSet() map[string]bool {
	if len(p.HiddenModels) == 0 {
		return nil
	}
	m := make(map[string]bool, len(p.HiddenModels))
	for _, id := range p.HiddenModels {
		m[id] = true
	}
	return m
}

// profileLoader is the tenant-custom profile store hook. The sibling
// settings-UI task registers a loader; until then only built-ins exist.
type profileLoader func(ctx context.Context, tenantID string) ([]Profile, error)

var (
	customMu     sync.RWMutex
	customLoader profileLoader
)

// SetCustomProfileLoader installs the tenant-custom profile loader (the
// settings task calls this when wiring the daemon). nil clears it.
func SetCustomProfileLoader(fn profileLoader) {
	customMu.Lock()
	customLoader = fn
	customMu.Unlock()
}

func loadCustomProfiles(ctx context.Context, tenantID string) ([]Profile, error) {
	customMu.RLock()
	fn := customLoader
	customMu.RUnlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx, tenantID)
}
