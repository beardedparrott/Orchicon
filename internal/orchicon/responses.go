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

// ResponsesClient is the OpenAI Responses wire client (D2): SSE streaming,
// output_text/reasoning deltas, function_call accumulation, and usage from
// response.completed. OpenCode Zen/Go serve their GPT / muse-spark models on
// this wire (https://opencode.ai/docs/zen/). Hand-written against the public
// API — no SDK.
type ResponsesClient struct {
	BaseURL string // e.g. https://opencode.ai/zen/v1 (path appends /responses)
	APIKey  string

	HTTP  *http.Client
	Retry RetryPolicy

	// ModelsFn supplies ListModels (registry wires the sourcing service).
	ModelsFn func(ctx context.Context) ([]ModelInfo, error)

	// ProviderID labels errors/logs ("opencode", ...).
	ProviderID string
}

// Capabilities reports the Responses wire surface.
func (c *ResponsesClient) Capabilities() Capabilities {
	return Capabilities{Streaming: true, Tools: true}
}

// ListModels resolves through the sourcing service.
func (c *ResponsesClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.ModelsFn != nil {
		return c.ModelsFn(ctx)
	}
	return nil, fmt.Errorf("%s: model sourcing not wired for this client", c.label())
}

func (c *ResponsesClient) label() string {
	if c.ProviderID != "" {
		return c.ProviderID
	}
	return "responses"
}

// --- request wire types -----------------------------------------------------

// respContentPart is one typed content element of a Responses input item.
type respContentPart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
}

// respInputItem is one Responses input item (system/user/assistant/tool).
type respInputItem struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
}

type respToolDef struct {
	Type        string          `json:"type"` // function
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type respRequest struct {
	Model           string          `json:"model"`
	Input           []respInputItem `json:"input"`
	Tools           []respToolDef   `json:"tools,omitempty"`
	MaxOutputTokens int64           `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Stream          bool            `json:"stream"`
}

// buildResponsesRequest shapes the Responses wire request from a normalized
// TurnRequest: system blocks + history → input[] items, max_output_tokens,
// temperature, tools when present, stream:true.
func buildResponsesRequest(req TurnRequest) respRequest {
	rr := respRequest{
		Model: req.Model, Stream: true,
		MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature,
	}
	if sys := systemText(req.System); sys != "" {
		rr.Input = append(rr.Input, respInputItem{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleTool:
			for _, c := range m.Content {
				if c.ToolResult != nil {
					rr.Input = append(rr.Input, respInputItem{Role: "user", Content: []respContentPart{{
						Type: "function_call_output", CallID: c.ToolResult.ToolCallID, Output: c.ToolResult.Content,
					}}})
				}
			}
		case RoleAssistant:
			item := respInputItem{Role: "assistant"}
			var parts []respContentPart
			for _, c := range m.Content {
				switch {
				case c.Text != nil:
					parts = append(parts, respContentPart{Type: "output_text", Text: *c.Text})
				case c.ToolUse != nil:
					parts = append(parts, respContentPart{
						Type: "function_call", ID: c.ToolUse.ToolCallID, CallID: c.ToolUse.ToolCallID,
						Name: c.ToolUse.Name, Arguments: c.ToolUse.ArgsJSON,
					})
				}
			}
			if len(parts) == 1 && parts[0].Type == "output_text" {
				item.Content = parts[0].Text
			} else {
				item.Content = parts
			}
			rr.Input = append(rr.Input, item)
		default: // user
			hasImage := false
			for _, c := range m.Content {
				if c.Image != nil {
					hasImage = true
				}
			}
			if hasImage {
				var parts []respContentPart
				for _, c := range m.Content {
					switch {
					case c.Text != nil:
						parts = append(parts, respContentPart{Type: "input_text", Text: *c.Text})
					case c.Image != nil:
						parts = append(parts, respContentPart{Type: "input_image", ImageURL: *c.Image})
					}
				}
				rr.Input = append(rr.Input, respInputItem{Role: "user", Content: parts})
			} else {
				var text strings.Builder
				for _, c := range m.Content {
					if c.Text != nil {
						text.WriteString(*c.Text)
					}
				}
				rr.Input = append(rr.Input, respInputItem{Role: "user", Content: text.String()})
			}
		}
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			td := respToolDef{Type: "function", Name: t.Name, Description: t.Description}
			if t.ParamsJSON != "" {
				td.Parameters = json.RawMessage(t.ParamsJSON)
			}
			rr.Tools = append(rr.Tools, td)
		}
	}
	return rr
}

// --- stream -------------------------------------------------------------------

// StreamTurn streams one turn on the Responses wire. Pre-stream failures
// retry per the policy; mid-stream failures surface as StreamError + error.
func (c *ResponsesClient) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	body, err := json.Marshal(buildResponsesRequest(req))
	if err != nil {
		return nil, fmt.Errorf("%s: marshal responses request: %w", c.label(), err)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/responses"

	var resp *http.Response
	err = doWithRetries(ctx, c.Retry, func(attempt int) (bool, error, time.Duration) {
		r, err2 := postJSON(ctx, httpc, url, c.requestHeaders(req.SessionID), body)
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
	return newResponsesStream(resp.Body), nil
}

func (c *ResponsesClient) requestHeaders(sessionID string) map[string]string {
	h := map[string]string{
		"content-type": "application/json",
		"accept":       "text/event-stream",
		// First-party user-agent: OpenCode Zen/Go reject generic SDK/HTTP
		// library names (https://opencode.ai/docs/go/#where-can-i-use-it).
		"user-agent": userAgent(),
	}
	if c.APIKey != "" {
		h["authorization"] = "Bearer " + c.APIKey
	}
	// Stable per-conversation/per-execution session id (OpenCode D1).
	if sessionID != "" {
		h["x-opencode-session"] = sessionID
	}
	return h
}

// --- SSE decoding -------------------------------------------------------------

type respUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type respEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Item        *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Usage  *respUsage `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Usage *respUsage `json:"usage"`
}

type respToolAcc struct {
	ID    string
	Name  string
	Index int
	Args  strings.Builder
}

type responsesStream struct {
	r    *sseReader
	body io.Closer

	usage    Usage
	stop     StopReason
	haveStop bool
	drained  bool

	tools   map[string]*respToolAcc // keyed by item_id
	toolOrd []string
	queue   []Event

	// think splits INLINE reasoning (the "think" tag pair inside a
	// output_text delta) into ReasoningDelta events (thinksplit.go), the
	// same global routing every provider wire applies.
	think *thinkSplitter
}

func newResponsesStream(body io.ReadCloser) *responsesStream {
	return &responsesStream{r: newSSEReader(body), body: body, tools: map[string]*respToolAcc{}, think: newThinkSplitter()}
}

func (s *responsesStream) Close() error {
	if rc, ok := s.body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Next yields normalized events. Tool-call event order is ToolCallStart →
// ToolCallDelta* → (at drain) ToolCall → Finish. The Finish event is HELD
// until the body is fully drained (parity with openaiStream.flush): usage
// arrives on response.completed, which may precede the stream's end.
func (s *responsesStream) Next(ctx context.Context) (Event, bool, error) {
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
			return s.fail(fmt.Errorf("responses: stream read: %w", err))
		}
		if !ok || frame.Done() {
			s.flush()
			continue
		}
		if frame.Data == "" {
			continue
		}
		var ev respEvent
		if err := json.Unmarshal([]byte(frame.Data), &ev); err != nil {
			return s.fail(fmt.Errorf("responses: bad sse payload: %w", err))
		}
		switch ev.Type {
		case "response.output_text.delta":
			s.think.feed(ev.Delta, &s.queue)
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			s.queue = append(s.queue, ReasoningDelta{Text: ev.Delta})
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				callID := ev.Item.CallID
				if callID == "" {
					callID = ev.Item.ID
				}
				s.tools[ev.ItemID] = &respToolAcc{ID: callID, Name: ev.Item.Name, Index: ev.OutputIndex}
				s.toolOrd = append(s.toolOrd, ev.ItemID)
				s.queue = append(s.queue, ToolCallStart{Index: ev.OutputIndex, ToolCallID: callID, Name: ev.Item.Name})
			}
		case "response.function_call_arguments.delta":
			if acc := s.tools[ev.ItemID]; acc != nil {
				acc.Args.WriteString(ev.Delta)
				s.queue = append(s.queue, ToolCallDelta{Index: ev.OutputIndex, ArgsJSONDelta: ev.Delta})
			}
		case "response.completed":
			s.haveStop = true
			s.stop = StopStop
			s.recordUsage(ev)
		case "response.incomplete":
			reason := ""
			if ev.Response != nil && ev.Response.IncompleteDetails != nil {
				reason = ev.Response.IncompleteDetails.Reason
			}
			return s.fail(fmt.Errorf("responses: response incomplete: %s", reason))
		case "response.failed":
			msg := "provider error"
			if ev.Response != nil && ev.Response.Error != nil {
				msg = ev.Response.Error.Code + ": " + ev.Response.Error.Message
			}
			return s.fail(fmt.Errorf("responses: %s", msg))
		}
	}
}

func (s *responsesStream) recordUsage(ev respEvent) {
	u := ev.Usage
	if u == nil && ev.Response != nil {
		u = ev.Response.Usage
	}
	if u == nil {
		return
	}
	cached := int64(0)
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	// input_tokens is cache-INCLUSIVE (like OpenAI prompt_tokens) — normalize
	// to the FRESH bucket per oaNoCache parity so cache tokens are never
	// double-counted in the window-pressure basis or CostFor pricing.
	s.usage.InputTokens = oaNoCache(u.InputTokens, cached)
	s.usage.OutputTokens = u.OutputTokens
	s.usage.CacheReadTokens = cached
}

func (s *responsesStream) pop() Event {
	if len(s.queue) == 0 {
		return nil
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	return ev
}

// flush emits accumulated complete tool calls (in first-appearance order),
// then the held Finish. Any think-splitter holdback is drained first so a
// truncated final tag cannot swallow the response tail.
func (s *responsesStream) flush() {
	s.think.drain(&s.queue)
	for _, id := range s.toolOrd {
		acc := s.tools[id]
		args := acc.Args.String()
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}" // flush-on-drain: never emit unparseable args silently
		}
		s.queue = append(s.queue, ToolCall{Index: acc.Index, ToolCallID: acc.ID, Name: acc.Name, ArgsJSON: args})
	}
	s.toolOrd = nil
	s.drained = true
	if !s.haveStop {
		// NO provider stop signal: the stream ended without response.completed
		// (a truncated/aborted generation). StopOther is the honest terminal.
		s.stop = StopOther
	}
	s.queue = append(s.queue, Finish{StopReason: s.stop, Usage: s.usage})
}

func (s *responsesStream) fail(err error) (Event, bool, error) {
	s.drained = true
	return StreamError{Err: err}, true, err
}
