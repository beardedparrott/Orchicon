package work

// edit_reference_seed_test.go — THE EDIT FORM'S REFERENCE PICKERS MUST BE SEEDED FROM THE ITEM.
//
// The operator: "when you edit a work item, it shows the workflow as none."
//
// It was not a display shortfall. Every other field in the detail editor is seeded from the item
// (title, description, acceptance, kind, status, priority, budgets, context window, context files,
// scheduled start, auto-start) and the two REFERENCE pickers were the only exceptions — no
// `Initial:` on either spec. `Initial:` is the only thing that populates a form's value map
// (kit2.NewForm writes f.Values[name] only when Initial != ""), and the only other writer is a
// deliberate pick. So the pickers opened blank and, because the submit handler reads the value map
// straight through, EVERY save sent workflow_id="" and runtime_image="" — the proto's spelling for
// unbind/clear. An operator editing only the priority unbound the workflow driving the item and
// cleared the image it runs in, with no error and nothing on screen to suggest it happened.
//
// pickerOptsWithCurrent is NOT the fix and was NOT changed: its guarantee (the item's value is
// present in the option list) is sound, and its synthetic-option branch is what keeps a since-deleted
// workflow from being dropped. It does not SELECT anything, so it cannot seed the form. The defect
// was the missing seed.
//
// WHAT IS PINNED HERE. Both option-list branches (the primary assertion is (a), the normal case that
// fails today), asserted on the RECORDED UpdateWorkItem REQUEST rather than on the rendering alone —
// a field can render correctly and still submit empty. Plus the no-false-positive direction: an item
// that genuinely has no reference must still open on "— none —" and still send empty.
//
// THE AUDIT (the class, not the instance). Every FieldSpec in the work screen's forms was checked for
// the same omission. The two seeds added to editFormFor are the only instances:
//   - newItemCreateForm (workitems.go): workflow Initial:"" and runtime_image/parent unseeded are
//     CORRECT — a new item has no reference yet, and empty is what the create request should carry.
//   - newItemStatusForm: status + priority both seeded.
//   - projects.go: runtimeImageField seeds Initial: current; ProjectMCPField seeds Initial: the
//     comma-joined selection; every other project field seeds. project_dir seeds.
//   - images.go: the edit form seeds every field; the create form is legitimately empty.
// None of these needed a change, so none was made.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// openItemEditorFor builds a plane with a project (and, when seedImage, the image the fake plane
// lists as "go-node:latest"), ONE work item carrying the given workflow id + runtime image, and opens
// its detail editor with 'e'. It returns the recorded-request plane alongside the editor.
func openItemEditorFor(t *testing.T, workflowID, runtimeImage string, seedImage bool) (*Model, *fakePlane, *kit2.Form) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	if seedImage {
		p.seedImage("img-1", "go-node", apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY)
	}
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		WorkflowId: workflowID, RuntimeImage: runtimeImage,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the detail editor")
	}
	return m, p, f
}

// optionValues returns a picker field's option values, in order, so a test can pin WHICH branch of
// pickerOptsWithCurrent it is looking at.
func optionValues(f *kit2.Form, name string) []string {
	out := []string{}
	for _, o := range f.Spec(name).Options {
		out = append(out, o.Value)
	}
	return out
}

// hasCurrentOption reports whether the field carries a synthetic "… (current)" option — the branch
// pickerOptsWithCurrent takes when the item's value is ABSENT from the fetched list.
func hasCurrentOption(f *kit2.Form, name string) bool {
	for _, o := range f.Spec(name).Options {
		if strings.Contains(o.Label, "(current)") {
			return true
		}
	}
	return false
}

// ------------- branch (a): the value IS among the loaded options (the normal case) -------------

// The primary assertion. The item's workflow is `wf-1` — exactly what the fake plane's ListWorkflows
// returns — so pickerOptsWithCurrent returns the option list UNCHANGED with "— none —" first. Nothing
// selects the item's value except the spec's seed, which is why this is the case that fails today.
func TestEditFormSeedsReferencesWhenListed(t *testing.T) {
	m, p, f := openItemEditorFor(t, "wf-1", "go-node:latest", true)

	// The form's VALUE map holds the item's existing references — not "".
	if got := f.Values["workflow"]; got != "wf-1" {
		t.Fatalf("the editor's workflow value = %q, want the item's wf-1: a blank value is what the "+
			"save sends, unbinding the workflow the operator never touched", got)
	}
	if got := f.Values["runtime_image"]; got != "go-node:latest" {
		t.Fatalf("the editor's runtime-image value = %q, want the item's go-node:latest", got)
	}

	// And the field SHOWS the human name, not "— none —" or a bare id.
	if got := f.DisplayForTest("workflow"); got != "Fanout" {
		t.Fatalf("the Workflow field renders %q, want the workflow's NAME %q", got, "Fanout")
	}
	if got := f.DisplayForTest("runtime_image"); got != "go-node (go-node:latest)" {
		t.Fatalf("the Runtime image field renders %q, want the image's label", got)
	}

	// This is branch (a): the value is present, so the list is untouched — no synthetic option.
	if hasCurrentOption(f, "workflow") || hasCurrentOption(f, "runtime_image") {
		t.Fatalf("the listed branch must not prepend a synthetic option: workflow=%v image=%v",
			optionValues(f, "workflow"), optionValues(f, "runtime_image"))
	}
	if got := optionValues(f, "workflow"); len(got) == 0 || got[0] != "" {
		t.Fatalf("workflow options = %v, want \"— none —\" first", got)
	}

	// SAVE WITHOUT TOUCHING EITHER PICKER. Asserted on the RECORDED REQUEST: this is the acceptance
	// criterion, and it is the half a rendering assertion cannot reach.
	run(t, m, submit(t, m, "auto_start"))
	if len(p.updated) == 0 {
		t.Fatal("saving the editor must send an UpdateWorkItem")
	}
	req := p.updated[len(p.updated)-1]
	if req.GetWorkflowId() != "wf-1" {
		t.Fatalf("saving without touching the workflow picker sent workflow_id=%q, want the item's "+
			"existing %q — this is the silent unbind", req.GetWorkflowId(), "wf-1")
	}
	if req.GetRuntimeImage() != "go-node:latest" {
		t.Fatalf("saving without touching the image picker sent runtime_image=%q, want %q",
			req.GetRuntimeImage(), "go-node:latest")
	}
}

// ------------- branch (b): the value is ABSENT from the loaded options -------------

// A since-deleted workflow: pickerOptsWithCurrent prepends the synthetic "workflow wf-gone (current)"
// option. That branch worked by accident before the fix only in the sense that the value was LISTED —
// it was still not SELECTED, so the form opened blank and saved empty here too.
func TestEditFormSeedsReferencesWhenDelisted(t *testing.T) {
	m, p, f := openItemEditorFor(t, "wf-gone", "ghost:latest", false)

	if got := f.Values["workflow"]; got != "wf-gone" {
		t.Fatalf("the editor's workflow value = %q, want the delisted item's wf-gone", got)
	}
	if got := f.Values["runtime_image"]; got != "ghost:latest" {
		t.Fatalf("the editor's runtime-image value = %q, want ghost:latest", got)
	}

	// The synthetic branch is in play and the field shows it rather than "— none —".
	if !hasCurrentOption(f, "workflow") {
		t.Fatalf("the delisted branch must prepend the synthetic option: %v", optionValues(f, "workflow"))
	}
	if got := optionValues(f, "workflow"); len(got) == 0 || got[0] != "wf-gone" {
		t.Fatalf("workflow options = %v, want the synthetic wf-gone first", got)
	}
	if got := f.DisplayForTest("workflow"); got != "workflow wf-gone (current)" {
		t.Fatalf("the Workflow field renders %q, want the synthetic current label", got)
	}
	if got := f.DisplayForTest("runtime_image"); got != "image ghost:latest (current)" {
		t.Fatalf("the Runtime image field renders %q, want the synthetic current label", got)
	}

	run(t, m, submit(t, m, "auto_start"))
	req := p.updated[len(p.updated)-1]
	if req.GetWorkflowId() != "wf-gone" || req.GetRuntimeImage() != "ghost:latest" {
		t.Fatalf("saving sent workflow_id=%q runtime_image=%q, want the item's delisted references "+
			"preserved", req.GetWorkflowId(), req.GetRuntimeImage())
	}
}

// ------------- no false positive: an unset reference stays unset -------------

// The fix must not INVENT a binding. An item with no workflow and no image still opens on the empty
// option and still sends empty — clearing a reference on purpose has to remain possible.
func TestEditFormDoesNotInventAReference(t *testing.T) {
	m, p, f := openItemEditorFor(t, "", "", true)

	if got := f.Values["workflow"]; got != "" {
		t.Fatalf("an unbound item's workflow value = %q, want the empty option", got)
	}
	if got := f.Values["runtime_image"]; got != "" {
		t.Fatalf("an image-less item's runtime-image value = %q, want the empty option", got)
	}
	if got := f.DisplayForTest("workflow"); got != "— none —" {
		t.Fatalf("an unbound item's Workflow field renders %q, want \"— none —\"", got)
	}
	if got := f.DisplayForTest("runtime_image"); got != "— none (base image) —" {
		t.Fatalf("an image-less item's Runtime image field renders %q, want \"— none (base image) —\"", got)
	}
	if hasCurrentOption(f, "workflow") || hasCurrentOption(f, "runtime_image") {
		t.Fatal("an empty value must not gain a synthetic option")
	}

	run(t, m, submit(t, m, "auto_start"))
	req := p.updated[len(p.updated)-1]
	if req.GetWorkflowId() != "" || req.GetRuntimeImage() != "" {
		t.Fatalf("saving an unbound item sent workflow_id=%q runtime_image=%q, want both empty — "+
			"the fix must not invent a reference", req.GetWorkflowId(), req.GetRuntimeImage())
	}
}
