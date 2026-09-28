package askorchicon

// quickwork_monitor_prompt_test.go — RUN-MONITORING DISCIPLINE IS IN THE PERSONA.
//
// Two real Quick Work dispatches reported a wedged run as healthy: the agent
// watched TokenUsage, saw a large frozen number, and narrated poll after poll as
// "still mid-turn" while HealthState had already gone to stalled — and its first
// error was never reported until the run had ended. The class lives in the
// prompt, so the rule is pinned here, in the same content-assertion style as the
// neighbouring prompt tests, so a future edit cannot quietly drop it.

import (
	"strings"
	"testing"
)

// The persona must teach the agent which signal says a run is ALIVE, that
// staleness is the agent's own call to make, that an error is reported the
// moment it appears, and that the run's own words matter more than its
// counters — plus why an early commit is what makes work survive.
func TestQuickWorkPromptEnforcesRunMonitoring(t *testing.T) {
	p := BuildSystemPrompt(modeQuickWork, testAgentConfig(), NewToolRegistry(nil, nil, nil))
	for _, want := range []string{
		// HealthState is the authoritative liveness signal, not the token count.
		"HealthState is the authoritative liveness signal",
		"A large TokenUsage is NOT evidence of progress",
		"a dead run, not a busy one",
		// Declare staleness yourself; name what to compare.
		"DECLARE STALENESS YOURSELF",
		"WorktreeStatus, WorktreePath and WorktreeBranch",
		"STOP reporting it as working",
		// Surface ErrorMessage the moment it is non-empty; the first is often the more diagnostic.
		"Report ErrorMessage the MOMENT it is non-empty",
		"the FIRST error is usually the more diagnostic one",
		// Read the run's own output, not only its counters.
		"the execution's Output and its error",
		// Why committing early matters: a reaped worktree takes uncommitted work with it.
		"committed and pushed work is the ONLY work that survives",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("quick work prompt missing %q", want)
		}
	}
}

// The dispatch-brief contract must require each worker brief to carry the rules
// that stop a dead session from costing the work: commit before verify, an
// explicit shell timeout, a narrowed build/test scope, and non-interactive gh.
func TestQuickWorkDispatchBriefsCarryTheSurvivalRules(t *testing.T) {
	var b strings.Builder
	writeQuickWorkDispatchRules(&b)
	got := b.String()
	for _, want := range []string{
		"commit → push → verify",
		"timeout_seconds",
		"never `go build ./...` or `go test ./...` as a first action",
		"NON-INTERACTIVE `gh` only",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("quick work dispatch brief rules missing %q", want)
		}
	}
}
