// Package execution implements the Execution screen: executions, workflow runs, WORKFLOWS and
// workers, with the live StreamExecutionEvents subscription.
package execution

import (
	"context"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Execution screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamExecutionEventsResponse]
	wfSub       *stream.Sub[*apiv1.StreamWorkflowEventsResponse]
	reconnected bool

	w, h int
	// form is the open interjection form (nil when closed); pending is the action the open confirmation
	// dialog will run; bar is the footer strip of the selected row's actions.
	form    *kit2.Form
	pending *kit2.Action
	bar     *kit2.ActionBar
	notice  string

	// modelPicker is the open worker-model picker (adapter → provider → model,
	// with search). A model_ref is CHOSEN, never typed, so it gets its own modal.
	modelPicker *kit2.ModelPicker
	// modelPickerWorkers is the SET of workers the open picker writes to — the marked selection it was
	// opened for.
	modelPickerWorkers []string
	// modelPickerField is the FORM FIELD the open picker writes back into, when it was opened from a
	// KModel field inside a form (the worker create form and the version editor).
	modelPickerField string
	// workerOp / workerOpID remember which worker CRUD operation is waiting on a load, so its form
	// opens when the data lands (and a late result for a worker the operator has left is dropped).
	workerOp   workerOp
	workerOpID string
	// workerModel caches each worker's ACTIVE model_ref — the workers list already carries it
	// (WorkerListItem.active_model_ref), so the picker seeds without another round trip.
	workerMu    sync.Mutex
	workerModel map[string]string

	// Model-picker loads: thunks so a test drives the cascade without a plane.
	rpcModelKinds     func(ctx context.Context) ([]string, []string, error)
	rpcModelProviders func(ctx context.Context, adapter string) ([]kit2.PickerOption, error)
	rpcModelModels    func(ctx context.Context, adapter, provider string) ([]kit2.PickerOption, bool, error)
	rpcSetWorkerModel func(ctx context.Context, workerIDs []string, ref string) error
	// Worker CRUD loads: thunks for the same reason (worker_forms.go) — the interactive operations need
	// the worker's CURRENT state before they can seed a form or decide whether they apply.
	rpcGetWorker          func(ctx context.Context, id string) (*apiv1.Worker, error)
	rpcListWorkerVersions func(ctx context.Context, id string) ([]*apiv1.WorkerVersion, error)
	// rpcListRoles backs the Plane-role picker on every worker form (create, edit, new version).
	rpcListRoles func(ctx context.Context) ([]*apiv1.Role, error)
	// Workflow lifecycle loads (workflow_forms.go), same shape.
	rpcGetWorkflow          func(ctx context.Context, id string) (*apiv1.Workflow, error)
	rpcListWorkflowVersions func(ctx context.Context, id string) ([]*apiv1.WorkflowVersion, error)
	// rpcUpdateWorkflowVersion persists the DRAFT's steps (workflow_steps.go).
	rpcUpdateWorkflowVersion func(ctx context.Context, workflowID, steps string) error

	// --- the workflow STEP editor (workflow_steps.go) ---
	stepWorkflowID string
	stepVersionID  string
	stepSteps      string
	stepSel        string
	// flowEditing is the WORKFLOW EDIT MODE. Off, the flow view is a READ-ONLY view of the workflow;
	// on, the step cursor and the step chords (enter/a/x/E/esc) are live.
	flowEditing bool
	// flowEditPending is set by CREATE (n) and consumed when the new workflow's flow loads, so a new
	// workflow opens straight into the mode — ready for its first step.
	flowEditPending bool
	// flowWorkers backs the step editor's worker LOOKUP: the operator picks a worker by name instead of
	// typing an id, the way work items do.
	flowWorkers []kit2.Option
	// runNames resolves the ids a workflow run carries (workflow_id / work_item_id) into the names an
	// operator reads — the proto has no name fields, so the client resolves them.
	runNames runNames
	// sched is the Schedules pane's state: which lens it is showing, and the row → run
	// bindings the `g` jump needs (schedules.go).
	sched schedState
	// runFlow is the RUNS pane's step-flow state: the step the cursor is on and the rows it
	// walks, so `enter` can jump to that step's execution (run_flow.go).
	runFlow runFlowState
	// todos caches each execution's worker todo list, shown in the detail pane
	// (execution_detail.go).
	todos todosCache
	// execUsage caches each execution's context / token / cost picture, derived from its usage
	// records (execution_context.go).
	execUsage usageCache
	// blocks is the execution transcript's COLLAPSE state and cursor, and composer is the inline
	// message box at the bottom of the pane (execution_blocks.go).
	blocks   blockState
	composer composer
	// execDetail holds the REST of an execution's detail — the run's record (facts, error, output) and
	// the merged session transcript.
	execDetail execDetailState
	// rpcCreateWorkflowVersion creates the draft the step editor writes to when the
	// version it is showing is published (immutable) — step editing implies a draft.
	rpcCreateWorkflowVersion func(ctx context.Context, workflowID string) error
	// stepWorkflowName is the header the editor repaints against.
	stepWorkflowName string
	// Workflow lifecycle WRITES, thunks for the same reason: a test asserts which
	// write fired without a plane.
	rpcCreateWorkflow func(ctx context.Context, req *apiv1.CreateWorkflowRequest) (*apiv1.Workflow, error)
	// rpcDeleteExecution is the Executions pane's DELETE.
	rpcDeleteExecution func(ctx context.Context, id string) error
	// rpcDeleteRun is the Runs pane's DELETE, same shape and same reason (one call per id, so a partial
	// failure can be counted).
	rpcDeleteRun func(ctx context.Context, id string) error
	// rpcListWorkers backs the step editor's worker picker. A thunk for the same reason as
	// the rest: a test asserts the picker's options without a plane.
	rpcListWorkers       func(ctx context.Context) ([]*apiv1.Worker, error)
	rpcUpdateWorkflow    func(ctx context.Context, id, name string) error
	rpcPublishWorkflow   func(ctx context.Context, id, note string) error
	rpcDeprecateWorkflow func(ctx context.Context, id string) error
	rpcDeleteWorkflow    func(ctx context.Context, id string) error
	// Worker CRUD writes: thunks so a test asserts WHICH write fired without a
	// plane, mirroring rpcSetWorkerModel.
	rpcCreateWorker             func(ctx context.Context, req *apiv1.CreateWorkerRequest) error
	rpcUpdateWorker             func(ctx context.Context, req *apiv1.UpdateWorkerRequest) error
	rpcDeleteWorker             func(ctx context.Context, id string) error
	rpcPublishWorkerVersion     func(ctx context.Context, req *apiv1.PublishWorkerVersionRequest) error
	rpcDeprecateWorker          func(ctx context.Context, id string) error
	rpcSetActiveWorkerVersion   func(ctx context.Context, workerID string, version int32) error
	rpcUpdateWorkerVersion      func(ctx context.Context, req *apiv1.UpdateWorkerVersionRequest) error
	rpcCreateWorkerVersionWrite func(ctx context.Context, req *apiv1.CreateWorkerVersionRequest) error
}

// New builds the screen. Execution events stream live; workflow events
// are fetched on demand only (v1 keeps one live stream per screen).
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "execution"
	m.AddSource("executions", "Executions", m.fetchExecutions)
	m.AddSource("runs", "Workflow Runs", m.fetchRuns)
	// Schedules sits beside the runs: it is the same subject seen through three lenses (queued /
	// in flight / already run), and the operator asked for it "under Executions".
	m.AddSource(srcSchedules, "Schedules", m.fetchSchedules)
	m.Base.SetSourceEmpty(srcSchedules, "nothing scheduled here — v switches to running / finished")
	m.AddSource("workflows", "Workflows", m.fetchWorkflows)
	m.AddSource("workers", "Workers", m.fetchWorkers)
	m.SetDetail(m.detail)
	m.SetOnDetail(m.onDetail)
	// THE FLOW EDITOR PAINTS ITS OWN DETAIL, so the shell's rolling refresh must keep away from it.
	//
	// The operator: "I recently noticed a weird issue on workflows in the TUI that looks to be
	// refreshing over and over and you can see the screen blink if you watch it… It is just the detail
	// view. It seems to be going back and forth between two screens very quickly." Two screenshots,
	// alternating: the READ-ONLY detail (header fields, "FLOW v10 published · 9 steps", VERSIONS) and
	// the EDIT-MODE flow (workflow / steps / "editing <version>").
	//
	// The loop was two renderers overwriting each other every five seconds. kit2's default RefreshView
	// re-requests the read-only detail for the selected row — for a workflow that is the VERSIONS
	// renderer — while paintFlow keeps repainting the EDIT-MODE flow. Nothing decided which owned the
	// pane, so they alternated.
	//
	// The guard in kit2 could not see it: it refuses while `editForm` is open (the INLINE form editor),
	// and the workflow edit mode is this screen's own `flowEditing` flag. So the screen answers for
	// itself, which is the only place that knows.
	// THE FLOW OWNS THE PANE ONLY WHILE THE OPERATOR IS IN THE EDITOR.
	//
	// The pane has TWO renderers for a workflow and only one may be showing: the read-only detail
	// (header fields, "FLOW v10 published · 9 steps", VERSIONS) and the edit-mode flow
	// (workflow / steps / "editing …"). Which one is correct depends entirely on whether the operator
	// is EDITING, and saying so is all this predicate has to do.
	//
	// MY LAST TWO ATTEMPTS BOTH GOT THIS WRONG, in opposite directions, and the operator's follow-up
	// named the result: "sometimes it shows the screen with no versions and sometimes it shows the
	// screens with versions. Shouldn't we just always show the screen that lists the versions
	// underneath?" — yes. They were seeing whichever renderer painted LAST:
	//
	//   - `flowEditing` alone was right, and appeared not to work because the rebuilt binary was never
	//     loaded by the running TUI (a restart is required, not just a rebuild);
	//   - "a workflow is remembered" then suppressed the refresh for the WHOLE pane — so the list
	//     stopped refreshing at all, and the flow still painted on every selection, which is why the
	//     versions view came and went.
	//
	// The read-only detail stays the pane's content whenever the editor is closed, which is what makes
	// the version list always visible underneath the flow.
	m.Base.SetDetailPaintOwner(func() bool { return m.flowEditing })
	m.Base.SetSourceEmpty("workflows", "no workflows yet — define one to run, or to bind a recurring item to")
	m.bar = kit2.NewActionBar()
	m.workerModel = map[string]string{}
	// The model picker's per-adapter loads (model_picker.go).
	m.rpcModelKinds = m.defaultModelKinds
	m.rpcModelProviders = m.defaultModelProviders
	m.rpcModelModels = m.defaultModelModels
	m.rpcSetWorkerModel = m.defaultSetWorkerModelRefs
	m.rpcGetWorker = m.defaultGetWorker
	m.rpcListWorkerVersions = m.defaultListWorkerVersions
	m.rpcListRoles = m.defaultListRoles
	m.rpcGetWorkflow = m.defaultGetWorkflow
	m.rpcListWorkflowVersions = m.defaultListWorkflowVersions
	m.rpcUpdateWorkflowVersion = m.defaultUpdateWorkflowVersion
	m.rpcCreateWorkflowVersion = m.defaultCreateWorkflowVersion
	m.rpcCreateWorkflow = m.defaultCreateWorkflow
	m.rpcListWorkers = m.defaultListWorkers
	m.rpcUpdateWorkflow = m.defaultUpdateWorkflow
	m.rpcPublishWorkflow = m.defaultPublishWorkflow
	m.rpcDeprecateWorkflow = m.defaultDeprecateWorkflow
	m.rpcDeleteWorkflow = m.defaultDeleteWorkflow
	m.rpcDeleteExecution = m.defaultDeleteExecution
	m.rpcDeleteRun = m.defaultDeleteRun
	m.rpcCreateWorker = m.defaultCreateWorker
	m.rpcUpdateWorker = m.defaultUpdateWorker
	m.rpcDeleteWorker = m.defaultDeleteWorker
	m.rpcPublishWorkerVersion = m.defaultPublishWorkerVersion
	m.rpcDeprecateWorker = m.defaultDeprecateWorker
	m.rpcSetActiveWorkerVersion = m.defaultSetActiveWorkerVersion
	m.rpcUpdateWorkerVersion = m.defaultUpdateWorkerVersion
	m.rpcCreateWorkerVersionWrite = m.defaultCreateWorkerVersion
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "execution-events", Status: "idle"},
		{Name: "workflow-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "execution" }

// EnsureSubscriptions starts the live streams once (idempotent; the shell calls it on every switch
// to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.ExecutionEvents(m.cl, m.tenantID)
	}
	if m.wfSub == nil {
		m.wfSub = m.reg.WorkflowEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() {
	m.reg.CloseAll()
	m.sub, m.wfSub = nil, nil
}

func (m *Model) SetSize(w, h int) {
	m.w, m.h = w, h
	m.Base.SetSize(w, h)
	// The picker derives its centered box from the screen size, so a resize must
	// reach it or its mouse mapping drifts from what is drawn.
	if m.modelPicker != nil {
		m.modelPicker.SetScreen(w, h)
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("execution-events"), m.reg.WaitStatus("workflow-events"), m.reg.WaitEventPoke("execution-events"))
}

func (m *Model) fetchExecutions(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	// The work item TITLES come from the shared name index (names.go), the same cache the runs pane
	// uses.
	m.runNames.ensure(ctx, m)
	items := make([]screenkit.Item, 0, len(resp.Msg.Executions))
	for _, e := range resp.Msg.Executions {
		items = append(items, screenkit.Item{
			ID: e.GetId(),
			// `<workflow> · <work item>` — the two names the operator asked for, with the ID kept as a fallback
			// so a row is never blank.
			Title: executionListTitle(e, &m.runNames),
			Meta:  executionListMeta(e),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

// executionListTitle composes the row's primary line: the workflow name and the bound work item's
// title.
func executionListTitle(e *apiv1.WorkerExecution, names *runNames) string {
	// THE ORDER IS THE OPERATOR'S: "Workflow Name - Worker Name - Work Item title (current truncated is
	// perfect) - Status".
	parts := make([]string, 0, 3)
	if wf := strings.TrimSpace(e.GetWorkflowName()); wf != "" {
		parts = append(parts, wf)
	}
	if w := strings.TrimSpace(e.GetWorkerName()); w != "" {
		parts = append(parts, w)
	}
	if item := screenkit.TruncateRunes(strings.TrimSpace(names.itemTitle(e.GetTaskId())), executionItemTitleMax); item != "" {
		parts = append(parts, item)
	}
	if len(parts) == 0 {
		// Nothing resolved: the ID is ALWAYS the fallback rather than a blank line, so a row can identify
		// itself even when its names cannot be resolved.
		return e.GetId()
	}
	return strings.Join(parts, " · ")
}

// executionItemTitleMax is how much of a work item's title an execution row keeps. See executionListTitle.
const executionItemTitleMax = 15

// executionListMeta is the row's right-hand field.
func executionListMeta(e *apiv1.WorkerExecution) string {
	meta := strings.ToLower(strings.TrimPrefix(e.GetStatus().String(), "EXECUTION_STATUS_"))
	if meta == "" {
		meta = strings.ToLower(e.GetStatus().String())
	}
	return meta
}

func (m *Model) fetchWorkers(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	// WorkerListItem carries the ACTIVE version's model_ref, so the model picker can seed without a
	// second round trip.
	items := make([]screenkit.Item, 0, len(resp.Msg.GetItems()))
	models := make(map[string]string, len(resp.Msg.GetItems()))
	add := func(id, name, status string, version int32, modelRef string) {
		models[id] = modelRef
		items = append(items, screenkit.Item{
			ID:    id,
			Title: name,
			Meta:  status + " v" + screenkit.FmtInt(int(version)),
		})
	}
	for _, it := range resp.Msg.GetItems() {
		w := it.GetWorker()
		add(w.GetId(), w.GetName(), strings.ToLower(w.GetStatus().String()), w.GetCurrentVersion(), it.GetActiveModelRef())
	}
	if len(items) == 0 {
		// An older plane may populate only the deprecated field: fall back so the
		// pane is never empty.
		for _, w := range resp.Msg.GetWorkers() {
			add(w.GetId(), w.GetName(), strings.ToLower(w.GetStatus().String()), w.GetCurrentVersion(), "")
		}
	}
	m.workerMu.Lock()
	m.workerModel = models
	m.workerMu.Unlock()
	// GROUPED LAST, over the finished list and using THIS RESPONSE'S OWN categories: the grouping is a
	// presentation of the same rows, and the response is the freshest thing we hold (see grouped).
	return m.grouped(items, resp.Msg.GetCategories(), resp.Msg.GetAssignments()), resp.Msg.GetNextPageToken(), nil
}

func (m *Model) fetchRuns(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflowRuns(ctx, connect.NewRequest(&apiv1.ListWorkflowRunsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	// Resolve ids to names BEFORE rendering the rows, so the pane never flashes bare ids on
	// a page it could have labelled. Best effort: an unresolvable name falls back to the id.
	if m.runNames.stale() {
		m.loadRunNames(ctx)
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Runs))
	for _, r := range resp.Msg.Runs {
		items = append(items, screenkit.Item{
			ID:    r.GetId(),
			Title: m.runsTitle(r),
			// The BARE status word, which is what the pane's chords compare against.
			Meta: workflowRunStatusWord(r.GetStatus()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

// workflowRunStatusWord is a run's status as a single lowercase word: the enum's own suffix with
// the WORKFLOW_RUN_STATUS_ prefix removed.
func workflowRunStatusWord(s apiv1.WorkflowRunStatus) string {
	w := strings.ToLower(s.String())
	w = strings.TrimPrefix(w, "workflow_run_status_")
	if w == "unspecified" || w == "" {
		return "unknown"
	}
	return w
}

func (m *Model) fetchWorkflows(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Workflows))
	for _, w := range resp.Msg.Workflows {
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetName(),
			Meta:  strings.ToLower(w.GetStatus().String()),
		})
	}
	return m.grouped(items, resp.Msg.GetCategories(), resp.Msg.GetAssignments()), resp.Msg.NextPageToken, nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case "workflows":
		// Workflows are an Execution-domain surface (the operator's "Workflows should be under Execution
		// not Automation").
		resp, err := m.cl.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkflow()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "name", Value: w.GetName()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "type", Value: w.GetType()},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "current ver", Value: screenkit.FmtInt(int(w.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(w.GetUpdatedAt())},
		}
		// The FLOW first — what a run would actually execute, computed from depends_on — then the version
		// trail.
		var body strings.Builder
		if vr, err := m.cl.Workflows.ListWorkflowVersions(ctx, connect.NewRequest(&apiv1.ListWorkflowVersionsRequest{WorkflowId: id})); err == nil {
			versions := vr.Msg.GetVersions()
			if v := pickFlowVersion(versions); v != nil {
				shown := "draft"
				if v.GetStatus() == apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_PUBLISHED {
					shown = "published"
				}
				n := flowStepCount(v.GetSteps())
				body.WriteString(theme.ListTitle.Render(fmt.Sprintf("FLOW  v%d %s · %d steps", v.GetVersion(), shown, n)) + "\n")
				if flow := renderWorkflowFlow(v.GetSteps(), m.w); flow != "" {
					body.WriteString(flow + "\n")
				} else {
					body.WriteString(theme.HintText.Render("  no steps in this version") + "\n")
				}
				body.WriteString("\n")
			}
			body.WriteString(theme.ListTitle.Render("VERSIONS") + "\n")
			for _, v := range versions {
				body.WriteString("  v" + screenkit.FmtInt(int(v.GetVersion())) + "  " +
					strings.ToLower(v.GetStatus().String()) + "  " +
					v.GetVersionNote() + "\n")
			}
		}
		return "Workflow: " + w.GetName(), fields, strings.TrimRight(body.String(), "\n"), nil

	case "workers":
		// Workers belong to the Execution domain (GUI nav-config groups
		// Workers with Workflows/Executions/Schedules/Recovery).
		resp, err := m.cl.Workers.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorker()
		// The VERSION TRAIL is read HERE, before the fields are built, because the worker's MODEL is a
		// property of its newest version (ADR-0003: the ref is versioned state) and it belongs in the
		// FIELDS.
		var versions []*apiv1.WorkerVersion
		if vr, err := m.cl.Workers.ListWorkerVersions(ctx, connect.NewRequest(&apiv1.ListWorkerVersionsRequest{WorkerId: id})); err == nil {
			versions = vr.Msg.GetVersions()
		}
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "name", Value: w.GetName()},
			{Key: "slug", Value: w.GetSlug()},
			{Key: "status", Value: workerStatusLabel(w.GetStatus())},
			{Key: "current ver", Value: screenkit.FmtInt(int(w.GetCurrentVersion()))},
			// The model of the version dispatch would USE.
			{Key: "model", Value: workerModelRef(w, versions)},
			{Key: "description", Value: w.GetDescription()},
			{Key: "purpose", Value: w.GetPurpose()},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}
		// Version trail (published versions are immutable; the model_ref is
		// pinned by a human, so surfacing it per version matters).
		var body strings.Builder
		if len(versions) > 0 {
			body.WriteString(theme.ListTitle.Render("VERSIONS") + "\n")
			for _, v := range versions {
				body.WriteString("  v" + screenkit.FmtInt(int(v.GetVersion())) +
					"  " + workerVersionStatusWord(v.GetStatus()) +
					"  " + v.GetModelRef() + "\n")
			}
		}
		return "Worker: " + w.GetName(), fields, strings.TrimRight(body.String(), "\n"), nil
	case "schedules":
		// The row is a WORK ITEM in the upcoming/running views and a RUN in the finished view,
		// so the detail dispatches on the view rather than guessing from the id.
		if m.sched.view() == schedFinished {
			m.runNames.ensure(ctx, m)
			resp, err := m.cl.Workflows.GetWorkflowRun(ctx, connect.NewRequest(&apiv1.GetWorkflowRunRequest{Id: id}))
			if err != nil {
				return "", nil, "", err
			}
			r := resp.Msg.GetRun()
			fields := []screenkit.Field{
				{Key: "run", Value: r.GetId()},
				{Key: "status", Value: strings.ToLower(strings.TrimPrefix(r.GetStatus().String(), "WORKFLOW_RUN_STATUS_"))},
				{Key: "workflow", Value: m.runsWorkflowField(r)},
				{Key: "work item", Value: m.runsWorkItemField(r)},
				{Key: "started", Value: screenkit.FmtTime(r.GetStartedAt())},
				{Key: "ended", Value: screenkit.FmtTime(r.GetEndedAt())},
			}
			fields = append(fields, runSpanFields(r)...)
			fields = append(fields, screenkit.Field{Key: "actions", Value: "x: remove schedule · g: go to the run"})
			return "Finished run " + r.GetId(), fields, "", nil
		}
		return m.scheduleItemDetail(ctx, id)

	case "executions":
		resp, err := m.cl.Executions.GetExecution(ctx, connect.NewRequest(&apiv1.GetExecutionRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		e := resp.Msg.GetExecution()
		// The GUI's context strip as fields: worker NAME, workflow, work item, iteration, tokens, cost,
		// branch, PR — each omitted when empty rather than rendered as a zero the operator has to
		// interpret.
		meta := ""
		if it, ok := m.Base.SourceItem("executions", id); ok {
			meta = it.Meta
		}
		fields := executionFields(e, meta)
		// Store the run's own detail and COMPOSE the body from it, the todo list and the session —
		// three writers, one pane, so none of them can blank the others.
		m.execDetail.put(id, e, fields)
		return "Execution " + e.GetId(), fields, m.composeExecutionBodyFor(id), nil

	case "runs":
		resp, err := m.cl.Workflows.GetWorkflowRun(ctx, connect.NewRequest(&apiv1.GetWorkflowRunRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		r := resp.Msg.GetRun()
		// The names the list shows must also appear HERE, or opening a row would lose the
		// context the row gave (the ids alone are what the operator asked to be rid of).
		if m.runNames.stale() {
			m.loadRunNames(ctx)
		}
		wf := m.runsWorkflowField(r)
		item := m.runsWorkItemField(r)
		fields := []screenkit.Field{
			{Key: "id", Value: r.GetId()},
			{Key: "status", Value: strings.ToLower(r.GetStatus().String())},
			{Key: "workflow", Value: wf},
			{Key: "version", Value: screenkit.FmtInt(int(r.GetWorkflowVersion()))},
			{Key: "current step", Value: r.GetCurrentStep()},
			{Key: "work item", Value: item},
			{Key: "branch", Value: r.GetWorktreeBranch()},
			{Key: "pr", Value: r.GetPrUrl()},
			{Key: "started", Value: screenkit.FmtTime(r.GetStartedAt())},
			{Key: "ended", Value: screenkit.FmtTime(r.GetEndedAt())},
		}
		fields = append(fields, runSpanFields(r)...)
		// Step runs are rendered as a FLOW, not a list (run_flow.go): the operator's "mimic a similar look
		// to our new workflow view where we have the steps.
		if msg := tuiFailureText(r); msg != "" {
			fields = append(fields, screenkit.Field{Key: "error", Value: msg})
			if kind := tuiFailureKind(r); kind != "" {
				fields = append(fields, screenkit.Field{Key: "error kind", Value: kind})
			}
		}
		var body string
		if sr, err := m.cl.Workflows.GetWorkflowStepRuns(ctx, connect.NewRequest(&apiv1.GetWorkflowStepRunsRequest{RunId: id})); err == nil {
			m.runFlow.setRows(runStepRows(sr.Msg.GetStepRuns(), m.runStepOrder(ctx, r)), id)
			body, _ = renderRunFlow(m.runFlow.rows(), m.w, m.runFlow.sel())
		}
		fields = append(fields, screenkit.Field{Key: "steps", Value: screenkit.FmtInt(m.runFlow.count())})
		if cur := m.runFlow.current(); cur != nil {
			if cur.executionID != "" {
				fields = append(fields, screenkit.Field{Key: "selected", Value: cur.name + " → enter: execution " + cur.executionID})
			} else {
				fields = append(fields, screenkit.Field{Key: "selected", Value: cur.name + " — no execution linked"})
			}
		}
		return "Workflow Run " + r.GetId(), fields, body, nil
	}
	return "", nil, "", nil
}

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	// The MODEL PICKER is a modal layered ABOVE everything else on this screen:
	// while it is up it owns every key and the mouse, and load results route to it.
	if mp := m.modelPicker; mp != nil {
		switch msg := msg.(type) {
		case modelKindsMsg:
			return m, m.applyModelKinds(msg)
		case modelProvidersMsg:
			return m, m.applyModelProviders(msg)
		case modelModelsMsg:
			return m, m.applyModelModels(msg)
		case tea.KeyMsg:
			_, cmd := mp.HandleKey(msg)
			return m, tea.Batch(cmd, m.finishModelPicker(mp))
		case tea.MouseMsg:
			_, cmd := mp.HandleMouse(msg)
			return m, tea.Batch(cmd, m.finishModelPicker(mp))
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case workerDetailMsg:
		// A worker CRUD chord's load finished: open the form it was waiting for
		// (or refuse, naming the reason). See worker_forms.go.
		return m, m.openWorkerOpForm(msg)

	case workflowDetailMsg:
		// The workflow equivalent (workflow_forms.go).
		return m, m.openWorkflowOpForm(msg)

	case workflowEditorMsg:
		// A workflow's detail landed: point the STEP editor at the version shown, and if this is the load a
		// CREATE was waiting for, drop straight into the edit mode so the operator can add the first step.
		cmd := m.enterStepEditor(msg.id, msg.name, msg.version)
		// The worker list feeds the step editor's worker picker, so it loads with the
		// editor rather than waiting for the operator to open a step form.
		cmd = tea.Batch(cmd, m.loadFlowWorkers())
		if m.flowEditPending {
			m.flowEditPending = false
			if c := m.beginFlowEdit(); c != nil {
				cmd = tea.Batch(cmd, c)
			}
		}
		return m, cmd

	case flowWorkersMsg:
		// The picker's option list landed.
		if msg.err == nil {
			m.flowWorkers = msg.options
			if m.flowEditing {
				return m, m.paintFlow()
			}
		}
		return m, nil

	case execUsageMsg:
		// The context / usage picture landed. Like the todo list, it repaints from cache and asks
		// for nothing — a landing that requests work is a loop with no base case.
		if msg.err == nil && msg.usage != nil {
			m.execUsage.put(msg.execID, msg.usage)
		}
		if m.Base.DetailID() == msg.execID && m.Base.ActiveSourceName() == srcExecutions {
			return m, m.repaintExecutionDetail()
		}
		return m, nil

	case execTodosMsg:
		// The worker's todo list landed.
		if msg.err == nil {
			m.todos.put(msg.execID, msg.todos)
		}
		// Repaint in place when this is the execution on screen, so the list appears without the operator
		// reselecting the row.
		if m.Base.DetailID() == msg.execID && m.Base.ActiveSourceName() == srcExecutions {
			return m, m.repaintExecutionDetail()
		}
		return m, nil

	case subs.EventPokeMsg:
		if msg.Name == "execution-events" && m.Base.ActiveSourceName() == "executions" && m.Base.DetailID() != "" {
			if sh, ok := m.Shell().(interface {
				RefreshExecutionSession(execID string, events []*apiv1.StreamExecutionEventsResponse)
			}); ok {
				sh.RefreshExecutionSession(m.Base.DetailID(), m.SessionEvents(m.Base.DetailID()))
			}
		}
		// A LIVE run's step flow must not go stale: the whole point of rendering a run as a flow is
		// watching its steps turn, so an execution event on the RUNS pane re-reads the run rather than
		// waiting.
		if msg.Name == "execution-events" && m.Base.ActiveSourceName() == srcRuns && m.Base.DetailID() != "" {
			return m, m.Base.RequestDetail(srcRuns, m.Base.DetailID())
		}
		return m, m.reg.WaitEventPoke("execution-events")

	case subs.StatusMsg:
		m.Base.SetStatus(msg.Name, string(msg.Status))
		// Re-arm the channel that actually reported: two streams feed this screen now (execution events and
		// workflow events), and re-arming only the first would strand the other's status.
		cmd := m.reg.WaitStatus(msg.Name)
		if msg.Name != "execution-events" {
			return m, cmd
		}
		if msg.Status == "open" && m.reconnected {
			// reconnect gap: refetch lists (invalidate-on-reconnect)
			return m, tea.Batch(cmd, m.Load(), m.requestSessionRefresh())
		}
		if msg.Status != "open" {
			m.reconnected = true
		} else {
			// first open with a detail already showing: refresh its
			// session view so buffered events merge in
			return m, tea.Batch(cmd, m.requestSessionRefresh())
		}
		return m, cmd

	case tea.KeyMsg:
		// The inline DETAILS-PANE editor owns every key while it is up — it is the focused surface.
		if m.Base.EditingDetail() {
			if handled, cmd := m.Base.Update(msg); handled {
				return m, cmd
			}
		}
		// The interjection form owns every key while it is up.
		if m.form != nil {
			if msg.String() == "esc" {
				m.form = nil
				m.notice = "cancelled"
				return m, nil
			}
			cmd, _ := m.form.HandleKey(msg)
			if m.form.Submitted {
				m.form = nil
			}
			return m, cmd
		}
		// Write chords run only when no confirmation dialog is open (the
		// dialog owns every key through the kit2 base).
		if m.Open == nil {
			if cmd, handled := m.handleActionKey(msg.String()); handled {
				return m, cmd
			}
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

func (m *Model) View() string {
	m.refreshActionBar()

	body := m.Base.View()
	if m.notice != "" {
		body += "\n" + theme.HintText.Render(m.notice)
	}
	if m.w > 0 && m.h > 0 {
		body = kit2.FitLines(body, m.w, m.h)
		if m.form != nil {
			body = kit2.Center(body, executionFormBox(m.form, m.w), m.w, m.h)
		} else if m.Open != nil {
			box := m.Open.Box(minInt(72, m.w-4), minInt(12, m.h-2))
			body = kit2.Center(body, box, m.w, m.h)
		}
		// The worker-model picker is spliced LAST so it layers above the form.
		if m.modelPicker != nil {
			m.modelPicker.SetScreen(m.w, m.h)
			body = kit2.Center(body, m.modelPicker.View(), m.w, m.h)
		}
		// The hint line is rendered by the shell (HintLine()), never
		// appended here — the screen must fill EXACTLY the content region.
		return kit2.FitLines(body, m.w, m.h)
	}
	return m.Base.Frame(body)
}

// refreshActionBar publishes the focused row's actions into the footer strip.
func (m *Model) refreshActionBar() {
	actions := m.actionsForSelection()
	m.bar.Actions = actions
	if m.bar.Sel >= len(actions) {
		m.bar.Sel = 0
	}
	m.Base.Bar = m.bar
}

// executionFormBox wraps the interjection form in a titled dialog-sized box.
func executionFormBox(f *kit2.Form, w int) string {
	bw := minInt(70, w-4)
	if bw < 24 {
		bw = 24
	}
	d := &kit2.Dialog{Title: f.Title, Body: f.View(), Buttons: []string{"send", "cancel"}}
	return d.Box(bw, minInt(14, len(f.Specs)*2+5))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RunningExecutionID returns the selected execution's ID when its status is RUNNING — the
// interjection context for the chat dock (plan §3.
func (m *Model) RunningExecutionID() (string, bool) {
	it, ok := m.Base.ActiveItem()
	if !ok || m.Base.ActiveSourceName() != "executions" {
		return "", false
	}
	return m.runningID(it.ID), true
}

// runningID reports whether the execution is running, from the detail
// status the screen has cached (ListExecutions meta / GetExecution).
func (m *Model) runningID(id string) string {
	// The detail pane refreshes the status; consult the cached fetch
	// state via the list item meta (set to the lowercase status string).
	if it, ok := m.Base.SourceItem("executions", id); ok {
		if strings.EqualFold(it.Meta, "running") || strings.EqualFold(it.Meta, "in_progress") ||
			strings.EqualFold(it.Meta, "in-progress") || strings.EqualFold(it.Meta, "starting") ||
			strings.EqualFold(it.Meta, "dispatching") || strings.EqualFold(it.Meta, "queued") {
			return id
		}
	}
	return ""
}

// ActiveContext returns the context engine triple (source, id, label)
// for the dock chip / footer chip.
func (m *Model) ActiveContext() (src, id, label string) {
	src = m.Base.ActiveSourceName()
	it, ok := m.Base.ActiveItem()
	if !ok {
		return src, "", ""
	}
	label = it.Title
	if label == "" {
		label = it.ID
	}
	if m.runningID(it.ID) != "" {
		label = "running " + label
	}
	return src, it.ID, label
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// DetailWidth exposes the detail pane width (session bubble wrapping).
func (m *Model) DetailWidth() int { return m.Base.DetailWidth() }

// onDetail fires when the detail pane shows an execution: the shell
// loads its durable session and merges the live event stream into it.
func (m *Model) onDetail(src, id string) tea.Cmd {
	// A WORKFLOW detail opens the STEP editor: the flow view IS the editing surface,
	// so selecting a workflow puts the pane (and its cursor) into edit mode.
	if src == srcWorkflows {
		return m.onDetailWorkflow(id)
	}
	if src != "executions" {
		return nil
	}
	// INSTALL THE MESSAGE BOX, which is the moment the pane knows WHICH execution it is showing (the
	// base calls this hook right after SetContent on a detail landing).
	m.installComposerFooter(id)
	// An EXECUTION detail loads its SESSION TRANSCRIPT (via the shell, which owns the durable+live
	// merge) and, when its cache is stale, its worker TODO LIST.
	cmds := []tea.Cmd{m.todosRefreshCmd(id), m.usageRefreshCmd(id)}
	if sh, ok := m.Shell().(interface{ OpenExecutionSession(string) tea.Cmd }); ok {
		cmds = append(cmds, sh.OpenExecutionSession(id))
	}
	return tea.Batch(cmds...)
}

// requestSessionRefresh re-opens the session view for the detail pane's
// current execution (sub reconnect: buffered events changed).
func (m *Model) requestSessionRefresh() tea.Cmd {
	id := m.Base.DetailID()
	if id == "" || m.Base.ActiveSourceName() != "executions" {
		return nil
	}
	if sh, ok := m.Shell().(interface{ OpenExecutionSession(string) tea.Cmd }); ok {
		return sh.OpenExecutionSession(id)
	}
	return nil
}

// SessionEvents returns the live stream events for one execution,
// filtered from the subscription's ring buffer (oldest first).
func (m *Model) SessionEvents(execID string) []*apiv1.StreamExecutionEventsResponse {
	if m.sub == nil {
		return nil
	}
	var out []*apiv1.StreamExecutionEventsResponse
	for _, ev := range m.sub.Events() {
		if ev.GetEvent().GetExecutionId() == execID {
			out = append(out, ev)
		}
	}
	return out
}

// RenderSession paints the merged live-session view into the open
// detail pane (durable parts + live events, phase-grouped).
func (m *Model) RenderSession(items []chat.ChatItem) {
	id := m.Base.DetailID()
	// The transcript is ONE of the three parts of this pane, so it is RECORDED and then composed with
	// the run's facts and the todo list.
	m.execDetail.putTranscript(id, items)
	body, fields := m.composeExecutionBody(id)
	if len(fields) == 0 {
		// The session can paint before the detail arrives. Keep the pane's existing shape rather
		// than blanking it — the facts are on their way, and they carry the fields.
		return
	}
	m.paintExecution(id, fields, body)
}

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// tuiFailureText returns the run's recorded failure, wrapped so the field list can render it in a
// narrow pane.
func tuiFailureText(r *apiv1.WorkflowRun) string {
	msg := strings.TrimSpace(r.GetErrorMessage())
	if msg == "" {
		return ""
	}
	lines := strings.Split(msg, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, " ")
}

// tuiFailureKind is the run's error_kind, or "" when unclassified.
func tuiFailureKind(r *apiv1.WorkflowRun) string {
	return strings.TrimSpace(r.GetErrorKind())
}
