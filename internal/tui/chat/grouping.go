// Package chat is the TUI port of the GUI's Ask Orchicon streaming semantics
// (frontend/src/routes/ask-orchicon.tsx) and the execution session pane's phase-keyed grouping.
package chat

import (
	"encoding/json"
	"sort"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ItemKind discriminates ChatItem (mirrors the TS union: user | text |
// tool | reasoning | error | artifact | session).
type ItemKind string

const (
	KindUser      ItemKind = "user"
	KindText      ItemKind = "text"
	KindTool      ItemKind = "tool"
	KindReasoning ItemKind = "reasoning"
	KindError     ItemKind = "error"
	KindArtifact  ItemKind = "artifact"
	KindSession   ItemKind = "session"
	// KindConsent is a permission ask or clarifying question rendered in the
	// transcript: a CARD while it is pending, a one-line record once decided.
	KindConsent ItemKind = "consent"
	// KindAsk is a recorded ask_user clarifying question rendered as a card in
	// the transcript (the non-blocking, recorded-tool-call path).
	KindAsk ItemKind = "ask"
	// KindNotice is a `system` row: something the PLATFORM said about the conversation (today, a record
	// that its context was compacted), not something either party said in it.
	KindNotice ItemKind = "notice"
)

// ParsedTool mirrors the TS ParsedTool interface.
type ParsedTool struct {
	ID       string
	ToolName string
	Input    string
	Output   string
	At       int64 // ms since epoch (matches the TS Date.now() usage)
}

// AskOption is one choice on a clarifying-question card.
type AskOption struct {
	Label       string
	Description string
}

// ParsedAsk is a recorded ask_user clarifying question the TUI renders as a card.
type ParsedAsk struct {
	// ID is the ASK's identity: the recorded tool call's id, which is what ReplyPermissionAsk
	// addresses.
	ID         string
	Question   string
	Options    []AskOption
	AllowOther bool
	// Answered reports that this ask_user call HAS A RESULT — the operator answered it and the answer
	// came back as the tool result.
	Answered bool
	// AnswerText is what the operator answered, read from that result, so a settled
	// card can say what was decided instead of silently going inert.
	AnswerText string

	// Refused reports that a result EXISTS BUT IS AN ERROR: the call resolved without the operator ever
	// being shown a question, so nothing was asked and nothing was answered.
	Refused bool
	// RefusalText is WHY it was refused (the tool's own validator message), so the record says what was wrong
	// rather than only that something was.
	RefusalText string

	// Drafting and Draft are the FREE-TEXT ROW's live state: the recorded-card twin of
	// ConsentState.OtherMode/OtherInput.
	Drafting bool
	Draft    string

	// Sel is the HIGHLIGHTED ROW — the recorded card's cursor, the twin of ConsentState.Sel.
	Sel int
}

// isAskUserCall reports whether a recorded tool call is the clarifying question this client renders
// (tolerating the orchicon_ MCP-style prefix the model may emit.
func isAskUserCall(functionName string) bool {
	// THE MCP-QUALIFIED NAME IS A REAL SPELLING.
	if strings.HasSuffix(functionName, "__ask_user") {
		return true
	}
	return functionName == "ask_user" || functionName == "orchicon_ask_user"
}

// It NEVER panics or errors outward: a malformed arguments payload yields a card carrying the
// question-less error text the operator can see.
func IsAwaitingAskAnswer(output string) bool {
	return strings.TrimSpace(output) == "__orchicon_ask_awaiting_answer__"
}

// results is the message's tool results: a call that has one has been answered, and its output IS
// the operator's answer.
func parseAskUserCall(calls []*apiv1.ToolCall, results []*apiv1.ToolResult) *ParsedAsk {
	for _, c := range calls {
		if c == nil || !isAskUserCall(c.GetFunctionName()) {
			continue
		}
		// THE RESULT IS LOOKED UP FIRST, because whether one EXISTS decides what an unreadable
		// argument list means. See the unmarshal failure below.
		var result *apiv1.ToolResult
		for _, r := range results {
			if r != nil && r.GetToolCallId() == c.GetId() {
				result = r
				break
			}
		}
		var in struct {
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
			AllowOther bool `json:"allow_other"`
		}
		if err := json.Unmarshal([]byte(c.GetArguments()), &in); err != nil {
			// AN OPEN CALL IS A PLACEHOLDER, NOT A CORRUPT ONE.
			if result == nil {
				return nil
			}
			// ANSWERED, because the call HAS resolved: the card must not render as an interactive question
			// inviting an answer to something already finished — nothing is waiting on it.
			if result.GetIsError() {
				return &ParsedAsk{
					ID:          c.GetId(),
					Question:    "(this clarifying question's arguments could not be read)",
					Refused:     true,
					RefusalText: strings.TrimSpace(result.GetOutput()),
				}
			}
			return &ParsedAsk{
				ID:         c.GetId(),
				Question:   "(this clarifying question's arguments could not be read)",
				Answered:   true,
				AnswerText: strings.TrimSpace(result.GetOutput()),
			}
		}
		ask := &ParsedAsk{ID: c.GetId(), Question: strings.TrimSpace(in.Question), AllowOther: in.AllowOther}
		for _, o := range in.Options {
			if strings.TrimSpace(o.Label) == "" {
				continue
			}
			ask.Options = append(ask.Options, AskOption{Label: o.Label, Description: o.Description})
		}
		// The answer IS the tool result, when there is one.
		if result != nil {
			// AN ERROR RESULT IS NOT AN ANSWER.
			if IsAwaitingAskAnswer(result.GetOutput()) {
				// leave Answered false: the operator has not replied
			} else if result.GetIsError() {
				ask.Refused = true
				ask.RefusalText = strings.TrimSpace(result.GetOutput())
			} else {
				ask.Answered = true
				ask.AnswerText = strings.TrimSpace(result.GetOutput())
			}
		}
		return ask
	}
	return nil
}

// ChatItem mirrors the TS ChatItem union as one struct: exactly the
// fields each variant carries; Kind selects the shape.
type ChatItem struct {
	Kind      ItemKind
	Text      string // user | text | reasoning | error
	Source    string // user only ("goal" | "chat" | ...)
	Tool      *ParsedTool
	Ask       *ParsedAsk // ask — a recorded clarifying question (client card)
	Name      string     // artifact
	Type      string     // artifact
	Content   string     // artifact
	SessionID string     // session
	ServeURL  string     // session
	// AdapterKind is the transport identity from the session_info part (adapter.KindOpencode /
	// adapter.KindOrchicon); empty on a legacy part written before the field existed.
	AdapterKind string // session
	At          int64  // ms since epoch
	Key         string
	// MessageID is the DURABLE message id this item came from, or empty when it came from something
	// else (a live stream fragment, a card, an artifact).
	MessageID string
	Live      bool
	Phase     string

	// Attachments are the markers for the files this message carried, in the operator's vocabulary
	// ("[image]", "[file: notes.md]").
	Attachments []string

	// AskID identifies a KindConsent item's ask, so a resolve/decision event can
	// find the row it settles rather than appending a second one.
	AskID string

	// Consent is a KindConsent item's live state (pending vs resolved, the highlight, any free text).
	Consent *ConsentState
}

// itemAt mirrors itemAt(): tool items timestamp through their tool.
func itemAt(i ChatItem) int64 {
	if i.Kind == KindTool && i.Tool != nil {
		return i.Tool.At
	}
	return i.At
}

// --- phase-keyed grouping (groupPhaseGroups / groupByPhase) -------------

// phaseGroupable is the interface subset grouping reads (the TS
// PhaseGroupable interface: kind, phase?, live?).
type phaseGroupable interface {
	phaseKey() string
	phaseLive() bool
	groupKind() string
}

func (i ChatItem) phaseKey() string  { return i.Phase }
func (i ChatItem) phaseLive() bool   { return i.Live }
func (i ChatItem) groupKind() string { return string(i.Kind) }

// itemPhase mirrors itemPhase(): explicit phase, else live/hist fallback
// so untagged history and live chunks never merge by accident.
func itemPhase(i phaseGroupable) string {
	if p := i.phaseKey(); p != "" {
		return p
	}
	if i.phaseLive() {
		return "live"
	}
	return "hist"
}

// GroupPhaseGroups is a faithful port of groupPhaseGroups: one group per (kind, phase) run, sealed
// on boundary items and on phase changes, in first-appearance order.
func GroupPhaseGroups(items []ChatItem, absorb map[string]bool) [][]ChatItem {
	if absorb == nil {
		absorb = map[string]bool{}
	}
	out := [][]ChatItem{}
	var text, reasoning []ChatItem
	var order []string // "text" | "reasoning", first-appearance
	phase := ""

	flush := func() {
		for _, k := range order {
			if k == "text" {
				out = append(out, text)
			} else {
				out = append(out, reasoning)
			}
		}
		text, reasoning, order, phase = nil, nil, nil, ""
	}

	for _, item := range items {
		p := itemPhase(item)
		switch {
		case item.Kind == KindText:
			if phase != "" && p != phase {
				flush()
			}
			if phase == "" {
				phase = p
			}
			if !containsStr(order, "text") {
				order = append(order, "text")
			}
			text = append(text, item)
		case item.Kind == KindReasoning:
			if phase != "" && p != phase {
				flush()
			}
			if phase == "" {
				phase = p
			}
			if !containsStr(order, "reasoning") {
				order = append(order, "reasoning")
			}
			reasoning = append(reasoning, item)
		case absorb[string(item.Kind)]:
			if len(text) > 0 {
				text = append(text, item)
			} else {
				out = append(out, []ChatItem{item})
			}
		default:
			flush()
			out = append(out, []ChatItem{item})
		}
	}
	flush()
	return out
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// GroupByPhase is the port of groupByPhase: merge each (kind, phase) group into one growing
// ChatItem — text concatenated, At from the last chunk, Key from the first.
func GroupByPhase(items []ChatItem) []ChatItem {
	groups := GroupPhaseGroups(items, nil)
	out := make([]ChatItem, 0, len(groups))
	for _, g := range groups {
		if len(g) == 0 {
			continue
		}
		if len(g) == 1 {
			out = append(out, g[0])
			continue
		}
		first := g[0]
		if first.Kind != KindText && first.Kind != KindReasoning {
			out = append(out, first)
			continue
		}
		var b []byte
		for _, t := range g {
			b = append(b, t.Text...)
		}
		merged := first
		merged.Text = string(b)
		merged.At = g[len(g)-1].At
		merged.Live = false
		for _, t := range g {
			if t.Live {
				merged.Live = true
				break
			}
		}
		merged.Phase = first.Phase
		out = append(out, merged)
	}
	return out
}

// MergeSessionItems is the port of mergeSessionItems: durable transcript history + live event
// stream for a RUNNING execution.
func MergeSessionItems(history, live []ChatItem) []ChatItem {
	lastHistoryAt := int64(0)
	for _, i := range history {
		if at := itemAt(i); at > lastHistoryAt {
			lastHistoryAt = at
		}
	}
	fresh := make([]ChatItem, 0, len(live))
	for _, i := range live {
		if itemAt(i) >= lastHistoryAt {
			fresh = append(fresh, i)
		}
	}
	merged := append(append([]ChatItem{}, history...), fresh...)
	SortChronologically(merged)
	return GroupByPhase(merged)
}

// SortChronologically orders items oldest-first by their effective timestamp (a tool item
// timestamps through its tool).
func PendingCardsLast(items []ChatItem) []ChatItem {
	cards := 0
	for _, it := range items {
		if pendingCard(it) {
			cards++
		}
	}
	if cards == 0 {
		return items
	}
	rest := make([]ChatItem, 0, len(items))
	tail := make([]ChatItem, 0, cards)
	for _, it := range items {
		if pendingCard(it) {
			tail = append(tail, it)
			continue
		}
		rest = append(rest, it)
	}
	return append(rest, tail...)
}

// pendingCard reports whether this item is an ask still waiting on the operator: a consent card
// whose decision has not been made.
func pendingCard(it ChatItem) bool {
	return it.Kind == KindConsent && it.Consent != nil && it.Consent.Pending()
}

func SortChronologically(items []ChatItem) {
	sort.SliceStable(items, func(a, b int) bool {
		return itemAt(items[a]) < itemAt(items[b])
	})
}
