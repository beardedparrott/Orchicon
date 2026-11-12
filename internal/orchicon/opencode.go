package orchicon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// OpenCode routing (D2): OpenCode Zen and Go do NOT accept every model on /chat/completions.

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
		// Go table (https://opencode.ai/docs/go/): MiniMax/Qwen → messages; Grok / GPT-5.6-Luna /
		// Muse-Spark-Contributors → responses; rest → chat/completions.
		if strings.Contains(m, "minimax") || strings.Contains(m, "qwen") {
			return opencodeRouteMessages
		}
		if strings.Contains(m, "grok") || strings.Contains(m, "gpt-5.6-luna") ||
			strings.Contains(m, "muse-spark") {
			return opencodeRouteResponses
		}
		return opencodeRouteChat
	}
	// Zen table (https://opencode.ai/docs/zen/): Claude → messages; GPT / muse-spark → responses;
	// Gemini → responses (model-specific); rest → chat/completions.
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

// opencodeClient is the model-aware OpenCode client (D2).
type opencodeClient struct {
	provider string // "opencode" | "opencode-go"
	baseURL  string
	apiKey   string
	http     *http.Client
	retry    RetryPolicy
	modelsFn func(ctx context.Context) ([]ModelInfo, error)

	// routeCache is the sticky per-model route cache (keyed provider|model).
	routeCache sync.Map
}

// StreamTurn routes per model id: chat-route models stream via the OpenAI-compat client,
// responses-route models via the Responses client, messages-route models fail loudly.
func (c *opencodeClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	route := c.cachedRoute(req.Model)
	if route == opencodeRouteMessages {
		return nil, opencodeRouteError(c.provider, req.Model, route)
	}
	ts, err := c.streamOnRoute(ctx, req, route)
	if err == nil {
		return ts, nil
	}
	if !isRoutingClassError(err) {
		return nil, err
	}
	// Routing-class failure: evict the failing route and retry the SAME turn once on the next wire.
	c.evictRoute(req.Model)
	next := nextRoute(route)
	if next == opencodeRouteMessages {
		return nil, opencodeRouteError(c.provider, req.Model, next)
	}
	ts2, err2 := c.streamOnRoute(ctx, req, next)
	if err2 != nil {
		return nil, err2
	}
	c.cacheRoute(req.Model, next)
	return ts2, nil
}

// cachedRoute returns the sticky cached route for a model, or the static
// table route when none is cached (the fast path — zero extra latency).
func (c *opencodeClient) cachedRoute(model string) opencodeRoute {
	if v, ok := c.routeCache.Load(c.provider + "|" + model); ok {
		return v.(opencodeRoute)
	}
	return opencodeRouteFor(c.provider, model)
}

func (c *opencodeClient) cacheRoute(model string, r opencodeRoute) {
	c.routeCache.Store(c.provider+"|"+model, r)
}

func (c *opencodeClient) evictRoute(model string) {
	c.routeCache.Delete(c.provider + "|" + model)
}

// streamOnRoute streams a turn on the given wire.
func (c *opencodeClient) streamOnRoute(ctx context.Context, req TurnRequest, route opencodeRoute) (TurnStream, error) {
	switch route {
	case opencodeRouteResponses:
		rc := &ResponsesClient{
			BaseURL: strings.TrimRight(c.baseURL, "/"), APIKey: c.apiKey,
			HTTP: c.http, Retry: c.retry, ProviderID: c.provider, ModelsFn: c.modelsFn,
		}
		return rc.StreamTurn(ctx, req)
	case opencodeRouteMessages:
		return nil, opencodeRouteError(c.provider, req.Model, route)
	default: // chat
		oc := &OpenAICompatClient{
			BaseURL: strings.TrimRight(c.baseURL, "/"), APIKey: c.apiKey,
			Quirks: builtinQuirks()[c.provider], HTTP: c.http, Retry: c.retry,
			ProviderID: c.provider, ModelsFn: c.modelsFn,
		}
		return oc.StreamTurn(ctx, req)
	}
}

// nextRoute returns the wire to try after a routing-class failure on r.
// chat ↔ responses; messages stays loud-fail in this task.
func nextRoute(r opencodeRoute) opencodeRoute {
	switch r {
	case opencodeRouteResponses:
		return opencodeRouteChat
	default:
		return opencodeRouteResponses
	}
}

// isRoutingClassError reports whether err is a routing-class failure — the shapes Zen returns for
// wrong-wire posts (404/415/422 with a body, or a 400 whose body names the route/wire mismatch).
func isRoutingClassError(err error) bool {
	se, ok := err.(*StatusError)
	if !ok {
		return false
	}
	switch se.StatusCode {
	case 404, 415, 422:
		return true
	case 400:
		return isWrongWireBody(se.Body)
	}
	return false
}

// isWrongWireBody reports whether a 400 body names a wire/route mismatch (as opposed to a generic
// bad request).
func isWrongWireBody(body string) bool {
	for _, hint := range []string{
		"wrong wire", "wrong endpoint", "wrong route",
		"/responses", "/chat/completions", "/messages",
		"not found", "no such", "unknown endpoint", "unsupported",
		"does not support", "not supported on",
	} {
		if len(hint) > 0 && len(body) >= len(hint) && containsFold([]byte(body), []byte(hint)) {
			return true
		}
	}
	return false
}

// ListModels resolves through the sourcing service.
func (c *opencodeClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.modelsFn != nil {
		return c.modelsFn(ctx)
	}
	return nil, fmt.Errorf("%s: model sourcing not wired for this client", c.provider)
}

// Capabilities reports the chat/responses surface (tools + streaming).
func (c *opencodeClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true}
}
