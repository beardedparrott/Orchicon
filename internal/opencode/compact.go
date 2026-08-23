package opencode

import (
	"encoding/json"
	"os"
	"strconv"
)

// Compact-on-budget-breach gate (soft-first, no hard abort).
//
// Orchicon runs every worker inside a persistent opencode session. The
// only hard per-execution killers are the wall-clock deadline and the
// stall/repetition signals (progress.go); there is no cost gate — a raw
// token budget cannot express money because cache reads are discounted
// differently per provider (DeepSeek cache ≈ 5% of input, Claude cache ≈
// ~30% of a far costlier input). The work item's design: a PRICED budget
// (cache-aware cost) is the compaction trigger. Whatever cost_usd /
// tokens budget is configured is the magnitude at which compaction fires —
// no extra margin, no fraction-of-budget concept — and it is SOFT-FIRST:
// on breach the adapter compacts the session once and CONTINUES. The
// wall-clock and stall signals are left untouched; nothing hard-stops the
// run at the budget threshold.
//
// The gate is evaluated at a QUIET STEP BOUNDARY (after a step_finish), at
// most once per step and never before a minimum-turn floor, so a single
// log-flush of step_finish events triggers at most one compact and a fresh
// summary is never immediately re-collapsed (the compact-at-start/loop
// pathology).
//
// See architecture-notes/compact-session-on-token-budget-breach.md for the
// full ADR. The Claude adapter sibling must be driven by the SAME gate —
// keep this block provider-neutral so the claude bridge can reuse
// budgetBreached.

// budgetSpec is the parsed merged per-execution budget
// (budget_overrides after tenant-default merge, docs/05 §8). It captures
// the three live gates the adapter consults:
//
//   - wallClockSeconds: the hard per-execution deadline (enforced via
//     context.WithDeadline; NOT a compaction gate).
//   - costUSD: the PRIMARY compaction gate (accumulated cache-aware cost).
//   - tokens: the FALLBACK compaction gate (raw fresh tokens, with cache
//     reads weighted by the cache-discount factor).
//
// nil in a field means "no gate for that dimension" (mirrors
// wallClockDeadline's 0/wall_clock_seconds=disabled semantics). cacheDiscount
// is the cache-read weight used only by the token fallback.
type budgetSpec struct {
	wallClockSeconds *float64
	costUSD          *float64
	tokens           *float64
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
// configured budget. Primary gate: cache-aware cost_usd. Fallback (cost
// unset): fresh input+output+reasoning PLUS cache_read weighted by the
// cache-discount factor, so cheap-cache providers (DeepSeek) do not fire
// early on cache bloat while paid-cache providers (Claude) count the real
// money. The configured number IS the threshold — no margin, no fraction.
// Returns false when neither a cost nor a token gate is configured.
func budgetBreached(spec budgetSpec, acc *budgetAccumulator) bool {
	if acc == nil {
		return false
	}
	if spec.costUSD != nil && acc.costUSD >= *spec.costUSD {
		return true
	}
	if spec.tokens != nil {
		budgeted := acc.prompt + acc.completion + acc.reasoning + acc.cacheRead*spec.cacheDiscount
		if budgeted >= *spec.tokens {
			return true
		}
	}
	return false
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
	}
	if err := json.Unmarshal(budgets, &raw); err != nil {
		return spec
	}
	spec.wallClockSeconds = raw.WallClockSeconds
	spec.costUSD = raw.CostUSD
	spec.tokens = raw.Tokens
	return spec
}

// Compact env knobs (docs/06 budget §8 adjacent). Defaults are safe.
const (
	// defaultCompactCacheDiscount weights cache.read in the token fallback
	// (≈ a cheap provider's cache-read rate). Conservative: it under-counts
	// paid-cache providers (Claude ≈ 30%), but the cost_usd gate is the
	// primary for those.
	defaultCompactCacheDiscount = 0.1
	// defaultCompactMinTurns is the minimum completed turns before the gate
	// is armed — never at start, and re-arms only after this floor elapses
	// across normal forward progress (prevents the compact loop).
	defaultCompactMinTurns = 2
	// defaultCompactMax bounds how many compacts fire per execution. The
	// spend accumulator is cumulative, so once a compact runs the budget
	// stays tripped; capping at 1 prevents the compact/step loop that would
	// re-collapse the fresh summary every turn. Operators who want periodic
	// re-compact on very long runs raise it.
	defaultCompactMax = 1
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
