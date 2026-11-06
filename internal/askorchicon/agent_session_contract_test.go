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

// The Ask prompt must not carry worker-execution identity or tool-call-economy
// framing. These are the phrases that produced the budget complaint, so they are
// banned explicitly rather than left to drift back in through a shared prompt
// block.
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

// And the worker path must KEEP its identity and economy discipline: the fix
// neutralized the SHARED serve shell, so the worker's own composite prompt is now
// the only place that guidance lives. If this fails, workers lost their tuning.
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

	// THE BLANKET BAN IS GONE, DELIBERATELY. "split calls are FORBIDDEN" forbade the
	// investigate-then-confirm loop as well as the wasteful one, and a worker reading it cannot tell
	// the two apart — so it did not re-run the failing test, did not re-read the diff, and reported
	// done. The economy instruction survives (batch your READS); the prohibition on ever making a
	// second call does not. A blanket ban that suppresses verification costs more than the tokens it
	// saves.
	if strings.Contains(prefix, "split calls are FORBIDDEN") {
		t.Error("the blanket split-call ban must not return: it forbade verification as well as waste")
	}

	// THE PROMPT MUST TELL THE WORKER IT IS CHECKED, and say on what. This is the half of the
	// verification feature that lives outside the engine: a worker that knows its diff and commit
	// count are compared to its claim behaves differently from one told vaguely to be accurate.
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

	// MEMORY IS MANDATORY AND MUST BE UNAMBIGUOUS. "On an unfamiliar task" was a judgement call the
	// model could always answer "not unfamiliar"; the instruction now requires the search on EVERY
	// execution, and makes recording a discovered workaround non-optional. This platform's own
	// FACTS LEARNED ledger shows the same sandbox lessons being rediscovered across 200+ runs.
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
