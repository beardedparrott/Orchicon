package execution

// workflow_refresh_loop_test.go — the pane that flashed between two screens.
//
// The operator: "It is just the detail view. It seems to be going back and forth between two screens
// very quickly", with two captures alternating between the READ-ONLY detail (header fields,
// "FLOW v10 published · 9 steps", VERSIONS) and the EDIT-MODE flow (workflow / steps / "editing …").
//
// The loop, read out of the real message chain:
//
//	the rolling refresh (every 5s, refresh.go)
//	  → kit2's default RefreshView re-requests the read-only detail for the selected row
//	    → detailMsg lands → Base paints the READ-ONLY detail, then calls the onDetail hook
//	      → onDetailWorkflow does two RPCs → workflowEditorMsg
//	        → enterStepEditor → paintFlow → paints the FLOW
//
// So the pane's body was written TWICE per tick, by two renderers, with the flow second — and any
// interleaving that let the read-only land last showed the other screen. Worse, and not visible in a
// screenshot: enterStepEditor re-seeded the cursor, so the operator's selected step was jerked back
// to the first one every five seconds.
//
// TWO FIXES, and this file measures both.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestTheFlowOwnsThePaneWhileAWorkflowIsShown pins the PREDICATE.
//
// It is "is the flow what this pane shows?", not "is the operator editing?" — the narrower question
// was my first attempt and it is why the flashing survived a rebuild, because remembering a workflow
// paints the flow WITHOUT entering the edit mode.
func TestTheFlowOwnsThePaneWhileAWorkflowIsShown(t *testing.T) {
	m := newModel(t, &fakePlane{})

	// Nothing remembered yet: no owner, so the refresh behaves as it always did.
	if m.Base.RefreshView() == nil {
		t.Fatal("with no workflow shown the refresh must still run")
	}

	// Remember a workflow, as the detail-landing chain does.
	m.stepWorkflowID = "wf-1"
	m.stepVersionID = "v-10"

	// Only while the WORKFLOWS source is the active one — stepWorkflowID is never cleared, so an
	// unscoped predicate would switch the refresh off for the REST of the session, on every pane.
	if !m.SelectSource("workers") {
		t.Fatal("fixture: no workers source")
	}
	if m.Base.RefreshView() == nil {
		t.Error("the flow must not mute the refresh on another pane — stepWorkflowID outlives this pane")
	}
	if !m.SelectSource("workflows") {
		t.Fatal("fixture: no workflows source")
	}
	if cmd := m.Base.RefreshView(); cmd != nil {
		t.Error("while the flow owns the pane the refresh must not run — it lands the READ-ONLY " +
			"detail on top of the flow, which is the pane flashing between two screens")
	}
}

// TestReEnteringTheSameStepEditorChangesNothing pins the IDEMPOTENCE, asserted on STATE.
//
// NOT on the returned command: paintFlow repaints as a SIDE EFFECT and always returns nil, so a test
// that treated "returned nil" as "did nothing" would be measuring nothing at all. What this function
// changes is the state it re-seeds — the workflow, the version, and the CURSOR — and that is what is
// checked here.
//
// The detail-landing chain runs on every refresh, so this was re-seeding the cursor once per tick: the
// operator's selected step was jerked back to the first one every five seconds, which a screenshot
// cannot show.
func TestReEnteringTheSameStepEditorChangesNothing(t *testing.T) {
	m := newModel(t, &fakePlane{})

	m.stepWorkflowID, m.stepWorkflowName = "wf-1", "SDLC (Non-human)"
	m.stepVersionID, m.stepSteps = "v-10", "a\nb"
	m.stepSel = "step-b" // the operator has walked down the flow

	// THE SAME workflow and version: the cursor must be left exactly where it was.
	m.enterStepEditor("wf-1", "SDLC (Non-human)", &apiv1.WorkflowVersion{Id: "v-10", Steps: "[]"})
	if m.stepSel != "step-b" {
		t.Errorf("the selected step became %q — the operator's cursor must survive a background "+
			"refresh", m.stepSel)
	}
	if m.stepVersionID != "v-10" {
		t.Errorf("version id = %q", m.stepVersionID)
	}

	// A DIFFERENT version is a real change (a draft's steps differ), so it must take effect.
	m.enterStepEditor("wf-1", "SDLC (Non-human)", &apiv1.WorkflowVersion{Id: "v-11", Steps: "[]"})
	if m.stepVersionID != "v-11" {
		t.Errorf("version id = %q after a different version — the change was swallowed", m.stepVersionID)
	}

	// And so is a different workflow.
	m.enterStepEditor("wf-2", "Other", &apiv1.WorkflowVersion{Id: "v-11", Steps: "[]"})
	if m.stepWorkflowID != "wf-2" || m.stepWorkflowName != "Other" {
		t.Errorf("workflow = %q/%q after a different one — the change was swallowed",
			m.stepWorkflowID, m.stepWorkflowName)
	}

	// A version-less landing (no published version to show) still has to take effect: its id is "",
	// and comparing "" against a real id must not be mistaken for "the same".
	m.stepVersionID = "v-11"
	m.enterStepEditor("wf-2", "Other", nil)
	if m.stepVersionID != "" {
		t.Errorf("version id = %q after a version-less landing — a nil version must clear it, not "+
			"be optimised away", m.stepVersionID)
	}
}
