package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// legacycc: the Command Code legacy /alpha/generate transport (D6) — custom
// request envelope + custom SSE event stream. Only consumer: commandcode.

// legacyConfig is the envelope's config stanza.
type legacyConfig struct {
	WorkingDir  string `json:"workingDir"`
	Date        string `json:"date"`
	Environment string `json:"environment"`
	IsGitRepo   bool   `json:"isGitRepo"`
	MainBranch  string `json:"mainBranch,omitempty"`
}

// legacyParams is the envelope's params stanza.
type legacyParams struct {
	Model           string       `json:"model"`
	Messages        []legacyMsg  `json:"messages"`
	Tools           []legacyTool `json:"tools,omitempty"`
	System          string       `json:"system,omitempty"`
	MaxTokens       int64        `json:"max_tokens,omitempty"`
	Temperature     *float64     `json:"temperature,omitempty"`
	Stream          bool         `json:"stream"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
}

// legacyEnvelope is the /alpha/generate request body.
type legacyEnvelope struct {
	Config   legacyConfig `json:"config"`
	Params   legacyParams `json:"params"`
	ThreadID string       `json:"threadId,omitempty"`
}

type legacyPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     *legacyOutput   `json:"output,omitempty"`
}

type legacyOutput struct {
	Type  string `json:"type"` // "text"
	Value string `json:"value"`
}

type legacyMsg struct {
	Role    string       `json:"role"`
	Content []legacyPart `json:"content"`
}

type legacyTool struct {
	Type     string `json:"type"` // function
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// buildLegacyEnvelope marshals the legacy request. Assistant reasoning is
// never replayed (normalized history carries none).
func buildLegacyEnvelope(req TurnRequest, threadID string) legacyEnvelope {
	env := legacyEnvelope{
		Config: legacyConfig{
			WorkingDir:  "/",
			Date:        time.Now().UTC().Format(time.RFC3339),
			Environment: "production",
		},
		Params: legacyParams{
			Model: req.Model, Stream: true, MaxTokens: req.MaxTokens,
			Temperature: req.Temperature,
		},
		ThreadID: threadID,
	}
	var sys strings.Builder
	for i, s := range req.System {
		if i > 0 {
			sys.WriteString("\n\n")
		}
		sys.WriteString(s.Text)
	}
	env.Params.System = sys.String()
	if req.ReasoningEffort != "" {
		env.Params.ReasoningEffort = req.ReasoningEffort
	}
	for _, t := range req.Tools {
		var lt legacyTool
		lt.Type = "function"
		lt.Function.Name = t.Name
		lt.Function.Description = t.Description
		if t.ParamsJSON != "" {
			lt.Function.Parameters = json.RawMessage(t.ParamsJSON)
		}
		env.Params.Tools = append(env.Params.Tools, lt)
	}
	for _, m := range req.Messages {
		lm := legacyMsg{Role: string(m.Role)}
		switch m.Role {
		case RoleTool:
			lm.Role = "tool"
			for _, c := range m.Content {
				if c.ToolResult != nil {
					lm.Content = append(lm.Content, legacyPart{
						Type: "tool-result", ToolCallID: c.ToolResult.ToolCallID,
						Output: &legacyOutput{Type: "text", Value: c.ToolResult.Content},
					})
				}
			}
		case RoleAssistant:
			lm.Role = "assistant"
			for _, c := range m.Content {
				switch {
				case c.Text != nil:
					lm.Content = append(lm.Content, legacyPart{Type: "text", Text: *c.Text})
				case c.ToolUse != nil:
					lm.Content = append(lm.Content, legacyPart{
						Type: "tool-call", ToolCallID: c.ToolUse.ToolCallID,
						ToolName: c.ToolUse.Name, Input: json.RawMessage(c.ToolUse.ArgsJSON),
					})
				}
			}
		default:
			lm.Role = "user"
			for _, c := range m.Content {
				if c.Text != nil {
					lm.Content = append(lm.Content, legacyPart{Type: "text", Text: *c.Text})
				}
			}
		}
		env.Params.Messages = append(env.Params.Messages, lm)
	}
	return env
}

// legacySSEEvent is one legacy stream event (event: line names the type;
// data carries the payload).
type legacySSEEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Text  string `json:"text"`

	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Input      json.RawMessage `json:"input"`

	FinishReason string       `json:"finishReason"`
	TotalUsage   *legacyUsage `json:"totalUsage"`
	Error        *legacyError `json:"error"`
	Message      string       `json:"message"`
}

type legacyError struct {
	Message string `json:"message"`
}

// legacyUsage is the cache-inclusive totalUsage block.
type legacyUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	InputTokenDetails struct {
		CacheReadTokens  int64 `json:"cacheReadTokens"`
		CacheWriteTokens int64 `json:"cacheWriteTokens"`
		NoCacheTokens    int64 `json:"noCacheTokens"`
	} `json:"inputTokenDetails"`
}

// legacyNoCache derives no-cache input tokens: total input is
// cache-INCLUSIVE, so noCache = max(0, total − cacheRead − cacheWrite)
// (mirrors the reference plugin's ccUsageToAiSdkUsage).
func legacyNoCache(total, cacheRead, cacheWrite int64) int64 {
	d := total - cacheRead - cacheWrite
	if d < 0 {
		return 0
	}
	return d
}

// legacyUsageToUsage normalizes totalUsage.
func legacyUsageToUsage(u *legacyUsage) Usage {
	if u == nil {
		return Usage{}
	}
	// InputTokens normalizes to noCache = total − cacheRead − cacheWrite
	// (ADR-0003 D6): totalUsage input is cache-INCLUSIVE and normalized
	// Usage prices the cache sub-buckets separately.
	return Usage{
		InputTokens:      legacyNoCache(u.InputTokens, u.InputTokenDetails.CacheReadTokens, u.InputTokenDetails.CacheWriteTokens),
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.InputTokenDetails.CacheReadTokens,
		CacheWriteTokens: u.InputTokenDetails.CacheWriteTokens,
	}
}

func mapLegacyStop(reason string) StopReason {
	switch reason {
	case "stop":
		return StopStop
	case "length", "max-tokens":
		return StopLength
	case "tool-calls", "tool_calls":
		return StopToolUse
	case "content-filter":
		return StopContentFilter
	default:
		// An empty/unrecognized finishReason is NOT an end-of-turn signal —
		// StopOther is the honest terminal (the loop's success gate treats
		// it as a failure). Synthesizing StopStop from "" recorded hollow
		// successes on truncated responses.
		return StopOther
	}
}

// legacyStream decodes the legacy SSE event stream into normalized events.
type legacyStream struct {
	r    *sseReader
	body io.Closer

	stop    StopReason
	usage   Usage
	drained bool
}

func (s *legacyStream) Close() error {
	if rc, ok := s.body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Next yields normalized events; finish arrives only after drain.
func (s *legacyStream) Next(ctx context.Context) (Event, bool, error) {
	_ = ctx
	if s.drained {
		return nil, false, nil
	}
	for {
		frame, ok, err := s.r.Next()
		if err != nil {
			return s.fail(fmt.Errorf("commandcode legacy: stream read: %w", err))
		}
		if !ok {
			return s.flush()
		}
		if frame.Data == "" {
			continue
		}
		var ev legacySSEEvent
		if err := json.Unmarshal([]byte(frame.Data), &ev); err != nil {
			return s.fail(fmt.Errorf("commandcode legacy: bad event: %w", err))
		}
		typ := frame.Event
		if typ == "" {
			typ = ev.Type
		}
		switch typ {
		case "text-start", "text-end", "reasoning-start", "reasoning-end":
			continue
		case "text-delta":
			t := ev.Delta
			if t == "" {
				t = ev.Text
			}
			if t == "" {
				continue
			}
			return TextDelta{Text: t}, true, nil
		case "reasoning-delta":
			t := ev.Delta
			if t == "" {
				t = ev.Text
			}
			if t == "" {
				continue
			}
			return ReasoningDelta{Text: t}, true, nil
		case "tool-call":
			args := string(ev.Input)
			if !json.Valid([]byte(args)) || args == "" {
				args = "{}"
			}
			return ToolCall{Index: 0, ToolCallID: ev.ToolCallID, Name: ev.ToolName, ArgsJSON: args}, true, nil
		case "finish":
			s.stop = mapLegacyStop(ev.FinishReason)
			s.usage = legacyUsageToUsage(ev.TotalUsage)
			continue // held until drain — trailing events may follow
		case "error":
			msg := "provider error"
			if ev.Error != nil {
				msg = ev.Error.Message
			} else if ev.Message != "" {
				msg = ev.Message
			}
			return s.fail(fmt.Errorf("commandcode legacy: %s", msg))
		default:
			continue
		}
	}
}

func (s *legacyStream) flush() (Event, bool, error) {
	s.drained = true
	if s.stop == "" {
		// NO finish event arrived: the stream ended without an
		// end-of-response signal — a truncated/aborted generation, not a
		// completed turn (parity: openaicompat/anthropic report StopOther
		// here; synthesizing StopStop recorded hollow successes).
		s.stop = StopOther
	}
	return Finish{StopReason: s.stop, Usage: s.usage}, true, nil
}

func (s *legacyStream) fail(err error) (Event, bool, error) {
	s.drained = true
	return StreamError{Err: err}, true, err
}
