package opencode

import (
	"encoding/json"
	"os"
	"strconv"
)

// Compact-on-budget-breach gate (soft-first) + tool-call HARD-abort gate.
//
// Orchicon runs every worker inside a persistent opencode session. The
// hard per-execution killers are the wall-clock deadline, the
// stall/repetition signals (progress.go), and the tool-call ceiling
// (checkToolCallLimit in session_run.go). Cost/tokens/turn-count are SOFT:
// a raw token budget cannot express money because cache reads are
// discounted differently per provider (DeepSeek cache ≈ 5% of input,
// Claude cache ≈ ~30% of a far costlier input), so a PRICED budget
// (cache-aware cost) is the primary compaction trigger, with tokens as a
// secondary and turn-count as a third, cost-independent trigger (a chatty
// session that's individually cheap per turn still gets bounded). Whatever
// magnitude is configured (or the built-in default — see
// effectiveCostBudget/effectiveTokenBudget/effectiveCompactMaxTurns) is
// what compaction fires on — no extra margin, no fraction-of-budget
// concept — and it is SOFT-FIRST: on breach the adapter compacts the
// session and CONTINUES. tool_call_count is different in kind: there is no
// "compact away" a tool call already made, so effectiveToolCallLimit feeds
// a genuine hard abort instead.
//
// The compaction gate is evaluated at a QUIET STEP BOUNDARY (after a
// step_finish), never before a minimum-turn floor, so a single log-flush
// of step_finish events triggers at most one compact per boundary and a
// fresh summary is never immediately re-collapsed (the compact-at-start/
// loop pathology); the per-execution cap (compactMax) bounds how many
// times it can recur. The Claude adapter sibling must be driven by the
// SAME gates — keep this file provider-neutral so the claude bridge can
// reuse budgetBreached/effectiveToolCallLimit.

// budgetSpec is the parsed merged per-execution budget
// (budget_overrides after tenant-default merge, docs/05 §8). It captures
// the live gates the adapter consults:
//
//   - wallClockSeconds: the hard per-execution deadline (enforced via
//     context.WithDeadline; a HARD abort).
//   - costUSD: the PRIMARY compaction gate (accumulated cache-aware cost;
//     SOFT — compacts and continues).
//   - tokens: the SECONDARY compaction gate (raw fresh tokens, with cache
//     reads weighted by the cache-discount factor; SOFT).
//   - compactMaxTurns: the turn-count compaction gate (fires regardless of
//     cost once this many turns have elapsed since the last compact; SOFT).
//   - toolCallCount: the per-execution tool-call ceiling; a HARD abort
//     (checkToolCallLimit in session_run.go) — unlike the compaction gates,
//     there is no meaningful way to "compact away" tool calls already made.
//
// nil in a field means "unset" — for wallClockSeconds and toolCallCount
// that resolves to a real built-in default (mirrors wallClockDeadline's
// existing 0/wall_clock_seconds=disabled convention); for costUSD/tokens
// see effectiveCostBudget/effectiveTokenBudget. cacheDiscount is the
// cache-read weight used only by the token fallback.
type budgetSpec struct {
	wallClockSeconds *float64
	costUSD          *float64
	tokens           *float64
	compactMaxTurns  *float64
	toolCallCount    *float64
	cacheDiscount    float64
}

// budgetAccumulator tallies cumulative cache-aware spend for one execution.
// It is fed on each step_finish from the SAME token/cost unpacking the
// adapter already performs for recordUsage — the gate never builds a second
// pricing formula. The primary signal is costUSD (opencode's own
// provider-discounted, cache-aware per-step cost); the fresh + cache token
// buckets are kept so the token fallback can run without a cost signal.
type budgetAccumulator struct {
	costUSD    float64
	prompt     float64
	completion float64
	reasoning  float64
	cacheRead  float64
	steps      int
}

// add folds one step_finish's tokens + cost into the accumulator. `tokens`
// is the opencode `part.tokens` map; `cost` is `part.cost` (the runtime's
// provider-discounted float). Reuses toInt64/cacheToken so bucket semantics
// match recordUsage exactly.
func (a *budgetAccumulator) add(tokens map[string]any, cost float64) {
	a.costUSD += cost
	a.prompt += float64(toInt64(tokens["input"]))
	a.completion += float64(toInt64(tokens["output"]))
	a.reasoning += float64(toInt64(tokens["reasoning"]))
	a.cacheRead += float64(toInt64(cacheToken(tokens, "read")))
	a.steps++
}

// budgetBreached reports whether the accumulated spend has crossed the
// effective budget. Primary gate: cache-aware cost_usd. Secondary: fresh
// input+output+reasoning PLUS cache_read weighted by the cache-discount
// factor, so cheap-cache providers (DeepSeek) do not fire early on cache
// bloat while paid-cache providers (Claude) count the real money. Both
// gates resolve through effectiveCostBudget/effectiveTokenBudget, so an
// unset field is NOT "no gate" — it is the built-in default (defaultCompactCostUSD
// / defaultCompactTokens, calibrated from real usage telemetry — see the
// constants' doc comments). Settings has long described an empty field as
// falling back to a built-in default; that description was previously
// aspirational (nothing enforced it), this makes it real. A worker or
// tenant can still opt all the way out of a dimension with an explicit 0
// (mirrors wallClockDeadline's 0-disables convention).
func budgetBreached(spec budgetSpec, acc *budgetAccumulator) bool {
	if acc == nil {
		return false
	}
	if costUSD, ok := effectiveCostBudget(spec); ok && acc.costUSD >= costUSD {
		return true
	}
	if tokens, ok := effectiveTokenBudget(spec); ok {
		budgeted := acc.prompt + acc.completion + acc.reasoning + acc.cacheRead*spec.cacheDiscount
		if budgeted >= tokens {
			return true
		}
	}
	return false
}

// effectiveCostBudget resolves the cost_usd gate: unset → the built-in
// default (defaultCompactCostUSD, $0.50 — matches Settings' "Empty =
// built-in default ($0.50)" copy); an explicit value <= 0 disables the gate
// entirely (ok=false).
func effectiveCostBudget(spec budgetSpec) (usd float64, ok bool) {
	if spec.costUSD == nil {
		return defaultCompactCostUSD, true
	}
	if *spec.costUSD <= 0 {
		return 0, false
	}
	return *spec.costUSD, true
}

// effectiveTokenBudget resolves the tokens gate: unset → the built-in
// default (defaultCompactTokens, 500,000 — matches Settings' "Empty =
// built-in default (500,000)" copy); an explicit value <= 0 disables the
// gate entirely (ok=false).
func effectiveTokenBudget(spec budgetSpec) (tokens float64, ok bool) {
	if spec.tokens == nil {
		return defaultCompactTokens, true
	}
	if *spec.tokens <= 0 {
		return 0, false
	}
	return *spec.tokens, true
}

// effectiveCompactMaxTurns resolves the turn-count gate (compact_max_turns
// in the budget JSON): unset → the built-in default (defaultCompactMaxTurns,
// 8 turns); an explicit value <= 0 disables the turn-count trigger entirely
// (ok=false), leaving cost/tokens as the only compaction signals.
func effectiveCompactMaxTurns(spec budgetSpec) (turns int, ok bool) {
	if spec.compactMaxTurns == nil {
		return defaultCompactMaxTurns, true
	}
	if *spec.compactMaxTurns <= 0 {
		return 0, false
	}
	return int(*spec.compactMaxTurns), true
}

// effectiveToolCallLimit resolves the tool_call_count HARD-abort gate:
// unset → the built-in default (defaultToolCallLimit, 100 — matches
// Settings' "Empty = built-in default (100)" copy); an explicit value <= 0
// disables the limit entirely (ok=false). Unlike the compaction gates
// above, this is consulted by checkToolCallLimit (session_run.go), not
// maybeCompact — a tool-call ceiling has no "compact and continue" option.
func effectiveToolCallLimit(spec budgetSpec) (limit int, ok bool) {
	if spec.toolCallCount == nil {
		return defaultToolCallLimit, true
	}
	if *spec.toolCallCount <= 0 {
		return 0, false
	}
	return int(*spec.toolCallCount), true
}

// parseBudgetSpec parses the merged budget JSON (wall_clock_seconds,
// cost_usd, tokens). Empty or unparseable budgets yield a spec with all-nil
// gate fields (the built-in defaults apply — the caller decides the
// wall-clock backstop), mirroring wallClockDeadline's "unparseable → default"
// behaviour. cacheDiscount is always set from the environment so the token
// fallback can run regardless.
func parseBudgetSpec(budgets []byte) budgetSpec {
	spec := budgetSpec{cacheDiscount: compactCacheDiscount()}
	if len(budgets) == 0 {
		return spec
	}
	var raw struct {
		WallClockSeconds *float64 `json:"wall_clock_seconds"`
		CostUSD          *float64 `json:"cost_usd"`
		Tokens           *float64 `json:"tokens"`
		CompactMaxTurns  *float64 `json:"compact_max_turns"`
		ToolCallCount    *float64 `json:"tool_call_count"`
	}
	if err := json.Unmarshal(budgets, &raw); err != nil {
		return spec
	}
	spec.wallClockSeconds = raw.WallClockSeconds
	spec.costUSD = raw.CostUSD
	spec.tokens = raw.Tokens
	spec.compactMaxTurns = raw.CompactMaxTurns
	spec.toolCallCount = raw.ToolCallCount
	return spec
}

// Compact env knobs (docs/06 budget §8 adjacent). Defaults are safe.
const (
	// defaultCompactCacheDiscount weights cache.read in the token fallback
	// (≈ a cheap provider's cache-read rate). Conservative: it under-counts
	// paid-cache providers (Claude ≈ 30%), but the cost_usd gate is the
	// primary for those.
	defaultCompactCacheDiscount = 0.1
	// defaultCompactCostUSD is the built-in cost_usd gate applied when a
	// worker/tenant budget omits the field. First calibration pass (2026-08-23)
	// used the full 1,350-execution/~1-month usage_records history and landed
	// on $2 — WRONG: cache_read_tokens/cost_usd only started being computed
	// at all as of the 20260828000000_usage_cache_tokens migration
	// (2026-08-22T22:04:23Z, same day), so ~55k of ~58k rows in that sample
	// predate the instrumentation and read as cost_usd≈0/cache_read=0 not
	// because nothing happened but because nothing was measuring it — the
	// "94% of executions never touch cache" conclusion was an artifact of
	// that, not a real finding. Recalibrated on the 110 executions whose
	// entire usage_records set falls AFTER that cutoff (~20h clean window):
	// median cost/execution $0.0126 (unchanged — sane even before), p95
	// $0.0335, MAX OBSERVED $0.0508. $0.50 is ~10x that clean max — more
	// margin than the first pass used, deliberately, given the clean sample
	// is narrow (single model, 20h) and less trustworthy as "the true tail"
	// than a full month would be. Raise per-worker for materially costlier
	// providers (e.g. Claude) once real telemetry exists for those.
	defaultCompactCostUSD = 0.50
	// defaultCompactTokens is the built-in tokens fallback gate applied
	// when a worker/tenant budget omits the field. Same recalibration as
	// defaultCompactCostUSD above and same reason (the original 1,000,000
	// was set against a token-accounting history that also predates the
	// cache-token instrumentation and was not trustworthy). Clean-sample
	// fresh-token (prompt+completion+reasoning) usage per execution: p50
	// 54.8k (matches the ~56k "active tokens" figure that started this
	// whole investigation), p75 74.6k, p90 98.6k, p99 168k, MAX 172k.
	// 500,000 is ~3x the clean max — real headroom without being 6x
	// oversized like the previous number was relative to this evidence.
	defaultCompactTokens = 500_000.0
	// defaultCompactMaxTurns is the built-in turn-count gate: compact at
	// least once every N turns regardless of cost, so a chatty session
	// (many internal opencode steps re-sending the full cached prefix) is
	// bounded even when spend per-turn is individually cheap (e.g. a
	// DeepSeek-backed worker whose cumulative cache-read count still climbs
	// into the millions across dozens of turns). Independent of, and
	// evaluated alongside, the cost/token gates in maybeCompact.
	//
	// Raised 8 -> 12 (2026-08-23): at 8 the gate fired more often than a
	// deep-design worker finishes a unit of work, and each mid-flight
	// compact ended with the completion probe interjecting at the next turn
	// boundary ("your response appears cut off"), which is disruptive
	// noise for long single-goal steps. 12 keeps the chatty-session bound
	// while giving the turn cadence real headroom (minus the min-turn
	// floor guarding the compact loop).
	defaultCompactMaxTurns = 12
	// defaultCompactMinTurns is the minimum completed turns before the gate
	// is armed — never at start, and re-arms only after this floor elapses
	// across normal forward progress (prevents the compact loop).
	defaultCompactMinTurns = 2
	// defaultCompactMax bounds how many compacts fire per execution. Raised
	// from the original 1 (one-shot) now that compaction is expected to run
	// periodically over a long session (the turn-count gate re-arms every
	// defaultCompactMaxTurns turns) rather than only once on a cost breach
	// that then stays permanently tripped. The min-turn re-arm floor above
	// is what actually prevents the compact/step loop; this cap is now a
	// coarse safety ceiling, not the primary loop guard.
	defaultCompactMax = 10
	// defaultToolCallLimit is the built-in tool_call_count HARD-abort gate
	// applied when a worker/tenant budget omits the field. Matches
	// Settings' "Empty = built-in default (100)" copy — this was
	// previously accepted in the budget JSON and displayed in Settings but
	// never actually enforced by anything.
	defaultToolCallLimit = 100
)

// compactCacheDiscount returns the cache-discount factor for the token
// fallback gate (ORCHICON_COMPACT_CACHE_DISCOUNT).
func compactCacheDiscount() float64 {
	if v := os.Getenv("ORCHICON_COMPACT_CACHE_DISCOUNT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return defaultCompactCacheDiscount
}

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
