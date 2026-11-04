package chat

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// execsession.go — the converters from the two durable/live execution
// sources into ChatItems, mirroring
// frontend/src/components/executions/SessionChatPane.tsx:
//   - HistoryItems: GetExecutionSession parts → items tagged phase
//     `step-N` (N = opencode step counter, incremented on
//     step_start/step_finish boundaries).
//   - LiveItems: StreamExecutionEvents responses → items tagged phase
//     `live-N` (N = synthetic counter bumped on tool/error boundaries).

// msOf converts a protobuf timestamp to ms since epoch (0 when nil),
// falling back to wall-clock like the TS `Date.now()` fallback.
func msOf(ts interface{ GetSeconds() int64 }) int64 {
	if ts == nil {
		return time.Now().UnixMilli()
	}
	return ts.GetSeconds() * 1000
}

func decodePayload(payload []byte) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// strOf safely reads a string field off a decoded payload.
func strOf(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// strOfJSON stringifies a non-string JSON value (tool input objects).
func strOfJSON(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// HistoryItems converts GetExecutionSession parts into ChatItems
// (SessionChatPane.tsx transcriptItems). The result is phase-grouped so
// per-chunk reasoning/text parts of one step converge into one bubble.
func HistoryItems(parts []*apiv1.ExecutionSessionPart) []ChatItem {
	if len(parts) == 0 {
		return nil
	}
	out := []ChatItem{}
	step := 0
	for _, p := range parts {
		if p == nil {
			continue
		}
		at := msOf(p.GetCreatedAt())
		key := "t-" + itoa64(p.GetSeq())
		pl := decodePayload(p.GetPayload())
		switch p.GetKind() {
		case "step_start", "step_finish":
			step++
		case "user_message":
			if txt := strOf(pl, "text"); txt != "" {
				out = append(out, ChatItem{Kind: KindUser, Text: txt, Source: orDefault(strOf(pl, "source"), "goal"), At: at, Key: key})
			}
		case "text":
			if part, ok := pl["part"].(map[string]any); ok {
				if txt := strOf(part, "text"); txt != "" {
					out = append(out, ChatItem{Kind: KindText, Text: txt, At: at, Key: key, Phase: "step-" + itoa(int64(step))})
				}
			}
		case "tool_use":
			part, _ := pl["part"].(map[string]any)
			state, _ := part["state"].(map[string]any)
			toolName := "tool"
			if part != nil {
				if s := strOf(part, "tool"); s != "" {
					toolName = s
				}
			}
			input, output := "", ""
			if state != nil {
				if s, ok := state["input"].(string); ok {
					input = s
				} else {
					input = strOfJSON(state["input"])
				}
				if s, ok := state["output"].(string); ok {
					output = s
				}
			}
			out = append(out, ChatItem{Kind: KindTool, Tool: &ParsedTool{ID: key, ToolName: toolName, Input: input, Output: output, At: at}, Key: key})
		case "reasoning":
			if part, ok := pl["part"].(map[string]any); ok {
				if txt := strOf(part, "text"); txt != "" {
					out = append(out, ChatItem{Kind: KindReasoning, Text: txt, At: at, Key: key, Phase: "step-" + itoa(int64(step))})
				}
			}
		case "session_info":
			// The part is rendered as a session identity row when it carries
			// EITHER a session id or an adapter kind: a native execution has no
			// serve/session id to point at, only the adapter it ran on.
			sid := strOf(pl, "session_id")
			adapterKind := strOf(pl, "adapter_kind")
			if sid != "" || adapterKind != "" {
				out = append(out, ChatItem{Kind: KindSession, SessionID: sid, ServeURL: strOf(pl, "serve_url"), AdapterKind: adapterKind, At: at, Key: key})
			}
		case "compacted":
			// The platform collapsed this execution's context mid-run. Every adapter writes this part
			// (opencode, claude, and now the native engine) and NONE of the two renderers drew it, so the one
			// event that rewrites the conversation a provider must accept was the one event with no row.
			if txt := compactedNotice(pl); txt != "" {
				out = append(out, ChatItem{Kind: KindNotice, Text: txt, At: at, Key: key})
			}
		case "error":
			msg := "opencode session error"
			if e, ok := pl["error"].(map[string]any); ok {
				if s := strOf(e, "message"); s != "" {
					msg = s
				} else if s := strOf(e, "name"); s != "" {
					msg = s
				}
			}
			out = append(out, ChatItem{Kind: KindError, Text: msg, At: at, Key: key})
		}
	}
	return GroupByPhase(out)
}

// compactedNotice renders a `compacted` session part — from ANY adapter — as the platform notice it is.
//
// The payload shapes differ (the native engine reports messages/tool rounds evicted and the spend that fired
// the trigger; opencode and claude report step/reason/provider/model, and claude also a scope or an
// auto-compact source), so this reads what is there rather than assuming one adapter's fields. A part with
// nothing recognisable still says the context was compacted: that is the fact the operator needs, and losing
// it because an adapter named its fields differently is how the collapse went unseen in the first place.
func compactedNotice(pl map[string]any) string {
	var b strings.Builder
	b.WriteString("Context compacted mid-run")
	if step := numOf(pl, "step"); step > 0 {
		b.WriteString(" at step " + itoa(int64(step)))
	}
	b.WriteString(".")
	if before, after := numOf(pl, "messages_before"), numOf(pl, "messages_after"); before > 0 {
		b.WriteString(" " + itoa(int64(before)) + " messages collapsed to " + itoa(int64(after)) + ".")
	}
	if evicted := numOf(pl, "tool_rounds_evicted"); evicted > 0 {
		b.WriteString(" " + itoa(int64(evicted)) + " tool round(s) were moved to the offload file.")
	}
	if reason := strOf(pl, "reason"); reason != "" {
		b.WriteString(" Trigger: " + reason + ".")
	}
	if provider := strOf(pl, "provider"); provider != "" {
		if model := strOf(pl, "model"); model != "" {
			b.WriteString(" Provider: " + provider + " (" + model + ").")
		} else {
			b.WriteString(" Provider: " + provider + ".")
		}
	}
	if src := strOf(pl, "source"); src != "" {
		b.WriteString(" Source: " + src + ".")
	}
	b.WriteString(" The request the provider received after this point carries the collapsed history, not the original.")
	return b.String()
}

// numOf reads a numeric payload field (JSON numbers decode as float64, and a count may arrive either way).
func numOf(pl map[string]any, key string) int {
	switch v := pl[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// LiveItems converts StreamExecutionEvents responses into ChatItems
// (SessionChatPane.tsx liveItems): TELEMETRY text / reasoning-wrapped
// JSON chunks, TOOL_CALL rows, ERROR rows, ARTIFACT rows. The live
// phase counter bumps on tool/error boundaries.
func LiveItems(events []*apiv1.StreamExecutionEventsResponse) []ChatItem {
	out := []ChatItem{}
	phase := 0
	for _, resp := range events {
		if resp == nil {
			continue
		}
		evt := resp.GetEvent()
		if evt == nil {
			continue
		}
		at := msOf(evt.GetOccurredAt())
		id := orDefault(evt.GetEventId(), itoa64(resp.GetSequence()))
		pl := decodePayload(evt.GetPayload())
		switch evt.GetEventType() {
		case apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TELEMETRY:
			raw := strOf(pl, "text")
			if raw == "" {
				break
			}
			text, isReasoning := raw, false
			if strings.HasPrefix(raw, "{") {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(raw), &parsed); err == nil &&
					strOf(parsed, "kind") == "reasoning" && strOf(parsed, "text") != "" {
					isReasoning = true
					text = strOf(parsed, "text")
				}
			}
			if isReasoning {
				out = append(out, ChatItem{Kind: KindReasoning, Text: text, At: at, Key: "r-" + id, Live: true, Phase: "live-" + itoa(int64(phase))})
			} else {
				out = append(out, ChatItem{Kind: KindText, Text: text, At: at, Key: id, Live: true, Phase: "live-" + itoa(int64(phase))})
			}
		case apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_TOOL_CALL:
			toolName := orDefault(strOf(pl, "tool_name"), "tool")
			out = append(out, ChatItem{Kind: KindTool, Tool: &ParsedTool{
				ID: id, ToolName: toolName,
				Input:  strOf(pl, "input"),
				Output: strOf(pl, "output"), At: at,
			}, Key: id})
			phase++
		case apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ERROR:
			msg := orDefault(orDefault(strOf(pl, "text"), strOf(pl, "message")), "execution error")
			out = append(out, ChatItem{Kind: KindError, Text: msg, At: at, Key: id})
			phase++
		case apiv1.ExecutionEventType_EXECUTION_EVENT_TYPE_ARTIFACT:
			out = append(out, ChatItem{Kind: KindArtifact,
				Name:    orDefault(strOf(pl, "artifact_name"), "artifact"),
				Type:    orDefault(strOf(pl, "artifact_type"), "text"),
				Content: strOf(pl, "content"),
				At:      at, Key: id,
			})
		}
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func itoa(n int64) string { return itoa64(n) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
