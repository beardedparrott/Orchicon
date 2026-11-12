package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Per-execution budget enforcement for the opencode session transport,
// rebuilt around a UNIFIED escalation ladder applied to every dimension
// (tokens, cost, tool calls, wall-clock time):
//
//	warn       (default 50%  of limit) → inject a demanding scary message
//	escalate1  (default 75%  of limit) → inject a harsher message + compact
//	escalate2  (default 90%  of limit) → inject the FINAL warning + compact
//	abort      (100%        of limit) → KILL the session and recover (HARD)
//
// This replaces the old soft-first "compact and continue forever" model.
// The failure mode it fixes: a worker that is individually cheap per turn
// (DeepSeek cache reads at ~5%) but explodes in raw token volume or turn
// count never hit a real ceiling — Orchicon kept compacting and the spend
// kept climbing. Now every dimension has both a warning schedule (so the
// worker is told, in escalating and demanding terms, to remedy immediately)
// AND a hard abort at the limit. The only "soft, keep going" action left is
// context compaction on escalate1/escalate2 — never on abort. Compaction is
// additionally gated PER DIMENSION (see defaultCompactDims): it only fires on
// dimensions it can actually relieve (cost, fresh tokens), so a worker that
// is merely over its tool-call or wall-clock budget is warned but never
// interrupted by a lossy collapse that would force yet more tool calls.
//
// Two orthogonal gates are preserved from the old model because they are
// about context hygiene, not spend:
//   - compact_max_turns: force a compact every N turns regardless of cost,
//     so a chatty session's re-sent prefix stays bounded. Independent of
//     the ladder; it compacts but never warns or aborts.
//   - cacheDiscount: no longer used for a weighted token gate. Cost is now
//     priced (cache-aware) by the provider, and the token gate counts FRESH
//     tokens only (prompt+completion+reasoning), excluding cache reads —
//     which are re-sends of already-counted context and must not be able to
//     trip the token abort on their own. Cache reads still govern spend via
//     the cost gate (they are discounted but not free); there is no separate
//     hand-tuned cache weight.
//
// Config flows through the SAME budget JSON (tenant default_budget_overrides
// merged with the worker's budget_overrides) as the limits, so no DB schema
// or proto change is required. The `warnings` key (optional) carries the
// ladder thresholds and the exact message text for every stage:
//
//	{
//	  "tokens": 500000, "cost_usd": 0.50, "tool_call_count": 100,
//	  "wall_clock_seconds": 3600, "compact_max_turns": 30,
//	  "compact_dims": ["tokens","cost_usd"],
//	  "warnings": {
//	    "fractions": {
//	      "tokens": [0.5, 0.75, 0.9],
//	      "cost_usd": [0.5, 0.75, 0.9],
//	      "tool_call_count": [0.5, 0.75, 0.9],
//	      "wall_clock_seconds": [0.5, 0.75, 0.9]
//	    },
//	    "messages": {
//	      "tokens": ["warn msg", "esc1 msg", "esc2 msg"],
//	      "cost_usd": ["warn msg", "esc1 msg", "esc2 msg"],
//	      "tool_call_count": ["warn msg", "esc1 msg", "esc2 msg"],
//	      "wall_clock_seconds": ["warn msg", "esc1 msg", "esc2 msg"]
//	    }
//   }
//	}
//
// Any sub-key may be omitted to fall back to the built-in defaults
// (defaultWarnFracs / defaultWarnMsgs). An empty messages slot falls back
// per-stage to the built-in copy. This keeps a fresh tenant's budget
// legible while letting an operator override any or all of the text and
// thresholds from the Settings GUI.

// budgetDimension enumerates the four dimensions of the escalation ladder.
type budgetDimension int

const (
	dimTokens budgetDimension = 0 // fresh token volume (prompt+completion+reasoning; cache reads excluded)
	dimCost   budgetDimension = 1 // cache-aware priced cost
	dimTools  budgetDimension = 2 // tool-call count
	dimTime   budgetDimension = 3 // wall-clock elapsed
	dimCount  budgetDimension = 4
)

func dimName(d budgetDimension) string {
	switch d {
	case dimTokens:
		return "tokens"
	case dimCost:
		return "cost_usd"
	case dimTools:
		return "tool_call_count"
	case dimTime:
		return "wall_clock_seconds"
	}
	return "?"
}

// warnLevel is a stage in the unified ladder.
type warnLevel int

const (
	levelNone     warnLevel = 0 // under every threshold
	levelWarn     warnLevel = 1
	levelEscalate warnLevel = 2
	levelFinal    warnLevel = 3 // escalate2 / FINAL warning
	levelAbort    warnLevel = 4 // hard kill
)

// levelIndex maps a warnLevel to a 0..2 index into the warning arrays (warn=0, escalate=1,
// final=2).
func levelIndex(l warnLevel) int {
	switch l {
	case levelWarn:
		return 0
	case levelEscalate:
		return 1
	case levelFinal:
		return 2
	}
	return 0
}

// budgetSpec is the parsed merged per-execution budget.
type budgetSpec struct {
	wallClockSeconds *float64
	costUSD          *float64
	tokens           *float64
	compactMaxTurns  *float64
	toolCallCount    *float64

	warnFracs [dimCount][3]float64
	warnMsgs  [dimCount][3]string

	// compactTiers is the per-tier compaction policy for the ladder, indexed by ladder tier (0=warn,
	// 1=escalate, 2=final): whether that tier also triggers a context compaction.
	compactTiers [3]bool

	// compactDims is the per-dimension compaction policy: which budget dimensions are permitted to
	// trigger a context compaction at all.
	compactDims [dimCount]bool
}

// budgetAccumulator tallies cumulative spend for one execution.
type budgetAccumulator struct {
	costUSD    float64
	prompt     float64
	completion float64
	reasoning  float64
	cacheRead  float64
	steps      int
}

// add folds one step_finish's tokens + cost into the accumulator.
func (a *budgetAccumulator) add(tokens map[string]any, cost float64) {
	a.costUSD += cost
	a.prompt += float64(toInt64(tokens["input"]))
	a.completion += float64(toInt64(tokens["output"]))
	a.reasoning += float64(toInt64(tokens["reasoning"]))
	a.cacheRead += float64(toInt64(cacheToken(tokens, "read")))
	a.steps++
}

// totalTokens returns the FULL token count (no cache discount).
func (a *budgetAccumulator) totalTokens() float64 {
	return a.prompt + a.completion + a.reasoning + a.cacheRead
}

// freshTokens returns only the tokens that represent actual work for the execution: fresh input
// (prompt) + output (completion) + reasoning.
func (a *budgetAccumulator) freshTokens() float64 {
	return a.prompt + a.completion + a.reasoning
}

// elapsedSecs is a small helper to keep the dimension readout uniform.
func elapsedSecsToFloat(elapsed time.Duration) float64 {
	return elapsed.Seconds()
}

// ─── effective limits ─────────────────────────────────────────────────────

// effectiveTokenBudget resolves the tokens gate: UNSET → NO GATE (ok=false).
func effectiveTokenBudget(spec budgetSpec) (tokens float64, ok bool) {
	if spec.tokens == nil {
		return 0, false
	}
	if *spec.tokens <= 0 {
		return 0, false
	}
	return *spec.tokens, true
}

// effectiveCostBudget resolves the cost_usd gate: UNSET → NO GATE (see effectiveTokenBudget — a
// ceiling is the operator's decision).
func effectiveCostBudget(spec budgetSpec) (usd float64, ok bool) {
	if spec.costUSD == nil {
		return 0, false
	}
	if *spec.costUSD <= 0 {
		return 0, false
	}
	return *spec.costUSD, true
}

// effectiveToolCallLimit resolves the tool_call_count HARD-abort gate: UNSET → NO GATE (see
// effectiveTokenBudget; 100 was a ceiling no operator asked.
func effectiveToolCallLimit(spec budgetSpec) (limit int, ok bool) {
	if spec.toolCallCount == nil {
		return 0, false
	}
	if *spec.toolCallCount <= 0 {
		return 0, false
	}
	return int(*spec.toolCallCount), true
}

// effectiveWallClockBudget resolves the wall_clock_seconds gate: UNSET → NO DEADLINE (ok=false),
// mirroring wallClockDeadline.
func effectiveWallClockBudget(spec budgetSpec) (sec float64, ok bool) {
	if spec.wallClockSeconds == nil {
		return 0, false
	}
	if *spec.wallClockSeconds <= 0 {
		return 0, false
	}
	return *spec.wallClockSeconds, true
}

// effectiveCompactMaxTurns resolves the turn-count gate (compact_max_turns): UNSET → NO TURN-COUNT
// COLLAPSE (ok=false).
func effectiveCompactMaxTurns(spec budgetSpec) (turns int, ok bool) {
	if spec.compactMaxTurns == nil {
		return 0, false
	}
	if *spec.compactMaxTurns <= 0 {
		return 0, false
	}
	return int(*spec.compactMaxTurns), true
}

// limit returns the effective ceiling for a dimension. ok=false when the
// gate is explicitly disabled.
func (s budgetSpec) limit(d budgetDimension) (limit float64, ok bool) {
	switch d {
	case dimTokens:
		return effectiveTokenBudget(s)
	case dimCost:
		return effectiveCostBudget(s)
	case dimTools:
		l, ok := effectiveToolCallLimit(s)
		return float64(l), ok
	case dimTime:
		return effectiveWallClockBudget(s)
	}
	return 0, false
}

// fraction returns the current spend for a dimension as a fraction of its configured limit (>=1.0
// when at/over the ceiling).
func (s budgetSpec) fraction(d budgetDimension, acc *budgetAccumulator, elapsed time.Duration, toolUses int) float64 {
	limit, ok := s.limit(d)
	if !ok {
		return -1
	}
	var used float64
	switch d {
	case dimTokens:
		// The token gate counts FRESH tokens only (no cache re-sends) — see freshTokens.
		used = acc.freshTokens()
	case dimCost:
		used = acc.costUSD
	case dimTools:
		used = float64(toolUses)
	case dimTime:
		used = elapsedSecsToFloat(elapsed)
	}
	if limit <= 0 {
		return -1
	}
	return used / limit
}

// warnFracsFor returns the ladder thresholds for a dimension, falling back to the built-in
// fractions when the spec was constructed without them (a direct budgetSpec literal, e.g. in tests.
func (s budgetSpec) warnFracsFor(d budgetDimension) [3]float64 {
	if s.warnFracs[d][0] > 0 || s.warnFracs[d][1] > 0 || s.warnFracs[d][2] > 0 {
		return s.warnFracs[d]
	}
	return defaultWarnFracs()[d]
}

// levelFor computes the warn ladder stage for a dimension from its fraction of the limit.
func (s budgetSpec) levelFor(d budgetDimension, frac float64) warnLevel {
	if frac < 0 {
		return levelNone
	}
	thr := s.warnFracsFor(d)
	switch {
	case frac >= 1.0:
		return levelAbort
	case frac >= thr[2]:
		return levelFinal
	case frac >= thr[1]:
		return levelEscalate
	case frac >= thr[0]:
		return levelWarn
	}
	return levelNone
}

// message returns the configured message for a warn-tier of a dimension, falling back to the
// built-in copy when the slot is empty.
func (s budgetSpec) message(d budgetDimension, l warnLevel, frac float64) string {
	if l == levelAbort {
		return ""
	}
	idx := levelIndex(l)
	raw := s.warnMsgs[d][idx]
	if strings.TrimSpace(raw) == "" {
		raw = defaultWarnMsgs()[d][idx]
	}
	pct := "?"
	if frac >= 0 {
		pct = fmt.Sprintf("%d", int(frac*100))
	}
	return strings.ReplaceAll(raw, "{pct}", pct)
}

// budgetBreached reports whether the accumulated spend has crossed the abort limit on EITHER spend
// dimension (cost or tokens, both full-weight).
func budgetBreached(spec budgetSpec, acc *budgetAccumulator) bool {
	if acc == nil {
		return false
	}
	if costUSD, ok := effectiveCostBudget(spec); ok && acc.costUSD >= costUSD {
		return true
	}
	if tokens, ok := effectiveTokenBudget(spec); ok && acc.freshTokens() >= tokens {
		return true
	}
	return false
}

// ─── parsing ─────────────────────────────────────────────────────────────

// defaultWarnFracs returns the built-in ladder thresholds.
func defaultWarnFracs() [dimCount][3]float64 {
	return [dimCount][3]float64{
		{0.25, 0.5, 0.75}, // tokens
		{0.25, 0.5, 0.75}, // cost
		{0.25, 0.5, 0.75}, // tools
		{0.25, 0.5, 0.75}, // time
	}
}

// defaultWarnMsgs returns the built-in escalating messages per dimension.
func defaultWarnMsgs() [dimCount][3]string {
	return [dimCount][3]string{
		// tokens
		{
			"You have used {pct}% of your token budget. This is driven almost entirely by re-sending accumulated context on every turn, not by the work itself. You still have room — just be deliberate from here: batch ALL remaining reads into ONE round-trip, never re-read a file already in context, keep your todo tight, and deliver only the minimal delta. Do that and you'll finish comfortably.",
			"You have used {pct}% of your token budget, and the burn is still from re-sending context each turn. It's recoverable, but it needs a course-correction now rather than later: consolidate every remaining tool call into a single batch, stop re-reading or re-exploring, stick to the todo list, and deliver the deliverable. Re-sending less is what keeps this session alive.",
			"You have used {pct}% of your token budget — this is the last chance before the session is stopped. Stop all exploration. Finish in the next minimal number of tool calls: batch everything, do not re-read anything already in context, and deliver the completed work now.",
		},
		// cost
		{
			"You have used {pct}% of your cost budget. Most of that is re-sent context, not new output. There's room to finish if you act now: batch the remaining tool calls into one round-trip, keep only what the task actually needs in context, and deliver the minimal delta.",
			"You have used {pct}% of your cost budget and are on pace to exceed it. Shift to the cheapest path: batch all remaining tool calls, avoid re-deriving anything already established, stick to the todo list, and finish the deliverable now.",
			"You have used {pct}% of your cost budget — this is the final warning. Complete the work in the next few tool calls: batch them and deliver now, or the session will be stopped.",
		},
		// tools
		{
			"You have used {pct}% of your tool-call budget. Splitting work into many separate calls is what's consuming it — every call re-sends the whole conversation. You have room, but please batch here on: combine independent operations into ONE round-trip. Fewer, larger calls finish faster and cost less.",
			"You are at {pct}% of your tool-call budget and still making many small calls. Each one re-sends the whole conversation, which multiplies the cost. Stop the micro calls. Consolidate everything into a single round-trip and focus on completing the todo list.",
			"You are at {pct}% of your tool-call budget — only a handful of calls remain. Finish in the next tool calls: batch everything into one round-trip, do not re-read, and deliver the completed work now, or the session will be stopped.",
		},
		// time
		{
			"You have used {pct}% of your time budget. There's still time if you move deliberately from here: batch the remaining tool calls, keep to the todo list, and finish the deliverable.",
			"You have used {pct}% of your time budget. Shift to the fastest path: batch the remaining tool calls, stop re-checking settled state, and finish now.",
			"You have used {pct}% of your time budget — almost out of time. Complete the work in the next tool calls and deliver now, or the session will be stopped.",
		},
	}
}

// defaultCompactTiers returns the built-in per-tier compaction policy: warn does NOT compact (the
// earliest tier — the worker is only ~25% in and can still correct course without a destructive.
func defaultCompactTiers() [3]bool { return [3]bool{false, true, true} }

// Compaction is a lossy, mid-flight-interrupting collapse that buys headroom ONLY on a dimension
// driven by re-sent context — cost and fresh tokens.
func defaultCompactDims() [dimCount]bool {
	return [dimCount]bool{true, true, false, false} // tokens, cost only
}

// compactsDim reports whether a budget dimension is permitted to trigger a context compaction at
// all (independent of the per-tier policy).
func (s budgetSpec) compactsDim(d budgetDimension) bool {
	return s.compactDims[d]
}

// dimFromName maps a budget-JSON dimension name to its dimension.
func dimFromName(name string) (budgetDimension, bool) {
	switch name {
	case "tokens":
		return dimTokens, true
	case "cost_usd":
		return dimCost, true
	case "tool_call_count":
		return dimTools, true
	case "wall_clock_seconds":
		return dimTime, true
	}
	return 0, false
}

// compactsAt reports whether a ladder tier triggers a context compaction (independent of whether it
// always injects its warning message).
func (s budgetSpec) compactsAt(l warnLevel) bool {
	switch l {
	case levelWarn:
		return s.compactTiers[0]
	case levelEscalate:
		return s.compactTiers[1]
	case levelFinal:
		return s.compactTiers[2]
	}
	return false
}

// parseBudgetSpec parses the merged budget JSON.
func parseBudgetSpec(budgets []byte) budgetSpec {
	spec := budgetSpec{
		warnFracs:    defaultWarnFracs(),
		warnMsgs:     defaultWarnMsgs(),
		compactTiers: defaultCompactTiers(),
		compactDims:  defaultCompactDims(),
	}
	if len(budgets) == 0 {
		return spec
	}
	var raw struct {
		WallClockSeconds *float64 `json:"wall_clock_seconds"`
		CostUSD          *float64 `json:"cost_usd"`
		Tokens           *float64 `json:"tokens"`
		CompactMaxTurns  *float64 `json:"compact_max_turns"`
		ToolCallCount    *float64 `json:"tool_call_count"`
		CompactTiers     []bool   `json:"compact_tiers"`
		CompactDims      []string `json:"compact_dims"`
		Warnings         struct {
			Fractions map[string][3]float64 `json:"fractions"`
			Messages  map[string][3]string  `json:"messages"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(budgets, &raw); err != nil {
		return spec
	}
	spec.wallClockSeconds = raw.WallClockSeconds
	spec.costUSD = raw.CostUSD
	spec.tokens = raw.Tokens
	spec.compactMaxTurns = raw.CompactMaxTurns
	spec.toolCallCount = raw.ToolCallCount

	// Per-tier compaction toggles: a 3-element [warn, escalate, final] bool array.
	if len(raw.CompactTiers) == 3 {
		for i := 0; i < 3; i++ {
			spec.compactTiers[i] = raw.CompactTiers[i]
		}
	}

	// Per-dimension compaction policy: a `compact_dims` array of dimension names (e.g.
	// ["tokens","cost_usd"]).
	if raw.CompactDims != nil {
		for i := range spec.compactDims {
			spec.compactDims[i] = false
		}
		for _, name := range raw.CompactDims {
			if d, ok := dimFromName(name); ok {
				spec.compactDims[d] = true
			}
		}
	}

	for d := budgetDimension(0); d < dimCount; d++ {
		name := dimName(d)
		if fr, ok := raw.Warnings.Fractions[name]; ok {
			for i := 0; i < 3; i++ {
				if fr[i] >= 0 && fr[i] <= 1 {
					spec.warnFracs[d][i] = fr[i]
				}
			}
		}
		if ms, ok := raw.Warnings.Messages[name]; ok {
			for i := 0; i < 3; i++ {
				spec.warnMsgs[d][i] = ms[i]
			}
		}
	}
	return spec
}

// ── built-in defaults ────────────────────────────────────────────────────
const (
	// THE GATE CEILINGS HAVE NO BUILT-IN DEFAULTS.
	defaultCompactMinTurns = 2
	// defaultCompactMax bounds how many compacts fire per execution.
	defaultCompactMax = 10
)

// compactMinTurns returns the minimum-turn floor before the gate is armed
// (ORCHICON_COMPACT_MIN_TURNS).
func compactMinTurns() int {
	if v := os.Getenv("ORCHICON_COMPACT_MIN_TURNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultCompactMinTurns
}

// compactMax returns the per-execution compaction cap
// (ORCHICON_COMPACT_MAX). 0 disables compaction entirely.
func compactMax() int {
	if v := os.Getenv("ORCHICON_COMPACT_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultCompactMax
}
