package askorchicon

// ask_card_promote.go — RECOGNISE AN ASK FROM ITS RESULT, NOT FROM THE CALL'S NAME.
//
// REPORTED: "I tried an ask conversation with both opencode and claude adapters. Both say they sent a
// card, but no actual card appeared." The reporter added the decisive clue — "I know ask cards used to
// work" — and it was right: nothing in this pipeline regressed. The MODELS' CALLING STYLE did.
//
// Both clients render a question card from the TRANSCRIPT: they look for a persisted tool call whose
// function name is `ask_user` or `orchicon_ask_user` (GUI: isAskUserToolCall in AskCard.tsx; TUI: the
// same predicate in internal/tui/chat/grouping.go). That works on the native bridge, which calls the
// tool directly.
//
// THE MCP ADAPTERS REACH IT INDIRECTLY, and the recorded name is never the ask's:
//
//	opencode  function_name "execute" with the call inside a JavaScript body —
//	          const r = await tools.orchicon.ask_user({ question: "…", options: […] });
//	claude    the MCP-qualified name mcp__orchicon__ask_user, which the predicate does not accept.
//
// Measured in this deployment's own transcript table: the opencode attempt that "sent a card" was
// recorded as ONE tool call, function_name "execute", and its result contains
//
//	{"recorded": true, "question": "Ask-card test: …", "options": [ … ], "allow_other": true}
//
// So the ask DID execute — the model was not hallucinating — and nothing ever surfaced it.
//
// THE FIX READS THE RESULT. The tool's return value is its own declaration that an ask was recorded,
// and it carries every field the card needs. Detecting on that instead of on the caller's name is
// what makes one change cover every adapter and every nesting depth: opencode's JS sandbox, claude's
// MCP-qualified name, a future adapter that wraps tools differently, and the native direct call all
// produce the same result payload. Parsing JavaScript to recover the arguments would be fragile and
// adapter-specific; the result is already structured JSON.

import (
	"encoding/json"
	"strings"
)

// askUserResultArgument is the tool's own acknowledgement, and the anchor for detection.
const askUserResultArgument = "recorded"

// promotedAskArgs returns the ask arguments carried by a tool RESULT, or "" when the result is not a
// recorded ask.
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
// THE RECURSION IS THE POINT, not a nicety. opencode's sandbox returns its tool output WRAPPED — the
// ask sits as an escaped STRING inside an array:
//
//	[{"text":"{\n  \"recorded\": true, …}","type":"text"}]
//
// A bracket scanner that respects strings will not see inside that (correctly — it IS one string), so
// the first version of this found nothing against the real payload. Trying each string value as JSON
// reaches it without a special case per wrapper dialect.
func askArgsFromJSON(doc string) string {
	var generic any
	if err := json.Unmarshal([]byte(doc), &generic); err != nil {
		return ""
	}
	return askArgsFromValue(generic, 0)
}

// askArgsFromValue walks a decoded value looking for the recorded-ask payload. depth bounds the
// recursion: real wrappers nest one or two levels, and a bound keeps a pathological payload cheap.
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
		// A wrapped payload: the string IS JSON. Try it.
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

// jsonCandidates yields every embedded JSON document in s, outermost first, without a parser for the
// caller's wrapper dialect: each '{' or '[' starts a candidate that must both begin and end a valid
// document.
//
// Bounded on purpose. A tool output is capped before it reaches here (truncateLedgerString, 8000
// bytes), and this walks at most that many characters a fixed number of times, so a pathological
// output cannot turn into quadratic work.
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
//
// It counts brackets and respects strings/escapes, which is all that is needed to find the end of an
// embedded document. It does NOT validate the contents — the caller unmarshals, which is the real
// check.
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
