package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// CommandCodeClient is the dual-transport Command Code provider (D6): one
// Bearer key, transport selected PER REQUEST by model id — `claude-*` →
// POST {base}/provider/v1/messages (Anthropic wire); everything else →
// POST {base}/provider/v1/chat/completions (OpenAI wire). The legacy
// /alpha/generate envelope is the Go-plan / 403-flip fallback.
type CommandCodeClient struct {
	BaseURL string // default https://api.commandcode.ai (env COMMANDCODE_API_BASE)
	APIKey  string
	HTTP    *http.Client
	Retry   RetryPolicy

	// PlanOverride pins the plan resolution (explicit config wins over env).
	PlanOverride string

	// ThreadID rides legacy envelopes.
	ThreadID string

	// ModelsFn supplies ListModels (registry wires the sourcing service).
	ModelsFn func(ctx context.Context) ([]ModelInfo, error)

	env func(string) string

	mu           sync.Mutex
	plan         string
	planResolved bool
	pinnedLegacy bool
}

const (
	planProvider       = "provider"
	planGo             = "go"
	envCommandCodePlan = "COMMANDCODE_PLAN"
	envCommandCodeZDR  = "CMD_ZDR"
)

// Capabilities reports the union surface (both transports support tools+streaming).
func (c *CommandCodeClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true, ReasoningEfforts: true}
}

// ListModels resolves through the sourcing service. NO catalog fallback:
// per the no-synthesized-models directive, a failed sourcing probe yields
// no models — never the vendored snapshot.
func (c *CommandCodeClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.ModelsFn != nil {
		return c.ModelsFn(ctx)
	}
	return nil, fmt.Errorf("commandcode: model sourcing not wired for this client")
}

func (c *CommandCodeClient) getenv(k string) string {
	if c.env != nil {
		return c.env(k)
	}
	return os.Getenv(k)
}

// isClaudeModel reports whether the model id routes to the Anthropic wire
// (per the reference plugin: modelId starting with `claude-`).
func isClaudeModel(model string) bool { return strings.HasPrefix(model, "claude-") }

// resolvePlan implements the documented order: explicit override →
// COMMANDCODE_PLAN env → cached GET {base}/alpha/whoami → default provider.
// Cached per provider instance (fetched at most once).
func (c *CommandCodeClient) resolvePlan(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.planResolved {
		return c.plan
	}
	plan := planProvider
	switch {
	case c.PlanOverride != "":
		plan = c.PlanOverride
	case c.getenv(envCommandCodePlan) != "":
		plan = c.getenv(envCommandCodePlan)
	default:
		if p, err := c.fetchWhoamiPlan(ctx); err == nil && p != "" {
			plan = p
		}
	}
	c.plan, c.planResolved = plan, true
	return plan
}

func (c *CommandCodeClient) fetchWhoamiPlan(ctx context.Context) (string, error) {
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	url := strings.TrimRight(c.base(), "/") + "/alpha/whoami"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+c.APIKey)
	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whoami: status %d", resp.StatusCode)
	}
	var body struct {
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&body); err != nil {
		return "", err
	}
	return body.Plan, nil
}

func (c *CommandCodeClient) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	if v := c.getenv(envCommandCodeBase); v != "" {
		return v
	}
	return "https://api.commandcode.ai"
}

// preferLegacy reports whether turns must go through /alpha/generate.
func (c *CommandCodeClient) preferLegacy(ctx context.Context) bool {
	c.mu.Lock()
	pinned := c.pinnedLegacy
	c.mu.Unlock()
	if pinned {
		return true
	}
	return c.resolvePlan(ctx) == planGo
}

// pinLegacy pins the instance to the legacy transport (sticky for lifetime).
func (c *CommandCodeClient) pinLegacy() {
	c.mu.Lock()
	c.pinnedLegacy = true
	c.mu.Unlock()
}

// StreamTurn routes per model id, flipping to legacy on the documented 403
// upgrade_required (bounded: flip once, sticky for the instance lifetime).
func (c *CommandCodeClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	if c.preferLegacy(ctx) {
		return c.streamLegacy(ctx, req)
	}
	s, err := c.streamProvider(ctx, req)
	if err != nil && isUpgradeRequired403Err(err) {
		// Provider route rejected: pin to legacy and retry ONCE.
		c.pinLegacy()
		return c.streamLegacy(ctx, req)
	}
	return s, err
}

func isUpgradeRequired403Err(err error) bool {
	if se, ok := err.(*StatusError); ok {
		return isUpgradeRequired403(se.StatusCode, []byte(se.Body))
	}
	return false
}

// streamProvider routes by model id to the Provider API route.
func (c *CommandCodeClient) streamProvider(ctx context.Context, req TurnRequest) (TurnStream, error) {
	if isClaudeModel(req.Model) {
		// AnthropicClient appends /v1/messages → POST {base}/provider/v1/messages.
		ac := &AnthropicClient{
			BaseURL: strings.TrimRight(c.base(), "/") + "/provider", APIKey: c.APIKey, AuthStyle: "bearer",
			HTTP: c.HTTP, Retry: c.Retry,
		}
		if c.zdr() {
			ac.ExtraHeaders = map[string]string{"x-cmd-zdr": "1"}
		}
		return ac.StreamTurn(ctx, req)
	}
	oc := &OpenAICompatClient{
		BaseURL: strings.TrimRight(c.base(), "/") + "/provider/v1", APIKey: c.APIKey,
		Quirks: Quirks{StreamOptionsIncludeUsage: true, SupportsToolCalls: true,
			SupportsReasoningEffort: true, CacheReadFromPromptTokensDetails: true,
			UsageInFinalChunk: true},
		HTTP: c.HTTP, Retry: c.Retry, ProviderID: "commandcode",
	}
	if c.zdr() {
		oc.ExtraHeaders = map[string]string{"x-cmd-zdr": "1"}
	}
	return oc.StreamTurn(ctx, req)
}

// zdr reports the ZDR opt-in: header x-cmd-zdr: 1 ONLY when CMD_ZDR=1 —
// never otherwise.
func (c *CommandCodeClient) zdr() bool { return c.getenv(envCommandCodeZDR) == "1" }

// streamLegacy runs the /alpha/generate envelope transport with legacy headers.
func (c *CommandCodeClient) streamLegacy(ctx context.Context, req TurnRequest) (TurnStream, error) {
	env := buildLegacyEnvelope(req, c.ThreadID)
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("commandcode legacy: marshal: %w", err)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	url := strings.TrimRight(c.base(), "/") + "/alpha/generate"

	var resp *http.Response
	err = doWithRetries(ctx, c.Retry, func(attempt int) (bool, error, time.Duration) {
		r, err2 := postJSON(ctx, httpc, url, c.legacyHeaders(), body)
		if err2 != nil {
			return isConnectionErr(err2), err2, 0
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			resp = r
			return false, nil, 0
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		_ = r.Body.Close()
		e := httpStatusError(r.StatusCode, r.Status, b)
		if retryableStatus(r.StatusCode) {
			ra, _ := RetryAfter(r.Header.Get("Retry-After"), time.Now())
			return true, e, ra
		}
		return false, e, 0
	})
	if err != nil {
		return nil, err
	}
	return &legacyStream{r: newSSEReader(resp.Body), body: resp.Body}, nil
}

// legacyHeaders builds the documented legacy header set.
func (c *CommandCodeClient) legacyHeaders() map[string]string {
	h := map[string]string{
		"content-type":           "application/json",
		"accept":                 "text/event-stream",
		"authorization":          "Bearer " + c.APIKey,
		"x-command-code-version": "1.6.1",
		"x-cli-environment":      "production",
		"x-project-slug":         "orchicon",
		"x-taste-learning":       "true",
		"x-co-flag":              "false",
	}
	if c.zdr() {
		h["x-cmd-zdr"] = "1"
	}
	return h
}
