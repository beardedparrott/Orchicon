package orchicon

// Live smoke suite — env-gated, operator-run, NEVER in default CI.
//
// Gate: ORCHICON_TEST_LIVE_ORCHICON=1 (single gate for the whole matrix),
// mirroring the ORCHICON_TEST_DSN skip pattern used by the DB-backed tests
// (skip in default CI, run explicitly with real keys). The operator runs:
//
//   ORCHICON_TEST_LIVE_ORCHICON=1 go test ./internal/orchicon/ -run TestLiveSmokeOrchicon
//
// Cost-capped by construction: one tiny prompt per provider, tiny
// max_tokens, NO tools, no retries. Each provider asserts LIVE TRUTH ONLY:
//  1. the models list is non-empty straight from the endpoint probe
//     (no synthesized fallback — a probe failure/empty list is a FAILURE),
//  2. one tiny streaming turn completes with real text + usage,
//  3. usage is captured (OutputTokens > 0); cache buckets recorded where
//     the wire reports them (a cold one-shot may be 0 — never assumed),
//  4. a follow-up probe is non-empty again (no leaked/orphaned state).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLiveSmokeOrchicon(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_LIVE_ORCHICON") != "1" {
		t.Skip("live smoke disabled: set ORCHICON_TEST_LIVE_ORCHICON=1 + provider keys (cost-capped, single tiny call per provider)")
	}

	// Registry wired to env-credential resolution (secret -> env fallback)
	// and a live-truth SourcingService: Registry.Get builds each provider
	// with ModelsFn = the real endpoint probe, exactly as production does.
	reg := NewRegistry(NewCredentialResolver(nil, nil), NewSourcingService(nil, nil), nil, nil)

	tests := []struct {
		name       string
		providerID string
		model      string // default; override via ORCHICON_LIVE_MODEL_<PROVIDER>
	}{
		{name: "anthropic", providerID: "anthropic", model: "claude-haiku-4"},
		{name: "openai", providerID: "openai", model: "gpt-4o-mini"},
		{name: "openrouter", providerID: "openrouter", model: "openrouter/auto"},
		{name: "opencode", providerID: "opencode", model: "anthropic/claude-3-5-haiku-latest"},
		{name: "opencode-go", providerID: "opencode-go", model: "qwen/qwen3-coder-30b"},
		{name: "commandcode", providerID: "commandcode", model: "z-ai/glm-5.3-flash"},
		{name: "ollama", providerID: "ollama", model: "llama3.2"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			prov, err := reg.Get(context.Background(), "tnt_live", tt.providerID)
			if err != nil {
				t.Fatalf("resolve %s: %v", tt.providerID, err)
			}
			liveProviderSmoke(t, prov, liveModelOverride(tt.providerID, tt.model))
		})
	}

	// One custom OpenAI-compat (llama-server class): configured via env,
	// since a custom profile has no built-in registry entry. The base URL
	// MUST include the version root (…/v1):
	//   ORCHICON_LIVE_CUSTOM_BASE_URL=http://<host>:<port>/v1
	//   ORCHICON_LIVE_CUSTOM_API_KEY=…
	//   ORCHICON_LIVE_CUSTOM_MODEL=…
	t.Run("custom_openai_compat", func(t *testing.T) {
		base := os.Getenv("ORCHICON_LIVE_CUSTOM_BASE_URL")
		if base == "" || os.Getenv("ORCHICON_LIVE_CUSTOM_MODEL") == "" {
			t.Skip("custom OpenAI-compat smoke: set ORCHICON_LIVE_CUSTOM_BASE_URL (must include /v1) + ORCHICON_LIVE_CUSTOM_API_KEY + ORCHICON_LIVE_CUSTOM_MODEL")
		}
		key := os.Getenv("ORCHICON_LIVE_CUSTOM_API_KEY")
		prov := &OpenAICompatClient{
			BaseURL:    base,
			APIKey:     key,
			ProviderID: "custom",
			ModelsFn: func(ctx context.Context) ([]ModelInfo, error) {
				res := NewSourcingService(nil, nil).ListModels(ctx, Profile{
					ID: "custom", Kind: ProfileKindCustom, BaseURL: base,
				}, key)
				if res.Degraded || len(res.Models) == 0 {
					return nil, fmt.Errorf("custom probe degraded (no live models): %s", base)
				}
				return res.Models, nil
			},
		}
		liveProviderSmoke(t, prov, os.Getenv("ORCHICON_LIVE_CUSTOM_MODEL"))
	})
}

// liveModelOverride honours ORCHICON_LIVE_MODEL_<PROVIDER> (dashes ->
// underscores), defaulting to the table suggestion.
func liveModelOverride(providerID, def string) string {
	env := "ORCHICON_LIVE_MODEL_" + strings.ToUpper(strings.ReplaceAll(providerID, "-", "_"))
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// liveProviderSmoke asserts the live-truth contract for one provider:
// endpoint models non-empty, one tiny streamed turn with real usage, and
// a follow-up probe still non-empty (cleanup/no-leak).
func liveProviderSmoke(t *testing.T, prov Provider, model string) {
	t.Helper()

	// 1. LIVE models list (no synthesized fallback).
	models, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatalf("ListModels returned ZERO models — a synthesized fallback is forbidden (probe degraded or endpoint returned nothing)")
	}
	if models[0].ID == "" {
		t.Fatalf("ListModels returned a model with empty id (%+v)", models[0])
	}

	// 2. One tiny streamed turn (cost-capped).
	ts, err := prov.StreamTurn(testCtx(), TurnRequest{
		Model:     model,
		MaxTokens: 16,
		Messages:  []Message{{Role: RoleUser, Content: []Content{{Text: strPtr("Reply with the single word: ok")}}}},
	})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("live stream: %v", err)
	}
	var sawText, sawFinish bool
	var usage Usage
	for _, e := range evs {
		switch e := e.(type) {
		case TextDelta:
			sawText = true
			_ = e
		case Finish:
			sawFinish = true
			usage = e.Usage
		}
	}
	if !sawText || !sawFinish {
		t.Fatalf("live smoke incomplete: text=%v finish=%v", sawText, sawFinish)
	}
	// 3. Real usage captured. Output must be > 0 on a completed turn;
	// cache read/write may be 0 on a cold one-shot (never assumed), but
	// must never go negative and must stay internally consistent.
	if usage.OutputTokens <= 0 {
		t.Fatalf("live usage zeroed: %+v", usage)
	}
	if usage.InputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 {
		t.Fatalf("live usage has negative buckets: %+v", usage)
	}
	t.Logf("usage in=%d out=%d cacheRead=%d cacheWrite=%d", usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)

	// 4. Cleanup / no-leak: a follow-up probe still returns live models.
	models2, err := prov.ListModels(context.Background())
	if err != nil {
		t.Fatalf("follow-up ListModels: %v", err)
	}
	if len(models2) == 0 {
		t.Fatalf("follow-up ListModels returned zero models — provider state leaked/corrupted after the turn")
	}
}
