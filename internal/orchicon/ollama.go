package orchicon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OllamaClient (D5): turns via OpenAI-compat /v1 OR native /api/chat (when
// num_ctx is set — OpenAI-compat does NOT accept num_ctx), plus native
// metadata: /api/tags discovery, /api/show TRUE model metadata (context
// length + capabilities), /api/ps effective context of loaded models.
type OllamaClient struct {
	Host  string // default http://localhost:11434 (env OLLAMA_HOST)
	HTTP  *http.Client
	Retry RetryPolicy

	// NumCtxDefault is the configured context window (options.num_ctx value
	// used when req.OllamaNumCtx == 0); 0 = server default (~4096 — the
	// silent truncation hazard this provider warns about).
	NumCtxDefault int64

	// Warnf receives context warnings (nil = drop); consumed by provider
	// status metadata and the context-management task.
	Warnf func(format string, args ...any)

	// ModelsFn supplies ListModels (registry wires sourcing).
	ModelsFn func(ctx context.Context) ([]ModelInfo, error)

	mu       sync.Mutex
	showMeta map[string]ollamaShow // per-model cache (process-lifetime of instance)
}

// Capabilities: streaming, tools (model-dependent passthrough), native metadata.
func (c *OllamaClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true, NativeMetadata: true}
}

// ListModels merges native /api/tags discovery with sourcing (catalog +
// manual entries); /api/show TRUE metadata fills context where absent.
func (c *OllamaClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var out []ModelInfo
	if c.ModelsFn != nil {
		m, err := c.ModelsFn(ctx)
		if err != nil {
			return nil, err
		}
		out = m
	}
	// Native metadata enrichment for SOURCED entries — BEFORE the tags
	// call so ollama CLOUD endpoints (whose /api/tags may not exist) still
	// get it: cloud /v1/models carries no context data, and /api/show on
	// the same host fills the true context length. Best-effort: a
	// /api/show failure leaves the entry as-is (0 → the picker annotates,
	// compaction must not guess).
	for i := range out {
		if out[i].Context > 0 {
			continue
		}
		if ctxLen := c.contextLength(ctx, out[i].ID); ctxLen > 0 {
			out[i].Context = ctxLen
		}
	}
	tags, err := c.tags(ctx)
	if err != nil {
		// Native discovery failure is non-fatal when sourcing already served.
		if len(out) > 0 {
			return out, nil
		}
		return nil, fmt.Errorf("ollama: list models: %w", err)
	}
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.ID] = true
	}
	for _, t := range tags.Models {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		ctxLen := c.contextLength(ctx, t.Name)
		out = append(out, ModelInfo{ID: t.Name, Context: ctxLen, Visible: true, Provenance: "probe"})
	}
	return out, nil
}

// effectiveNumCtx is the context actually requested for a turn.
func (c *OllamaClient) effectiveNumCtx(req TurnRequest) int64 {
	if req.OllamaNumCtx > 0 {
		return req.OllamaNumCtx
	}
	return c.NumCtxDefault
}

// checkContextWarn warns when the configured/effective context is below the
// model's TRUE max (Ollama silently truncates to a small default otherwise,
// breaking compaction math — D5).
func (c *OllamaClient) checkContextWarn(ctx context.Context, model string, effective int64) {
	if c.Warnf == nil {
		return
	}
	maxCtx := c.contextLength(ctx, model)
	if maxCtx <= 0 {
		if effective <= 0 {
			c.Warnf("ollama: model %s has no context hint; compaction math may be wrong (set num_ctx or a manual context hint)", model)
		}
		return
	}
	if effective > 0 && effective < maxCtx {
		c.Warnf("ollama: configured num_ctx (%d) is below model %s maximum context (%d); Ollama will silently truncate to the configured size", effective, model, maxCtx)
	}
	if effective <= 0 {
		c.Warnf("ollama: no num_ctx configured for model %s (max %d); server default (~4096) silently truncates", model, maxCtx)
	}
}

// StreamTurn streams one turn. num_ctx > 0 (or NumCtxDefault) routes native
// /api/chat (options.num_ctx); 0 rides the OpenAI-compat /v1 client.
func (c *OllamaClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	effective := c.effectiveNumCtx(req)
	c.checkContextWarn(ctx, req.Model, effective)
	if effective > 0 {
		return c.streamNative(ctx, req, effective)
	}
	oc := &OpenAICompatClient{
		BaseURL: strings.TrimRight(c.host(), "/") + "/v1",
		Quirks:  builtinQuirks()["ollama"],
		HTTP:    c.HTTP, Retry: c.Retry, ProviderID: "ollama",
	}
	return oc.StreamTurn(ctx, req)
}

// --- native /api/chat (minimal second decoder, D5) ----------------------------

type ollamaChatRequest struct {
	Model    string        `json:"model"`
	Messages []ollamaNMsg  `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  ollamaOptions `json:"options"`
	Tools    []oaToolDef   `json:"tools,omitempty"`
}

// ollamaOptions carries num_ctx (pointer: omit when unset).
type ollamaOptions struct {
	NumCtx int64 `json:"num_ctx,omitempty"`
}

type ollamaNMsg struct {
	Role      string            `json:"role"`
	Content   string            `json:"content,omitempty"`
	Images    []string          `json:"images,omitempty"`
	ToolCalls []ollamaNToolCall `json:"tool_calls,omitempty"`
}

type ollamaNToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// buildOllamaNativeRequest shapes the native /api/chat body (system rides
// the system message; assistant reasoning never replayed).
func buildOllamaNativeRequest(req TurnRequest, numCtx int64) ollamaChatRequest {
	r := ollamaChatRequest{Model: req.Model, Stream: true}
	r.Options.NumCtx = numCtx
	if sys := systemText(req.System); sys != "" {
		r.Messages = append(r.Messages, ollamaNMsg{Role: "system", Content: sys})
	}
	for _, t := range req.Tools {
		var td oaToolDef
		td.Type = "function"
		td.Function.Name = t.Name
		td.Function.Description = t.Description
		if t.ParamsJSON != "" {
			td.Function.Parameters = json.RawMessage(t.ParamsJSON)
		}
		r.Tools = append(r.Tools, td)
	}
	for _, m := range req.Messages {
		om := ollamaNMsg{Role: string(m.Role)}
		var text strings.Builder
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				text.WriteString(*c.Text)
			case c.ToolUse != nil:
				var tc ollamaNToolCall
				tc.Function.Name = c.ToolUse.Name
				tc.Function.Arguments = json.RawMessage(c.ToolUse.ArgsJSON)
				om.ToolCalls = append(om.ToolCalls, tc)
			case c.ToolResult != nil:
				text.WriteString(c.ToolResult.Content)
			}
		}
		om.Content = text.String()
		r.Messages = append(r.Messages, om)
	}
	return r
}

func (c *OllamaClient) streamNative(ctx context.Context, req TurnRequest, numCtx int64) (TurnStream, error) {
	body, err := json.Marshal(buildOllamaNativeRequest(req, numCtx))
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal: %w", err)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	url := strings.TrimRight(c.host(), "/") + "/api/chat"
	var resp *http.Response
	err = doWithRetries(ctx, c.Retry, func(attempt int) (bool, error, time.Duration) {
		r, err2 := postJSON(ctx, httpc, url, map[string]string{"content-type": "application/json"}, body)
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
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &ollamaNativeStream{sc: sc, body: resp.Body}, nil
}

// ollamaNativeChunk is one /api/chat NDJSON stream line — the native API
// emits one JSON object PER LINE (not SSE), so the native stream decodes
// with a bufio scanner, never the shared sseReader.
type ollamaNativeChunk struct {
	Message struct {
		Content   string            `json:"content"`
		ToolCalls []ollamaNToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
	Error           string `json:"error"`
}

// ollamaNativeStream decodes the native /api/chat NDJSON stream.
type ollamaNativeStream struct {
	sc   *bufio.Scanner
	body io.Closer

	usage   Usage
	stop    StopReason
	toolIdx int
	drained bool
}

func (s *ollamaNativeStream) Close() error {
	if rc, ok := s.body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Next yields text deltas and tool calls, then a single Finish after the
// done chunk. EOF before the done chunk is a mid-stream disconnect (clean
// failure per D11).
func (s *ollamaNativeStream) Next(ctx context.Context) (Event, bool, error) {
	_ = ctx
	if s.drained {
		return nil, false, nil
	}
	for s.sc.Scan() {
		line := s.sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ch ollamaNativeChunk
		if err := json.Unmarshal(line, &ch); err != nil {
			return s.fail(fmt.Errorf("ollama: bad chunk: %w", err))
		}
		if ch.Error != "" {
			return s.fail(fmt.Errorf("ollama: provider error: %s", ch.Error))
		}
		for _, tc := range ch.Message.ToolCalls {
			args := string(tc.Function.Arguments)
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}" // native arguments is an object; never emit junk
			}
			idx := s.toolIdx
			s.toolIdx++
			return ToolCall{Index: idx, ToolCallID: fmt.Sprintf("ollama_call_%d", idx), Name: tc.Function.Name, ArgsJSON: args}, true, nil
		}
		if ch.Message.Content != "" {
			return TextDelta{Text: ch.Message.Content}, true, nil
		}
		if ch.Done {
			s.usage.InputTokens = ch.PromptEvalCount
			s.usage.OutputTokens = ch.EvalCount
			s.stop = mapOllamaDone(ch.DoneReason)
			s.drained = true
			if s.stop == "" {
				s.stop = StopStop
			}
			return Finish{StopReason: s.stop, Usage: s.usage}, true, nil
		}
	}
	if err := s.sc.Err(); err != nil {
		return s.fail(fmt.Errorf("ollama: stream read: %w", err))
	}
	return s.fail(fmt.Errorf("ollama: stream ended without done chunk"))
}

func (s *ollamaNativeStream) fail(err error) (Event, bool, error) {
	s.drained = true
	return StreamError{Err: err}, true, err
}

func mapOllamaDone(reason string) StopReason {
	switch reason {
	case "stop", "":
		return StopStop
	case "length":
		return StopLength
	case "load":
		return StopOther
	default:
		return StopOther
	}
}

// --- native metadata: /api/tags, /api/show, /api/ps --------------------------

type ollamaTags struct {
	Models []struct {
		Name    string         `json:"name"`
		Model   string         `json:"model"`
		Size    int64          `json:"size"`
		Digest  string         `json:"digest"`
		Details map[string]any `json:"details"`
	} `json:"models"`
}

type ollamaShow struct {
	ModelInfo    map[string]any `json:"model_info"`
	Capabilities []string       `json:"capabilities"`
	Parameters   map[string]any `json:"parameters"`
}

// contextLength extracts TRUE context length from /api/show model_info:
// the key is "<arch>.context_length" (any architecture prefix).
func (s *ollamaShow) contextLength() int64 {
	for k, v := range s.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case int64:
				return n
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return i
				}
			}
		}
	}
	return 0

}

func (s *ollamaShow) supportsTools() bool {
	for _, cap := range s.Capabilities {
		if cap == "tools" {
			return true
		}
	}
	return false
}

func (c *OllamaClient) host() string {
	if c.Host != "" {
		return c.Host
	}
	return "http://localhost:11434"
}

func (c *OllamaClient) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *OllamaClient) tags(ctx context.Context) (ollamaTags, error) {
	var out ollamaTags
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.host(), "/")+"/api/tags", nil)
	if err != nil {
		return out, err
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return out, httpStatusError(resp.StatusCode, resp.Status, b)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// show fetches (and caches) TRUE model metadata via POST /api/show.
func (c *OllamaClient) show(ctx context.Context, model string) (ollamaShow, error) {
	c.mu.Lock()
	if c.showMeta == nil {
		c.showMeta = map[string]ollamaShow{}
	}
	if m, ok := c.showMeta[model]; ok {
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.host(), "/")+"/api/show", strings.NewReader(string(body)))
	if err != nil {
		return ollamaShow{}, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return ollamaShow{}, err
	}
	defer resp.Body.Close()
	var out ollamaShow
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return out, httpStatusError(resp.StatusCode, resp.Status, b)
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	c.mu.Lock()
	c.showMeta[model] = out
	c.mu.Unlock()
	return out, nil
}

// contextLength returns the model's TRUE max context (0 = unknown).
func (c *OllamaClient) contextLength(ctx context.Context, model string) int64 {
	sh, err := c.show(ctx, model)
	if err != nil {
		return 0
	}
	return sh.contextLength()
}

// EffectiveContext returns the effective context of a LOADED model via
// /api/ps (0 when not loaded/unknown) — used to compute the warning delta.
func (c *OllamaClient) EffectiveContext(ctx context.Context, model string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.host(), "/")+"/api/ps", nil)
	if err != nil {
		return 0
	}
	resp, err := c.httpc().Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return 0
	}
	for _, m := range ps.Models {
		if m.Name == model || strings.HasPrefix(m.Name, model+":") {
			return m.ContextLength
		}
	}
	return 0
}
