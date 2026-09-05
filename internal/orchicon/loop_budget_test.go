package orchicon

import (
	"testing"
)

// Turn-budget resolution: work-item budgets.max_steps (layered over tenant
// defaults by mergeBudgets before it reaches us) beats the server env,
// which beats the 25-turn default. A bad key never disables the guard.

func TestMaxStepsFromBudgetsKeyWins(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_MAX_STEPS", "7")
	if got := maxStepsFromBudgets([]byte(`{"max_steps":10,"tokens":500000}`)); got != 10 {
		t.Fatalf("budgets key → %d, want 10 (env must lose)", got)
	}
}

func TestMaxStepsFromBudgetsEnvFallback(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_MAX_STEPS", "7")
	for _, b := range [][]byte{nil, {}, []byte(`{}`), []byte(`{"tokens":1}`)} {
		if got := maxStepsFromBudgets(b); got != 7 {
			t.Fatalf("budgets %q → %d, want env 7", b, got)
		}
	}
}

func TestMaxStepsFromBudgetsDefault(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_MAX_STEPS", "")
	if got := maxStepsFromBudgets(nil); got != maxStepsDefault {
		t.Fatalf("empty → %d, want default %d", got, maxStepsDefault)
	}
}

func TestMaxStepsFromBudgetsBadKeyFallsThrough(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_MAX_STEPS", "9")
	bad := []string{
		`{"max_steps":0}`,
		`{"max_steps":-3}`,
		`{"max_steps":"abc"}`,
		`{"max_steps":null}`,
		`{"max_steps":[10]}`,
		`not json at all`,
	}
	for _, b := range bad {
		if got := maxStepsFromBudgets([]byte(b)); got != 9 {
			t.Fatalf("budgets %q → %d, want env 9 (guard never disabled)", b, got)
		}
	}
}

func TestTurnBudgetZeroValueFallsBack(t *testing.T) {
	// A Session built outside NewSession must not fail on step one.
	t.Setenv("ORCHICON_SESSION_MAX_STEPS", "")
	if got := (&Session{}).turnBudget(); got != maxStepsDefault {
		t.Fatalf("zero Session → %d, want default %d", got, maxStepsDefault)
	}
	s := &Session{maxStepsVal: 12}
	if got := s.turnBudget(); got != 12 {
		t.Fatalf("resolved Session → %d, want 12", got)
	}
}
