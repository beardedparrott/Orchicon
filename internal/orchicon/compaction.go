package orchicon

// compaction.go is the NATIVE guarded compaction engine (D1). It fires
// ONLY on (a) approaching the model's TRUE context window (resolved via
// live hints only — see contextwindow.go) or (b) the shared budget-gate
// ladder (evaluated through the opencode budget facade over the SAME
// merged budget JSON the adapters parse). It NEVER fires on token count
// alone when no window hint exists, never guesses a window, and never
// uses estimated/interpolated usage.
//
// The compaction POLICY is middle-only eviction of OLD TOOL RESULTS
// (originals disk-offloaded), with the pinned goal head, assistant
// tool_use/reasoning and user turns untouched, and the recent tail kept
// verbatim. At most once per threshold band (anti-loop latch), with a
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
	// the shared accumulator (never an estimate).
	s.cs.spend.AddFromUsage(usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.CacheReadTokens, 0)

	// Trigger (a): true context-window pressure (armed only when a live
	// hint resolved — no hint means the window trigger is inert).
	hint := s.resolveContextWindow(ctx)
	if hint.Ok && hint.Tokens > 0 {
		s.cs.armed = true
		s.cs.hint = hint
		// The pressure figure is the LIVE accumulated re-sent prefix the
		// provider reported each turn (input + cache reads).
		s.cacheMu.Lock()
		input := s.cacheStats.InputTokens + s.cacheStats.CacheReadTokens
		s.cacheMu.Unlock()
		frac := float64(input) / float64(hint.Tokens)
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
	evicted := s.evictMiddleToolResults()
	if len(evicted) == 0 {
		return false // nothing to evict — never compact for show
	}
	if err := s.offloadToolResults(evicted); err != nil {
		s.log.Warn("compaction: offload failed — skipping eviction", "execution", s.id, "error", err)
		// Undo the history mutation (the offload is the originals store;
		// without it the eviction would lose data).
		return false
	}
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

// evictMiddleToolResults removes middle RoleTool messages (keeping the
// pinned goal head, every non-tool message, and the recent tail verbatim)
// and returns the evicted originals for offload. Mutates s.history only
// after eviction is committed by the caller (offload first, then the
// caller's latch update; the mutation itself is applied here).
func (s *Session) evictMiddleToolResults() []evictedToolResult {
	keep := s.cp.RecentTurns
	if keep < 1 {
		keep = 1
	}
	n := len(s.history)
	if n <= keep+1 {
		return nil
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
		return nil
	}

	var evicted []evictedToolResult
	headSeg := make([]Message, 0, head+1)
	midSeg := make([]Message, 0, n)
	tailSeg := make([]Message, 0, keep)
	seq := 0
	for i, m := range s.history {
		switch {
		case i == head:
			headSeg = append(headSeg, m)
		case i >= tailStart:
			tailSeg = append(tailSeg, m) // recent tail verbatim
		case m.Role == RoleTool:
			// Middle tool result: evict + offload the original.
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
		default:
			midSeg = append(midSeg, m) // assistant/user turns survive
		}
	}
	if len(evicted) == 0 {
		return nil
	}

	// Rebuild: head + surviving middle + digest marker + recent tail. The
	// marker is a USER message at the eviction boundary so the model knows
	// what disappeared and how to re-read an original.
	markerText := s.offloadDigestMarker(evicted)
	marker := Message{Role: RoleUser, Content: []Content{{Text: &markerText}}}
	out := make([]Message, 0, len(headSeg)+len(midSeg)+1+len(tailSeg))
	out = append(out, headSeg...)
	out = append(out, midSeg...)
	out = append(out, marker)
	out = append(out, tailSeg...)
	s.history = out
	return evicted
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
