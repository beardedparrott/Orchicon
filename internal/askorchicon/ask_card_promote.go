package askorchicon

// ask_card_promote.go — RECOGNISE AN ASK FROM ITS RESULT, NOT FROM THE CALL'S NAME.
//
// REPORTED: "I tried an ask conversation with both opencode and claude adapters. Both say they sent a
// card, but no actual card appeared. I know ask cards used to work."
//
// Both halves were true and they are consistent: nothing in this pipeline regressed — the adapters'
// CALLING STYLE did. Both clients render a question card from the TRANSCRIPT, matching a tool call named
// ask_user/orchicon_ask_user, which is exactly what the native bridge records. The MCP-served adapters
// never record that name. Measured in this deployment's own transcript table:
//
//	opencode  function_name "execute", the call inside a JavaScript body —
//	          const r = await tools.orchicon.ask_user({ question: "…", options: […] });
//	claude    function_name "mcp__orchicon__ask_user", the MCP-qualified spelling.
//
// The ask DID execute — opencode's result is {"recorded": true, "question": …, "options": […]} — which
// is why the model truthfully reported having sent a card. Nothing surfaced it.
//
// So detection reads the RESULT, which is the tool's own declaration that an ask was recorded and which
// carries every field the card needs. That makes ONE rule cover every adapter and every nesting depth.

import (
	"encoding/json"
	"strings"
)

// askUserResultArgument is the tool's own acknowledgement, and the anchor for detection.
const askUserResultArgument = "recorded"

// AskAwaitingAnswerMarker is the output of a promoted ask's result while it is UNANSWERED.
//
// A PROMOTED ASK MUST HAVE A RESULT, and this is the crux of two reports that looked unrelated:
//
//	opencode  "the card goes away before I can click on it and it says 'You answered … — tool call
//	           aborted — the turn ended before this tool returned a result'"
//	claude    "Question … was dismissed — no answer sent"
//
// Both are the ledger repairing a call that has no result: sanitizeAssistantToolLedger appends
// abortedToolResultOutput to EVERY unresolved call at turn end, because a provider rejects an assistant
// tool_calls message whose ids have no results. So an ask that is legitimately still open was written
// off as aborted, and the clients — which settle a card as soon as its call HAS a result — drew it
// settled. The card existed; the repair killed it.
//
// The promoted call therefore carries a result IMMEDIATELY, so nothing needs repairing, but that result
// says "no answer yet". The clients read the marker as STILL OPEN, and the reply replaces it with the
// operator's words.
const AskAwaitingAnswerMarker = "__orchicon_ask_awaiting_answer__"

// IsAwaitingAnswer reports whether a recorded ask result means "asked, not yet answered".
func IsAwaitingAnswer(output string) bool {
	return strings.TrimSpace(output) == AskAwaitingAnswerMarker
}

// promotedAskArgs returns the ask arguments carried by a tool RESULT, or "" when it is not a recorded ask.
//
// It requires all three of: `recorded: true`, a non-empty `question`, and the result parsing as the
// tool's documented shape. A model quoting the tool's description, or an error mentioning it, cannot
// satisfy that — which is why the anchor is the payload rather than a substring search for the name.
func promotedAskArgs(output string) string {
	s := strings.TrimSpace(output)
	if s == "" || !strings.Contains(s, askUserResultArgument) {
		return ""
	}
	for _, doc := range jsonCandidates(s) {
		if args := askArgsFromJSON(doc); args != "" {
			return args
		}
	}
	return ""
}

// askArgsFromJSON decodes one JSON document, RECURSING INTO STRING VALUES.
//
// THE RECURSION IS THE POINT. opencode's sandbox returns its tool output WRAPPED — the ask sits as an
// escaped STRING inside an array:
//
//	[{"text":"{\\n  \\"recorded\\": true, …}","type":"text"}]
//
// A bracket scanner that respects strings will not see inside that (correctly — it IS one string), so
// the first version of this found nothing against the real payload.
func askArgsFromJSON(doc string) string {
	var generic any
	if err := json.Unmarshal([]byte(doc), &generic); err != nil {
		return ""
	}
	return askArgsFromValue(generic, 0)
}

// askArgsFromValue walks a decoded value looking for the recorded-ask payload. depth bounds the
// recursion: real wrappers nest one or two levels.
func askArgsFromValue(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch val := v.(type) {
	case map[string]any:
		if recorded, _ := val["recorded"].(bool); recorded {
			if q, _ := val["question"].(string); strings.TrimSpace(q) != "" {
				// Rebuild STRICTLY: only the fields the card parses travel on, so nothing the model
				// wrote outside the tool's contract leaks into the rendered card.
				out, err := json.Marshal(struct {
					Question   any  `json:"question"`
					Options    any  `json:"options"`
					AllowOther bool `json:"allow_other"`
				}{val["question"], val["options"], boolOf(val["allow_other"])})
				if err == nil {
					return string(out)
				}
			}
		}
		for _, inner := range val {
			if got := askArgsFromValue(inner, depth+1); got != "" {
				return got
			}
		}
	case []any:
		for _, inner := range val {
			if got := askArgsFromValue(inner, depth+1); got != "" {
				return got
			}
		}
	case string:
		trimmed := strings.TrimSpace(val)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			if got := askArgsFromJSON(trimmed); got != "" {
				return got
			}
		}
	}
	return ""
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// jsonCandidates yields every embedded JSON document in s, outermost first.
//
// Bounded on purpose: a tool output is capped before it reaches here (truncateLedgerString, 8000 bytes),
// and this walks at most that many characters a fixed number of times.
func jsonCandidates(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		if doc := balancedJSON(s[i:]); doc != "" {
			out = append(out, doc)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

// balancedJSON returns the JSON document starting at s[0] (a '{' or '['), or "" when there is none.
func balancedJSON(s string) string {
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}
