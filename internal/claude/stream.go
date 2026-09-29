// Package claude implements the Claude Code CLI adapter bridge: the second
// concrete scheduler.AdapterBridge, proving the dispatcher abstraction.
//
// Transport shape (deliberately different from opencode's `serve`): claude
// runs as ONE long-lived streaming subprocess per conversation, launched in
// non-interactive print mode with stream-json on BOTH sides:
//
//	claude -p --input-format stream-json \
//	    --output-format stream-json --verbose --include-partial-messages \
//	    --model <model> [--resume <session-id>]
//
// stdin carries one JSON user-turn object per turn (kept open across turns);
// stdout carries one JSON event per line. A turn's boundary is the streamed
// "result" message — NOT process exit (the process stays alive for the next
// stdin turn) — so the session id it carries can be resumed.
//
// This file holds the pure stream-json parsers: no I/O, no subprocess, no
// network. They are exercised by canned fixtures (stream_test.go) so the
// core acceptance runs with ZERO real Anthropic spend.
package claude

import "encoding/json"

// Result message subtypes (the terminal turn marker).
const (
	ResultSuccess        = "success"
	ResultErrorMaxTurns  = "error_max_turns"
	ResultErrorMaxBudget = "error_max_budget_usd"
)

// ToolUse is one tool_use content block from an `assistant` message.
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is one tool_result content block from a `user` message.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// Usage carries the token counts a `result` message reports.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// StreamEvent is the adapter-relevant decode of one stdout JSON line.
//
// Type mirrors the raw `type` field ("system" | "stream_event" |
// "assistant" | "user" | "result"); a few shapes are normalized into
// synthetic kinds the session loop switches on:
//
//	"text_delta"  — one partial text delta (--include-partial-messages)
type StreamEvent struct {
	Type      string
	Subtype   string
	SessionID string

	// Text is the assistant text for this line: the whole text for an
	// `assistant` message (all text blocks concatenated), or one delta for a
	// `stream_event` text delta.
	Text string

	// ToolUses / ToolResults are populated for `assistant` / `user` messages.
	ToolUses    []ToolUse
	ToolResults []ToolResult

	// Result fields (Type == "result").
	ResultText   string
	IsError      bool
	TotalCostUSD float64
	NumTurns     int

	// Usage carries the token counts decoded for this line. UsagePresent is
	// true when a usage object was actually decoded (result / assistant /
	// message_start / message_delta) so "no usage" is never mistaken for
	// "zero usage". UsageIsAggregate is true ONLY for the terminal `result`
	// line, whose usage is the authoritative TURN AGGREGATE. Per-message
	// samples (assistant / message_start / message_delta) set it false so the
	// mapper can accumulate them as a fallback WITHOUT ever summing them into
	// the aggregate — that would double-count the turn.
	Usage            Usage
	UsagePresent     bool
	UsageIsAggregate bool
	// MessageID is the Anthropic message a per-message usage sample belongs
	// to (empty when the wire carries none); repeated samples for one message
	// are merged once by the mapper rather than summed.
	MessageID string

	// Raw is the original JSON line, kept for the durable transcript.
	Raw []byte
}

// ParseLine decodes one stdout line of the claude stream-json protocol into
// a StreamEvent. An unknown/irrelevant shape yields a zero-value event with
// only Type populated (never an error) so a CLI version that adds event
// types cannot break the loop; only genuinely malformed JSON errors.
func ParseLine(line []byte) (StreamEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return StreamEvent{}, err
	}
	ev := StreamEvent{
		Type:      strField(raw, "type"),
		Subtype:   strField(raw, "subtype"),
		SessionID: strField(raw, "session_id"),
		Raw:       append([]byte(nil), line...),
	}
	switch ev.Type {
	case "stream_event":
		inner, _ := raw["event"].(map[string]any)
		if inner == nil {
			break
		}
		switch strField(inner, "type") {
		case "content_block_delta":
			delta, _ := inner["delta"].(map[string]any)
			if delta != nil && strField(delta, "type") == "text_delta" {
				ev.Type = "text_delta"
				ev.Text = strField(delta, "text")
			}
		case "message_start":
			// With --include-partial-messages each API message opens with a
			// message_start carrying the SAME usage object as the eventual
			// assistant message. Decode it as a PER-MESSAGE sample (not the
			// authoritative aggregate) so the mapper can use it as a fallback
			// without double-counting the terminal result.
			msg, _ := inner["message"].(map[string]any)
			if msg != nil {
				if u, ok := usageMap(msg["usage"]); ok {
					ev.MessageID = strField(msg, "id")
					ev.Usage, ev.UsagePresent = u, true
				}
			}
		}
		// NOTE: message_delta is deliberately NOT decoded into usage. Its
		// `usage` carries only the cumulative output_tokens and NO message id,
		// so it cannot be merged into the in-flight message's accumulator and
		// would double-count that message's output. The `assistant` message
		// (which the partial-message stream always emits) reports the message's
		// FINAL usage under the same message id, and the terminal `result`
		// carries the authoritative turn aggregate.
	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		for _, block := range contentBlocks(msg) {
			switch strField(block, "type") {
			case "text":
				ev.Text += strField(block, "text")
			case "tool_use":
				in, _ := block["input"].(map[string]any)
				ev.ToolUses = append(ev.ToolUses, ToolUse{
					ID:    strField(block, "id"),
					Name:  strField(block, "name"),
					Input: in,
				})
			}
		}
		// Per-message usage sample (the full message report). Authoritative
		// only as a FALLBACK when the terminal result carries no aggregate.
		if u, ok := usageMap(msg["usage"]); ok {
			ev.MessageID = strField(msg, "id")
			ev.Usage, ev.UsagePresent = u, true
		}
	case "user":
		msg, _ := raw["message"].(map[string]any)
		for _, block := range contentBlocks(msg) {
			if strField(block, "type") != "tool_result" {
				continue
			}
			ev.ToolResults = append(ev.ToolResults, ToolResult{
				ToolUseID: strField(block, "tool_use_id"),
				Content:   flattenContent(block["content"]),
				IsError:   boolField(block, "is_error"),
			})
		}
	case "result":
		ev.ResultText = strField(raw, "result")
		ev.IsError = boolField(raw, "is_error")
		ev.NumTurns = intField(raw, "num_turns")
		ev.TotalCostUSD = floatField(raw, "total_cost_usd")
		if u, ok := usageMap(raw["usage"]); ok {
			ev.Usage, ev.UsagePresent = u, true
			// The result line's usage is the authoritative TURN AGGREGATE.
			ev.UsageIsAggregate = true
		}
	}
	return ev, nil
}

// usageMap decodes an Anthropic `usage` object into the adapter Usage. The
// key vocabulary is identical across the result / assistant / message_start
// shapes (input_tokens, output_tokens, cache_read_input_tokens,
// cache_creation_input_tokens). ok is false when the field is absent so a
// caller never mistakes "no usage" for "zero usage".
func usageMap(v any) (Usage, bool) {
	u, ok := v.(map[string]any)
	if !ok {
		return Usage{}, false
	}
	return Usage{
		InputTokens:         int64(intField(u, "input_tokens")),
		OutputTokens:        int64(intField(u, "output_tokens")),
		CacheReadTokens:     int64(intField(u, "cache_read_input_tokens")),
		CacheCreationTokens: int64(intField(u, "cache_creation_input_tokens")),
	}, true
}

// TurnSucceeded reports whether a terminal `result` message represents a
// successful turn. Only ResultSuccess counts.
func (e StreamEvent) TurnSucceeded() bool { return e.Subtype == ResultSuccess }

// IsTerminalResult reports whether this line is the turn-boundary `result`
// message (success OR a max-turns/max-budget error result).
func (e StreamEvent) IsTerminalResult() bool {
	if e.Type != "result" {
		return false
	}
	switch e.Subtype {
	case ResultSuccess, ResultErrorMaxTurns, ResultErrorMaxBudget:
		return true
	}
	return false
}

func contentBlocks(msg map[string]any) []map[string]any {
	if msg == nil {
		return nil
	}
	raw, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if b, ok := item.(map[string]any); ok {
			out = append(out, b)
		}
	}
	return out
}

// flattenContent renders a tool_result `content` field (which may be a
// string or an array of {type:text,text} blocks) to plain text.
func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var out string
		for _, item := range t {
			if b, ok := item.(map[string]any); ok {
				out += strField(b, "text")
			}
		}
		return out
	}
	return ""
}

func strField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch n := m[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func floatField(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	f, _ := m[key].(float64)
	return f
}

// writtenFilesFromTool extracts the file paths a claude built-in file tool
// touched, given the tool name and its input map. Write/Edit/MultiEdit/
// NotebookEdit all carry a `file_path`. Any other tool yields nil. Tolerant:
// a shape change yields nil rather than an error.
func writtenFilesFromTool(toolName string, input map[string]any) []string {
	switch toolName {
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "ApplyPatch":
	default:
		return nil
	}
	p := strField(input, "file_path")
	if p == "" {
		p = strField(input, "notebook_path")
	}
	if p == "" {
		return nil
	}
	return []string{p}
}
