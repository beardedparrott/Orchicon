package orchicon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// OpenCode routing (D2): OpenCode Zen and Go do NOT accept every model on
// /chat/completions — each model lives on a specific wire endpoint, and
// posting to the wrong route yields an opaque 500/400 (observed) instead of
// a clean 404. The route tables below are re-verified against the CURRENT
// public docs (Sep 2026):
//
//   - Zen  (https://opencode.ai/docs/zen/):  chat models → /v1/chat/completions
//     (deepseek-v4-flash, minimax-m2.5, kimi-k2.5, big-pickle…);
//     muse-spark-1.3-contributor-free + GPT models → /v1/responses;
//     Claude → /v1/messages; Gemini → model-specific.
//   - Go   (https://opencode.ai/docs/go/):  MiniMax/Qwen → /v1/messages
//     (Anthropic SDK); Grok / GPT-5.6-Luna / Muse-Spark-Contributors →
//     /v1/responses; everything else → /v1/chat/completions.
//
// The live /v1/models catalogs (https://opencode.ai/zen/v1/models,
// https://opencode.ai/zen/go/v1/models) are machine-readable and drive
// which model ids are valid; the route table maps a valid id to its wire.
// A model the catalog does not list is a loud warning (sourcing-aware
// feedback), never a silent guess.

// opencodeRoute is the wire endpoint a model lives on.
type opencodeRoute string

const (
	opencodeRouteChat      opencodeRoute = "chat"      // /chat/completions
	opencodeRouteResponses opencodeRoute = "responses" // /responses
	opencodeRouteMessages  opencodeRoute = "messages"  // /messages (Anthropic wire)
)

// opencodeRouteFor classifies a model id onto its documented wire route for
// the given OpenCode flavor ("opencode" = Zen, "opencode-go" = Go).
func opencodeRouteFor(provider, model string) opencodeRoute {
	m := strings.ToLower(model)
	if provider == "opencode-go" {
		// Go table (https://opencode.ai/docs/go/): MiniMax/Qwen → messages;
		// Grok / GPT-5.6-Luna / Muse-Spark-Contributors → responses; rest →
		// chat/completions.
		if strings.Contains(m, "minimax") || strings.Contains(m, "qwen") {
			return opencodeRouteMessages
		}
		if strings.Contains(m, "grok") || strings.Contains(m, "gpt-5.6-luna") ||
			strings.Contains(m, "muse-spark") {
			return opencodeRouteResponses
		}
		return opencodeRouteChat
	}
	// Zen table (https://opencode.ai/docs/zen/): Claude → messages; GPT /
	// muse-spark → responses; Gemini → responses (model-specific); rest →
	// chat/completions.
	if strings.HasPrefix(m, "claude-") {
		return opencodeRouteMessages
	}
	if strings.HasPrefix(m, "gpt-") || strings.Contains(m, "muse-spark") ||
		strings.HasPrefix(m, "gemini-") {
		return opencodeRouteResponses
	}
	return opencodeRouteChat
}

// opencodeRoutePath maps a route to the wire path appended to the base URL.
func opencodeRoutePath(r opencodeRoute) string {
	switch r {
	case opencodeRouteResponses:
		return "responses"
	case opencodeRouteMessages:
		return "messages"
	default:
		return "chat/completions"
	}
}

// opencodeRouteError builds the LOUD failure naming the model + required
// route, so Zen's opaque 500/400 never reaches the user unsupplemented.
func opencodeRouteError(provider, model string, r opencodeRoute) error {
	return fmt.Errorf(
		"opencode %s: model %q lives on /%s (per https://opencode.ai/docs/%s) — Orchicon's native client does not yet speak that wire; pick a chat-route model (e.g. deepseek-v4-flash) or add the %s client",
		provider, model, opencodeRoutePath(r), zenGoDocSlug(provider), opencodeRoutePath(r))
}

func zenGoDocSlug(provider string) string {
	if provider == "opencode-go" {
		return "go"
	}
	return "zen"
}

// opencodeClient is the model-aware OpenCode client (D2). It wraps the
// OpenAI-compat client for chat-route models and fails LOUDLY (naming the
// model + required route) for responses/messages models the native client
// does not yet speak — Zen's opaque 500/400 never reaches the user
// unsupplemented.
type opencodeClient struct {
	provider string // "opencode" | "opencode-go"
	baseURL  string
	apiKey   string
	http     *http.Client
	retry    RetryPolicy
	modelsFn func(ctx context.Context) ([]ModelInfo, error)
}

// StreamTurn routes per model id: chat-route models stream via the
// OpenAI-compat client (with the session header + first-party user-agent);
// responses/messages models fail loudly naming the model + required route.
func (c *opencodeClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	route := opencodeRouteFor(c.provider, req.Model)
	if route != opencodeRouteChat {
		return nil, opencodeRouteError(c.provider, req.Model, route)
	}
	oc := &OpenAICompatClient{
		BaseURL:    strings.TrimRight(c.baseURL, "/"),
		APIKey:     c.apiKey,
		Quirks:     builtinQuirks()[c.provider],
		HTTP:       c.http,
		Retry:      c.retry,
		ProviderID: c.provider,
		ModelsFn:   c.modelsFn,
	}
	return oc.StreamTurn(ctx, req)
}

// ListModels resolves through the sourcing service.
func (c *opencodeClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.modelsFn != nil {
		return c.modelsFn(ctx)
	}
	return nil, fmt.Errorf("%s: model sourcing not wired for this client", c.provider)
}

// Capabilities reports the chat-route surface (tools + streaming).
func (c *opencodeClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true}
}

