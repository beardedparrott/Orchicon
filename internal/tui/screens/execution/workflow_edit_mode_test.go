package execution

// workflow_edit_mode_test.go — the WORKFLOW EDIT MODE.
//
// The operator: "I want to hit 'e' and edit all steps and add steps, delete steps, etc. IN
// THIS SCREEN. I want the FULL visual editing to occur as it is much easier and makes more
// logical sense. Right now you have edit step, add step, and remove step as separate
// commands outside of a normal edit view. When someone hits 'e' to edit a workflow, they
// are going to think they are editing the entire workflow and all its steps at once, not in
// pieces. I see you can move up and down with the arrow keys to select a step. I think that
// was at least in the right direction. Though I think 'e' should be edit workflow mode (and
// 'n' new workflow should work like this as well), then you can move the arrow keys down,
// then hit 'enter' on the specific step to edit a step. Not sure how we can handle adding a
// step in this mode, maybe we can keep it 'a'."
//
// The mode is the whole point, so the tests below check BOTH halves: the keys work inside
// it, and they DO NOTHING outside it (that second half is what stops the step commands
// looking like top-level actions living outside an edit view).

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// keyPress builds the KeyMsg a named key produces, so the dispatch tests read as the keys
// the operator presses rather than as bubbletea types.
func keyPress(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

// modeModel loads the real SDLC fixture into the step editor AND establishes the rest of
// the state the app has when a workflow's flow is on screen: a SELECTED workflow row and a
// loaded detail.
//
// handleFlowKeys requires both because stepWorkflowID alone is stale during the window
// between selecting another workflow and its detail arriving — being in the mode there
// would edit the PREVIOUS workflow's steps while the pane showed something else. So the
// test has to reproduce the real state rather than bypass the guard.
func modeModel(t *testing.T) (*Model, *[]string) {
	t.Helper()
	m, writes := stepEditorModel(t)
	if !m.Base.SelectSource(srcWorkflows) {
		t.Fatal("fixture: could not focus the Workflows pane")
	}
	if !m.Base.LoadItems(srcWorkflows, []kit2.Item{{ID: "wf-1", Title: "SDLC (Human)", Meta: "published"}}, "") {
		t.Fatal("fixture: could not load the workflow row")
	}
	if !m.Base.SelectItem(srcWorkflows, "wf-1") {
		t.Fatal("fixture: could not select the workflow row")
	}
	// Resolve the detail through the base's own path, so detailID is set exactly as a real
	// selection sets it. The detail FUNCTION is stubbed (no plane here) and left in place:
	// paintFlow writes the pane through SetDetailContent, so the stub cannot mask what these
	// tests assert about the pane.
	m.Base.SetDetail(func(context.Context, string, string) (string, []kit2.Field, string, error) {
		return "Workflow: wf-1", nil, "", nil
	})
	cmd := m.Base.RequestDetail(srcWorkflows, "wf-1")
	if cmd == nil {
		t.Fatal("fixture: RequestDetail returned no cmd")
	}
	m.Base.Update(cmd())
	if m.Base.DetailID() == "" {
		t.Fatal("fixture: the workflow detail did not load, so the flow-keys guard will refuse")
	}
	return m, writes
}

// Entering: `e` opens the mode rather than a step form.
func TestFlowEditModeEntersOnE(t *testing.T) {
	m, _ := modeModel(t)
	if m.flowEditing {
		t.Fatal("fixture: the mode must start off")
	}
	if _, handled := m.handleFlowKeys(keyFlowEdit); !handled {
		t.Fatal("`e` was not handled with a flow on screen")
	}
	if !m.flowEditing {
		t.Fatal("`e` did not enter the workflow edit mode — the operator's central ask")
	}
	// The mode must be VISIBLE, not just a variable: the operator has to be able to tell from
	// the pane they are looking at (and are about to change) that it is writable.
	if got := m.Base.View(); !strings.Contains(got, "EDIT") {
		t.Errorf("the pane does not show that it is in edit mode:\n%s", got)
	}
	if !strings.Contains(m.notice, "editing") {
		t.Errorf("the notice does not name the mode: %q", m.notice)
	}
}

// OUTSIDE the mode, the step chords must do nothing: that is what makes the flow view a
// read-only VIEW until the operator opts in.
func TestStepChordsAreInertOutsideTheMode(t *testing.T) {
	m, _ := modeModel(t)
	m.flowEditing = false
	for _, k := range []string{keyFlowEditStep, keyFlowAddStep, keyFlowAddStepAlt, keyFlowRemoveStep, keyStepDown, keyStepUp} {
		if _, handled := m.handleFlowKeys(k); handled {
			t.Errorf("%q was handled while the mode is OFF — the step commands must not be "+
				"live outside an edit view", k)
		}
	}
	if m.Base.EditingDetail() {
		t.Fatal("a step form opened outside the mode")
	}
}

// ENTER edits the step the cursor is on — the operator's explicit gesture.
func TestEnterEditsTheSelectedStepInMode(t *testing.T) {
	m, _ := modeModel(t)
	m.beginFlowEdit()
	step, ok := m.selectedFlowStep()
	if !ok {
		t.Fatal("fixture: no step selected")
	}
	if _, handled := m.handleFlowKeys(keyFlowEditStep); !handled {
		t.Fatal("enter was not handled in the mode")
	}
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("enter did not open a step editor — it must claim the key before kit2's own enter")
	}
	if !strings.Contains(f.Title, orDefaultStr(step.Name, step.ID)) {
		t.Errorf("the form edits %q, want the cursor step %q", f.Title, orDefaultStr(step.Name, step.ID))
	}
}

// The cursor moves with the arrows, and ENTER follows it.
func TestEnterFollowsTheCursor(t *testing.T) {
	m, _ := modeModel(t)
	m.beginFlowEdit()
	steps := m.flowStepsOf()
	if len(steps) < 2 {
		t.Fatal("fixture: need >= 2 steps")
	}
	m.handleFlowKeys(keyStepDown)
	second := m.stepSel
	if second != steps[1].ID {
		t.Fatalf("cursor = %q, want %q", second, steps[1].ID)
	}
	m.handleFlowKeys(keyFlowEditStep)
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("enter opened nothing")
	}
	if !strings.Contains(f.Title, orDefaultStr(steps[1].Name, steps[1].ID)) {
		t.Errorf("enter edited %q, want the SECOND step %q", f.Title, orDefaultStr(steps[1].Name, steps[1].ID))
	}
}

// `a` adds (the operator's suggested key ~ kept), and `-` still works.
func TestAddStepKeysInMode(t *testing.T) {
	for _, k := range []string{keyFlowAddStep, keyFlowAddStepAlt} {
		m, _ := modeModel(t)
		m.beginFlowEdit()
		if _, handled := m.handleFlowKeys(k); !handled {
			t.Fatalf("%q did not open the add form in the mode", k)
		}
		f := m.Base.DetailForm()
		if f == nil {
			t.Fatalf("%q opened no form", k)
		}
		if !strings.Contains(f.Title, "Add step") {
			t.Errorf("%q opened %q, want the add-step form", k, f.Title)
		}
	}
}

// `x` removes, behind a confirm (it rewires the graph).
func TestRemoveStepKeyInMode(t *testing.T) {
	m, _ := modeModel(t)
	m.beginFlowEdit()
	if _, handled := m.handleFlowKeys(keyFlowRemoveStep); !handled {
		t.Fatal("x was not handled in the mode")
	}
	if m.Open == nil {
		t.Fatal("removing a step must ask for confirmation — it rewires the graph")
	}
}

// esc leaves the mode.
func TestEscLeavesTheMode(t *testing.T) {
	m, _ := modeModel(t)
	m.beginFlowEdit()
	if _, handled := m.handleFlowKeys(keyFlowExit); !handled {
		t.Fatal("esc was not handled in the mode")
	}
	if m.flowEditing {
		t.Fatal("esc did not leave the mode")
	}
}

// THROUGH THE REAL DISPATCH. The handlers above are called directly; this drives the
// screen's own Update, which is the path the shell uses — so it catches a key being
// shadowed by an earlier branch (a form, a dialog, kit2's own enter) rather than reaching
// the mode at all.
func TestFlowEditModeThroughRealDispatch(t *testing.T) {
	m, _ := modeModel(t)

	// `e` reaches the mode through Update.
	m.Update(keyPress("e"))
	if !m.flowEditing {
		t.Fatal("`e` did not reach the mode through the screen's own dispatch")
	}

	// `enter` inside the mode opens a STEP editor rather than reloading the pane's detail
	// — the one ordering that kit2 could have stolen.
	m.Update(keyPress("enter"))
	if m.Base.DetailForm() == nil {
		t.Fatal("enter did not open a step editor through dispatch — kit2's own enter won")
	}
	if !strings.Contains(m.Base.DetailForm().Title, "Edit step") {
		t.Errorf("dispatch opened %q, want a step editor", m.Base.DetailForm().Title)
	}

	// `esc` leaves the mode. It has to get past the open form first (the form owns keys
	// while it is up), which is correct: esc cancels the form, then esc leaves the mode.
	m.Base.CloseDetailEdit()
	m.Update(keyPress("esc"))
	if m.flowEditing {
		t.Fatal("esc did not leave the mode through dispatch")
	}
}

// `a` through dispatch opens the add form.
func TestAddStepThroughRealDispatch(t *testing.T) {
	m, _ := modeModel(t)
	m.Update(keyPress("e"))
	m.Update(keyPress("a"))
	f := m.Base.DetailForm()
	if f == nil {
		t.Fatal("`a` did not reach the mode's add form through dispatch")
	}
	if !strings.Contains(f.Title, "Add step") {
		t.Errorf("dispatch opened %q, want the add-step form", f.Title)
	}
}

// A new workflow lands IN the mode, so its empty draft is immediately editable. The
// pending flag is consumed by the flow load, which is when the id becomes selectable.
func TestCreateLandsInTheEditMode(t *testing.T) {
	m, _, writes := wfExec(t, nil, nil)
	var createdName string
	m.rpcCreateWorkflow = func(_ context.Context, req *apiv1.CreateWorkflowRequest) (*apiv1.Workflow, error) {
		createdName = req.GetName()
		*writes = append(*writes, "create")
		return &apiv1.Workflow{Id: "wf-new", Name: req.GetName()}, nil
	}
	if !m.Base.SelectSource(srcWorkflows) {
		t.Fatal("fixture: could not focus the Workflows pane")
	}
	if _, handled := m.handleActionKey(keyNewWorkflow); !handled {
		t.Fatal("n was not handled")
	}
	f := detailForm(m)
	if f == nil {
		t.Fatal("n did not open the create form")
	}
	f.Set("name", "release pipeline")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if createdName != "release pipeline" {
		t.Fatalf("created %q, want the operator's name", createdName)
	}
	if !m.flowEditPending {
		t.Fatal("creating a workflow did not arm the edit mode — a new workflow's version is an " +
			"empty draft, so landing in a read-only view gives the operator nothing to do")
	}

	// The flow load consumes it — driven through Update, which is the real path.
	m.Update(workflowEditorMsg{id: "wf-new", name: "release pipeline"})
	if m.flowEditPending {
		t.Fatal("the pending edit mode was not consumed by the flow load")
	}
	if !m.flowEditing {
		t.Fatal("the new workflow did not open in the edit mode")
	}
}
