package execution

// categories_test.go — the Categorize chord on the Workers and Workflows panes.
//
// The operator: "For categories, we could assign a key to create new category and a key to assign an
// item to a specific category." The screen's half of that is: resolve WHAT is selected, and hand it to
// the shell with the right target type. The shell's half (the modal, the list, the write) is tested
// there — and these tests assert the handover, which is the only part that lives in this package.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// categorizeSpy records what the shell was asked to categorize.
type categorizeSpy struct {
	entity string
	target apiv1.CategoryTargetType
	calls  int
}

func (s *categorizeSpy) OpenAssignCategory(entityID, _ string, target apiv1.CategoryTargetType) {
	s.calls++
	s.entity, s.target = entityID, target
}

// TestCategorizeChordOnTheWorkersPaneHandsTheSelectionToTheShell: `C` must pass the SELECTED worker
// and the WORKER target type, because the target type is what selects which groupings the picker may
// offer — getting it wrong would offer a conversation's groupings for a worker.
func TestCategorizeChordOnTheWorkersPaneHandsTheSelectionToTheShell(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "Sweeper"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	// THE ROW MUST EXIST. crudExec stubs the RPCs but seeds no rows, so without this the selection
	// resolves to nothing, categorizeSelected refuses, and the test reads as "the chord is not wired"
	// when the wiring is fine — the exact confusion this fixture comment exists to prevent.
	m.Base.LoadItems(srcWorkers, []kit2.Item{{ID: "w1", Title: "Sweeper"}}, "")
	spy := &categorizeSpy{}
	m.SetShell(spy)

	m.Base.SelectItem(srcWorkers, "w1")
	if _, handled := m.handleActionKey(keyCategorize); !handled {
		t.Fatal("C must be claimed by the Workers pane")
	}
	if spy.calls != 1 {
		t.Fatalf("the shell hook must be called exactly once, got %d", spy.calls)
	}
	if spy.entity != "w1" {
		t.Fatalf("the hook got entity %q, want the selected worker w1", spy.entity)
	}
	if spy.target != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER {
		t.Fatalf("the hook got target %v, want WORKER", spy.target)
	}
}

// TestCategorizeChordOnTheWorkflowsPaneUsesTheWorkflowTargetType: the same key, the other pane, the
// other grouping kind.
func TestCategorizeChordOnTheWorkflowsPaneUsesTheWorkflowTargetType(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	fp := &fakePlane{workflows: []*apiv1.Workflow{{Id: "wf1", Name: "Quick Work"}}}
	m = newModel(t, fp)
	_ = m.Base.SelectSource(srcWorkflows)
	// Same reason as the workers fixture above: the plane provides the type, LoadItems provides the
	// ROW — SelectItem only moves the cursor to a row that is already there.
	m.Base.LoadItems(srcWorkflows, []kit2.Item{{ID: "wf1", Title: "Quick Work"}}, "")
	spy := &categorizeSpy{}
	m.SetShell(spy)

	m.Base.SelectItem(srcWorkflows, "wf1")
	if _, handled := m.handleActionKey(keyCategorize); !handled {
		t.Fatal("C must be claimed by the Workflows pane")
	}
	if spy.entity != "wf1" {
		t.Fatalf("the hook got entity %q, want the selected workflow wf1", spy.entity)
	}
	if spy.target != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW {
		t.Fatalf("the hook got target %v, want WORKFLOW", spy.target)
	}
}

// TestCategorizeWithoutASelectionRefusesLoudly: a key that silently no-ops reads as a broken key.
func TestCategorizeWithoutASelectionRefusesLoudly(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	spy := &categorizeSpy{}
	m.SetShell(spy)
	m.notice = ""

	if _, handled := m.handleActionKey(keyCategorize); !handled {
		t.Fatal("C must be handled even with no selection (to refuse)")
	}
	if spy.calls != 0 {
		t.Fatal("the hook must not be called with nothing selected")
	}
	if m.notice == "" {
		t.Fatal("the refusal must say something")
	}
}

// TestCategorizeIsScopedToItsPanes: C means nothing on the other panes of this screen, so it must not
// be claimed there — otherwise a future binding would be silently stolen.
func TestCategorizeIsScopedToItsPanes(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcExecutions) {
		t.Fatal("fixture: could not focus the Executions pane")
	}
	spy := &categorizeSpy{}
	m.SetShell(spy)
	if _, handled := m.handleActionKey(keyCategorize); handled {
		t.Fatal("C must not be claimed outside the Workers and Workflows panes")
	}
}
