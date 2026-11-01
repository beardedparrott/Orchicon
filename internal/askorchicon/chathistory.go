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
	// abortedToolResultOutput is the explicit result attached in place of a
	// tool result that never arrived: the turn is repaired rather than
	// silently losing that the call happened.
	abortedToolResultOutput = "tool call aborted — the turn ended before this tool returned a result"

	// The provider rejection shapes from the operator reports, matched as
	// substrings of a provider error body.
	//
	// The first two are the ORIGINAL bug — an assistant tool_calls message
	// replayed without its results ("BUG: Ask Orchicon model switch fails").
	danglingToolCallNoOutputMarker   = "no tool output found for function call"
	danglingToolCallUnansweredMarker = "must be followed by tool messages responding to each"

	// The MIRROR shape, from the compact-button regression on Ask conversation
	// 01M2C8VXFQY5ZE26PYBSNKA2CA: an ORPHANED tool result — a `tool` message
	// whose preceding assistant `tool_calls` message was cropped away when
	// compaction cut a tool round in half. It must classify here too, or the
	// poisoned session is never dropped and the conversation stays wedged on
	// every send.
	danglingToolCallOrphanedResultMarker = "must be a response to a preceding message with"
)

// askToolCallJSON / askToolResultJSON mirror the tool_calls / tool_results
// column shapes (the ChatMessage proto's ToolCall / ToolResult).
type askToolCallJSON struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments    string `json:"arguments"`
	// IssuedAtUnixMs must be carried here even though the repair neither reads
	// nor writes it: this struct is RE-MARSHALLED (not passed through), so any
	// field absent from it is silently and permanently stripped from every
	// tool_calls document that goes through the terminal write or the per-turn
	// history window.
	IssuedAtUnixMs int64 `json:"issued_at_unix_ms"`
}

type askToolResultJSON struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

// sanitizeAssistantToolLedger returns the (tool_calls, tool_results) pair for
// one assistant message such that every call id has at least one result. A
// call with no id cannot be paired with a result at all (replaying it is the
// bare-tool_calls shape providers reject), so it is dropped; a call whose
// result never arrived gets an explicit aborted result. Both documents always
// encode as "[]" (never nil) so the NOT NULL jsonb columns stay valid.
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

// sanitizeHistoryRows returns the DB-history window with every assistant row's
// tool ledger repaired: no row keeps a tool_call_id without a matching tool
// result. Rows that are already well-formed (the overwhelming majority) are
// returned untouched, so the caller can compare identity cheaply.
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
//
// WHY THIS EXISTS NOW: "UNDO" REMOVES A USER MESSAGE AND LEAVES ITS REPLY. That is what the operator asked for —
// one message, not the exchange — and it is the right behaviour for "forget that I asked this; keep what we
// built afterwards". But a replayed history whose OLDEST turn is an assistant message has no question in front
// of it, and providers reject that shape (the first non-system turn must be the user's). Before this the only
// way a leading assistant could arise was a hand-edited database.
//
// IT DROPS FROM THE REPLAY, IT DOES NOT DELETE. The row stays on disk and in the transcript the operator is
// looking at, so undoing a message never silently takes its reply with it. Only the replayed history is
// trimmed, which is the smallest change that makes the request valid: the model sees the conversation from its
// first surviving question.
//
// DIRECTION COMES FROM THE TIMESTAMPS AND NOTHING ELSE. The turn path hands this function DESC (ListMessages
// is ORDER BY created_at DESC) and other readers hand it ASC, so index 0 is not reliably the oldest. Two
// earlier versions got this wrong in instructive ways: comparing the two ends by (created_at, id) trimmed the
// NEWEST replies whenever ids happened not to sort like time, and sorting indices by the same key did the same
// for rows that share a timestamp. So:
//
//   - if the ends' timestamps DIFFER, that settles the direction outright;
//   - if they do not (one row, a single transaction's rows, or fixtures with no clock), the slice is taken to
//     be in the caller's intended order — ASCENDING, because that is what a chronological reader means and
//     there is no evidence to contradict it.
//
// Guessing has a real cost here: the failure mode is the model quietly forgetting recent turns, which looks
// like a model problem rather than a repair problem.
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

// isDanglingToolCallProviderError reports whether a provider error text is a
// rejected tool-pairing shape — any of the three the operators have hit: an
// unanswered call, and its mirror, an orphaned result. It drives the repair
// path: the poisoned session is dropped so the next send dispatches on a fresh,
// sanitized session instead of replaying it forever.
func isDanglingToolCallProviderError(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, danglingToolCallNoOutputMarker) ||
		strings.Contains(lower, danglingToolCallUnansweredMarker) ||
		strings.Contains(lower, danglingToolCallOrphanedResultMarker)
}

// sessionCreatedUnderDifferentModel reports whether the conversation's stored
// session was created under a model other than the one this turn dispatches
// on. Assistant rows record `model_ref` (and `session_id`) in their metadata,
// so the newest assistant row that names the stored session identifies the
// model that owns it. A session created under a different model must never be
// reused: the new provider re-validates the replayed session history and 400s
// on it. history is the DESC (newest-first) DB window.
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
	// No row names the stored session (legacy rows written before the
	// session id was mirrored into metadata): fall back to the newest model
	// the conversation ran on.
	return newestModel != "" && newestModel != modelRef
}

// emitTurnError surfaces a turn failure verbatim on the live stream so the
// operator sees the provider's own words (the TUI dock notice + the error row
// the poll renders) instead of a silent spinner. Nil-safe (a turn dispatched
// without a stream, e.g. from a test, is a no-op).
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
