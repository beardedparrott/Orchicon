package orchicon

// askreduce.go — deterministic context reduction for Ask turns.
//
// Native Ask sessions are SESSIONLESS: every turn re-sends the whole
// accumulated history as the prompt (see dispatchTurnMessage in chatturn.go).
// A long enough conversation therefore exceeds the model's context window and
// EVERY subsequent send fails with a provider 400 — and it can never
// self-heal, because the next turn re-sends the very same oversized history.
//
// This pass is the escape hatch. It is DETERMINISTIC (no model call, so it
// cannot itself fail, hang, or cost anything) and it targets the two buckets
// that actually dominate a bloated prompt: inline image data URLs and verbose
// tool results. Conversation TEXT is preserved verbatim up to a generous
// per-part cap, because text is the part a reader follows.
//
// Measured on the real wedge that motivated this work (a 1574-message Ask
// conversation at ~1.02M prompt tokens against a 1,048,576-token window):
// inline images were 46% of the prompt and tool results 37%, while ALL
// conversation text together was 3.3%. Reducing the first two returns the
// session to a size that fits any window — and small enough that a follow-up
// summarize call can succeed in ONE request.

import (
	"context"
	"fmt"
	"strings"
)

// contextReduceStages is the bounded escalation tried when a provider rejects a
// turn for exceeding its context window: first reduce all but the most recent
// turns (surgical — the live thread stays intact), then, only if the provider
// still refuses, reduce the whole history including the tail. Two stages, then
// give up: an unbounded retry loop against a hard limit would burn the turn.
var contextReduceStages = []int{6, 0}

const (
	// imageElidedMarker replaces an inline image data URL. The image is not
	// recoverable from history, so the marker states WHY it is gone rather than
	// leaving a silent hole the reader cannot explain.
	imageElidedMarker = "[image omitted: conversation history was reduced to fit the model's context window]"

	// toolResultHeadChars is how much of a tool result survives elision — enough
	// to keep the gist of what the tool returned.
	toolResultHeadChars = 200

	// textPartCapChars caps ONE text part. Conversation text is a small slice of
	// a bloated prompt, so this only bites on a genuinely enormous message.
	textPartCapChars = 4000
)

// contextLengthSignatures are the provider phrasings that mean "the prompt
// exceeded my context window". There is no shared error type across the native
// wires, so detection is textual and deliberately conservative: a MISS simply
// means no reduction is attempted (the turn fails exactly as it does today),
// never that a healthy turn is mangled.
var contextLengthSignatures = []string{
	"maximum context length",
	"context length exceeded",
	"context_length_exceeded",
	"prompt is too long",
	"reduce the length",
	"too many tokens",
}

// isContextLengthError reports whether err is a provider context-window
// overflow.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range contextLengthSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// ReduceStats reports what one reduction pass reclaimed, so the rescue of a
// turn is visible in the log rather than silent.
type ReduceStats struct {
	ImagesDropped      int
	ToolResultsElided  int
	TextPartsTruncated int
	BytesBefore        int
	BytesAfter         int
}

// Reclaimed reports the payload bytes the pass removed.
func (s ReduceStats) Reclaimed() int { return s.BytesBefore - s.BytesAfter }

// ReduceConversationHistoryForContext returns a copy of history with the
// prompt-dominating payloads elided, leaving the conversation's substance in
// place. keepTail is the number of trailing messages left VERBATIM (the live
// thread a reader is following); 0 reduces the whole history.
//
// Structure is preserved exactly: roles, message order, and every tool_use /
// tool_result id pairing survive, because a dropped pairing is itself a 400
// (see sanitizeChatHistory). Only payload SIZE changes.
func ReduceConversationHistoryForContext(history []Message, keepTail int) ([]Message, ReduceStats) {
	st := ReduceStats{BytesBefore: conversationBytes(history)}
	out := make([]Message, 0, len(history))
	reduceFrom := len(history) - keepTail
	if reduceFrom < 0 {
		reduceFrom = 0
	}
	for i, m := range history {
		if i >= reduceFrom {
			out = append(out, m)
			continue
		}
		nm := Message{Role: m.Role}
		for _, c := range m.Content {
			switch {
			case c.Image != nil:
				st.ImagesDropped++
				marker := imageElidedMarker
				nm.Content = append(nm.Content, Content{Text: &marker})
			case c.ToolResult != nil:
				st.ToolResultsElided++
				// ToolCallID is carried through DELIBERATELY: the matching
				// assistant tool_use must still find its result, or the provider
				// rejects the turn for an unanswered tool call.
				elided := ContentToolResult{
					ToolCallID: c.ToolResult.ToolCallID,
					Content:    elideToolResult(c.ToolResult.Content),
					IsError:    c.ToolResult.IsError,
				}
				nm.Content = append(nm.Content, Content{ToolResult: &elided})
			case c.Text != nil:
				capped, truncated := capTextPart(*c.Text)
				if truncated {
					st.TextPartsTruncated++
				}
				nm.Content = append(nm.Content, Content{Text: &capped})
			default:
				nm.Content = append(nm.Content, c)
			}
		}
		out = append(out, nm)
	}
	st.BytesAfter = conversationBytes(out)
	return out, st
}

// elideToolResult keeps the head of a tool result and states how much was
// dropped, so the model still knows a tool ran and roughly what it returned.
func elideToolResult(s string) string {
	head, truncated := headRunes(s, toolResultHeadChars)
	if !truncated {
		return s
	}
	return fmt.Sprintf("%s\n… [tool result truncated: %d of %d characters elided to fit the context window]",
		head, int64(len([]rune(s))-toolResultHeadChars), len([]rune(s)))
}

// capTextPart trims one oversized text part. Rune-based so a multi-byte
// character is never split in half.
func capTextPart(s string) (string, bool) {
	head, truncated := headRunes(s, textPartCapChars)
	if !truncated {
		return s, false
	}
	return fmt.Sprintf("%s\n… [truncated: %d of %d characters elided to fit the context window]",
		head, int64(len([]rune(s))-textPartCapChars), len([]rune(s))), true
}

// headRunes returns the first n runes of s, reporting whether it had to cut.
func headRunes(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}

// conversationBytes approximates the serialized size of a history — exactly
// the accounting the reduce pass optimizes. It sums the payload fields that
// dominate a prompt (text, image data, tool results and args) and ignores
// envelope overhead, which is negligible at these sizes.
func conversationBytes(history []Message) int {
	n := 0
	for _, m := range history {
		for _, c := range m.Content {
			switch {
			case c.Text != nil:
				n += len(*c.Text)
			case c.Image != nil:
				n += len(*c.Image)
			case c.ToolResult != nil:
				n += len(c.ToolResult.Content)
			case c.ToolUse != nil:
				n += len(c.ToolUse.ArgsJSON)
			}
		}
	}
	return n
}

// startTurnWithContextRecovery starts the provider stream for a turn and, when
// the provider refuses it for exceeding the context window, reduces the
// session's replayed history and retries. Escalation is bounded
// (contextReduceStages), and EVERY reduction is persisted, so a wedged session
// heals permanently instead of failing on every subsequent send.
//
// req is mutated in place, so the caller drains against exactly the history the
// provider accepted.
func (b *NativeBridge) startTurnWithContextRecovery(turnCtx context.Context, prov Provider, req *TurnRequest, sessionID string) (TurnStream, error) {
	stream, err := prov.StreamTurn(turnCtx, *req)
	if err == nil || !isContextLengthError(err) {
		return stream, err
	}
	firstErr := err
	for _, keepTail := range contextReduceStages {
		reduced, st, ok := b.reduceSessionHistory(sessionID, keepTail)
		if !ok {
			continue
		}
		b.log.Warn("orchicon: Ask turn exceeded the context window — history reduced, retrying",
			"session", sessionID,
			"keep_tail", keepTail,
			"images_dropped", st.ImagesDropped,
			"tool_results_elided", st.ToolResultsElided,
			"text_parts_truncated", st.TextPartsTruncated,
			"bytes_before", st.BytesBefore,
			"bytes_after", st.BytesAfter)
		req.Messages = reduced
		if stream, err = prov.StreamTurn(turnCtx, *req); err == nil {
			return stream, nil
		}
		if !isContextLengthError(err) {
			return nil, err
		}
	}
	// Every stage was rejected, or nothing could be reclaimed: surface the
	// ORIGINAL failure, which names the real limit.
	return nil, firstErr
}

// reduceSessionHistory applies one reduction stage to the session's history
// under the lock, persists it, and returns the reduced history. ok is false
// when there is nothing to reduce (an empty history, or a pass that reclaimed
// nothing), so the caller knows not to retry that stage.
func (b *NativeBridge) reduceSessionHistory(sessionID string, keepTail int) ([]Message, ReduceStats, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.chatHistory[sessionID]
	if len(current) == 0 {
		return nil, ReduceStats{}, false
	}
	reduced, st := ReduceConversationHistoryForContext(current, keepTail)
	if st.Reclaimed() <= 0 {
		return nil, st, false
	}
	b.chatHistory[sessionID] = reduced
	b.persistAskHistoryLocked(sessionID)
	return reduced, st, true
}
