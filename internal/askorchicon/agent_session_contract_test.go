package askorchicon

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// The Ask persona must state POSITIVELY that no execution budget applies.
//
// Why this is a test and not just prompt copy: Ask conversations were reporting
// being "almost at my budget" because the shared serve agent shell described a
// worker spending a per-call budget, and every Ask session inherits the serve
// default. The shell is neutral now, but the belief was plausible enough that the
// persona should never leave the model to infer one. If a budget is ever attached
// to Ask, this test forces the claim to be revisited rather than silently lying.
func TestAskPromptDeclaresNoExecutionBudget(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg)

	if !strings.Contains(p, "## Session contract") {
		t.Fatal("the Ask prompt must declare its session contract")
	}
	for _, want := range []string{
		"LIVE CONVERSATION",
		"NO tool-call, token, cost, or turn budget",
		"Never report being \"near a budget\"",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the session contract must state %q", want)
		}
	}
}

// The Ask prompt must not carry worker-execution identity or tool-call-economy
// framing. These are the phrases that produced the budget complaint, so they are
// banned explicitly rather than left to drift back in through a shared prompt
// block.
func TestAskPromptCarriesNoWorkerIdentityOrBudgetFraming(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg)

	banned := []string{
		"autonomous coding agent",
		"autonomous worker",
		"split calls are FORBIDDEN",
		"re-sends the whole conversation",
		"tool-call budget",
		"prefer the fewest tool calls",
	}
	for _, b := range banned {
		if strings.Contains(p, b) {
			t.Errorf("the Ask prompt must not contain worker/budget framing %q (it makes the model report a budget it does not have)", b)
		}
	}
}

// And the worker path must KEEP its identity and economy discipline: the fix
// neutralized the SHARED serve shell, so the worker's own composite prompt is now
// the only place that guidance lives. If this fails, workers lost their tuning.
func TestWorkerCompositePromptKeepsItsIdentityAndEconomyDiscipline(t *testing.T) {
	prefix := db.StablePromptPrefix("", "local")
	for _, want := range []string{
		"autonomous worker",
		"Efficiency",
		"split calls are FORBIDDEN",
		"re-sends the whole conversation",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the worker composite prompt must still carry %q — it is now the only home of the worker discipline", want)
		}
	}
}
