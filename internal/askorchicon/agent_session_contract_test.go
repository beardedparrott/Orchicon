package askorchicon

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// The Ask persona must state POSITIVELY that no execution budget applies.
func TestAskPromptDeclaresNoExecutionBudget(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg, "")

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

// The Ask prompt must not carry worker-execution identity or tool-call-economy framing.
func TestAskPromptCarriesNoWorkerIdentityOrBudgetFraming(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeBrainstorm, db.AgentConfigRow{}, reg, "")

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

// And the worker path must KEEP its identity and economy discipline: the fix neutralized the SHARED
// serve shell, so the worker's own composite prompt is now the only place that guidance lives.
func TestWorkerCompositePromptKeepsItsIdentityAndEconomyDiscipline(t *testing.T) {
	prefix := db.StablePromptPrefix("", "local")
	for _, want := range []string{
		"autonomous worker",
		"Efficiency",
		"re-sends the whole conversation",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the worker composite prompt must still carry %q — it is now the only home of the worker discipline", want)
		}
	}

	// THE BLANKET BAN IS GONE, DELIBERATELY.
	if strings.Contains(prefix, "split calls are FORBIDDEN") {
		t.Error("the blanket split-call ban must not return: it forbade verification as well as waste")
	}

	// THE PROMPT MUST TELL THE WORKER IT IS CHECKED, and say on what.
	for _, want := range []string{
		"checked against the repository",
		".orchicon/verify",
		"UNVERIFIED",
		"commit count",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the worker prompt must state what is checked (%q) — the evidence gate is invisible otherwise", want)
		}
	}

	// MEMORY IS MANDATORY AND MUST BE UNAMBIGUOUS.
	for _, want := range []string{
		"`memory_search` FIRST, before you explore",
		"Recording a workaround is MANDATORY",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("the memory playbook must carry %q — a soft instruction is what failed before", want)
		}
	}
	if strings.Contains(prefix, "On an unfamiliar task, start with") {
		t.Error("the conditional memory instruction must not return: it let the model skip the search")
	}
}

// THE SESSION CONTRACT MUST STATE THE ONE BOUND THAT IS REAL.
//
// "No budget" is true and must stay — but it reads as "effort is free", and two live sessions took that
// licence and died: one made 355 tool calls and produced 263KB of reasoning inside a single turn, which
// was cut off by the reply window with nothing reported. The wall clock IS a bound, so the contract has
// to say so, together with the turn shape that keeps a turn inside it.
func TestAskPromptStatesTheRealWallClockBoundAndTurnShape(t *testing.T) {
	reg := NewToolRegistry(nil, nil, nil)
	p := BuildSystemPrompt(modeIteration, db.AgentConfigRow{}, reg, "")

	for _, want := range []string{
		"BOUNDED BY WALL CLOCK",
		"ONE TURN SHOULD FINISH ONE DELIVERABLE",
		"Do not write large scripts inline",
		"hand the full run to the operator",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the session contract must state %q — without it the model treats effort as free "+
				"and spends the whole turn on one sweep", want)
		}
	}

	// AND IT MUST STILL NOT REINTRODUCE BUDGET FRAMING: the model must not start reporting a quota.
	// (Note "running low on calls" is deliberately NOT banned: it appears inside the "never report"
	// sentence, which forbids that framing rather than using it.)
	for _, banned := range []string{
		"tool-call budget",
		"prefer the fewest tool calls",
		"split calls are FORBIDDEN",
	} {
		if strings.Contains(p, banned) {
			t.Errorf("the session contract reintroduced budget framing %q", banned)
		}
	}
}
