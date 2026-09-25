// Package chat is the TUI port of the GUI's Ask Orchicon streaming
// semantics (frontend/src/routes/ask-orchicon.tsx) and the execution
// session pane's phase-keyed grouping
// (frontend/src/components/executions/sessionItems.ts). The grouping
// helpers here are a faithful 1:1 port of sessionItems.ts so terminal
// bubbles converge with the GUI's: one growing text bubble and one
// growing reasoning bubble per phase, interleaving preserved in
// first-appearance order.
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
//
// IT IS A RECORD, NOT A PROMPT. The tool RECORDED the question and the turn
// ENDED; the operator's answer is sent as the NEXT user message
// (Controller.AnswerQuestion delegates to Send) — there is no blocking rendezvous
// here, and this shape must not grow one. The CONSENT ask is genuinely BLOCKING
// on the transport and must not reuse this non-blocking model, even though it
// shares the card rendering.
type ParsedAsk struct {
	Question   string
	Options    []AskOption
	AllowOther bool
}

// isAskUserCall reports whether a recorded tool call is the clarifying question
// this client renders (tolerating the orchicon_ MCP-style prefix the model may
// emit — the native registry is keyed bare, the opencode path prefixed).
func isAskUserCall(functionName string) bool {
	return functionName == "ask_user" || functionName == "orchicon_ask_user"
}

// parseAskUserCall parses the FIRST recorded ask_user call on a message into a
// ParsedAsk. It NEVER panics or errors outward: a malformed arguments payload
// yields a card carrying the question-less error text the operator can see,
// rather than dropping the call or crashing the pane.
func parseAskUserCall(calls []*apiv1.ToolCall) *ParsedAsk {
	for _, c := range calls {
		if c == nil || !isAskUserCall(c.GetFunctionName()) {
			continue
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
			return &ParsedAsk{Question: "(this clarifying question's arguments could not be read)"}
		}
		ask := &ParsedAsk{Question: strings.TrimSpace(in.Question), AllowOther: in.AllowOther}
		for _, o := range in.Options {
			if strings.TrimSpace(o.Label) == "" {
				continue
			}
			ask.Options = append(ask.Options, AskOption{Label: o.Label, Description: o.Description})
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
	// AdapterKind is the transport identity from the session_info part
	// (adapter.KindOpencode / adapter.KindOrchicon); empty on a legacy part
	// written before the field existed.
	AdapterKind string // session
	At          int64  // ms since epoch
	Key         string
	Live        bool
	Phase       string

	// Attachments are the markers for the files this message carried, in the operator's vocabulary
	// ("[image]", "[file: notes.md]"). They are a DISPLAY field: the bytes belong to the request that sent
	// them, and the transcript only needs to say the turn was not text alone.
	//
	// It is separate from Text rather than prefixed onto it because the two have different owners — Text is
	// the operator's own words, which the durable transcript also carries and the dedupe matches on, while
	// these markers exist only for this client's rendering. Folding them into Text would put a synthetic
	// token into the message the server stores and the dedupe compares.
	Attachments []string

	// AskID identifies a KindConsent item's ask, so a resolve/decision event can
	// find the row it settles rather than appending a second one.
	AskID string

	// Consent is a KindConsent item's live state (pending vs resolved, the
	// highlight, any free text). It is a POINTER so the screen that handles the
	// card's keys and the renderer that draws it share ONE object; it is mutated
	// only on the tea update loop.
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

// GroupPhaseGroups is a faithful port of groupPhaseGroups: one group per
// (kind, phase) run, sealed on boundary items and on phase changes, in
// first-appearance order. `absorb` names kinds that do not seal the
// phase — they append to the open text group when one exists (artifacts
// render inline inside the assistant bubble), else stand alone.
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

// GroupByPhase is the port of groupByPhase: merge each (kind, phase)
// group into one growing ChatItem — text concatenated, At from the last
// chunk, Key from the first, Live true when any chunk is live. Boundary
// items pass through unchanged. Empty input yields nil.
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

// MergeSessionItems is the port of mergeSessionItems: durable transcript
// history + live event stream for a RUNNING execution — history plus
// only the live events not yet reflected in it (covered chunks dropped),
// sorted by timestamp, grouped per phase into single growing bubbles.
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

// SortChronologically orders items oldest-first by their effective timestamp
// (a tool item timestamps through its tool). The sort is STABLE, so rows that
// share a timestamp keep their input order.
//
// Callers MERGE two streams that each arrive in order — durable history and a
// live buffer — and concatenating them puts every live row at the END, which
// renders a just-sent user message BELOW the model's reply. Sorting the
// combined list restores true chronological order whatever the source.
func SortChronologically(items []ChatItem) {
	sort.SliceStable(items, func(a, b int) bool {
		return itemAt(items[a]) < itemAt(items[b])
	})
}
