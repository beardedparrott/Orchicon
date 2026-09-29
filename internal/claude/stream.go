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
	Usage        Usage

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
		if strField(inner, "type") == "content_block_delta" {
			delta, _ := inner["delta"].(map[string]any)
			if delta != nil && strField(delta, "type") == "text_delta" {
				ev.Type = "text_delta"
				ev.Text = strField(delta, "text")
			}
		}
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
		if u, ok := raw["usage"].(map[string]any); ok {
			ev.Usage = Usage{
				InputTokens:         int64(intField(u, "input_tokens")),
				OutputTokens:        int64(intField(u, "output_tokens")),
				CacheReadTokens:     int64(intField(u, "cache_read_input_tokens")),
				CacheCreationTokens: int64(intField(u, "cache_creation_input_tokens")),
			}
		}
	}
	return ev, nil
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
