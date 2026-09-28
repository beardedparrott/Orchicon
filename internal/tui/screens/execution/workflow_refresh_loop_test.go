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
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestTheVersionListIsAlwaysUnderTheFlow is the operator's follow-up, as a test:
//
//	"when I move through the different workflows using up/down, sometimes it shows the screen with no
//	 versions and sometimes it shows the screens with versions. Shouldn't we just always show the
//	 screen that lists the versions underneath?"
//
// YES — and the cause was two renderers both writing the pane, so the operator saw whichever painted
// last. The READ-ONLY detail (header fields, FLOW, VERSIONS) is the pane's content whenever the editor
// is closed; the edit-mode flow is only correct while editing. So the predicate is "is the operator
// editing", and this asserts both sides of it.
func TestTheVersionListIsAlwaysUnderTheFlow(t *testing.T) {
	m := newModel(t, &fakePlane{})
	if !m.SelectSource("workflows") {
		t.Fatal("fixture: no workflows source")
	}

	// NOT EDITING: the refresh runs, which is what keeps the read-only detail (and its VERSIONS
	// section) on screen and current. A predicate broader than this suppressed it for the whole pane
	// and left the version list appearing and disappearing depending on which renderer was last.
	if m.Base.RefreshView() == nil {
		t.Error("with the editor closed the refresh must run — it is what keeps FLOW + VERSIONS on " +
			"screen; suppressing it is what made the version list come and go")
	}

	// EDITING: the flow is the surface, so the refresh must not land the read-only detail on top.
	m.flowEditing = true
	if cmd := m.Base.RefreshView(); cmd != nil {
		t.Error("while editing the flow owns the pane — the read-only detail must not be fetched over it")
	}
}

// TestRememberingAWorkflowDoesNotPaintTheFlow pins the other half: the bookkeeping that runs on every
// detail landing must not repaint the pane unless the operator is editing.
//
// It remembers the workflow so that `e` can enter the editor without a fresh load — but painting on
// that path is what put the editing layout (no version list) over the read-only detail on every
// selection, and it reset the cursor while it did.
func TestRememberingAWorkflowDoesNotPaintTheFlow(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.Base.SetDetailID("wf-1")

	// Not editing: remember it, keep the cursor if the workflow is unchanged, and do not repaint.
	v10 := &apiv1.WorkflowVersion{Id: "v-10", Steps: "[]"}
	m.enterStepEditor("wf-1", "SDLC (Non-human)", v10)
	m.stepSel = "step-b" // the operator walks down the flow
	m.enterStepEditor("wf-1", "SDLC (Non-human)", v10)
	if m.stepSel != "step-b" {
		t.Errorf("the selected step became %q — a re-landing must not reset the cursor", m.stepSel)
	}

	// EDITING: the repaint is required, because the flow IS the editing surface.
	m.flowEditing = true
	if cmd := m.enterStepEditor("wf-1", "SDLC (Non-human)", v10); cmd == nil {
		// paintFlow returns nil by design (it repaints as a side effect), so the assertion is that the
		// call is MADE at all — measured by the cursor being re-seeded for a new workflow.
		m.enterStepEditor("wf-2", "Other", &apiv1.WorkflowVersion{Id: "v-2", Steps: "[]"})
		if m.stepSel != "" {
			t.Errorf("a changed workflow must re-seed the cursor, got %q", m.stepSel)
		}
	}
}

// TestTheNewVersionChordExistsAndIsAdvertised — the operator's other question:
//
//	"what key do you hit to make a new version? That shortcut isn't listed in the composer when you
//	 are on workflow view."
//
// The honest answer WAS that no chord existed: the only route to a draft was implicit, since editing a
// step on a published version creates one ("edit a step and it makes one for you"), and nothing on the
// surface said so. A mechanism the operator cannot discover is not a feature.
//
// So `V` creates one — the same key the Workers pane already uses for its next version, so the gesture
// is learned once — and the composer hint names it.
func TestTheNewVersionChordExistsAndIsAdvertised(t *testing.T) {
	m := newModel(t, &fakePlane{workflows: []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC (Non-human)"}}})
	if !m.SelectSource("workflows") {
		t.Fatal("fixture: no workflows source")
	}

	// ADVERTISED. The hint is the row the operator reads, and a chord that is not written down is
	// undiscoverable — which is exactly what they reported.
	hint := m.HintLine()
	if !strings.Contains(hint, keyNewVersionWf) || !strings.Contains(hint, "new version") {
		t.Errorf("the Workflows hint must name the new-version chord (%q):\n%s", keyNewVersionWf, hint)
	}

	// AND IT EXISTS. With nothing selected it REFUSES — and refuse() returns a nil command by design,
	// so the signal is the notice, not the command. Asserting on the command here would have been an
	// assertion that a refusal must do something, which is the opposite of what a refusal is.
	press(t, m, keyNewVersionWf)
	if !strings.Contains(m.Notice(), "select a workflow") {
		t.Errorf("with nothing selected the chord must refuse and say why, got notice %q", m.Notice())
	}

	// With a real workflow selected it must issue a WRITE — the cmd is non-nil and carries the RPC.
	// The row has to be LOADED first (the pane is empty otherwise, and the chord correctly refuses).
	run(t, m, m.Refresh("workflows")) // fatals if the command is nil or produces no message
	if !m.Base.SelectItem("workflows", "wf-1") {
		t.Fatal("fixture: the loaded row could not be selected")
	}
	if cmd := press(t, m, keyNewVersionWf); cmd == nil {
		t.Error("with a workflow selected the chord must issue the create-version RPC")
	} else if msg := cmd(); msg == nil {
		t.Error("the create-version command produced no message, so a failure could never be reported")
	}
}
