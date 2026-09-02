package opencode

// budgetfacade.go is the thin EXPORTED surface over the existing
// shared budget-compact machinery in compact.go (ONE implementation —
// no parallel budget code). The native in-process engine
// (internal/orchicon) imports this package to evaluate the SAME merged
// budget JSON the opencode adapter evaluates, so the budget-compact
// pipeline is shared, never duplicated. All values flow through the
// unexported budgetSpec/budgetAccumulator types below.

import "time"

// BudgetLadder is the exported view of a parsed merged-execution budget.
// A nil *BudgetLadder is never valid — always use ParseBudgetLadder.
type BudgetLadder struct {
	spec budgetSpec
}

// ParseBudgetLadder parses the merged budget JSON (the same payload the
// opencode adapter parses via parseBudgetSpec — tenant defaults merged
// with the worker's budget_overrides by the scheduler). Empty or
// unparseable JSON yields a ladder with built-in defaults.
func ParseBudgetLadder(budgets []byte) *BudgetLadder {
	return &BudgetLadder{spec: parseBudgetSpec(budgets)}
}

// EffectiveTokens returns the resolved tokens gate (ok=false when
// explicitly disabled).
func (l *BudgetLadder) EffectiveTokens() (float64, bool) { return effectiveTokenBudget(l.spec) }

// EffectiveCost returns the resolved cost_usd gate (ok=false when
// explicitly disabled).
func (l *BudgetLadder) EffectiveCost() (float64, bool) { return effectiveCostBudget(l.spec) }

// EffectiveToolCalls returns the resolved tool_call_count gate.
func (l *BudgetLadder) EffectiveToolCalls() (float64, bool) {
	v, ok := effectiveToolCallLimit(l.spec)
	return float64(v), ok
}

// EffectiveWallClock returns the resolved wall_clock_seconds gate.
func (l *BudgetLadder) EffectiveWallClock() (float64, bool) { return effectiveWallClockBudget(l.spec) }

// Breached reports whether accumulated spend has crossed the abort limit
// on either spend dimension (cost or fresh tokens).
func (l *BudgetLadder) Breached(a *BudgetSpend) bool {
	if a == nil {
		return false
	}
	return budgetBreached(l.spec, a.acc)
}

// LevelName reports the ladder stage for a dimension at the given
// fraction: none|warn|escalate|final|abort.
func (l *BudgetLadder) LevelName(dim string, frac float64) string {
	d, ok := dimFromName(dim)
	if !ok {
		return "none"
	}
	return levelName(l.spec.levelFor(d, frac))
}

// CompactsAt reports whether the ladder tier triggers a context
// compaction (independent of injecting its warning). tier is one of
// none|warn|escalate|final|abort.
func (l *BudgetLadder) CompactsAt(tier string) bool {
	return l.spec.compactsAt(levelFromName(tier))
}

// CompactsDim reports whether a dimension is permitted to trigger a
// compaction (per the budget JSON compact_dims policy).
func (l *BudgetLadder) CompactsDim(dim string) bool {
	d, ok := dimFromName(dim)
	if !ok {
		return false
	}
	return l.spec.compactsDim(d)
}

// CompactionTurnFloor returns the minimum completed turns before the
// budget/turn gate is armed (ORCHICON_COMPACT_MIN_TURNS).
func (l *BudgetLadder) CompactionTurnFloor() int { return compactMinTurns() }

// CompactionMax returns the per-execution compaction cap
// (ORCHICON_COMPACT_MAX; 0 disables).
func (l *BudgetLadder) CompactionMax() int { return compactMax() }

// BudgetSpend is the exported view of a cumulative spend accumulator
// (fresh tokens + cost + step count). Fed from LIVE provider-reported
// usage only.
type BudgetSpend struct {
	acc *budgetAccumulator
}

// NewBudgetSpend creates an empty spend accumulator.
func NewBudgetSpend() *BudgetSpend { return &BudgetSpend{acc: &budgetAccumulator{}} }

// AddFromUsage folds one provider turn's LIVE usage + cost into the
// accumulator. cost is the provider-reported priced figure (0 when the
// provider does not price).
func (a *BudgetSpend) AddFromUsage(input, output, reasoning, cacheRead int64, cost float64) {
	a.acc.add(map[string]any{
		"input":     input,
		"output":    output,
		"reasoning": reasoning,
		"cache":     map[string]any{"read": cacheRead},
	}, cost)
	a.acc.steps++
}

// FreshTokens returns prompt+completion+reasoning (cache reads excluded).
func (a *BudgetSpend) FreshTokens() float64 { return a.acc.freshTokens() }

// TotalTokens returns the full token count including cache reads.
func (a *BudgetSpend) TotalTokens() float64 { return a.acc.totalTokens() }

// CostUSD returns the accumulated priced cost.
func (a *BudgetSpend) CostUSD() float64 { return a.acc.costUSD }

// Steps returns the accumulated step count.
func (a *BudgetSpend) Steps() int { return a.acc.steps }

// Fraction returns the spend of one dimension as a fraction of its
// configured limit (-1 when the dimension has no effective limit).
func (a *BudgetSpend) Fraction(l *BudgetLadder, dim string, elapsed time.Duration, toolUses int) float64 {
	if l == nil {
		return -1
	}
	d, ok := dimFromName(dim)
	if !ok {
		return -1
	}
	return l.spec.fraction(d, a.acc, elapsed, toolUses)
}

func levelName(l warnLevel) string {
	switch l {
	case levelNone:
		return "none"
	case levelWarn:
		return "warn"
	case levelEscalate:
		return "escalate"
	case levelFinal:
		return "final"
	case levelAbort:
		return "abort"
	}
	return "none"
}

func levelFromName(s string) warnLevel {
	switch s {
	case "none":
		return levelNone
	case "warn":
		return levelWarn
	case "escalate":
		return levelEscalate
	case "final":
		return levelFinal
	case "abort":
		return levelAbort
	}
	return levelNone
}
