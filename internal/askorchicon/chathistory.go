package askorchicon

import (
	"encoding/json"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// Ask-turn history sanitization.
//
// A turn can die between issuing a tool call and receiving its result (token
// exhaustion, Stop, provider error, superseded turn). The partial assistant
// row then carries `tool_calls` with no matching `tool_results` — the live
// tool ledger mirrors a call the moment it is ISSUED — and the next provider
// that re-validates that structured history rejects the whole request:
//
//	No tool output found for function call call_gskf7fpm.
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following
//	tool_calls message)
//
// Both are the operator's verbatim report (the regression fixtures asserted in
// chathistory_test.go). The rule is absolute: history replayed to a provider,
// and history persisted to the DB, is ALWAYS well-formed — every assistant
// tool_call_id has a matching tool result.
const (
	// abortedToolResultOutput is the explicit result attached in place of a tool result that never
	// arrived: the turn is repaired rather than silently losing that the call happened.
	abortedToolResultOutput = "tool call aborted — the turn ended before this tool returned a result"

	// The provider rejection shapes from the operator reports, matched as substrings of a provider
	// error body.
	danglingToolCallNoOutputMarker   = "no tool output found for function call"
	danglingToolCallUnansweredMarker = "must be followed by tool messages responding to each"

	// The MIRROR shape, from the compact-button regression on Ask conversation
	// 01M2C8VXFQY5ZE26PYBSNKA2CA: an ORPHANED tool result.
	danglingToolCallOrphanedResultMarker = "must be a response to a preceding message with"
)

// askToolCallJSON / askToolResultJSON mirror the tool_calls / tool_results
// column shapes (the ChatMessage proto's ToolCall / ToolResult).
type askToolCallJSON struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments    string `json:"arguments"`
	// IssuedAtUnixMs must be carried here even though the repair neither reads nor writes it: this
	// struct is RE-MARSHALLED (not passed through).
	IssuedAtUnixMs int64 `json:"issued_at_unix_ms"`
}

type askToolResultJSON struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

// sanitizeAssistantToolLedger returns the (tool_calls, tool_results) pair for one assistant message
// such that every call id has at least one result.
func sanitizeAssistantToolLedger(callsJSON, resultsJSON []byte) (callsOut, resultsOut []byte) {
	callsOut, resultsOut = []byte("[]"), []byte("[]")

	var decodedCalls []askToolCallJSON
	if len(callsJSON) > 0 {
		_ = json.Unmarshal(callsJSON, &decodedCalls)
	}
	var decodedResults []askToolResultJSON
	if len(resultsJSON) > 0 {
		_ = json.Unmarshal(resultsJSON, &decodedResults)
	}

	calls := make([]askToolCallJSON, 0, len(decodedCalls))
	seenCall := map[string]bool{}
	for _, c := range decodedCalls {
		if strings.TrimSpace(c.ID) == "" || seenCall[c.ID] {
			continue
		}
		seenCall[c.ID] = true
		if c.Type == "" {
			c.Type = "function"
		}
		calls = append(calls, c)
	}

	results := make([]askToolResultJSON, 0, len(decodedResults))
	answered := map[string]bool{}
	for _, r := range decodedResults {
		if strings.TrimSpace(r.ToolCallID) == "" || !seenCall[r.ToolCallID] {
			continue
		}
		answered[r.ToolCallID] = true
		results = append(results, r)
	}
	for _, c := range calls {
		if answered[c.ID] {
			continue
		}
		results = append(results, askToolResultJSON{
			ToolCallID: c.ID,
			Output:     abortedToolResultOutput,
			IsError:    true,
		})
	}

	if b, err := json.Marshal(calls); err == nil {
		callsOut = b
	}
	if b, err := json.Marshal(results); err == nil {
		resultsOut = b
	}
	return callsOut, resultsOut
}

// sanitizeHistoryRows returns the DB-history window with every assistant row's tool ledger
// repaired: no row keeps a tool_call_id without a matching tool result.
func sanitizeHistoryRows(history []db.MessageRow) []db.MessageRow {
	if len(history) == 0 {
		return history
	}
	out := repairLeadingAssistant(history)
	for i := range out {
		if out[i].Role != "assistant" || len(out[i].ToolCalls) == 0 {
			continue
		}
		calls, results := sanitizeAssistantToolLedger(out[i].ToolCalls, out[i].ToolResults)
		out[i].ToolCalls = calls
		out[i].ToolResults = results
	}
	return out
}

// repairLeadingAssistant drops the assistant run that precedes the first user message.
func repairLeadingAssistant(history []db.MessageRow) []db.MessageRow {
	if len(history) == 0 {
		return history
	}
	ascending := true
	if first, last := history[0], history[len(history)-1]; !first.CreatedAt.Equal(last.CreatedAt) {
		ascending = first.CreatedAt.Before(last.CreatedAt)
	}
	dropped := 0
	if ascending {
		for dropped < len(history) && history[dropped].Role == "assistant" {
			dropped++
		}
		out := make([]db.MessageRow, len(history)-dropped)
		copy(out, history[dropped:])
		return out
	}
	for dropped < len(history) && history[len(history)-1-dropped].Role == "assistant" {
		dropped++
	}
	out := make([]db.MessageRow, len(history)-dropped)
	copy(out, history[:len(history)-dropped])
	return out
}

// isDanglingToolCallProviderError reports whether a provider error text is a rejected tool-pairing
// shape — any of the three the operators have hit: an unanswered call, and its mirror.
func isDanglingToolCallProviderError(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, danglingToolCallNoOutputMarker) ||
		strings.Contains(lower, danglingToolCallUnansweredMarker) ||
		strings.Contains(lower, danglingToolCallOrphanedResultMarker)
}

// sessionCreatedUnderDifferentModel reports whether the conversation's stored session was created
// under a model other than the one this turn dispatches on.
func sessionCreatedUnderDifferentModel(sessionID, modelRef string, history []db.MessageRow) bool {
	if modelRef == "" {
		return false
	}
	newestModel := ""
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		var meta struct {
			ModelRef  string `json:"model_ref"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(m.Metadata, &meta); err != nil || meta.ModelRef == "" {
			continue
		}
		if sessionID != "" && meta.SessionID == sessionID {
			// Definitive: this row belongs to the stored session.
			return meta.ModelRef != modelRef
		}
		if newestModel == "" {
			newestModel = meta.ModelRef
		}
	}
	// No row names the stored session (legacy rows written before the session id was mirrored into
	// metadata): fall back to the newest model the conversation ran on.
	return newestModel != "" && newestModel != modelRef
}

// emitTurnError surfaces a turn failure verbatim on the live stream so the operator sees the
// provider's own words (the TUI dock notice + the error row the poll renders) instead of a silent.
func emitTurnError(cb func(*apiv1.ChatStreamResponse), text string) {
	if cb == nil || text == "" {
		return
	}
	cb(&apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_Error{
			Error: &apiv1.ErrorChunk{Message: text, Code: "turn_failed"},
		},
	})
}
