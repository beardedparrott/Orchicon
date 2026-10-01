package work

// autostart_test.go — AUTO-START IS NOT GATED ON A WORKFLOW, in either work-item form.
//
// The operator, after three failed attempts to make this validation go away:
//
//	"We shouldn't have the validation at all no matter what the circumstance is. If it is a
//	 feature, epic, or even a task that has children then it shouldnd't be blocking me. Workflows
//	 need to be empty for a sequential workflow to kick off. We just need to remove that validation
//	 from creating new items AND editing current items that already exist."
//
// He is right, and the client's rule was wrong at its PREMISE rather than at its edges. The server
// never asks "does this item have a workflow?" — it asks it only at a TRANSITION into a runnable
// status (ready/assigned/scheduled/running), and it exempts a sequence parent even then:
//
//	if !runnableWorkflowStatuses[newStatus] { return nil }   // pending is not runnable
//	if hasChildren { return nil }
//	if workflowID != "" { return nil }
//	return error("Cannot move … to …: no workflow is set…")
//
// The client applied it to EVERY save, so it refused writes that transitioned nothing. That is why
// each earlier fix — exempt the parent, then exempt create — moved the boundary instead of removing
// it: they widened an exemption whose premise was wrong.
//
// So the rule is GONE from both clients: no refusal, no advisory, and no coupling. The coupling
// mattered as much as the refusal, because emptying the workflow must NOT clear auto-start — a
// workflow-less parent is precisely how a sequential workflow is kicked off.
//
// These tests drive the REAL forms through the real ctrl+s path and assert on what the PLANE
// received, not on internal fields.

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
// THE EDIT FORM TAKES NO GATE EITHER, on ANY kind — leaf, feature, epic or parent.
//
// This replaces a test that asserted the opposite. The old rule refused "auto-start with no
// workflow" on every save; the server asks that question only at a transition into a runnable
// status, so the client was refusing writes that transitioned nothing.
func TestEditFormAllowsAutoStartWithNoWorkflow(t *testing.T) {
	for _, kind := range []apiv1.WorkItemKind{
		apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE,
		apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
	} {
		p := newPlane()
		p.seedProject("proj-1", "Orchicon")
		p.addItem(&apiv1.WorkItem{
			Id: "wi-a", Title: "Undeclared work", Kind: kind,
			ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1,
		})
		m := newModel(t, p)
		m.SelectSource(srcWorkItems)
		load(t, m, srcWorkItems)

		run(t, m, press(t, m, "e"))
		f := m.ActiveForm()
		if f == nil {
			t.Fatalf("%s: e must open the edit form", kind)
		}

		// THE BOX MUST BE HOLDABLE with no workflow bound: "Workflows need to be empty for a
		// sequential workflow to kick off."
		f.Set("auto_start", "true")
		f.Set("workflow", "")
		if got := f.Values["auto_start"]; got != "true" {
			t.Fatalf("%s: auto-start was CLEARED by an empty workflow (got %q) — a workflow-less "+
				"parent is exactly how a sequential run is kicked off", kind, got)
		}

		// AND THE SAVE MUST GO THROUGH, asserting on what the PLANE received.
		run(t, m, submit(t, m, "auto_start"))
		if len(p.updated) == 0 {
			t.Fatalf("%s: the save was refused (SubmitErr=%q) — the client must not gate auto-start "+
				"on a workflow at all", kind, f.SubmitErr)
		}
		if req := p.updated[0]; !req.GetAutoStartWorkflow() {
			t.Fatalf("%s: the request must carry auto-start as the operator set it", kind)
		}
	}
}

// THE COUPLING IS GONE TOO, and it is the half that would be forgotten.
//
// Clearing auto-start when the workflow was emptied is not a neutral safety net: it removes the
// exact state a sequential run needs. This asserts the box SURVIVES an emptied workflow on both
// forms, which is the behaviour the operator asked for and the one neither earlier fix delivered.
func TestAnEmptyWorkflowDoesNotClearAutoStart(t *testing.T) {
	_, m := autoStartPlane(t)

	for _, open := range []struct {
		name string
		key  string
	}{{"create", "n"}, {"edit", "e"}} {
		run(t, m, press(t, m, open.key))
		f := m.ActiveForm()
		if f == nil {
			t.Fatalf("%s: form did not open", open.name)
		}
		// Bind a workflow first, so the clear would have something to fire on.
		f.Set("workflow", "wf-1")
		f.Set("auto_start", "true")
		if got := f.Values["auto_start"]; got != "true" {
			t.Fatalf("%s: auto-start must be held while a workflow is bound, got %q", open.name, got)
		}
		// Empty it — the coupling used to untick the box right here.
		f.Set("workflow", "")
		if got := f.Values["auto_start"]; got != "true" {
			t.Fatalf("%s: emptying the workflow CLEARED auto-start (got %q) — that removes the "+
				"state a sequential workflow needs", open.name, got)
		}
		m.Base.Update(tea.KeyMsg{Type: tea.KeyEsc})
	}
}

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

// THE PLANE IS STILL THE DECIDER, and the client must not pre-empt it.
//
// Removing the client guard does NOT remove the server's rule: a transition into a runnable status
// with no workflow is still refused by the plane, and that refusal must reach the operator on the
// composer dock (the transport FitLines never truncates) rather than being swallowed or faked by a
// client-side check.
func TestThePlanesOwnRefusalStillReachesTheDock(t *testing.T) {
	p, m := autoStartPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)
	p.setUpdateErr(errors.New(`Cannot move "Undeclared work" to "ready": no workflow is set, so there is nothing to run. Bind a workflow first.`))

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	f.Set("auto_start", "true")
	f.Set("status", "ready") // the transition the plane DOES gate on

	run(t, m, press(t, m, "ctrl+s"))

	if f.SubmitErr != "" {
		t.Fatalf("the CLIENT must not be what refused: SubmitErr = %q", f.SubmitErr)
	}
	if len(shell.errors) == 0 || !strings.Contains(shell.errors[0], "no workflow is set") {
		t.Fatalf("the plane's own rejection must reach the dock, errors = %v", shell.errors)
	}
	if len(p.updated) != 0 {
		t.Fatalf("a rejected update must not be recorded, got %d", len(p.updated))
	}
}
