package work

// autostart_test.go — Concern 1: a work item with NO WORKFLOW BOUND cannot be given auto-start,
// through EITHER work-item form, and the refusal is visible where the operator is looking.
//
// Task A makes such an item permanently unrunnable — every transition to ready / assigned /
// scheduled / running is rejected and the reconciler backstops fail loudly — so the client must not
// offer the state at all. The rule implemented is "auto-start requires a bound workflow" (see
// autoStartRefusal in workitems.go for why it is not "workflow is Required").
//
// These tests drive the REAL forms through the real submit path: the assertion is on the refusal
// that reaches the operator (the form's own error line and the composer dock) and on the request the
// plane would have received (none), not on internal fields alone.

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// autoStartPlane is the smallest real plane that can open both work-item forms: one project (a form
// cannot be built without one) and one item with NO workflow bound.
func autoStartPlane(t *testing.T) (*fakePlane, *Model) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-a", Title: "Undeclared work", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return p, m
}

// The create form: tick auto-start with an empty Workflow picker and the write is REFUSED — the form
// stays open, says why inside itself, and the dock carries the same sentence.
// THE CREATE FORM TAKES NO GATE. The server says so in its own words —
//
//	"The CREATE path needs no gate — new items always start pending."
//	                                        — workitem.IsStartableForAutoStart
//
// — because the plane gates on TRANSITION to a runnable status (ready/assigned/scheduled/running)
// and a new item is PENDING. This test used to assert the opposite (a refusal), which is exactly
// what blocked the operator: "I tried to kick off a feature and it denied me in the TUI saying
// that it has to have a workflow set, but that is incorrect."
//
// A create form ALSO cannot know whether the item will become a parent, so the client has no
// grounds to decide — the plane is the right decider, and it already accepts this.
func TestCreateFormAllowsAutoStartWithNoWorkflow(t *testing.T) {
	p, m := autoStartPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)

	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	if f.Values["workflow"] != "" {
		t.Fatalf("the create form must start with no workflow bound, got %q", f.Values["workflow"])
	}
	f.Set("title", "No workflow, auto-start ticked")

	// THE BOX MUST BE HOLDABLE. The coupling used to clear it the instant the workflow was
	// empty, so the operator could not even express the state the plane accepts — the second
	// half of the same defect, and it would have survived a fix to the refusal alone.
	f.Set("auto_start", "true")
	if got := f.Values["auto_start"]; got != "true" {
		t.Fatalf("the create form must HOLD auto-start with no workflow (got %q) — the plane "+
			"accepts it (a new item is pending), so the client must not silently untick it", got)
	}

	// AND THE SAVE MUST GO THROUGH, driven via the real ctrl+s path (submit returns the command
	// without running it, so it must be executed for the write to land).
	run(t, m, submit(t, m, "auto_start"))
	if len(p.created) != 1 {
		t.Fatalf("auto-start with no workflow must be creatable (the plane's own rule), got %d "+
			"request(s); SubmitErr=%q", len(p.created), f.SubmitErr)
	}
	if req := p.created[0]; !req.GetAutoStartWorkflow() {
		t.Fatal("the create request must carry auto-start as the operator set it")
	}
}

// The edit form: same rule, and the same two surfaces.
func TestEditFormRefusesAutoStartWithoutWorkflow(t *testing.T) {
	p, m := autoStartPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	f.Set("auto_start", "true")

	submit(t, m, "auto_start")

	if len(p.updated) != 0 {
		t.Fatalf("a refused combination must not save anything, got %d request(s)", len(p.updated))
	}
	if m.ActiveForm() == nil {
		t.Fatal("a refused submit must keep the edit form OPEN")
	}
	if !strings.Contains(f.SubmitErr, "needs a workflow") {
		t.Fatalf("SubmitErr = %q", f.SubmitErr)
	}
	if got := f.View(); !strings.Contains(got, "needs a workflow") {
		t.Fatalf("the refusal must be drawn inside the edit form:\n%s", got)
	}
	if len(shell.errors) == 0 || !strings.Contains(shell.errors[0], "needs a workflow") {
		t.Fatalf("the refusal must reach the dock, errors = %v", shell.errors)
	}
}

// The COUPLING half of the rule, on the path where it still applies: the EDIT form.
//
// Emptying the workflow CLEARS auto-start there, so the value cannot be HELD and submitted from a
// state the operator can no longer see. This is exactly why the coupling and the refusal must carry
// the SAME condition — on the CREATE form neither applies (the plane gates on transition, and a new
// item is pending), so the box must be holdable; on an EDIT of a LEAF they both do.
func TestClearingTheWorkflowClearsAutoStartOnEdit(t *testing.T) {
	p, m := autoStartPlane(t)
	// A LEAF: no children, so neither exemption applies.
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}

	// The picker carries the workflows the plane lists (the fake returns wf-1 / Fanout).
	f.Set("workflow", "wf-1")
	f.Set("auto_start", "true")
	if got := f.Values["auto_start"]; got != "true" {
		t.Fatalf("auto-start must be held while a workflow is bound, got %q", got)
	}

	f.Set("workflow", "") // the "— none —" option
	if got := f.Values["auto_start"]; got != "false" {
		t.Fatalf("emptying the workflow must clear auto-start on a leaf, got %q", got)
	}

	// And it cannot be re-held: tick it once more and the form refuses, because a LEAF with
	// no workflow cannot be moved to a runnable status.
	f.Set("auto_start", "true")
	// A REFUSED submit returns NO command (there is nothing to run), so this is a plain
	// `submit` — the refusal is asserted on the form, not on a write that never happened.
	submit(t, m, "auto_start")
	if len(p.updated) != 0 {
		t.Fatalf("a leaf must not be re-holdable without a workflow, %d request(s)", len(p.updated))
	}
	if !strings.Contains(f.SubmitErr, "needs a workflow") {
		t.Fatalf("SubmitErr = %q, want the leaf refusal", f.SubmitErr)
	}
}

// AC2: the SERVER's rejection — the sentence Task A's enforcement produces — lands on the dock, not
// merely in the screen's notice line that FitLines truncates. The local guard must NOT be what
// refuses here (a bound workflow makes the combination legal), so the assertion is specifically that
// the plane's own wording arrives.
func TestServerRejectionLandsOnTheDock(t *testing.T) {
	p, m := autoStartPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)
	p.setUpdateErr(errors.New(`Cannot schedule "wi-a": no workflow is set, so there is nothing to run.`))

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	f.Set("workflow", "wf-1")
	f.Set("auto_start", "true")

	run(t, m, press(t, m, "ctrl+s"))

	if f.SubmitErr != "" {
		t.Fatalf("the local guard must not be what refused: SubmitErr = %q", f.SubmitErr)
	}
	if len(shell.errors) == 0 || !strings.Contains(shell.errors[0], "no workflow is set") {
		t.Fatalf("the server's rejection must reach the dock, errors = %v", shell.errors)
	}
	if got := m.Notice(); !strings.HasPrefix(got, "✗ ") {
		t.Fatalf("the screen's notice must mark the failure, got %q", got)
	}
	// A rejected write is not recorded, and the item is unchanged.
	if len(p.updated) != 0 {
		t.Fatalf("a rejected update must not be recorded, got %d", len(p.updated))
	}
	it, ok := p.items["wi-a"]
	if !ok {
		t.Fatal("the item disappeared")
	}
	if it.GetAutoStartWorkflow() || it.GetWorkflowId() != "" {
		t.Fatalf("a rejected write must leave the row alone: auto_start=%v workflow=%q",
			it.GetAutoStartWorkflow(), it.GetWorkflowId())
	}
}

// A SEQUENCE PARENT IS EXEMPT — the operator's report, and the client disagreeing with the plane.
//
//	"I tried to kick off a feature and it denied me in the TUI saying that it has to have a
//	 workflow set, but that is incorrect. Parents should not have a workflow set in order to fire
//	 off the children."
//
// The server always exempted a parent (ValidateWorkflowFirstTransition: "A sequence PARENT with
// children is exempt: it is a container that contributes ordering only and never executes itself"),
// and the TUI refused anyway — so the plane would have accepted a save the client blocked.
//
// These pin BOTH halves of the rule on a parent, because fixing only the refusal would leave the
// operator able to save the state but unable to HOLD it: the coupling would silently untick the box
// the moment the workflow was emptied.
func TestASequenceParentNeedsNoWorkflowForAutoStart(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// A FEATURE with a child — a container. Its own binding is inert (its children each run
	// their own), which is exactly why the server exempts it.
	p.addItem(&apiv1.WorkItem{
		Id: "feat", Title: "A feature", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	p.addItem(&apiv1.WorkItem{
		Id: "kid", Title: "Its child", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ParentId: "feat", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// The screen must know it is a parent: that fact is what the exemption keys on.
	if !m.itemHasChildren("feat") {
		t.Fatal("the screen must recognise an item with children as a sequence parent")
	}
	if m.itemHasChildren("kid") {
		t.Fatal("a leaf must not be treated as a parent")
	}

	if !m.SelectItem(srcWorkItems, "feat") {
		t.Fatal("could not select the feature")
	}
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}

	// HALF 1 — the coupling must not strip the box on a parent.
	f.Set("auto_start", "true")
	f.Set("workflow", "") // "— none —"
	if got := f.Values["auto_start"]; got != "true" {
		t.Fatalf("a parent's auto-start was cleared by emptying the workflow (got %q) — the "+
			"coupling must carry the same exemption as the refusal, or the operator cannot even "+
			"HOLD the state the plane accepts", got)
	}

	// HALF 2 — the submit must go through, driven through the REAL ctrl+s path.
	//
	// submit() drives ctrl+s and returns the command WITHOUT running it, so the write only lands
	// when the command is executed — which is what `run` does here. Asserting on p.updated
	// without running it would report a refusal that never happened (it did, on the first draft
	// of this test).
	run(t, m, submit(t, m, "auto_start"))
	if len(p.updated) == 0 {
		t.Fatalf("the save was refused (SubmitErr=%q): the plane exempts a parent, so the client "+
			"must not block it", f.SubmitErr)
	}
}

// AND THE LEAF RULE STANDS: the exemption must not have opened the door for a leaf, which is the
// item that genuinely has nothing to run.
func TestALeafStillNeedsAWorkflowForAutoStart(t *testing.T) {
	p, m := autoStartPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	f.Set("auto_start", "true")
	submit(t, m, "auto_start")

	if len(p.updated) != 0 {
		t.Fatalf("a leaf with auto-start and no workflow must still be refused, got %d request(s)",
			len(p.updated))
	}
	if !strings.Contains(f.SubmitErr, "needs a workflow") {
		t.Fatalf("SubmitErr = %q, want the leaf refusal", f.SubmitErr)
	}
}
