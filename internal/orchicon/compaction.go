package orchicon

// compaction.go is the NATIVE guarded compaction engine (D1). It fires
// ONLY on (a) approaching the model's TRUE context window (resolved via
// live hints only — see contextwindow.go) or (b) the shared budget-gate
// ladder (evaluated through the opencode budget facade over the SAME
// merged budget JSON the adapters parse). It NEVER fires on token count
// alone when no window hint exists, never guesses a window, and never
// uses estimated/interpolated usage.
//
// The compaction POLICY is middle-only eviction of OLD TOOL ROUNDS (an
// assistant tool_use with its paired tool_result — originals disk-offloaded),
// with the pinned goal head (the eviction digest marker is merged INTO the
// head user message — never a standalone user message, which would break the
// Anthropic role-alternation rule), assistant text and non-tool user turns
// untouched, and the recent tail kept verbatim — so no orphaned tool_use (or
// orphaned tool_result) ever remains in the marshaled history. Eviction is
// plan-then-offload-then-commit: s.history is replaced only after the
// originals are safely offloaded, so a failed offload never loses data from
// the live session. At most once per threshold band (anti-loop latch), with a
// min-turn floor and a per-execution cap.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// Default compaction policy knobs (env-tunable).
const (
	// defaultWindowPressureFrac is the fraction of the model's true
	// context window at which window compaction arms.
	defaultWindowPressureFrac = 0.8
	// defaultRecentTurns is how many trailing messages stay verbatim.
	defaultRecentTurns = 6
	// defaultCompactionBands divides the window into bands for the
	// at-most-once-per-band latch.
	defaultCompactionBands = 4
)

// CompactPolicy is the session's compaction configuration. Populated from
// tenant/worker settings (merged BudgetJSON keys) with built-in defaults.
type CompactPolicy struct {
	// Enabled is the master switch (context_compaction.enabled).
	Enabled bool
	// PressureFrac is the window-pressure trigger threshold.
	PressureFrac float64
	// RecentTurns keeps this many trailing messages verbatim.
	RecentTurns int
}

// DefaultCompactPolicy returns the built-in policy (used when the merged
// settings JSON carries no context_compaction keys).
func DefaultCompactPolicy() CompactPolicy {
	return CompactPolicy{Enabled: true, PressureFrac: defaultWindowPressureFrac, RecentTurns: defaultRecentTurns}
}

// compactState latches compaction progress on the session.
type compactState struct {
	mu sync.Mutex

	lastCompactStep int  // step of the last compaction (0 = never)
	compactions     int  // per-execution compaction count
	armed           bool // window trigger armed (only when a live hint resolved)
	hint            ContextWindowHint
	budget          *opencode.BudgetLadder
	spend           *opencode.BudgetSpend
	lastBand        int    // last fired window-pressure band
	lastBudgetTier  string // last fired budget tier (dim:tier)
}

// MemoryPolicy is the session's memory settings (D3/D4).
type MemoryPolicy struct {
	Enabled       bool
	DigestEntries int
}

// DefaultMemoryPolicy returns the built-in memory policy.
func DefaultMemoryPolicy() MemoryPolicy {
	return MemoryPolicy{Enabled: true, DigestEntries: 5}
}

// policyFromSettings parses the compaction/memory policy from the merged
// budget JSON's context_compaction / memory keys (D4: these ride the
// existing BudgetLadder transport, so tenant defaults + per-worker
// overrides already layer through mergeBudgets). Unknown/malformed keys
// fall back to the built-in defaults — never a guessed value.
func policyFromSettings(budgetJSON []byte) (CompactPolicy, MemoryPolicy) {
	cp := DefaultCompactPolicy()
	mp := DefaultMemoryPolicy()
	if len(budgetJSON) == 0 {
		return cp, mp
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(budgetJSON, &raw); err != nil {
		return cp, mp
	}
	if ccRaw, ok := raw["context_compaction"]; ok {
		var cc struct {
			Enabled      *bool    `json:"enabled"`
			PressureFrac *float64 `json:"pressure_frac"`
			RecentTurns  *int     `json:"recent_turns"`
		}
		if err := json.Unmarshal(ccRaw, &cc); err == nil {
			if cc.Enabled != nil {
				cp.Enabled = *cc.Enabled
			}
			if cc.PressureFrac != nil && *cc.PressureFrac > 0 && *cc.PressureFrac <= 1 {
				cp.PressureFrac = *cc.PressureFrac
			}
			if cc.RecentTurns != nil && *cc.RecentTurns > 0 {
				cp.RecentTurns = *cc.RecentTurns
			}
		}
	}
	if mRaw, ok := raw["memory"]; ok {
		var m struct {
			Enabled       *bool `json:"enabled"`
			DigestEntries *int  `json:"digest_entries"`
		}
		if err := json.Unmarshal(mRaw, &m); err == nil {
			if m.Enabled != nil {
				mp.Enabled = *m.Enabled
			}
			if m.DigestEntries != nil && *m.DigestEntries > 0 {
				mp.DigestEntries = *m.DigestEntries
			}
		}
	}
	return cp, mp
}

// maybeCompact evaluates the two guarded triggers at a quiet boundary
// (after a tool round) and fires compactPolicy when a trigger is armed.
// usage is this turn's LIVE provider-reported usage. Returns true when a
// compaction ran.
func (s *Session) maybeCompact(ctx context.Context, step int, usage Usage) bool {
	s.cs.mu.Lock()
	defer s.cs.mu.Unlock()
	if !s.cp.Enabled || s.cs.budget == nil {
		return false
	}
	if step < 2 {
		return false // min-turn floor: never compact at session start
	}
	maxC := s.cs.budget.CompactionMax()
	if maxC == 0 || s.cs.compactions >= maxC {
		return false // per-execution cap reached
	}

	// Live usage only: fold this turn's REAL provider-reported usage into
	// the shared accumulator (never an estimate). The cost figure is the
	// LIVE provider-priced cost the wire client resolved from the model's
	// catalog/probe pricing (0 when the model has no pricing — the cost
	// dimension then never fires, never a synthesized estimate).
	s.cs.spend.AddFromUsage(usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CacheReadTokens, usage.CostUSD)

	// Trigger (a): true context-window pressure (armed only when a live
	// hint resolved — no hint means the window trigger is inert). The
	// pressure figure is THIS TURN's LIVE provider-reported occupancy —
	// the input+cache-read tokens of the request just sent — never the
	// cumulative rollup (which re-sums the whole re-sent prefix every turn
	// and would over-count by ~turn count). This keeps the comparison
	// honest: a session whose individual requests never approach the window
	// never fires, and one request that genuinely approaches it fires once.
	hint := s.resolveContextWindow(ctx)
	if hint.Ok && hint.Tokens > 0 {
		s.cs.armed = true
		s.cs.hint = hint
		occupancy := usage.InputTokens + usage.CacheReadTokens
		if occupancy <= 0 {
			return false
		}
		frac := float64(occupancy) / float64(hint.Tokens)
		if frac >= s.cp.PressureFrac {
			band := int(frac * defaultCompactionBands)
			if band > s.cs.lastBand {
				s.cs.lastBand = band
				return s.doCompact(step, "window_pressure")
			}
		}
	}

	// Trigger (b): the budget gate (shared ladder over the merged JSON).
	// Escalate/final tiers compact when the tier AND dimension permit it;
	// abort is terminal and is NOT a compaction. Fires at most once per
	// reached tier.
	for _, dim := range []string{"tokens", "cost_usd"} {
		if !s.cs.budget.CompactsDim(dim) {
			continue
		}
		frac := s.cs.spend.Fraction(s.cs.budget, dim, time.Since(s.startedAt), s.toolUses)
		if frac < 0 {
			continue
		}
		for _, tier := range []string{"escalate", "final"} {
			if !s.cs.budget.CompactsAt(tier) {
				continue
			}
			lvl := s.cs.budget.LevelName(dim, frac)
			if lvl != tier {
				continue
			}
			key := dim + ":" + tier
			if s.cs.lastBudgetTier == key {
				continue // already fired on this tier
			}
			s.cs.lastBudgetTier = key
			return s.doCompact(step, "budget:"+key)
		}
	}
	return false
}

// doCompact applies the middle-only compaction policy to history.
// Caller holds s.cs.mu.
func (s *Session) doCompact(step int, reason string) bool {
	if s.cs.lastCompactStep == step {
		return false // same-turn repeat
	}
	if s.cs.lastCompactStep > 0 && step-s.cs.lastCompactStep < s.cs.budget.CompactionTurnFloor() {
		return false // min-turn floor between compactions
	}
	// Plan-then-offload-then-commit (QA bug B): planning computes the
	// eviction WITHOUT mutating s.history; the offload (the originals
	// store) runs BEFORE the replacement history is committed, so a failed
	// offload leaves the live session byte-identical — the evicted
	// originals are never lost from history.
	evicted, replacement := s.planMiddleEviction()
	if len(evicted) == 0 || replacement == nil {
		return false // nothing to evict — never compact for show
	}
	if err := s.offloadToolResults(evicted); err != nil {
		s.log.Warn("compaction: offload failed — skipping eviction", "execution", s.id, "error", err)
		return false
	}
	s.history = replacement
	s.cs.lastCompactStep = step
	s.cs.compactions++
	s.cs.armed = false // re-arm only after forward progress (next step)
	return true
}

// evictedToolResult is one offloaded original.
type evictedToolResult struct {
	Seq        int    `json:"seq"`
	Turn       int    `json:"turn"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
	Args       string `json:"args"`
	Output     string `json:"output"`
	EvictedAt  string `json:"evicted_at"`
}

// planMiddleEviction computes the middle-only tool-round eviction WITHOUT
// mutating s.history (QA bug B): it returns the evicted originals for
// offload and the replacement history to commit AFTER the offload succeeds,
// so a failed offload leaves the live session byte-identical — the evicted
// originals are never lost from history. It removes an assistant tool_use
// and its paired tool-result message TOGETHER as a unit, keeping the pinned
// goal head, every non-tool message, and the recent tail verbatim. Evicting
// the pair (never a result alone, never a tool_use alone) preserves the
// provider contract: every assistant tool_use that REMAINS has a matching
// tool_result, so the marshaled next request is valid for Anthropic and
// OpenAI. The digest marker is merged into the pinned head user message
// (never a standalone user message — QA bug C: consecutive plain-text user
// messages break the Anthropic role-alternation rule). replacement is nil
// when nothing is evictable.
func (s *Session) planMiddleEviction() ([]evictedToolResult, []Message) {
	keep := s.cp.RecentTurns
	if keep < 1 {
		keep = 1
	}
	n := len(s.history)
	if n <= keep+1 {
		return nil, nil
	}
	// Pinned head: the first user message (the goal).
	head := -1
	for i, m := range s.history {
		if m.Role == RoleUser {
			head = i
			break
		}
	}
	if head < 0 {
		head = 0
	}
	tailStart := n - keep
	if tailStart <= head+1 {
		tailStart = head + 2
	}
	if tailStart >= n || tailStart-head <= 1 {
		return nil, nil
	}

	// resultMsgIndex maps each tool_call_id to the history index of its
	// RoleTool result message. An assistant tool_use ALWAYS precedes its
	// result message, so when the result message sits in the evictable
	// middle (head < index < tailStart) its paired tool_use is also in the
	// middle — the whole round is evictable as a unit. A result that landed
	// in the pinned tail keeps its tool_use (evicting the use would orphan
	// the kept result).
	resultMsgIndex := map[string]int{}
	for i, m := range s.history {
		if m.Role != RoleTool {
			continue
		}
		for _, c := range m.Content {
			if c.ToolResult != nil {
				resultMsgIndex[c.ToolResult.ToolCallID] = i
			}
		}
	}

	// toolUseInTail marks a tool_call_id whose tool_use message sits in the
	// pinned tail — its result message (which precedes it) must never be
	// evicted, or the tail's tool_use would be orphaned.
	toolUseInTail := map[string]bool{}
	for i := tailStart; i < n; i++ {
		for _, c := range s.history[i].Content {
			if c.ToolUse != nil {
				toolUseInTail[c.ToolUse.ToolCallID] = true
			}
		}
	}

	// dropMsg[i] = true when history[i] is removed entirely.
	dropMsg := make([]bool, n)
	// Evictable middle tool results: offload each original and drop the
	// result message. A result message is skipped (kept verbatim) when its
	// tool_use lives in the pinned tail.
	var evicted []evictedToolResult
	seq := 0
	for i := head + 1; i < tailStart; i++ {
		m := s.history[i]
		if m.Role != RoleTool {
			continue
		}
		skip := false
		for _, c := range m.Content {
			if c.ToolResult != nil && toolUseInTail[c.ToolResult.ToolCallID] {
				skip = true // tail tool_use depends on this result — keep it
			}
		}
		if skip {
			continue
		}
		dropMsg[i] = true
		for _, c := range m.Content {
			if c.ToolResult == nil {
				continue
			}
			seq++
			name, args := "", ""
			if j := toolCallByID(s.history, c.ToolResult.ToolCallID); j != nil {
				name, args = j.Name, j.ArgsJSON
			}
			evicted = append(evicted, evictedToolResult{
				Seq:        seq,
				Turn:       i,
				ToolCallID: c.ToolResult.ToolCallID,
				Tool:       name,
				Args:       args,
				Output:     c.ToolResult.Content,
				EvictedAt:  time.Now().UTC().Format(time.RFC3339),
			})
		}
	}
	if len(evicted) == 0 {
		return nil, nil
	}

	// Plan the paired assistant tool_use removal: a middle assistant
	// message's tool_use whose result message was dropped above is removed
	// with it. A message that had text or surviving tool_use blocks keeps
	// them; a pure tool_use message whose every call was evicted is dropped
	// wholesale (no orphaned tool_use remains either way). Trimming is
	// staged in `trimmed` copies — s.history is not mutated (QA bug B).
	trimmed := map[int]Message{}
	for i := head + 1; i < tailStart; i++ {
		m := s.history[i]
		if m.Role != RoleAssistant || dropMsg[i] {
			continue
		}
		kept := make([]Content, 0, len(m.Content))
		droppedAny := false
		for _, c := range m.Content {
			if c.ToolUse != nil && dropMsg[resultMsgIndex[c.ToolUse.ToolCallID]] {
				droppedAny = true
				continue // evicted with its result
			}
			kept = append(kept, c)
		}
		if droppedAny && len(kept) == 0 {
			dropMsg[i] = true // pure tool_use message, fully evicted
		} else if droppedAny {
			// Text and/or surviving calls stay verbatim — but only as a
			// COPY staged for the replacement; s.history is not touched
			// until the offload commits (QA bug B).
			m.Content = kept
			trimmed[i] = m
		}
	}

	// Build the replacement: surviving messages (head + kept middle, with
	// the digest marker MERGED INTO the pinned head user message) + recent
	// tail verbatim. The head merge (never a standalone user message) keeps
	// the marshaled history role-alternating — QA bug C.
	markerText := s.offloadDigestMarker(evicted)
	out := make([]Message, 0, n)
	for i := 0; i < tailStart; i++ {
		if dropMsg[i] {
			continue
		}
		m := s.history[i]
		if tm, ok := trimmed[i]; ok {
			m = tm
		}
		if i == head && markerText != "" {
			m = appendMarkerToHead(m, markerText)
		}
		out = append(out, m)
	}
	for i := tailStart; i < n; i++ {
		out = append(out, s.history[i])
	}
	return evicted, out
}

// appendMarkerToHead merges the compaction digest-marker text into the
// pinned head user message (its first text block) instead of injecting a
// standalone user message. The marker tells the model what was evicted and
// how to re-read an original; riding the goal's user message keeps the
// marshaled history strictly role-alternating (a second consecutive
// plain-text user message is rejected by the Anthropic Messages API).
func appendMarkerToHead(m Message, markerText string) Message {
	// Copy the content slice so the merged text never writes through to
	// s.history's shared backing array — planning must stay pure until the
	// offload commits (QA bug B: a failed offload must leave history
	// byte-identical).
	content := make([]Content, len(m.Content))
	copy(content, m.Content)
	for i := range content {
		if content[i].Text != nil {
			merged := *content[i].Text + "\n\n" + markerText
			content[i].Text = &merged
			m.Content = content
			return m
		}
	}
	m.Content = append(content, Content{Text: &markerText})
	return m
}

// offloadDigestMarker renders the injected marker line for the evicted
// tool results: one line per tool with its name + the offload path, so
// the model can re-read a specific original on demand.
func (s *Session) offloadDigestMarker(evicted []evictedToolResult) string {
	var sb strings.Builder
	sb.WriteString("Compacted old tool results (originals offloaded; re-read the file at the given path + seq if needed):\n")
	for _, e := range evicted {
		fmt.Fprintf(&sb, "- tool %s seq %d → %s\n", e.Tool, e.Seq, s.offloadPath())
	}
	return strings.TrimRight(sb.String(), "\n")
}

// toolCallByID finds the assistant tool_use for a tool_call_id in history.
func toolCallByID(hist []Message, id string) *ContentToolUse {
	for _, m := range hist {
		for _, c := range m.Content {
			if c.ToolUse != nil && c.ToolUse.ToolCallID == id {
				return c.ToolUse
			}
		}
	}
	return nil
}

// offloadPath returns the session's offload file path.
func (s *Session) offloadPath() string {
	return filepath.Join(s.projectDir, ".orchicon", "offload", s.id+".jsonl")
}

// offloadToolResults appends the evicted originals to the offload file
// (one JSON object per line) before history mutation. The transcript is
// NOT rewritten — the offload file is the originals store.
func (s *Session) offloadToolResults(evicted []evictedToolResult) error {
	dir := filepath.Dir(s.offloadPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.offloadPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, e := range evicted {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return w.Flush()
}
