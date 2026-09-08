// Package chat is the TUI port of the GUI's Ask Orchicon streaming
// semantics (frontend/src/routes/ask-orchicon.tsx) and the execution
// session pane's phase-keyed grouping
// (frontend/src/components/executions/sessionItems.ts). The grouping
// helpers here are a faithful 1:1 port of sessionItems.ts so terminal
// bubbles converge with the GUI's: one growing text bubble and one
// growing reasoning bubble per phase, interleaving preserved in
// first-appearance order.
package chat

import "sort"

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
)

// ParsedTool mirrors the TS ParsedTool interface.
type ParsedTool struct {
	ID       string
	ToolName string
	Input    string
	Output   string
	At       int64 // ms since epoch (matches the TS Date.now() usage)
}

// ChatItem mirrors the TS ChatItem union as one struct: exactly the
// fields each variant carries; Kind selects the shape.
type ChatItem struct {
	Kind      ItemKind
	Text      string // user | text | reasoning | error
	Source    string // user only ("goal" | "chat" | ...)
	Tool      *ParsedTool
	Name      string // artifact
	Type      string // artifact
	Content   string // artifact
	SessionID string // session
	ServeURL  string // session
	At        int64  // ms since epoch
	Key       string
	Live      bool
	Phase     string
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
	sort.SliceStable(merged, func(a, b int) bool {
		return itemAt(merged[a]) < itemAt(merged[b])
	})
	return GroupByPhase(merged)
}
