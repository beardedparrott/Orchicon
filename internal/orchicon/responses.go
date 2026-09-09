package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

// respInputItem is one Responses input item: a message item (role +
// content) or a function item (function_call / function_call_output as
// top-level items — the wire rejects function payloads nested inside a
// message's content array).
type respInputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Output is a REQUIRED field on function_call_output items (the
	// Responses provider rejects a call without it as "missing required
	// field `output`" — observed on a mid-session provider switch onto a
	// muse-spark model where a prior tool round returned empty content).
	// Never omitempty: an empty output must still be sent, and we default
	// an empty string to a neutral placeholder so the field is always
	// present.
	Output string `json:"output"`
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
			// Tool results are top-level function_call_output items —
			// never nested in a message content array. The `output` field
			// is REQUIRED by the Responses provider; an empty tool result
			// (a call that returned no text, or a cross-provider replayed
			// history where a prior wire produced an empty result) must
			// still carry a present output or the provider rejects the
			// turn with `missing required field "output"`.
			for _, c := range m.Content {
				if c.ToolResult != nil {
					out := c.ToolResult.Content
					if out == "" {
						// Neutral placeholder — never an empty/absent field.
						out = "(empty result)"
					} else if c.ToolResult.IsError {
						out = "ERROR: " + out
					}
					rr.Input = append(rr.Input, respInputItem{
						Type: "function_call_output", CallID: c.ToolResult.ToolCallID, Output: out,
					})
				}
			}
		case RoleAssistant:
			// Assistant text rides a message item; each tool use rides
			// its own top-level function_call item (in order). Plain
			// single-text messages stay the compact string form.
			var text strings.Builder
			flushText := func() {
				if text.Len() == 0 {
					return
				}
				t := text.String()
				text.Reset()
				rr.Input = append(rr.Input, respInputItem{Role: "assistant", Content: t})
			}
			for _, c := range m.Content {
				switch {
				case c.Text != nil:
					text.WriteString(*c.Text)
				case c.ToolUse != nil:
					flushText()
					args := c.ToolUse.ArgsJSON
					if args == "" || !json.Valid([]byte(args)) {
						args = "{}"
					}
					rr.Input = append(rr.Input, respInputItem{
						Type: "function_call", ID: c.ToolUse.ToolCallID, CallID: c.ToolUse.ToolCallID,
						Name: c.ToolUse.Name, Arguments: args,
					})
				}
			}
			flushText()
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
	rr := buildResponsesRequest(req)
	// Wire diagnostic (2026-09-09 follow-up no-tools repro): log the
	// EXACT outgoing request shape (model, input item count, tools count,
	// last input item type) so a text-only reply can be attributed to the
	// provider vs the request builder. Emitted through the standard logger
	// (stdout → docker logs / container.sh logs) at Info level — one line
	// per Responses turn, negligible in prod, essential for the repro.
	{
		var lastT, lastRole string
		if n := len(rr.Input); n > 0 {
			lastT = rr.Input[n-1].Type
			lastRole = rr.Input[n-1].Role
		}
		slog.Info("orchicon responses request",
			"model", rr.Model, "input_items", len(rr.Input), "tools", len(rr.Tools),
			"last_type", lastT, "last_role", lastRole)
	}
	body, err := json.Marshal(rr)
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
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type respEvent struct {
	Type        string    `json:"type"`
	Delta       respDelta `json:"delta"`
	Text        string    `json:"text"`
	ItemID      string    `json:"item_id"`
	OutputIndex int       `json:"output_index"`
	Item        *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Text      string `json:"text"`
	} `json:"item"`
	Response *struct {
		ID                string     `json:"id"`
		Status            string     `json:"status"`
		Usage             *respUsage `json:"usage"`
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

// respDelta tolerates both delta wire shapes: the compact string form
// ({"type":"response.output_text.delta","delta":"Hi"}) and the object form
// some gateways emit ({"delta":{"text":"Hi"}} or {"delta":{"content":"Hi"}}.
// Without this, an object-shaped delta fails to unmarshal into a string,
// the whole frame errors as "bad sse payload", and the turn fails while
// the first turn (deltas only) may have looked fine.
type respDelta string

func (d *respDelta) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*d = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*d = respDelta(s)
		return nil
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
		Value   string `json:"value"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*d = respDelta(obj.Text + obj.Content + obj.Value)
	return nil
}

func (d respDelta) String() string { return string(d) }

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

	// sawTextDelta / sawReasoningDelta record, per output index, whether
	// the streaming deltas already delivered that output. The Responses
	// wire emits the COMPLETE text again on the output_text.done /
	// reasoning_text.done frame — feeding it too duplicates the whole
	// reply (and re-splits inline reasoning into text). Done frames are
	// only used for gateways that emit done WITHOUT deltas.
	sawTextDelta      map[int]bool
	sawReasoningDelta map[int]bool

	// think splits INLINE reasoning (the "think" tag pair inside a
	// output_text delta) into ReasoningDelta events (thinksplit.go), the
	// same global routing every provider wire applies.
	think *thinkSplitter
}

func newResponsesStream(body io.ReadCloser) *responsesStream {
	return &responsesStream{
		r: newSSEReader(body), body: body,
		tools:             map[string]*respToolAcc{},
		think:             newThinkSplitter(),
		sawTextDelta:      map[int]bool{},
		sawReasoningDelta: map[int]bool{},
	}
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
			if t := ev.Delta.String(); t != "" {
				s.sawTextDelta[ev.OutputIndex] = true
				s.think.feed(t, &s.queue)
			} else if ev.Text != "" {
				s.sawTextDelta[ev.OutputIndex] = true
				s.think.feed(ev.Text, &s.queue)
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if t := ev.Delta.String(); t != "" {
				s.sawReasoningDelta[ev.OutputIndex] = true
				s.queue = append(s.queue, ReasoningDelta{Text: t})
			} else if ev.Text != "" {
				s.sawReasoningDelta[ev.OutputIndex] = true
				s.queue = append(s.queue, ReasoningDelta{Text: ev.Text})
			}
		case "response.output_text.done", "response.reasoning_text.done":
			// The done frame carries the COMPLETE output text. It is
			// authoritative ONLY when the deltas did not already deliver
			// it — some gateways stream deltas then echo the full text on
			// done, and feeding it too duplicates the reply (and re-splits
			// inline reasoning into text). Skip when deltas were seen for
			// this output index.
			if ev.Type == "response.reasoning_text.done" {
				if s.sawReasoningDelta[ev.OutputIndex] {
					break
				}
			} else if s.sawTextDelta[ev.OutputIndex] {
				break
			}
			if ev.Text != "" {
				if ev.Type == "response.reasoning_text.done" {
					s.queue = append(s.queue, ReasoningDelta{Text: ev.Text})
				} else {
					s.think.feed(ev.Text, &s.queue)
				}
			} else if t := ev.Delta.String(); t != "" {
				if ev.Type == "response.reasoning_text.done" {
					s.queue = append(s.queue, ReasoningDelta{Text: t})
				} else {
					s.think.feed(t, &s.queue)
				}
			}
		case "response.output_item.added":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				if _, dup := s.tools[ev.ItemID]; dup {
					// Replayed added frame (SSE replays happen) — the
					// call is already tracked; re-appending the order
					// would emit a second ToolCall for one call and the
					// wire rejects the duplicate outputs.
					break
				}
				callID := ev.Item.CallID
				if callID == "" {
					callID = ev.Item.ID
				}
				s.tools[ev.ItemID] = &respToolAcc{ID: callID, Name: ev.Item.Name, Index: ev.OutputIndex}
				s.toolOrd = append(s.toolOrd, ev.ItemID)
				s.queue = append(s.queue, ToolCallStart{Index: ev.OutputIndex, ToolCallID: callID, Name: ev.Item.Name})
			}
		case "response.output_item.done":
			// A completed function-call item carried on the done frame
			// (some gateways never emit added/arguments-delta pairs).
			if ev.Item != nil && ev.Item.Type == "function_call" {
				callID := ev.Item.CallID
				if callID == "" {
					callID = ev.Item.ID
				}
				if acc := s.tools[ev.ItemID]; acc != nil {
					// The done item carries the COMPLETE arguments —
					// authoritative over any streamed fragments (which
					// some gateways also send). Replace, never append:
					// appending yields "{...}{...}", which fails JSON
					// validation and silently blanks the call's args.
					if ev.Item.Arguments != "" {
						acc.Args.Reset()
						acc.Args.WriteString(ev.Item.Arguments)
					}
				} else {
					s.tools[ev.ItemID] = &respToolAcc{ID: callID, Name: ev.Item.Name, Index: ev.OutputIndex}
					s.toolOrd = append(s.toolOrd, ev.ItemID)
					s.queue = append(s.queue, ToolCallStart{Index: ev.OutputIndex, ToolCallID: callID, Name: ev.Item.Name})
					if ev.Item.Arguments != "" {
						s.tools[ev.ItemID].Args.WriteString(ev.Item.Arguments)
						s.queue = append(s.queue, ToolCallDelta{Index: ev.OutputIndex, ArgsJSONDelta: ev.Item.Arguments})
					}
				}
			} else if ev.Item != nil && ev.Item.Text != "" {
				s.think.feed(ev.Item.Text, &s.queue)
			}
		case "response.function_call_arguments.delta":
			if acc := s.tools[ev.ItemID]; acc != nil {
				if t := ev.Delta.String(); t != "" {
					acc.Args.WriteString(t)
					s.queue = append(s.queue, ToolCallDelta{Index: ev.OutputIndex, ArgsJSONDelta: t})
				}
			}
		case "response.completed", "response.complete", "response.done", "completed", "done":
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
	nTools := 0
	for _, id := range s.toolOrd {
		acc := s.tools[id]
		args := acc.Args.String()
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}" // flush-on-drain: never emit unparseable args silently
		}
		s.queue = append(s.queue, ToolCall{Index: acc.Index, ToolCallID: acc.ID, Name: acc.Name, ArgsJSON: args})
		nTools++
	}
	s.toolOrd = nil
	s.drained = true
	if !s.haveStop {
		// NO provider stop signal: the stream ended without response.completed
		// (a truncated/aborted generation). StopOther is the honest terminal.
		s.stop = StopOther
	}
	// A response that CARRIED tool calls after the completion marker must
	// finish StopToolUse, never StopStop (2026-09-09 tool-swallow fix,
	// exec 01M23EVJV4RGGH8EHTWMG8Q21P): the leader loop dispatches +
	// persists + feeds back tool results ONLY on StopToolUse. Under Stop the
	// tool calls were counted toward the tool_call_count budget but never
	// executed or their results returned — the model re-emitted the same
	// "I'll start by…" preamble each turn (45 text parts, 0 tool parts), the
	// budget aborted at 100 calls, and the work never happened. Parity with
	// the anthropic/legacy/openai-compat providers, all of which finish
	// StopToolUse when tools are pending.
	if nTools > 0 && s.stop == StopStop {
		s.stop = StopToolUse
	}
	s.queue = append(s.queue, Finish{StopReason: s.stop, Usage: s.usage})
}

func (s *responsesStream) fail(err error) (Event, bool, error) {
	s.drained = true
	return StreamError{Err: err}, true, err
}
