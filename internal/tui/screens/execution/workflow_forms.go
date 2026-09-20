// workflow_forms.go — the Execution screen's WORKFLOW lifecycle forms.
//
// Mirrors worker_forms.go deliberately: the same load-then-open-a-form-in-the-
// DETAILS-PANE shape, the same thunks so the whole chain is testable without a
// plane, and the same rule that an operation which cannot apply is REFUSED with
// the reason rather than silently skipped.
//
// Scope: the workflow's own lifecycle (create / edit header / publish / deprecate
// / delete). Editing a version's STEPS is a separate, larger surface — the steps
// are a JSON array of nodes with their own dependencies — and is deliberately not
// half-built here.
package execution

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// workflowDetailMsg carries a workflow plus its version trail: one pair of reads
// serves every lifecycle operation that needs to know what state it is in.
type workflowDetailMsg struct {
	workerID string // reuse: the pending-op guard key (see worker_forms.go)
	op       workerOp
	id       string
	wf       *apiv1.Workflow
	versions []*apiv1.WorkflowVersion
	err      error
}

// workflowStatusOf reads a workflow's status out of a list row's meta, which
// fetchWorkflows sets to the lowercased status.
func workflowStatusOf(meta string) string {
	return strings.TrimSpace(strings.ToLower(meta))
}

// --- forms -----------------------------------------------------------------

// createWorkflowForm creates a workflow AND its first version in one call. The
// version is created EMPTY, deliberately: a workflow's steps are a DAG, and a DAG
// cannot be authored in a form — the flow view is the editor for exactly that reason
// (workflow_steps.go), so the form stops pretending otherwise.
//
// The first cut offered a `Steps (JSON array)` box here. It was the operator's "same
// goes for new workflows": a JSON blob is not an editor, and the steps it produced
// had no coordinates, no names worth reading, and no validation beyond the shape.
//
// CreateWorkflowTx creates version 1 as a DRAFT (internal/workflow/create.go:145), so
// the new workflow is immediately step-editable: select it — which opens the flow
// view — and press `-` to add the first step.
func (m *Model) createWorkflowForm() *kit2.Form {
	f := kit2.NewForm("New workflow",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true,
			Placeholder: "release pipeline"},
		kit2.FieldSpec{Name: "type", Label: "Type", Kind: kit2.KSelect, Initial: "template",
			Options: []kit2.Option{
				{Value: "template", Label: "template (tenant-level, bindable)"},
				{Value: "one_shot", Label: "one_shot (project-scoped)"},
			}},
		kit2.FieldSpec{Name: "version_note", Label: "Version note", Kind: kit2.KText,
			Placeholder: "steps are added in the flow view — select the workflow, then -"},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.CreateWorkflowRequest{
			Name:        strings.TrimSpace(v["name"]),
			Type:        strings.TrimSpace(v["type"]),
			Steps:       "", // authored in the flow view, not typed as JSON
			VersionNote: strings.TrimSpace(v["version_note"]),
		}
		return m.Mutate(mutate.Request{
			Name: "create workflow " + req.Name, Source: srcWorkflows,
			Do: func(ctx context.Context) error {
				wf, err := m.rpcCreateWorkflow(ctx, req)
				if err != nil {
					return err
				}
				// Land IN the edit mode on the new workflow.
				//
				// The operator: "I think 'e' should be edit workflow mode (and 'n' new workflow
				// should work like this as well)". A new workflow's first version is created as a
				// DRAFT with no steps (CreateWorkflowTx), so there is nothing to look at until a
				// step exists — opening the mode is the only useful place to land. The id is
				// selected PENDING the list reload, because the row does not exist client-side
				// yet, and flowEditPending is consumed when that workflow's flow loads.
				if id := wf.GetId(); id != "" {
					m.flowEditPending = true
					m.Base.SelectWhenLoaded(srcWorkflows, id)
				}
				return nil
			},
		}), nil
	}
	return f
}

// editWorkflowForm edits the header. Only the NAME is header-editable (the proto
// says so): the steps belong to a version, and git strategy is not something to
// type into a terminal.
func (m *Model) editWorkflowForm(w *apiv1.Workflow) *kit2.Form {
	f := kit2.NewForm("Edit workflow: "+w.GetName(),
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Initial: w.GetName()},
	)
	f.Focused = true
	f.Width = 70
	id := w.GetId()
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		if name == w.GetName() {
			return nil, errors.New("nothing changed")
		}
		return m.Mutate(mutate.Request{
			Name: "update workflow " + id, Source: srcWorkflows,
			Do: func(ctx context.Context) error { return m.rpcUpdateWorkflow(ctx, id, name) },
		}), nil
	}
	return f
}

// publishWorkflowForm collects the optional publish note and names the version it
// will ship, so a publish is never a surprise.
func (m *Model) publishWorkflowForm(id string, draft *apiv1.WorkflowVersion) *kit2.Form {
	title := "Publish " + id
	if draft != nil {
		title = "Publish v" + itoa32(draft.GetVersion()) + " of " + id
	}
	f := kit2.NewForm(title,
		kit2.FieldSpec{Name: "note", Label: "Version note", Kind: kit2.KText,
			Placeholder: "what changed (optional)"},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		return m.Mutate(mutate.Request{
			Name: "publish workflow " + id, Source: srcWorkflows,
			Do: func(ctx context.Context) error {
				return m.rpcPublishWorkflow(ctx, id, strings.TrimSpace(v["note"]))
			},
		}), nil
	}
	return f
}

// itoa32 is a tiny local helper so this file needs no strconv import for one call.
func itoa32(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- the load --------------------------------------------------------------

func (m *Model) beginWorkflowOp(id string, op workerOp) tea.Cmd {
	get, list := m.rpcGetWorkflow, m.rpcListWorkflowVersions
	if get == nil || list == nil {
		return m.refuse("no workflow client")
	}
	m.notice = "loading " + id + "…"
	m.workerOp, m.workerOpID = op, id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		w, err := get(ctx, id)
		if err != nil {
			return workflowDetailMsg{op: op, id: id, err: err}
		}
		out := workflowDetailMsg{op: op, id: id, wf: w}
		if vs, verr := list(ctx, id); verr == nil {
			out.versions = vs
		}
		return out
	}
}

func (m *Model) openWorkflowOpForm(msg workflowDetailMsg) tea.Cmd {
	if m.workerOpID != msg.id || m.workerOp == "" {
		return nil
	}
	op := m.workerOp
	m.workerOp, m.workerOpID = "", ""
	if msg.err != nil {
		return m.refuse("workflow load failed: " + msg.err.Error())
	}
	switch op {
	case opEditHeader:
		m.Base.BeginDetailEdit("Edit workflow", m.editWorkflowForm(msg.wf))
		m.notice = ""
	case opPublish:
		m.Base.BeginDetailEdit("Publish workflow", m.publishWorkflowForm(msg.id, latestDraft(msg.versions)))
		m.notice = ""
	}
	return nil
}

// latestDraft returns the newest DRAFT version, or nil.
func latestDraft(vs []*apiv1.WorkflowVersion) *apiv1.WorkflowVersion {
	var best *apiv1.WorkflowVersion
	for _, v := range vs {
		if v.GetStatus() != apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_DRAFT {
			continue
		}
		if best == nil || v.GetVersion() > best.GetVersion() {
			best = v
		}
	}
	return best
}

// --- the writes ------------------------------------------------------------

func (m *Model) defaultCreateWorkflow(ctx context.Context, req *apiv1.CreateWorkflowRequest) (*apiv1.Workflow, error) {
	if m.cl == nil || m.cl.Workflows == nil {
		return nil, errors.New("no workflow client")
	}
	// The CREATED workflow is returned rather than discarded: `n` has to select it so the
	// flow editor can open on it, and the id only exists once the server has made it.
	resp, err := m.cl.Workflows.CreateWorkflow(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetWorkflow(), nil
}

func (m *Model) defaultUpdateWorkflow(ctx context.Context, id, name string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.UpdateWorkflow(ctx, connect.NewRequest(&apiv1.UpdateWorkflowRequest{
		WorkflowId: id, Name: name,
	}))
	return err
}

func (m *Model) defaultPublishWorkflow(ctx context.Context, id, note string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.PublishWorkflow(ctx, connect.NewRequest(&apiv1.PublishWorkflowRequest{
		WorkflowId: id, VersionNote: note,
	}))
	return err
}

func (m *Model) defaultDeprecateWorkflow(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.DeprecateWorkflow(ctx, connect.NewRequest(&apiv1.DeprecateWorkflowRequest{WorkflowId: id}))
	return err
}

func (m *Model) defaultDeleteWorkflow(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.DeleteWorkflow(ctx, connect.NewRequest(&apiv1.DeleteWorkflowRequest{Id: id}))
	return err
}

// default thunks -----------------------------------------------------------

func (m *Model) defaultGetWorkflow(ctx context.Context, id string) (*apiv1.Workflow, error) {
	if m.cl == nil || m.cl.Workflows == nil {
		return nil, errors.New("no workflow client")
	}
	resp, err := m.cl.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetWorkflow(), nil
}

func (m *Model) defaultListWorkflowVersions(ctx context.Context, id string) ([]*apiv1.WorkflowVersion, error) {
	if m.cl == nil || m.cl.Workflows == nil {
		return nil, errors.New("no workflow client")
	}
	resp, err := m.cl.Workflows.ListWorkflowVersions(ctx, connect.NewRequest(&apiv1.ListWorkflowVersionsRequest{WorkflowId: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetVersions(), nil
}

// openFormWorkflowPicker is the model-picker host for a workflow form. Workflows
// name no model directly today, so this exists only to satisfy a future step
// editor without inventing a second picker host.
