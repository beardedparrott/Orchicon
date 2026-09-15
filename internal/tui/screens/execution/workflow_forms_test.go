package execution

// workflow_forms_test.go — the workflow lifecycle (create / edit / publish /
// deprecate / delete), driven through the real chord → load → form → submit path.

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

func wfExec(t *testing.T, wf *apiv1.Workflow, versions []*apiv1.WorkflowVersion) (*Model, *[]string, *[]string) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	loads, writes := &[]string{}, &[]string{}
	m.rpcGetWorkflow = func(context.Context, string) (*apiv1.Workflow, error) {
		*loads = append(*loads, "get")
		if wf == nil {
			return nil, errors.New("not found")
		}
		return wf, nil
	}
	m.rpcListWorkflowVersions = func(context.Context, string) ([]*apiv1.WorkflowVersion, error) {
		*loads = append(*loads, "versions")
		return versions, nil
	}
	m.rpcCreateWorkflow = func(context.Context, *apiv1.CreateWorkflowRequest) error {
		*writes = append(*writes, "create")
		return nil
	}
	m.rpcUpdateWorkflow = func(context.Context, string, string) error {
		*writes = append(*writes, "update")
		return nil
	}
	m.rpcPublishWorkflow = func(context.Context, string, string) error {
		*writes = append(*writes, "publish")
		return nil
	}
	m.rpcDeprecateWorkflow = func(context.Context, string) error {
		*writes = append(*writes, "deprecate")
		return nil
	}
	m.rpcDeleteWorkflow = func(context.Context, string) error {
		*writes = append(*writes, "delete")
		return nil
	}
	return m, loads, writes
}

func wfDraft(id string, n int32) *apiv1.WorkflowVersion {
	return &apiv1.WorkflowVersion{Id: id, Version: n, Status: apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_DRAFT}
}

// The lifecycle chords are scoped to the Workflows pane: `p` is ALSO
// force-progress on a run and `n`/`e`/`x` mean other things elsewhere, so an
// unscoped switch would hijack them.
func TestWorkflowChordsAreScopedToTheWorkflowsPane(t *testing.T) {
	m, _, _ := wfExec(t, &apiv1.Workflow{Id: "w1"}, nil)
	m.SelectSource(srcRuns)
	for _, k := range []string{keyNewWorkflow, keyEditWorkflow, keyPublishWf} {
		if _, handled := m.handleActionKey(k); handled {
			t.Fatalf("chord %q was handled while the Runs pane was focused — it must fall through", k)
		}
	}
}

// `n` opens the create form in the DETAILS PANE with no load, and its submit calls
// CreateWorkflow with the operator's values — and NO steps: the version starts empty
// because a DAG is authored in the flow view, not typed as JSON ("same goes for new
// workflows").
func TestWorkflowCreateOpensInlineFormAndSubmits(t *testing.T) {
	m, loads, writes := wfExec(t, nil, nil)
	var got *apiv1.CreateWorkflowRequest
	m.rpcCreateWorkflow = func(_ context.Context, req *apiv1.CreateWorkflowRequest) error {
		got = req
		*writes = append(*writes, "create")
		return nil
	}
	if !m.Base.SelectSource(srcWorkflows) {
		t.Fatal("fixture: could not focus the Workflows pane")
	}
	if _, handled := m.handleActionKey(keyNewWorkflow); !handled {
		t.Fatal("n must be handled")
	}
	if m.form != nil {
		t.Fatal("the workflow form must not be a modal")
	}
	f := detailForm(m)
	if f == nil {
		t.Fatal("n must open the create form in the details pane")
	}
	if len(*loads) != 0 {
		t.Fatalf("create must not load, got %v", *loads)
	}
	// There is no raw-JSON steps field. This is the assertion that would have caught
	// the form still asking the operator to type a DAG.
	for _, s := range f.Specs {
		if s.Name == "steps" {
			t.Error("the create form still offers a Steps JSON blob — steps are authored in the flow view")
		}
		if s.Kind == kit2.KJSON {
			t.Errorf("the create form has a raw JSON field %q", s.Name)
		}
	}
	f.Set("name", "release pipeline")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(*writes) != 1 || (*writes)[0] != "create" {
		t.Fatalf("writes = %v, want [create]", *writes)
	}
	if got == nil {
		t.Fatal("the create request never reached the client")
	}
	if got.GetName() != "release pipeline" {
		t.Errorf("name = %q, want the operator's value", got.GetName())
	}
	if got.GetSteps() != "" {
		t.Errorf("steps = %q — a new workflow's version must start EMPTY", got.GetSteps())
	}
}

// publish names the DRAFT it will ship; with no draft it still opens (the server
// publishes the current version) but the title says so.
func TestWorkflowPublishNamesTheDraft(t *testing.T) {
	wf := &apiv1.Workflow{Id: "w1", Name: "sdlc"}
	m, _, writes := wfExec(t, wf, []*apiv1.WorkflowVersion{wfDraft("v3", 3)})
	m.workerOp, m.workerOpID = opPublish, "w1"
	m.Update(workflowDetailMsg{op: opPublish, id: "w1", wf: wf,
		versions: []*apiv1.WorkflowVersion{wfDraft("v3", 3)}})
	f := detailForm(m)
	if f == nil {
		t.Fatal("publish must open a form")
	}
	if !strings.Contains(f.Title, "Publish v3") {
		t.Fatalf("title = %q, want it to name the draft version", f.Title)
	}
	cmd, _ := f.OnSubmit(f.Values, nil)
	runWrite(t, cmd)
	if len(*writes) != 1 || (*writes)[0] != "publish" {
		t.Fatalf("writes = %v, want [publish]", *writes)
	}
}

// The pane offers the lifecycle for a published workflow and not for a draft.
func TestWorkflowActionsFollowStatus(t *testing.T) {
	m, _, _ := wfExec(t, nil, nil)
	m.SelectSource(srcWorkflows)
	for _, c := range []struct {
		status    string
		wantDepre bool
	}{
		{"published", true},
		{"draft", false},
	} {
		m.LoadItems(srcWorkflows, []kit2.Item{{ID: "w1", Title: "sdlc", Meta: c.status}}, "")
		m.SelectSource(srcWorkflows)
		found := false
		for _, a := range m.actionsForSelection() {
			if a.Key == keyDeprecateWf {
				found = true
			}
		}
		if found != c.wantDepre {
			t.Errorf("status %q: deprecate offered = %v, want %v", c.status, found, c.wantDepre)
		}
	}
}
