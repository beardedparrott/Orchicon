package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AnthropicClient is the native Anthropic Messages wire client (D3):
// SSE streaming, tool_use block accumulation, cache_control breakpoints,
// usage incl. cache_read_input_tokens / cache_creation_input_tokens.
// Hand-written against the public API — no SDK.
type AnthropicClient struct {
	BaseURL string // default https://api.anthropic.com (no /v1 — path adds it)
	APIKey  string

	// AuthStyle: "x-api-key" (default) or "bearer" (Command Code provider
	// route). ExtraHeaders ride every request (x-cmd-zdr etc.).
	AuthStyle    string
	ExtraHeaders map[string]string

	HTTP  *http.Client
	Retry RetryPolicy

	// ModelsFn supplies ListModels (registry wires the sourcing service);
	// nil → catalog only.
	ModelsFn func(ctx context.Context) ([]ModelInfo, error)

	// CacheTTL is the Anthropic prompt-cache TTL for cache_control
	// breakpoints: "" or "5m" = the API default (5-minute TTL); "1h" =
	// the extended-TTL breakpoint (opt-in via
	// ORCHICON_ANTHROPIC_CACHE_TTL — ADR-0009 D3, revisitable: 1h is
	// priced higher per write, so it only pays off for sparse workflow
	// cadences).
	CacheTTL string
}

const anthropicVersion = "2023-06-01"

// Capabilities reports the Anthropic feature surface.
func (c *AnthropicClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true, ReasoningEfforts: true, ImageInput: true, CacheBreakpoints: true}
}

// ListModels resolves through the sourcing service (or catalog fallback).
func (c *AnthropicClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.ModelsFn != nil {
		return c.ModelsFn(ctx)
	}
	return catalogListByProvider("anthropic"), nil
}

// --- request wire types -----------------------------------------------------

type anthCacheCtrl struct {
	Type string `json:"type"`
	// TTL is the optional Anthropic prompt-cache TTL ("1h"); empty = the
	// API default (5m). Opt-in via AnthropicClient.CacheTTL /
	// ORCHICON_ANTHROPIC_CACHE_TTL (ADR-0009 D3).
	TTL string `json:"ttl,omitempty"`
}

type anthContent struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// image
	Source *anthImageSource `json:"source,omitempty"`

	// tool_use (assistant)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user)
	ToolUseID  string `json:"tool_use_id,omitempty"`
	ResultBody string `json:"-"`
	haveResult bool
	IsError    bool `json:"is_error,omitempty"`

	CacheControl *anthCacheCtrl `json:"cache_control,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthMessage struct {
	Role    string        `json:"role"`
	Content []anthContent `json:"content"`
}

type anthSystemBlock struct {
	Text         string         `json:"text"`
	CacheControl *anthCacheCtrl `json:"cache_control,omitempty"`
}

type anthToolDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *anthCacheCtrl  `json:"cache_control,omitempty"`
}

type anthRequest struct {
	Model       string            `json:"model"`
	System      []anthSystemBlock `json:"system,omitempty"`
	Messages    []anthMessage     `json:"messages"`
	Tools       []anthToolDef     `json:"tools,omitempty"`
	MaxTokens   int64             `json:"max_tokens"`
	Stream      bool              `json:"stream"`
	Temperature *float64          `json:"temperature,omitempty"`
}

// marshalAnthropicHistory converts normalized messages. Assistant reasoning
// is NEVER replayed (D2): reasoning content has no normalized storage in
// Message.Content — text-only + tool_use + tool_result round-trip here.
// Anthropic has no "tool" role: tool_result blocks ride USER messages, so
// consecutive tool-role messages merge into one user message.
func marshalAnthropicHistory(messages []Message) []anthMessage {
	out := make([]anthMessage, 0, len(messages))
	appendContent := func(am *anthMessage, c Content) {
		switch {
		case c.Text != nil:
			am.Content = append(am.Content, anthContent{Type: "text", Text: *c.Text})
		case c.Image != nil:
			if src, ok := parseDataURL(*c.Image); ok {
				am.Content = append(am.Content, anthContent{Type: "image", Source: &src})
			}
		case c.ToolUse != nil:
			am.Content = append(am.Content, anthContent{
				Type: "tool_use", ID: c.ToolUse.ToolCallID, Name: c.ToolUse.Name,
				Input: json.RawMessage(c.ToolUse.ArgsJSON),
			})
		case c.ToolResult != nil:
			ac := anthContent{Type: "tool_result", ToolUseID: c.ToolResult.ToolCallID, IsError: c.ToolResult.IsError}
			ac.ResultBody = c.ToolResult.Content
			ac.haveResult = true
			am.Content = append(am.Content, ac)
		}
	}
	for _, m := range messages {
		role := string(m.Role)
		if m.Role == RoleTool {
			role = "user" // Anthropic tool_result lives in user messages
		}
		// Consecutive tool-role messages merge into ONE user message.
		if m.Role == RoleTool && len(out) > 0 && out[len(out)-1].Role == "user" && len(out[len(out)-1].Content) > 0 && out[len(out)-1].Content[0].Type == "tool_result" {
			prev := &out[len(out)-1]
			for _, c := range m.Content {
				appendContent(prev, c)
			}
			continue
		}
		am := anthMessage{Role: role, Content: []anthContent{}}
		for _, c := range m.Content {
			appendContent(&am, c)
		}
		// Anthropic requires non-empty content per message.
		if len(am.Content) == 0 {
			am.Content = append(am.Content, anthContent{Type: "text", Text: ""})
		}
		out = append(out, am)
	}
	return out
}

// anthToolResultMarshal is a custom marshaler shim: tool_result content is a
// bare string on the wire.
func (a anthContent) MarshalJSON() ([]byte, error) {
	type alias anthContent
	type wire struct {
		alias
		Content any `json:"content,omitempty"`
	}
	w := wire{alias: alias(a)}
	if a.haveResult {
		w.Content = a.ResultBody
	}
	// drop the shim fields from JSON via alias (ResultBody has tag "-")
	b, err := json.Marshal(w)
	return b, err
}

func parseDataURL(u string) (anthImageSource, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return anthImageSource{}, false
	}
	rest := u[len(prefix):]
	semi := strings.Index(rest, ";")
	if semi < 0 {
		return anthImageSource{}, false
	}
	media := rest[:semi]
	tail := rest[semi+1:]
	const b64 = "base64,"
	if !strings.HasPrefix(tail, b64) {
		return anthImageSource{}, false
	}
	return anthImageSource{Type: "base64", MediaType: media, Data: tail[len(b64):]}, true
}

// buildAnthropicRequest shapes the wire request with cache_control
// breakpoints (D3). Breakpoint rule for the two-zone system layout
// (ADR-0009): when ANY system block carries an explicit Cache flag,
// ONLY the flagged blocks get cache_control — never the last block by
// default, because the mutable zone sits after the static prefix and a
// blanket last-block rule would cache mutable content. When no block is
// flagged (legacy single-block callers) the breakpoint stays on the
// LAST system block. Under the system+tools policy the LAST tool
// definition also carries a breakpoint. cacheTTL ("1h") is an opt-in
// extended TTL emitted with each breakpoint; "" uses the API default.
func buildAnthropicRequest(req TurnRequest, cacheTTL string) anthRequest {
	policy := req.CacheControl
	if policy == "" {
		policy = CacheControlSystemAndTools
	}
	ctrl := func() *anthCacheCtrl {
		if cacheTTL == "" {
			return &anthCacheCtrl{Type: "ephemeral"}
		}
		return &anthCacheCtrl{Type: "ephemeral", TTL: cacheTTL}
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096 // API-required; conservative default
	}
	ar := anthRequest{
		Model: req.Model, Messages: marshalAnthropicHistory(req.Messages),
		MaxTokens: maxTok, Stream: true, Temperature: req.Temperature,
	}
	if policy != CacheControlNone {
		anyFlagged := false
		for _, sb := range req.System {
			if sb.Cache {
				anyFlagged = true
				break
			}
		}
		for i, sb := range req.System {
			b := anthSystemBlock{Text: sb.Text}
			if sb.Cache || (!anyFlagged && i == len(req.System)-1) {
				b.CacheControl = ctrl()
			}
			ar.System = append(ar.System, b)
		}
	} else {
		for _, sb := range req.System {
			ar.System = append(ar.System, anthSystemBlock{Text: sb.Text})
		}
	}
	for i, t := range req.Tools {
		schema := json.RawMessage(t.ParamsJSON)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		td := anthToolDef{Name: t.Name, Description: t.Description, InputSchema: schema}
		if policy == CacheControlSystemAndTools && i == len(req.Tools)-1 {
			td.CacheControl = ctrl()
		}
		ar.Tools = append(ar.Tools, td)
	}
	return ar
}

// --- response stream ---------------------------------------------------------

// StreamTurn streams one turn. Pre-stream failures (non-2xx, connection)
// retry per the policy; mid-stream failures surface as StreamError + error.
func (c *AnthropicClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	body, err := json.Marshal(buildAnthropicRequest(req, c.CacheTTL))
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	url := strings.TrimRight(base, "/") + "/v1/messages"

	var resp *http.Response
	err = doWithRetries(ctx, c.Retry, func(attempt int) (bool, error, time.Duration) {
		r, err2 := postJSON(ctx, httpc, url, c.requestHeaders(), body)
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
	return newAnthropicStream(resp.Body), nil
}

func (c *AnthropicClient) requestHeaders() map[string]string {
	h := map[string]string{
		"content-type":      "application/json",
		"accept":            "text/event-stream",
		"anthropic-version": anthropicVersion,
	}
	if c.AuthStyle == "bearer" {
		h["authorization"] = "Bearer " + c.APIKey
	} else {
		h["x-api-key"] = c.APIKey
	}
	for k, v := range c.ExtraHeaders {
		h[strings.ToLower(k)] = v
	}
	return h
}

// postJSON is the shared POST helper (connection errors classify for retry).
func postJSON(ctx context.Context, httpc *http.Client, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return httpc.Do(req)
}

// --- SSE event decoding ------------------------------------------------------

type anthStreamState struct {
	usage    Usage
	stop     StopReason
	finished bool
	emitted  bool // any content event emitted (mid-stream failure policy)
	blocks   map[int]*anthBlockAcc
}

type anthBlockAcc struct {
	Type string // text | thinking | tool_use
	ID   string
	Name string
	Args strings.Builder
}

type anthropicStream struct {
	r    *sseReader
	body io.Closer
	st   anthStreamState
	err  error
}

func newAnthropicStream(body io.ReadCloser) *anthropicStream {
	return &anthropicStream{r: newSSEReader(body), body: body, st: anthStreamState{blocks: map[int]*anthBlockAcc{}}}
}

func (s *anthropicStream) Close() error {
	if rc, ok := s.body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Next yields the next normalized event.
func (s *anthropicStream) Next(ctx context.Context) (Event, bool, error) {
	_ = ctx
	if s.st.finished {
		return nil, false, nil
	}
	for {
		frame, ok, err := s.r.Next()
		if err != nil {
			return s.fail(fmt.Errorf("anthropic: stream read: %w", err))
		}
		if !ok {
			if !s.st.emitted {
				return s.fail(fmt.Errorf("anthropic: stream ended before message_stop"))
			}
			// Server closed without message_stop after content — fail cleanly.
			return s.fail(fmt.Errorf("anthropic: stream ended without message_stop"))
		}
		if frame.Data == "" {
			continue // comment/ping frame
		}
		ev := frame.Event
		var payload struct {
			Type  string `json:"type"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Message *struct {
				Usage anthUsage `json:"usage"`
			} `json:"message"`
			Index        int             `json:"index"`
			Delta        json.RawMessage `json:"delta"`
			Usage        *anthUsage      `json:"usage"`
			ContentBlock *struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
			// Anthropic pings are data-less; an unparseable data frame is fatal.
			return s.fail(fmt.Errorf("anthropic: bad sse payload: %w", err))
		}
		if ev == "" {
			ev = payload.Type
		}
		switch ev {
		case "ping":
			continue
		case "error":
			msg := "provider error"
			if payload.Error != nil {
				msg = payload.Error.Type + ": " + payload.Error.Message
			}
			return s.fail(fmt.Errorf("anthropic: %s", msg))
		case "message_start":
			if payload.Message != nil {
				s.st.usage.InputTokens = payload.Message.Usage.InputTokens
				s.st.usage.CacheReadTokens = payload.Message.Usage.CacheReadTokens
				s.st.usage.CacheWriteTokens = payload.Message.Usage.CacheCreationTokens
			}
			continue
		case "content_block_start":
			s.st.emitted = true
			cb := payload.ContentBlock
			if cb == nil {
				continue
			}
			acc := &anthBlockAcc{Type: cb.Type, ID: cb.ID, Name: cb.Name}
			// Some gateways inline the full tool input at block start (the
			// official API sends {}) — capture it so args are never lost.
			if cb.Type == "tool_use" && len(cb.Input) > 0 && string(cb.Input) != "{}" {
				acc.Args.Write(cb.Input)
			}
			s.st.blocks[payload.Index] = acc
			if cb.Type == "tool_use" {
				return ToolCallStart{Index: payload.Index, ToolCallID: cb.ID, Name: cb.Name}, true, nil
			}
			continue
		case "content_block_delta":
			s.st.emitted = true
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			if err := json.Unmarshal(payload.Delta, &d); err != nil {
				return s.fail(fmt.Errorf("anthropic: bad delta: %w", err))
			}
			switch d.Type {
			case "text_delta":
				return TextDelta{Text: d.Text}, true, nil
			case "thinking_delta":
				return ReasoningDelta{Text: d.Thinking}, true, nil
			case "input_json_delta":
				if acc := s.st.blocks[payload.Index]; acc != nil {
					acc.Args.WriteString(d.PartialJSON)
				}
				return ToolCallDelta{Index: payload.Index, ArgsJSONDelta: d.PartialJSON}, true, nil
			}
			continue
		case "content_block_stop":
			acc := s.st.blocks[payload.Index]
			if acc != nil && acc.Type == "tool_use" {
				args := acc.Args.String()
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					// flush-on-stop: emit with the raw accumulated args; the
					// consumer sees the malformed args rather than silence.
					args = "{}"
				}
				return ToolCall{Index: payload.Index, ToolCallID: acc.ID, Name: acc.Name, ArgsJSON: args}, true, nil
			}
			continue
		case "message_delta":
			// payload.Delta IS the inner {"stop_reason":...} object.
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(payload.Delta, &d); err != nil {
				return s.fail(fmt.Errorf("anthropic: bad message_delta: %w", err))
			}
			s.st.stop = mapAnthropicStop(d.StopReason)
			// usage rides TOP-LEVEL of the message_delta data frame, not delta.
			if payload.Usage != nil {
				if payload.Usage.OutputTokens > 0 {
					s.st.usage.OutputTokens = payload.Usage.OutputTokens
				}
				if payload.Usage.CacheReadTokens > 0 {
					s.st.usage.CacheReadTokens = payload.Usage.CacheReadTokens
				}
				if payload.Usage.CacheCreationTokens > 0 {
					s.st.usage.CacheWriteTokens = payload.Usage.CacheCreationTokens
				}
			}
			continue
		case "message_stop":
			s.st.finished = true
			if s.st.stop == "" {
				s.st.stop = StopStop
			}
			return Finish{StopReason: s.st.stop, Usage: s.st.usage}, true, nil
		default:
			continue
		}
	}
}

// fail converts a mid-stream failure: StreamError event then terminal error.
func (s *anthropicStream) fail(err error) (Event, bool, error) {
	s.st.finished = true
	s.err = err
	return StreamError{Err: err}, true, err
}

type anthUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

func mapAnthropicStop(reason string) StopReason {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return StopStop
	case "max_tokens":
		return StopLength
	case "tool_use":
		return StopToolUse
	case "refusal":
		return StopContentFilter
	default:
		return StopOther
	}
}

// anthropicCacheTTLOptIn reads the ORCHICON_ANTHROPIC_CACHE_TTL tenant
// env (ADR-0009 D3, revisitable default OFF): "1h" opts the session's
// cache breakpoints into the extended 1-hour TTL; "" or "5m" keep the
// API default. Any other value is ignored (empty → default) — an
// invalid opt-in must never break request assembly.
func anthropicCacheTTLOptIn() string {
	switch os.Getenv("ORCHICON_ANTHROPIC_CACHE_TTL") {
	case "1h":
		return "1h"
	default:
		return ""
	}
}
