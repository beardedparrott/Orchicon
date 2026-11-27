package automation

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS TEST EXISTS.
//
// The operator asked for the Schedules page's top-bar run-control buttons to have keyboard
// equivalents in the TUI, with spacebar multi-select. Space marking already existed (kit2.Base), so
// the work was three keys — and the risk is not that they are missing, it is that they silently
// override a key another source already owns.
//
// 'R' was already the Idea Cloud's "reject every listed idea". A bare `case "R":` added for
// "resume the marked schedules" would have made that triage gesture unreachable, and a worker-facing
// screen quietly losing a bulk reject is not a visible failure — it just stops happening.
//
// So: the run-control keys must be scoped to the recurring source, and the Idea Cloud must keep A/R.

func TestScheduleBulkKeysDoNotStealIdeaCloudTriage(t *testing.T) {
	src := readAutomationSource(t)

	// The Idea Cloud's triage stays reachable.
	if !strings.Contains(src, `case "A":`) || !strings.Contains(src, "m.acceptAllIdeas()") {
		t.Error("the Idea Cloud's accept-all (A) is gone")
	}
	if !strings.Contains(src, "m.rejectAllIdeas()") {
		t.Error("the Idea Cloud's reject-all is gone — 'R' was reassigned to schedule resume")
	}

	// And the run-control keys are guarded by the recurring source.
	for _, key := range []string{`case "P", "R", "S":`, "if src == srcRecurring {", "bulkControlRecurring"} {
		if !strings.Contains(src, key) {
			t.Errorf("the schedule bulk run-control wiring is missing %q", key)
		}
	}
}

// TestBulkControlRefusesAnEmptySelection: acting on the cursor when nothing is marked is how an
// operator pauses one item believing they paused six.
func TestBulkControlRefusesAnEmptySelection(t *testing.T) {
	src := readAutomationSource(t)
	i := strings.Index(src, "func (m *Model) bulkControlRecurring")
	if i < 0 {
		t.Fatal("bulkControlRecurring is missing")
	}
	body := src[i:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "len(ids) == 0") || !strings.Contains(body, "refuseAutomation") {
		t.Error("bulkControlRecurring does not refuse an empty selection — it would act on the focused row")
	}
	// Destructive fire must confirm, because it discards progress on rows that already ran.
	if !strings.Contains(body, "confirmNeeded") || !strings.Contains(body, "kit2.Confirm") {
		t.Error("firing marked schedules does not confirm — it discards progress on anything already run")
	}
}

func readAutomationSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("screen.go")
	if err != nil {
		t.Fatalf("read screen.go: %v", err)
	}
	return string(b)
}
