package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatClient is the native Chat Completions wire client (D4): SSE
// streaming, tool_calls fragment accumulation, usage incl.
// prompt_tokens_details.cached_tokens, and the trailing usage-only chunk
// drain invariant (Finish is held until the stream is fully drained).
type OpenAICompatClient struct {
	BaseURL string // e.g. https://api.openai.com/v1 (chat path appends /chat/completions)
	APIKey  string
	Quirks  Quirks

	// AuthStyle "bearer" (default) or "none". ExtraHeaders ride requests.
	AuthStyle    string
	ExtraHeaders map[string]string

	HTTP  *http.Client
	Retry RetryPolicy

	// ModelsFn supplies ListModels (registry wires the sourcing service).
	ModelsFn func(ctx context.Context) ([]ModelInfo, error)

	// ProviderID labels errors/logs ("openai", tenant custom slug, ...).
	ProviderID string
}

// Capabilities reports the compat feature surface (quirk-shaped).
func (c *OpenAICompatClient) Capabilities() Capabilities {
	return Capabilities{
		Streaming: true, Tools: c.Quirks.SupportsToolCalls,
		ReasoningEfforts: c.Quirks.SupportsReasoningEffort, ImageInput: true,
	}
}

// ListModels resolves through the sourcing service. NO catalog fallback:
// per the no-synthesized-models directive, a failed sourcing probe yields
// no models — never the vendored snapshot.
func (c *OpenAICompatClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.ModelsFn != nil {
		return c.ModelsFn(ctx)
	}
	return nil, fmt.Errorf("%s: model sourcing not wired for this client", c.ProviderID)
}

// --- request wire types -----------------------------------------------------

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// oaMsgMarshal carries the OpenAI message wire shape.
type oaMsg struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

type oaRequest struct {
	Model           string           `json:"model"`
	Messages        []oaMsg          `json:"messages"`
	Tools           []oaToolDef      `json:"tools,omitempty"`
	Stream          bool             `json:"stream"`
	StreamOptions   *oaStreamOptions `json:"stream_options,omitempty"`
	MaxTokens       int64            `json:"max_tokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	Reasoning       string           `json:"reasoning,omitempty"`
}

type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaToolDef struct {
	Type     string `json:"type"` // function
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// buildOpenAIRequest shapes the compat request per quirks (D4).
func buildOpenAIRequest(req TurnRequest, q Quirks) oaRequest {
	or := oaRequest{
		Model: req.Model, Stream: true,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature,
	}
	if q.StreamOptionsIncludeUsage {
		or.StreamOptions = &oaStreamOptions{IncludeUsage: true}
	}
	if req.ReasoningEffort != "" && q.SupportsReasoningEffort {
		field := q.ReasoningField
		if field == "" {
			field = "reasoning_effort"
		}
		if field == "reasoning" {
			or.Reasoning = req.ReasoningEffort
		} else {
			or.ReasoningEffort = req.ReasoningEffort
		}
	}
	if q.SupportsToolCalls && len(req.Tools) > 0 {
		for _, t := range req.Tools {
			var td oaToolDef
			td.Type = "function"
			td.Function.Name = t.Name
			td.Function.Description = t.Description
			if t.ParamsJSON != "" {
				td.Function.Parameters = json.RawMessage(t.ParamsJSON)
			}
			or.Tools = append(or.Tools, td)
		}
	}

	// Messages: system handling (developer role / merge-into-user), then
	// history. Assistant reasoning is never replayed (no reasoning storage).
	sysRole := "system"
	if q.SupportsDeveloperRole {
		sysRole = "developer"
	}
	sysText := systemText(req.System)
	if sysText != "" && !q.MergeSystemIntoUser {
		or.Messages = append(or.Messages, oaMsg{Role: sysRole, Content: sysText})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			for _, c := range m.Content {
				if c.ToolResult != nil {
					or.Messages = append(or.Messages, oaMsg{
						Role: "tool", ToolCallID: c.ToolResult.ToolCallID,
						Content: c.ToolResult.Content, Name: toolNameForID(req, c.ToolResult.ToolCallID),
					})
				}
			}
		case RoleAssistant:
			om := oaMsg{Role: "assistant"}
			var text strings.Builder
			for _, c := range m.Content {
				switch {
				case c.Text != nil:
					text.WriteString(*c.Text)
				case c.ToolUse != nil:
					tc := oaToolCall{ID: c.ToolUse.ToolCallID, Type: "function"}
					tc.Function.Name = c.ToolUse.Name
					tc.Function.Arguments = c.ToolUse.ArgsJSON
					om.ToolCalls = append(om.ToolCalls, tc)
				}
			}
			if text.Len() > 0 {
				om.Content = text.String()
			}
			or.Messages = append(or.Messages, om)
		default: // user
			hasImage := false
			for _, c := range m.Content {
				if c.Image != nil {
					hasImage = true
				}
			}
			if hasImage {
				type oaPart struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url,omitempty"`
				}
				parts := []oaPart{}
				for _, c := range m.Content {
					switch {
					case c.Text != nil:
						parts = append(parts, oaPart{Type: "text", Text: *c.Text})
					case c.Image != nil:
						parts = append(parts, oaPart{Type: "image_url", ImageURL: &struct {
							URL string `json:"url"`
						}{URL: *c.Image}})
					}
				}
				or.Messages = append(or.Messages, oaMsg{Role: "user", Content: parts})
			} else {
				var text strings.Builder
				for _, c := range m.Content {
					if c.Text != nil {
						text.WriteString(*c.Text)
					}
				}
				if sysText != "" && q.MergeSystemIntoUser && len(or.Messages) == 0 {
					text.WriteString(sysText + "\n\n")
				}
				or.Messages = append(or.Messages, oaMsg{Role: "user", Content: text.String()})
			}
		}
	}
	return or
}

func systemText(blocks []SystemBlock) string {
	var b strings.Builder
	for i, s := range blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.Text)
	}
	return b.String()
}

func toolNameForID(req TurnRequest, id string) string {
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == id {
				return c.ToolUse.Name
			}
		}
	}
	return ""
}

// --- stream -------------------------------------------------------------------

// StreamTurn streams one turn. Pre-stream failures retry (408/409/429/5xx,
// connection); the trailing usage-only chunk is drained before Finish.
func (c *OpenAICompatClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	body, err := json.Marshal(buildOpenAIRequest(req, c.Quirks))
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request: %w", c.label(), err)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"

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
	return newOpenAIStream(resp.Body), nil
}

func (c *OpenAICompatClient) label() string {
	if c.ProviderID != "" {
		return c.ProviderID
	}
	return "openai-compat"
}

func (c *OpenAICompatClient) requestHeaders() map[string]string {
	h := map[string]string{
		"content-type": "application/json",
		"accept":       "text/event-stream",
	}
	switch c.AuthStyle {
	case "none":
	case "bearer":
		fallthrough
	default:
		if c.APIKey != "" {
			h["authorization"] = "Bearer " + c.APIKey
		}
	}
	for k, v := range c.ExtraHeaders {
		h[strings.ToLower(k)] = v
	}
	return h
}

// --- chunk decoding -------------------------------------------------------------

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// oaNoCache derives the fresh (uncached) input-token figure from an
// OpenAI-compatible usage block: prompt_tokens is cache-INCLUSIVE, so
// fresh = max(0, prompt − cached). Mirrors legacyNoCache for the legacy
// envelope's cache-inclusive totalUsage.
func oaNoCache(total, cached int64) int64 {
	if d := total - cached; d > 0 {
		return d
	}
	return 0
}

type openaiStream struct {
	r    *sseReader
	body io.Closer

	usage    Usage
	stop     StopReason
	haveStop bool
	drained  bool

	tools   map[int]*oaToolAcc
	toolOrd []int
	queue   []Event // ordered event backlog (Start → Delta → … → Finish)
}

type oaToolAcc struct {
	ID   string
	Name string
	Args strings.Builder
	Done bool
}

func newOpenAIStream(body io.ReadCloser) *openaiStream {
	return &openaiStream{r: newSSEReader(body), body: body, tools: map[int]*oaToolAcc{}}
}

func (s *openaiStream) Close() error {
	if rc, ok := s.body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Next yields normalized events. Tool-call event order is
// ToolCallStart → ToolCallDelta* → (at drain) ToolCall → Finish. The
// Finish event is HELD until the body is fully drained: OpenAI-style
// streams send finish_reason on the last content chunk and real usage on a
// trailing usage-only chunk (choices: []) — finalizing on finish_reason
// would zero recorded costs.
func (s *openaiStream) Next(ctx context.Context) (Event, bool, error) {
	_ = ctx
	for {
		if ev := s.pop(); ev != nil {
			return ev, true, nil
		}
		if s.drained {
			return nil, false, nil
		}
		frame, ok, err := s.r.Next()
		if err != nil {
			return s.fail(fmt.Errorf("openai-compat: stream read: %w", err))
		}
		if !ok || frame.Done() {
			s.flush()
			continue
		}
		if frame.Data == "" {
			continue
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(frame.Data), &ch); err != nil {
			return s.fail(fmt.Errorf("openai-compat: bad chunk: %w", err))
		}
		if ch.Error != nil {
			return s.fail(fmt.Errorf("openai-compat: provider error: %s", ch.Error.Message))
		}
		// Usage arrives on the trailing usage-only chunk (choices empty) or
		// the final content chunk — record whenever present (last wins).
		if ch.Usage != nil {
			// Normalize InputTokens to the FRESH bucket (QA bug A):
			// OpenAI-compat prompt_tokens is cache-INCLUSIVE — it already
			// counts prompt_tokens_details.cached_tokens — while the
			// normalized Usage contract prices cache sub-buckets separately
			// (see legacyUsageToUsage). The window-pressure basis
			// InputTokens+CacheReadTokens must not double-count a cache hit,
			// and the fresh-token budget gate / CostFor pricing must not bill
			// cached tokens twice.
			cached := int64(0)
			if ch.Usage.PromptTokensDetails != nil {
				cached = ch.Usage.PromptTokensDetails.CachedTokens
			}
			s.usage.InputTokens = oaNoCache(ch.Usage.PromptTokens, cached)
			s.usage.OutputTokens = ch.Usage.CompletionTokens
			s.usage.CacheReadTokens = cached
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			if d.Content != "" {
				s.queue = append(s.queue, TextDelta{Text: d.Content})
			}
			switch {
			case d.ReasoningContent != "":
				s.queue = append(s.queue, ReasoningDelta{Text: d.ReasoningContent})
			case d.Reasoning != "":
				s.queue = append(s.queue, ReasoningDelta{Text: d.Reasoning})
			}
			for _, tc := range d.ToolCalls {
				acc := s.tools[tc.Index]
				isNew := acc == nil
				if isNew {
					acc = &oaToolAcc{}
					s.tools[tc.Index] = acc
					s.toolOrd = append(s.toolOrd, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				if isNew {
					s.queue = append(s.queue, ToolCallStart{Index: tc.Index, ToolCallID: acc.ID, Name: acc.Name})
				}
				if tc.Function.Arguments != "" {
					acc.Args.WriteString(tc.Function.Arguments)
					s.queue = append(s.queue, ToolCallDelta{Index: tc.Index, ArgsJSONDelta: tc.Function.Arguments})
				}
			}
			if choice.FinishReason != nil {
				if fr, isStr := choice.FinishReason.(string); isStr && fr != "" {
					s.stop = mapOpenAIStop(fr)
					s.haveStop = true
					// NOTE: Finish is NOT emitted here — a trailing
					// usage-only chunk may still follow. Drain continues.
				}
			}
		}
	}
}

// pop hands back one queued event (nil when empty).
func (s *openaiStream) pop() Event {
	if len(s.queue) == 0 {
		return nil
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev
}

// flush emits accumulated complete tool calls (in first-appearance order),
// then the held Finish.
func (s *openaiStream) flush() {
	for _, idx := range s.toolOrd {
		acc := s.tools[idx]
		args := acc.Args.String()
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}" // flush-on-drain: never emit unparseable args silently
		}
		s.queue = append(s.queue, ToolCall{Index: idx, ToolCallID: acc.ID, Name: acc.Name, ArgsJSON: args})
	}
	s.toolOrd = nil
	s.drained = true
	if !s.haveStop {
		// NO provider stop reason: the stream ended without delivering an
		// end-of-response signal (a truncated/aborted generation, not a
		// completed turn). Synthesizing StopStop here recorded hollow
		// successes: the loop's success gate ran on a turn the provider
		// never actually ended (observed: executions "succeeded" with the
		// model mid-monologue). StopOther is the honest terminal — the
		// loop fails a turn that ended without the provider's signal.
		s.stop = StopOther
	}
	s.queue = append(s.queue, Finish{StopReason: s.stop, Usage: s.usage})
}

func (s *openaiStream) fail(err error) (Event, bool, error) {
	s.drained = true
	return StreamError{Err: err}, true, err
}

func mapOpenAIStop(reason string) StopReason {
	switch reason {
	case "stop":
		return StopStop
	case "length":
		return StopLength
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopContentFilter
	default:
		return StopOther
	}
}
