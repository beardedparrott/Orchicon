package execution

// workflow_steps_test.go — the workflow STEP editor.
//
// The fixture is the REAL SDLC (Human) published workflow, because the shapes that
// matter are the ones the product stores: an 11-step flow with a two-way fan-in and
// three loops whose targets sit EARLIER in the flow.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func stepEditorModel(t *testing.T) (*Model, *[]string) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	writes := &[]string{}
	m.rpcUpdateWorkflowVersion = func(_ context.Context, id, steps string) error {
		*writes = append(*writes, "steps:"+id)
		return nil
	}
	m.rpcCreateWorkflowVersion = func(_ context.Context, id string) error {
		*writes = append(*writes, "draft:"+id)
		return nil
	}
	raw, err := os.ReadFile("testdata/sdlc_human.json")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	m.enterStepEditor("wf-1", "SDLC (Human)", &apiv1.WorkflowVersion{Id: "ver-1", Steps: string(raw)})
	return m, writes
}

// The kind list is the AUTHORING vocabulary, and it must match what the reconciler
// can run. `policy` is absent because it is not a step kind at all; `decision` is
// absent because its reconciler case is a stub that always takes one path.
func TestStepKindsMatchWhatTheBackendCanRun(t *testing.T) {
	got := map[string]bool{}
	for _, o := range stepKindOptions() {
		got[o.Value] = true
	}
	for _, want := range []string{"task", "approval", "loop_decision", "parallel", "end"} {
		if !got[want] {
			t.Errorf("kind %q must be offered", want)
		}
	}
	for _, banned := range []string{"policy", "decision"} {
		if got[banned] {
			t.Errorf("kind %q must NOT be offered: policy is not a backend kind, decision is a stub", banned)
		}
	}
	// Dual is NOT one flag: approvalConfig (workflow_reconciler.go:4892) has
	// loop_branch and max_iterations and NO success_branch field at all, so an
	// approval has no forward target to offer; loopDecisionConfig
	// (workflow_reconciler.go:4849) has both.
	if stepKindHasSuccess("approval") {
		t.Error("approval has no success_branch in its config struct — offering a SUCCESS target would write a key nothing reads")
	}
	if !stepKindHasLoop("approval") || !stepKindHasLoop("loop_decision") {
		t.Error("approval and loop_decision both re-enter a named step on rejection/failure — both carry a LOOP target")
	}
	if !stepKindHasSuccess("loop_decision") {
		t.Error("loop_decision carries success_branch — it must offer a SUCCESS target")
	}
	for _, none := range []string{"task", "parallel", "end"} {
		if stepKindHasLoop(none) || stepKindHasSuccess(none) {
			t.Errorf("%q has no branch targets — it must offer neither a SUCCESS nor a LOOP field", none)
		}
	}
}

// The cursor walks the flow, and the pane is told which step it is on.
func TestStepCursorWalksTheFlow(t *testing.T) {
	m, _ := stepEditorModel(t)
	steps := m.flowStepsOf()
	if len(steps) != 11 {
		t.Fatalf("fixture parsed %d steps, want 11", len(steps))
	}
	if m.stepSel != steps[0].ID {
		t.Fatalf("cursor starts at %q, want the first step %q", m.stepSel, steps[0].ID)
	}
	m.stepCursor(1)
	if m.stepSel != steps[1].ID {
		t.Fatalf("after down cursor = %q, want %q", m.stepSel, steps[1].ID)
	}
	m.stepCursor(-1)
	if m.stepSel != steps[0].ID {
		t.Fatalf("after up cursor = %q, want %q", m.stepSel, steps[0].ID)
	}
	// It clamps at both ends rather than wrapping.
	m.stepCursor(-5)
	if m.stepSel != steps[0].ID {
		t.Fatalf("cursor overshot the start: %q", m.stepSel)
	}
	m.stepCursor(100)
	if m.stepSel != steps[len(steps)-1].ID {
		t.Fatalf("cursor overshot the end: %q", m.stepSel)
	}
}

// Every step the parser reads must be written back: a round trip cannot silently
// drop a field the plane depends on.
func TestStepRoundTripPreservesEveryField(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdlc_human.json")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	before := parseFlowSteps(string(raw))
	blob, err := marshalSteps(before)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	after := parseFlowSteps(blob)
	if len(after) != len(before) {
		t.Fatalf("round trip changed the step count: %d → %d", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.ID != a.ID || b.Name != a.Name || b.Kind != a.Kind || b.Ref != a.Ref {
			t.Errorf("step %d lost identity fields on round trip: %+v → %+v", i, b, a)
		}
		if len(b.deps) != len(a.deps) {
			t.Errorf("step %q lost dependencies: %v → %v", b.ID, b.deps, a.deps)
		}
		// The branch config must survive: it is what makes a loop a loop.
		bb, ab := flowBranchOf(b), flowBranchOf(a)
		if (bb == nil) != (ab == nil) {
			t.Errorf("step %q lost its branch config: %v → %v", b.ID, b.Config, a.Config)
			continue
		}
		if bb != nil && (bb.Success != ab.Success || bb.Loop != ab.Loop || bb.MaxIter != ab.MaxIter) {
			t.Errorf("step %q branches changed: %+v → %+v", b.ID, bb, ab)
		}
	}
	// Positions are NOT re-emitted (they are presentation-only).
	if strings.Contains(blob, "position_x") {
		t.Error("marshalSteps emitted position_x — it is presentation-only and nothing reads it")
	}
}

// Editing a branch target rewrites ONLY the branch keys: a step's config carries
// recovery, conflict_value and more, and clobbering it would destroy settings the
// operator never touched.
func TestBranchEditPreservesTheRestOfTheConfig(t *testing.T) {
	s := flowStep{ID: "a", Kind: "loop_decision",
		Config: `{"max_iterations":6,"conflict_value":"conflict","success_branch":"x","loop_branch":"y"}`}
	out, err := marshalBranches(s, &flowBranches{Success: "b", Loop: "c", MaxIter: 3})
	if err != nil {
		t.Fatalf("marshalBranches: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if got["success_branch"] != "b" || got["loop_branch"] != "c" {
		t.Errorf("branches not rewritten: %v", got)
	}
	if got["conflict_value"] != "conflict" {
		t.Errorf("conflict_value was lost: %v", got)
	}
	if got["max_iterations"].(float64) != 3 {
		t.Errorf("max_iterations not updated: %v", got["max_iterations"])
	}
	// Clearing a branch REMOVES the key rather than writing an empty id.
	out2, _ := marshalBranches(s, &flowBranches{})
	if strings.Contains(out2, "success_branch") || strings.Contains(out2, "loop_branch") {
		t.Errorf("cleared branches must be deleted, not blanked: %s", out2)
	}
}

// Removing a step rewires every reference to it, so the workflow can never be saved
// with a dangling depends_on or a branch pointing at a step that no longer exists.
func TestRemoveStepRewiresEveryReference(t *testing.T) {
	steps := []flowStep{
		{ID: "a", Name: "A", Kind: "task"},
		{ID: "b", Name: "B", Kind: "task", deps: []string{"a"}},
		{ID: "c", Name: "C", Kind: "loop_decision", deps: []string{"b"},
			Config: `{"success_branch":"a","loop_branch":"b","max_iterations":3}`},
	}
	out := removeStep(steps, "a")
	if len(out) != 2 {
		t.Fatalf("removeStep left %d steps, want 2", len(out))
	}
	for _, s := range out {
		for _, d := range s.deps {
			if d == "a" {
				t.Errorf("step %q still depends on the removed step", s.ID)
			}
		}
		if b := flowBranchOf(s); b != nil {
			if b.Success == "a" || b.Loop == "a" {
				t.Errorf("step %q still branches to the removed step", s.ID)
			}
		}
	}
	// And the result must still pass the graph check that gates saving.
	if err := validateStepGraph(out); err != nil {
		t.Errorf("the rewired graph does not validate: %v", err)
	}
}

// The graph validation is what the SERVER does not do: it accepts any JSON array,
// so a dangling reference would be saved and then simply never become ready.
func TestStepGraphValidationCatchesDanglingReferences(t *testing.T) {
	cases := []struct {
		name  string
		steps []flowStep
		bad   bool
	}{
		{"depends on a missing step", []flowStep{{ID: "a", deps: []string{"ghost"}}}, true},
		{"duplicate ids", []flowStep{{ID: "a"}, {ID: "a"}}, true},
		{"branch target missing", []flowStep{{ID: "a", Kind: "loop_decision",
			Config: `{"success_branch":"ghost","max_iterations":2}`}}, true},
		{"loop with no iterations", []flowStep{{ID: "a", Kind: "loop_decision",
			Config: `{"loop_branch":"a"}`}}, true},
		{"a healthy chain", []flowStep{{ID: "a"}, {ID: "b", deps: []string{"a"}}}, false},
	}
	for _, c := range cases {
		err := validateStepGraph(c.steps)
		if c.bad && err == nil {
			t.Errorf("%s: expected a validation error", c.name)
		}
		if !c.bad && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
	}
}

// A target picker offers this workflow's OWN steps by NAME — the piece that makes
// branch editing possible, since a branch is a step id inside a JSON blob.
func TestTargetPickerUsesNamesAndExcludesSelf(t *testing.T) {
	steps := []flowStep{
		{ID: "step-a", Name: "Architect", Kind: "task"},
		{ID: "step-b", Name: "QA", Kind: "task"},
	}
	opts := targetOptions(steps, "step-a")
	if len(opts) != 1 {
		t.Fatalf("options = %v, want only the step that is not the excluded one", opts)
	}
	if opts[0].Value != "step-b" {
		t.Errorf("value = %q, want the step ID the wire needs", opts[0].Value)
	}
	if !strings.Contains(opts[0].Label, "QA") {
		t.Errorf("label = %q, want the NAME a human reads", opts[0].Label)
	}
}

// Saving to a published workflow creates a DRAFT and retries: published versions
// are immutable, and "edit a step" should not fail with a lecture about versioning.
func TestSavingStepsCreatesADraftWhenTheVersionIsPublished(t *testing.T) {
	m := newModel(t, &fakePlane{})
	writes := &[]string{}
	refuseFirst := true
	m.rpcUpdateWorkflowVersion = func(_ context.Context, id, steps string) error {
		if refuseFirst {
			refuseFirst = false
			return errFailedPrecondition
		}
		*writes = append(*writes, "steps")
		return nil
	}
	m.rpcCreateWorkflowVersion = func(_ context.Context, id string) error {
		*writes = append(*writes, "draft")
		return nil
	}
	m.enterStepEditor("wf-1", "SDLC (Human)", &apiv1.WorkflowVersion{Id: "ver-1", Steps: `[{"id":"a","name":"A","kind":"task","ref":"w_x"}]`})

	// Drive the form's own OnSubmit: that is what wires the mutation, and the
	// mutation's Do is where drafting happens. (Submit() alone only validates.)
	f := m.editStepForm(flowStep{ID: "a", Name: "A", Kind: "task", Ref: "w_x"})
	cmd, oerr := f.OnSubmit(f.Values, nil)
	if oerr != nil {
		t.Fatalf("submit rejected: %v", oerr)
	}
	runWrite(t, cmd)
	if len(*writes) != 2 || (*writes)[0] != "draft" || (*writes)[1] != "steps" {
		t.Fatalf("writes = %v, want [draft steps] — a published version must be made editable by drafting", *writes)
	}
}

var errFailedPrecondition = &preconditionErr{}

type preconditionErr struct{}

func (*preconditionErr) Error() string { return "latest version is not draft" }
