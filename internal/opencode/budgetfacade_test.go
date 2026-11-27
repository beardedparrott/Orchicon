package opencode_test

// budgetfacade_test.go — AC6: a work item with a `max_cost_usd` budget that runs on PLAN QUOTA must not
// fail on the cost gate.
//
// WHY THIS IS THE WHOLE RISK. An Antigravity run on oauth-personal/oauth-business records cost 0 for every
// turn, because the operator's plan already paid for it (db.BillingPlanQuota). A cost gate evaluated as
// "spend >= limit" is therefore never crossed — but a gate that divided by the limit, or treated a zero
// spend as "unset" and fell through to something else, would either panic or misfire. The ladder must
// simply not trip on cost, while the TOKEN budgets it shares the ladder with keep working, because tokens
// are what a plan-quota run actually consumes.
//
// The antigravity bridge does not consume this ladder today (it has no compaction ladder of its own — see
// the note in the SSE summary); the gate is the shared machinery the scheduler applies, and pinning its
// behaviour for a zero-cost, high-token spend is what makes the "no misfire" claim testable.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// planQuotaBudgets is a work item's merged budget with BOTH a cost ceiling and a token ceiling.
const planQuotaBudgets = `{"cost_usd": 0.5, "tokens": 1000000}`

func TestPlanQuotaSpendDoesNotBreachTheCostGate(t *testing.T) {
	ladder := opencode.ParseBudgetLadder([]byte(planQuotaBudgets))
	spend := opencode.NewBudgetSpend()

	// Six turns of a plan-quota run: real tokens, cost 0 for every one of them.
	for i := 0; i < 6; i++ {
		spend.AddFromUsage(50_000, 20_000, 5_000, 10_000, 0)
	}

	if got := spend.CostUSD(); got != 0 {
		t.Fatalf("plan-quota spend cost = %v, want 0 — a non-zero cost would be a fabricated charge", got)
	}
	if ladder.Breached(spend) {
		t.Fatalf("a plan-quota run with cost 0 breached the budget (fresh tokens %v, cost %v) — a "+
			"max_cost_usd gate must never misfire on a zero-cost spend", spend.FreshTokens(), spend.CostUSD())
	}
	// The cost gate is still RESOLVED (not silently disabled), which is what proves the non-breach is a
	// real comparison rather than "no gate".
	limit, ok := ladder.EffectiveCost()
	if !ok || limit != 0.5 {
		t.Fatalf("EffectiveCost() = (%v, %v), want (0.5, true)", limit, ok)
	}
}

func TestPlanQuotaTokenBudgetStillApplies(t *testing.T) {
	ladder := opencode.ParseBudgetLadder([]byte(planQuotaBudgets))
	spend := opencode.NewBudgetSpend()
	// Fresh tokens = input+output+reasoning, cache reads excluded. Three turns of 600k fresh each = 1.8M,
	// comfortably over the 1M token ceiling, still at cost 0.
	for i := 0; i < 3; i++ {
		spend.AddFromUsage(400_000, 150_000, 50_000, 0, 0)
	}
	if spend.CostUSD() != 0 {
		t.Fatalf("cost = %v, want 0", spend.CostUSD())
	}
	if spend.FreshTokens() < 1_000_000 {
		t.Fatalf("fresh tokens = %v, want >= 1e6 for this fixture", spend.FreshTokens())
	}
	if !ladder.Breached(spend) {
		t.Error("a plan-quota run that consumed 1.8M fresh tokens did NOT breach the 1M token budget — " +
			"plan quota removes the cost gate, not the token gate")
	}
}

// TestCostBudgetOfZeroIsDisabledNotADivideByZero pins the guard behind the same risk: a cost_usd of 0 in
// the budget JSON means "no cost gate", never "a limit of zero that any spend crosses".
func TestCostBudgetOfZeroIsDisabledNotADivideByZero(t *testing.T) {
	ladder := opencode.ParseBudgetLadder([]byte(`{"cost_usd": 0, "tokens": 1000000}`))
	if limit, ok := ladder.EffectiveCost(); ok {
		t.Fatalf("EffectiveCost() = (%v, true) for cost_usd 0, want ok=false (an explicit 0 disables the "+
			"gate; treating it as a limit would abort instantly)", limit)
	}
	spend := opencode.NewBudgetSpend()
	spend.AddFromUsage(100, 100, 0, 0, 0)
	if ladder.Breached(spend) {
		t.Error("a cost_usd:0 budget breached on a tiny spend; it must be disabled, not a zero limit")
	}
	// And the fraction math must not divide by zero when the gate is absent: the ladder reports -1
	// ("no fraction for a dimension with no gate") rather than +Inf or NaN.
	if frac := spend.Fraction(ladder, "cost", 0, 0); frac != -1 {
		t.Errorf("Fraction(cost) over a disabled gate = %v, want -1 (no gate, not a division by zero)", frac)
	}
}
