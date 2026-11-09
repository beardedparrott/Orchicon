// actions.go — the Execution screen's WRITE paths. Every mutation the GUI
// exposes lands here:
//
//	executions  CancelExecution (Confirm) · SendExecutionMessage (interject, form)
//	runs        RetryFailedWorkflowRun (Confirm) · ForceProgressWorkflowRun (Confirm)
//
// Discipline the acceptance criteria pin:
//   - cancel / retry / force-progress are Confirm-gated and the dialog states
//     PLAINLY what the write does (force-progress is a manual escape hatch);
//   - the write runs through the ONE mutation executor (dock feedback +
//     reconcile of the affected list), never a bare RPC from the update loop;
//   - a run that is not FAILED has no retry action, a run that is not RUNNING
//     has no force-progress action — and pressing the key anyway explains why
//     instead of being a silent no-op.
package execution

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Source names (also the slash-command slugs the shell generates).
const (
	srcExecutions = "executions"
	srcRuns       = "runs"
	srcWorkers    = "workers"
	srcWorkflows  = "workflows"
	// srcSchedules is the Schedules pane: upcoming / running / finished, cycled with `v`.
	srcSchedules = "schedules"
)

// cancelReason is the audit reason recorded on a TUI-initiated cancel.
const cancelReason = "cancelled from the orch TUI"

// abortReason is the audit reason recorded on a TUI-initiated run abort, mirroring the
// GUI's "user clicked Stop".
const abortReason = "aborted from the orch TUI"

// Write chords (matched by name in handleActionKey).
const (
	keyCancel        = "c"
	keyInterject     = "i"
	keyRetryRun      = "t"
	keyForceProgress = "p"
	// keyAbortRun aborts a LIVE run.
	keyAbortRun = "S"
	keySetModel = "m"
	// keyBulkSetModel sets the model for EVERY marked worker at once — the TUI's counterpart to the
	// GUI's BulkChangeWorkerModelDialog.
	keyBulkSetModel = "M"
	// Worker CRUD (Item 6). Each opens a form or a confirm; none writes from
	// Update directly.
	keyNewWorker   = "n"
	keyEditWorker  = "e"
	keyEditVersion = "V"
	keyPublish     = "p"
	keySetActive   = "a"
	keyDeprecate   = "u"
	// keyDelete is THE delete chord, for BOTH panes and for both single and bulk deletes.
	keyDelete = kit2.DeleteChord
	// keyCategorize ASSIGNS the selected item to a grouping (worker / workflow / conversation
	// categories).
	keyCategorize = "C"
	// Workflow lifecycle.
	keyNewWorkflow  = "n"
	keyEditWorkflow = "E"
	keyPublishWf    = "p"
	// keyNewVersionWf creates the workflow's NEXT version — a DRAFT seeded from the newest, which is
	// what editing a step would create anyway once the target stopped being a draft.
	keyNewVersionWf = "V"
	keyDeprecateWf  = "u"
	// Delete is keyDelete — ONE chord for both panes (see its declaration).
	keyFlowEdit       = "e"
	keyFlowEditStep   = "enter"
	keyFlowAddStep    = "a"
	keyFlowAddStepAlt = "-"
	keyFlowRemoveStep = "x"
	keyFlowExit       = "esc"
	// Retained so the flow cursor keys read the same as before.
	keyStepUp   = "up"
	keyStepDown = "down"
	// SCHEDULES chords.
	keySchedView   = "v"
	keyGoToRun     = "g"
	keySchedDelete = "x"
)

// The RETIRED chords.
const (
	keyFollowUpLegacy  = "f"
	keyInterjectLegacy = "i"
)

// DropKeyClaim releases the screen's key claim so the focus chord can return the operator to the
// composer (see kit2.Base.DropKeyClaim).
func (m *Model) DropKeyClaim() { m.Base.DropKeyClaim() }

// ClaimsKeys reports whether the screen owns every key right now (an open interjection form, the
// confirm dialog, or the inline detail editor).
func (m *Model) ClaimsKeys() bool {
	return m.form != nil || m.Open != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// FormOpen reports whether a FORM is open — modal OR inline details-pane editor, plus the model
// picker (a modal layered over a form).
func (m *Model) FormOpen() bool {
	return m.form != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// OwnsTab reports that Tab is a move INSIDE this pane, not a move of the shell's tab ring: on the
// Executions detail, Tab toggles between the TRANSCRIPT and the MESSAGE BOX.
func (m *Model) OwnsTab() bool {
	return m.Base.ActiveSourceName() == srcExecutions && m.Base.DetailFocusedForTest() && m.Base.DetailID() != ""
}

// ActiveForm returns the open form (nil when closed) — tests and the shell
// read the in-progress input through it.
func (m *Model) ActiveForm() *kit2.Form { return m.form }

// DialogOpen reports whether a confirmation dialog is up.
func (m *Model) DialogOpen() bool { return m.Open != nil }

// Notice returns the last action's status line ("" = none).
func (m *Model) Notice() string { return m.notice }

// actionsForSelection builds the entity-bound actions for the focused row.
// An action that does not apply to the row's state is simply absent.
func (m *Model) actionsForSelection() []kit2.Action {
	// The SHARED bulk rule comes first, for the source that offers bulk actions: more than one marked
	// row means the operator is operating on a SELECTION, not on the row the cursor.
	if m.ActiveSourceName() == srcSchedules {
		if ids := m.Base.BulkIDs(); len(ids) > 0 {
			return m.scheduleDeleteActions(ids)
		}
	}
	// WORKERS and WORKFLOWS take the same rule: more than one marked row means the operator is
	// operating on a SELECTION, so the single-row actions are replaced rather than mixed with it.
	if m.ActiveSourceName() == srcWorkers {
		if ids := m.markableIDs(); len(ids) >= kit2.BulkThreshold {
			acts := m.entityBulkActions(srcWorkers, keyDelete, "worker", ids, m.rpcDeleteWorker)
			// The worker-only bulk act.
			return append(acts, kit2.Action{
				Label: fmt.Sprintf("set model %d selected", len(ids)), Key: keyBulkSetModel, Source: srcWorkers,
				Do: func(context.Context) error {
					return errors.New("press " + keyBulkSetModel + " with rows marked to choose a model, or e to edit one worker")
				},
			})
		}
	}
	if m.ActiveSourceName() == srcWorkflows {
		if ids := m.markableIDs(); len(ids) >= kit2.BulkThreshold {
			return m.entityBulkActions(srcWorkflows, keyDelete, "workflow", ids, m.rpcDeleteWorkflow)
		}
	}
	// EXECUTIONS take the same rule.
	if m.ActiveSourceName() == srcExecutions {
		if ids := m.markableIDs(); len(ids) >= kit2.BulkThreshold {
			return m.executionBulkDeleteActions(ids)
		}
	}
	// RUNS take the same rule. Same reasoning as executions, plus the fact that a run's delete is the
	// ONLY one on this screen that reaches the server at all today (see DeleteWorkflowRun).
	if m.ActiveSourceName() == srcRuns {
		if ids := m.markableIDs(); len(ids) >= kit2.BulkThreshold {
			return m.runBulkDeleteActions(ids)
		}
	}
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case srcExecutions:
		id := item.ID
		// CANCEL is offered only for a LIVE execution — cancelling something already finished is not a
		// thing.
		acts := []kit2.Action{}
		if isLiveExecution(item.Meta) {
			acts = append(acts, kit2.Action{
				Label: "cancel", Key: keyCancel, Danger: true, Source: srcExecutions,
				Confirm: "Cancel execution " + id + "?\n" +
					"The execution transitions to TERMINATED and its worker session is stopped; " +
					"in-flight tool calls are abandoned (the reason is recorded as " + strconv.Quote(cancelReason) + ").",
				Do: func(ctx context.Context) error { return m.rpcCancelExecution(ctx, id) },
			})
		}
		acts = append(acts,
			// THE DELETE.
			kit2.Action{
				Label: "delete", Key: keyDelete, Danger: true, Source: srcExecutions,
				Confirm: "Delete execution " + id + "?\n" +
					"This REMOVES the execution and its transcript, and cannot be undone." +
					liveNote(item.Meta),
				Apply:    func() { m.Base.RemoveRow(srcExecutions, id) },
				Rollback: func() { m.Refresh(srcExecutions) },
				Do:       func(ctx context.Context) error { return m.rpcDeleteExecution(ctx, id) },
			},
			// The box, advertised as the gesture that reaches it.
			kit2.Action{
				Label: "↓ past the last block: message box", Source: srcExecutions,
				Do: func(context.Context) error {
					return errNeedForm("message box — walk down past the last block")
				},
			},
			// The COLLAPSE gesture, on the same principle: the operator needs to know it exists before trying
			// it on a 200-line tool dump.
			kit2.Action{
				Label: "enter on a block: expand / collapse", Source: srcExecutions,
				Do: func(context.Context) error {
					return errNeedForm("enter on a block toggles it — tool calls and thinking start collapsed")
				},
			},
		)
		return acts

	case srcRuns:
		id := item.ID
		meta := strings.ToLower(item.Meta)
		acts := []kit2.Action{}
		if meta == "failed" {
			acts = append(acts, kit2.Action{
				Label: "retry run", Key: keyRetryRun, Source: srcRuns,
				Confirm: "Retry failed run " + id + "?\n" +
					"The run resets to PENDING and its failed / skipped / blocked steps are re-armed; " +
					"steps that already SUCCEEDED stay succeeded — the DAG resumes where it stopped " +
					"instead of restarting.",
				Do: func(ctx context.Context) error { return m.rpcRetryFailedRun(ctx, id) },
			})
		}
		if meta == "running" {
			acts = append(acts, kit2.Action{
				Label: "force-progress", Key: keyForceProgress, Danger: true, Source: srcRuns,
				Confirm: "Force-progress wedged run " + id + "?\n" +
					"This marks every active non-terminal step run SUCCEEDED regardless of its real " +
					"state, terminates any still-running linked executions, and re-enqueues the run so " +
					"the reconciler advances the DAG. It is a manual escape hatch for a run wedged " +
					"\"running\" — a step that never actually finished will be recorded as succeeded.",
				Do: func(ctx context.Context) error { return m.rpcForceProgressRun(ctx, id) },
			})
		}
		// ABORT THE RUN — the TUI counterpart of the GUI's "Stop <run>" button
		// (frontend/src/routes/workflows_.$id.tsx, shown only while `latestRunActive`).
		if meta == "running" {
			acts = append(acts, kit2.Action{
				Label: "abort run", Key: keyAbortRun, Danger: true, Source: srcRuns,
				Confirm: "Abort run " + id + "?\n" +
					"The run goes to ABORTED, its in-flight step runs fail and any running worker " +
					"executions are terminated. Unlike delete, the run and its history are KEPT; " +
					"its runtime container is reaped on the next reconcile pass.",
				Do: func(ctx context.Context) error { return m.rpcAbortRun(ctx, id) },
			})
		}
		// THE DELETE.
		acts = append(acts, kit2.Action{
			Label: "delete", Key: keyDelete, Danger: true, Source: srcRuns,
			Confirm: "Delete run " + id + "?\n" +
				"This REMOVES the run and its step runs, and cannot be undone." + runLiveNote(meta),
			Apply:    func() { m.Base.RemoveRow(srcRuns, id) },
			Rollback: func() { m.Refresh(srcRuns) },
			Do:       func(ctx context.Context) error { return m.rpcDeleteRun(ctx, id) },
		})
		return acts

	case srcSchedules:
		// The single-row delete. The BULK case is handled before the switch, because a
		// selection replaces the cursor-row actions rather than joining them.
		return m.scheduleDeleteActions([]string{item.ID})

	case srcWorkers:
		// Item 6: the full CRUD surface.
		id, name := item.ID, item.Title
		status := workerStatusOf(item.Meta)
		acts := []kit2.Action{
			{Label: "new worker", Key: keyNewWorker, Source: srcWorkers,
				Do: func(context.Context) error { return errNeedForm("new worker") }},
			{Label: "edit", Key: keyEditWorker, Source: srcWorkers,
				Do: func(context.Context) error { return errNeedForm("edit worker") }},
			{Label: "new version", Key: keyEditVersion, Source: srcWorkers,
				Do: func(context.Context) error { return errNeedForm("new version") }},
		}
		if status != "retired" {
			// publish names the version it will ship (handleActionKey loads the trail first and refuses when
			// there is no draft), so the operator is never guessing what a publish does.
			acts = append(acts,
				kit2.Action{Label: "publish", Key: keyPublish, Source: srcWorkers,
					Do: func(context.Context) error { return errNeedForm("publish") }},
				kit2.Action{Label: "set active version", Key: keySetActive, Source: srcWorkers,
					Do: func(context.Context) error { return errNeedForm("set active version") }},
			)
		}
		if status == "published" {
			acts = append(acts, kit2.Action{
				Label: "deprecate", Key: keyDeprecate, Danger: true, Source: srcWorkers,
				Confirm: "Deprecate worker " + name + "?\n" +
					"New executions stop being dispatched to it. Its versions stay readable and " +
					"publishing again reverses this.",
				Do: func(ctx context.Context) error { return m.rpcDeprecateWorker(ctx, id) },
			})
		}
		if status != "retired" {
			acts = append(acts, kit2.Action{
				Label: "delete", Key: keyDelete, Danger: true, Source: srcWorkers,
				Confirm: "Delete worker " + name + "?\n" +
					"This removes the worker AND every version of it, and cannot be undone.",
				Apply:    func() { m.Base.RemoveRow(srcWorkers, id) },
				Rollback: func() { m.Refresh(srcWorkers) },
				Do:       func(ctx context.Context) error { return m.rpcDeleteWorker(ctx, id) },
			})
		}
		return acts
	case srcWorkflows:
		// The workflow lifecycle.
		id, name := item.ID, item.Title
		status := workflowStatusOf(item.Meta)
		acts := []kit2.Action{
			{Label: "new workflow", Key: keyNewWorkflow, Source: srcWorkflows,
				Do: func(context.Context) error { return errNeedForm("new workflow") }},
			{Label: "edit", Key: keyEditWorkflow, Source: srcWorkflows,
				Do: func(context.Context) error { return errNeedForm("edit workflow") }},
			{Label: "publish", Key: keyPublishWf, Source: srcWorkflows,
				Do: func(context.Context) error { return errNeedForm("publish") }},
		}
		if status == "published" {
			acts = append(acts, kit2.Action{
				Label: "deprecate", Key: keyDeprecateWf, Danger: true, Source: srcWorkflows,
				Confirm: "Deprecate workflow " + name + "?\n" +
					"New runs stop being started from it; its versions stay readable and publishing again reverses this.",
				Do: func(ctx context.Context) error { return m.rpcDeprecateWorkflow(ctx, id) },
			})
		}
		acts = append(acts, kit2.Action{
			Label: "delete", Key: keyDelete, Danger: true, Source: srcWorkflows,
			Confirm: "Delete workflow " + name + "?\n" +
				"This removes the workflow and every version of it, and cannot be undone.",
			Apply:    func() { m.Base.RemoveRow(srcWorkflows, id) },
			Rollback: func() { m.Refresh(srcWorkflows) },
			Do:       func(ctx context.Context) error { return m.rpcDeleteWorkflow(ctx, id) },
		})
		return acts
	}
	return nil
}

// onDetailWorkflow enters the STEP editor when a workflow's detail loads.
func (m *Model) onDetailWorkflow(id string) tea.Cmd {
	if m.rpcGetWorkflow == nil || m.rpcListWorkflowVersions == nil {
		return nil
	}
	get, list := m.rpcGetWorkflow, m.rpcListWorkflowVersions
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		w, err := get(ctx, id)
		if err != nil {
			return nil // the pane already reports the load failure
		}
		vs, err := list(ctx, id)
		if err != nil {
			vs = nil
		}
		return workflowEditorMsg{id: id, name: w.GetName(), version: pickFlowVersion(vs)}
	}
}

// workflowEditorMsg carries the version the step editor should target.
type workflowEditorMsg struct {
	id      string
	name    string
	version *apiv1.WorkflowVersion
}

// Outside it the flow view is a READ-ONLY view and this handler claims only the key that opens the
// mode — the previous shape claimed the step chords whenever a flow was on screen.
func (m *Model) handleFlowKeys(kstr string) (tea.Cmd, bool) {
	if m.stepWorkflowID == "" || m.Base.DetailID() == "" {
		return nil, false
	}
	if !m.flowEditing {
		if kstr == keyFlowEdit {
			return m.beginFlowEdit(), true
		}
		return nil, false
	}
	switch kstr {
	case keyStepDown, "j":
		return m.stepCursor(1), true
	case keyStepUp, "k":
		return m.stepCursor(-1), true
	case keyFlowEditStep, " ", "space":
		// ENTER edits the step the cursor is on — the operator's explicit ask.
		s, ok := m.selectedFlowStep()
		if !ok {
			return m.refuse("no step selected — press a to add the first one"), true
		}
		m.Base.BeginDetailEdit("Edit step", m.editStepForm(s))
		m.notice = ""
		return nil, true
	case keyFlowAddStep, keyFlowAddStepAlt:
		m.Base.BeginDetailEdit("Add step", m.addStepForm())
		m.notice = ""
		return nil, true
	case keyFlowRemoveStep:
		s, ok := m.selectedFlowStep()
		if !ok {
			return m.refuse("no step selected"), true
		}
		steps := m.flowStepsOf()
		if len(steps) <= 1 {
			return m.refuse("a workflow needs at least one step — edit this one instead of removing it"), true
		}
		name := orDefaultStr(s.Name, s.ID)
		desc := "Remove step " + name + "?\n"
		if len(s.deps) > 0 || flowBranchOf(s) != nil {
			desc += "Steps that ran after it, and any branch pointing at it, are rewired to skip it."
		}
		return m.confirmRemoveStep(s.ID, name, desc), true
	case keyEditWorkflow:
		// Renaming the workflow is a different act from editing its steps, so it keeps
		// its own key inside the mode rather than sharing `enter`.
		it, ok := m.ActiveItem()
		if !ok {
			return m.refuse("select a workflow first"), true
		}
		return m.beginWorkflowOp(it.ID, opEditHeader), true
	case keyFlowExit:
		return m.endFlowEdit(), true
	}
	return nil, false
}

// beginFlowEdit enters the workflow edit mode and says what the keys are: a mode the
// operator cannot tell they are in is worse than no mode at all.
func (m *Model) beginFlowEdit() tea.Cmd {
	if m.stepWorkflowID == "" {
		return m.refuse("select a workflow first")
	}
	m.flowEditing = true
	// Seed the cursor if this is a fresh entry and the flow has steps.
	if _, ok := m.selectedFlowStep(); !ok {
		if steps := m.flowStepsOf(); len(steps) > 0 {
			m.stepSel = steps[0].ID
		}
	}
	m.notice = "editing " + m.stepWorkflowName + " — " +
		"enter: edit step · a: add step · x: remove step · E: rename · esc: done"
	return m.paintFlow()
}

// createWorkflowVersion creates the workflow's next VERSION as a draft, then re-reads the detail so
// its steps and the VERSIONS trail reflect it.
func (m *Model) createWorkflowVersion(id string) tea.Cmd {
	create := m.rpcCreateWorkflowVersion
	if create == nil {
		return m.refuse("no workflow client")
	}
	m.notice = "creating a new version of " + id + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := create(ctx, id); err != nil {
			return mutate.Result{Name: "new workflow version", Source: srcWorkflows, Err: err}
		}
		return mutate.Result{Name: "new workflow version", Source: srcWorkflows}
	}
}

// endFlowEdit leaves the mode and reports it.
func (m *Model) endFlowEdit() tea.Cmd {
	if !m.flowEditing {
		return nil
	}
	m.flowEditing = false
	m.notice = "finished editing " + m.stepWorkflowName
	// THE PANE GOES BACK TO THE READ-ONLY DETAIL, not to the editor's own render.
	if id := m.Base.DetailID(); id != "" {
		return m.Base.RequestDetail(srcWorkflows, id)
	}
	return nil
}

// confirmRemoveStep gates a step removal behind the confirm dialog (it rewires the
// graph, so it is not silent).
func (m *Model) confirmRemoveStep(id, name, desc string) tea.Cmd {
	d := kit2.Confirm("Remove step", desc, "remove")
	d.Danger = true
	m.Open = d
	m.OnDialog = func(choice string) tea.Cmd {
		m.OnDialog = nil
		if choice == "" {
			m.notice = "cancelled"
			return nil
		}
		remaining := removeStep(m.rawFlowSteps(), id)
		// The cursor must move off the step that just disappeared.
		m.stepSel = ""
		if len(remaining) > 0 {
			m.stepSel = remaining[0].ID
		}
		cmd, err := m.saveSteps(remaining, "remove step "+name)
		if err != nil {
			m.notice = err.Error()
			return nil
		}
		return tea.Batch(cmd, m.paintFlow())
	}
	return nil
}

// handleActionKey dispatches a write chord for the focused source. handled
// is false when the key belongs to the shared navigation layer.
func (m *Model) handleActionKey(kstr string) (tea.Cmd, bool) {
	// THE EXECUTION TRANSCRIPT'S BLOCK CURSOR AND COMPOSER own the keys while the Executions pane's
	// detail is focused (execution_blocks.go).
	//
	// This is the operator's model, and it is ONE mechanism rather than two gestures:
	//
	//	"The executions are incredibly long ... a user can move down the line using tab or down
	//	 arrow and hitting enter on a block should collapse or expand it."
	//	"I would rather a chat box be at the bottom of the execution and you can ... gain focus with
	//	 the down arrow key or tab and then type in your response and hit enter to send it."
	//
	// So: up/down (or tab/shift+tab) move the cursor through the blocks, enter toggles a collapsible
	// one, and WALKING DOWN PAST THE LAST BLOCK lands in the composer — where the same keys type and
	// `enter` sends. Walking up from the composer returns to the blocks. There is no separate key to
	// learn for the input box, which is the whole point: the old `f` modal was unintuitive precisely
	// because reaching it was its own gesture.
	//
	// Scoped to the detail focus, like the runs flow: with the LIST focused the same keys move the
	// list, which is the operator's navigation model. And `enter` is NOT claimed while the composer
	// holds the keyboard — there it means SEND, and it is handled by the composer branch below.
	if m.ActiveSourceName() == srcExecutions && m.Base.DetailFocusedForTest() {
		if cmd, handled := m.handleTranscriptKeys(kstr); handled {
			return cmd, true
		}
	}
	// THE RUNS STEP FLOW.
	if m.ActiveSourceName() == srcRuns && m.Base.DetailFocusedForTest() {
		switch kstr {
		case "up", "k":
			m.runFlow.moveSteps(-1)
			return m.repaintRunFlow(), true
		case "down", "j":
			m.runFlow.moveSteps(1)
			return m.repaintRunFlow(), true
		case "enter":
			if m.runFlow.count() > 0 {
				return m.goToRunStepExecution(), true
			}
		}
	}
	// The SCHEDULES pane's own chords (view cycle, jump to the run) run first, scoped to its pane: `v`
	// and `g` mean nothing elsewhere on this screen.
	if m.ActiveSourceName() == srcSchedules {
		if cmd, handled := m.handleSchedulesKeys(kstr); handled {
			return cmd, true
		}
	}
	// CATEGORY ROWS FIRST, for the two panes that group their rows.
	if m.ActiveSourceName() == srcWorkers || m.ActiveSourceName() == srcWorkflows {
		if cmd, handled := m.groupRowKey(kstr); handled {
			return cmd, true
		}
	}
	// The STEP editor owns the chords while a workflow's flow view is open.
	if m.ActiveSourceName() == srcWorkflows {
		if cmd, handled := m.handleFlowKeys(kstr); handled {
			return cmd, true
		}
	}
	// WORKFLOW lifecycle chords, scoped to the Workflows pane (see the key block
	// above for why the scoping is load-bearing).
	if m.ActiveSourceName() == srcWorkflows {
		if kstr == keyCategorize {
			return m.categorizeSelected(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW), true
		}
		switch kstr {
		case keyNewWorkflow:
			m.Base.BeginDetailEdit("New workflow", m.createWorkflowForm())
			m.notice = ""
			return nil, true
		case keyEditWorkflow, keyPublishWf:
			it, ok := m.ActiveItem()
			if !ok {
				return m.refuse("select a workflow first"), true
			}
			if screenkit.IsGroupRow(it.ID) {
				return m.refuse("that is a category row — enter expands or collapses it; pick a workflow inside"), true
			}
			op := opEditHeader
			if kstr == keyPublishWf {
				op = opPublish
			}
			return m.beginWorkflowOp(it.ID, op), true
		case keyNewVersionWf:
			// A CATEGORY ROW IS NOT A WORKFLOW, and there is nothing to version.
			it, ok := m.ActiveItem()
			if !ok {
				return m.refuse("select a workflow first"), true
			}
			if screenkit.IsGroupRow(it.ID) {
				return m.refuse("that is a category row — pick a workflow inside"), true
			}
			// NO FORM AND NO OP-CODES: creating a version is ONE RPC that produces a DRAFT seeded from the
			// newest, which is a reversible, additive act.
			return m.createWorkflowVersion(it.ID), true
		case keyFlowEdit:
			// `e` on the Workflows pane is the WORKFLOW EDIT MODE, not a step chord: "when someone hits 'e' to
			// edit a workflow, they are going to think they are editing the entire workflow and all its steps.
			if m.flowEditing {
				return nil, true
			}
			if m.stepWorkflowID == "" || m.Base.DetailID() == "" {
				return m.refuse("select a workflow first"), true
			}
			return m.beginFlowEdit(), true
		}
	}
	// Worker CRUD chords open FORMS (or start a load that opens one), so they are dispatched before the
	// generic action lookup — but ONLY while the Workers pane is focused.
	if m.ActiveSourceName() == srcWorkers {
		if kstr == keyCategorize {
			return m.categorizeSelected(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER), true
		}
		switch kstr {
		case keyNewWorker:
			// Item 3: worker editing happens IN THE DETAILS PANE, like work items — not in a modal.
			return m.beginWorkerOp("", opCreateWorker), true
		case keyEditWorker, keyEditVersion, keyPublish, keySetActive:
			it, ok := m.ActiveItem()
			if !ok {
				return m.refuse("select a worker first"), true
			}
			// A CATEGORY ROW IS NOT A WORKER.
			if screenkit.IsGroupRow(it.ID) {
				return m.refuse("that is a category row — enter expands or collapses it; pick a worker inside"), true
			}
			op := map[string]workerOp{
				keyEditWorker:  opEditHeader,
				keyEditVersion: opNewVersion,
				keyPublish:     opPublish,
				keySetActive:   opSetActive,
			}[kstr]
			return m.beginWorkerOp(it.ID, op), true
		}
	}
	if kstr == keyFollowUpLegacy || kstr == keyInterjectLegacy {
		// The two modals are GONE: one inline box at the bottom of the execution does both jobs, and
		// WHICH job is decided by the execution's state rather than by which key was pressed
		// (execution_blocks.go — the GUI's own composer, which has the same single control).
		//
		// They are kept as EXPLAINING stubs rather than deleted outright for two reasons, and both
		// are about the operator rather than the code:
		//   1. `f` WAS ALSO THE BASE'S PAGER (`kit2.Base.loadMore`, advertised in the pane title as
		//      "more pages: press f"). One key, two meanings — and the meaning the operator was
		//      reading was not the one that fired. The chord has to stop claiming `f`, and a stub
		//      that says where the box went is the honest way to retire muscle memory.
		//   2. An operator who learned `i` last week should not get silence.
		//
		// The stub also moves the cursor INTO the box, so the answer to "where did it go" is the
		// thing itself rather than a description of it.
		if m.ActiveSourceName() != srcExecutions {
			return m.refuse("the message box is on the Executions pane — focus it first"), true
		}
		m.blocks.cursor.atComposer = true
		m.notice = "the message box is now INLINE at the bottom of the execution — press enter to send"
		return m.repaintTranscript(), true
	}
	if kstr == keyBulkSetModel && m.ActiveSourceName() == srcWorkers {
		// BULK SET MODEL.
		ids := m.markableIDs()
		if len(ids) < kit2.BulkThreshold {
			// Refuse with the two REAL routes rather than a bare "no": an operator pressing M with nothing
			// marked needs to know both that marking is the bulk gesture and that one worker's model lives.
			if len(ids) == 1 {
				return m.refuse("one worker is not a bulk selection — press e to edit it, or mark two or more with space"), true
			}
			return m.refuse("mark two or more workers first (space marks), then " + keyBulkSetModel + " sets their model together"), true
		}
		return m.beginBulkSetModel(ids), true
	}
	// `m` MANAGES the selected worker's MCP + skills in the ONE MODAL — the same surface a conversation
	// gets from /scope and a project gets from the Work screen's `m`.
	if kstr == "m" && m.ActiveSourceName() == srcWorkers {
		if it, ok := m.ActiveItem(); ok {
			if h, ok := m.mcpModalHost(); ok {
				return h.OpenWorkerMCPModal(it.ID, it.Title), true
			}
		}
	}
	if kstr == keySetModel {
		// The chord is gone (the model is a form field now), but a conversation —
		// or a muscle memory — may still send it. Explain rather than no-op.
		return m.refuse("the model is a field on the Edit form (e) and the version editor (V) — open one and choose it there, or press " + keyBulkSetModel + " to set it for several marked workers"), true
	}
	// A MARKED SELECTION THAT YIELDS NO BULK ACTION MUST REFUSE, NEVER RETARGET — AND THIS CHECK HAS TO
	// RUN BEFORE actionByKey, WHICH IS THE WHOLE POINT.
	if len(m.Base.MarkedIDs()) > 0 && kstr == m.paneDeleteChord() {
		n, k := len(m.Base.MarkedIDs()), len(m.markableIDs())
		// ONE MARK IS NOT A BULK SELECTION, BUT ON THE SCHEDULES PANE IT IS STILL A TARGET.
		if m.ActiveSourceName() == srcSchedules && k == 1 {
			if acts := m.scheduleDeleteActions(m.markableIDs()); len(acts) > 0 {
				return m.openAction(acts[0]), true
			}
		}
		if k < kit2.BulkThreshold {
			if k < n {
				return m.refuse(fmt.Sprintf("%d rows are marked but only %d can be deleted here — a "+
					"category folder is not a %s. Nothing was deleted; esc clears the selection.",
					n, k, m.paneNoun())), true
			}
			return m.refuse(fmt.Sprintf("%d rows are marked, which is not a bulk selection — space "+
				"marks more, esc clears. Nothing was deleted.", n)), true
		}
	}
	// `m` MANAGES the selected worker's MCP + skills in the ONE MODAL — the same surface a conversation
	// gets from /scope and a project gets from the Work screen's `m`.
	if a, ok := m.actionByKey(kstr); ok {
		return m.openAction(a), true
	}
	if why := m.unavailableReason(kstr); why != "" {
		return m.refuse(why), true
	}
	return nil, false
}

// paneDeleteChord is the delete chord the FOCUSED pane binds — not a fixed pair.
func (m *Model) paneDeleteChord() string {
	if m.ActiveSourceName() == srcSchedules {
		return keySchedDelete
	}
	return keyDelete
}

// markableCount is how many marked rows a bulk WRITE could actually act on — the number the hint
// must show, because it is the number the confirm will name.
func (m *Model) markableCount() int { return len(m.markableIDs()) }

// paneNoun names what a row on the focused pane IS, for refusal text.
func (m *Model) paneNoun() string {
	switch m.ActiveSourceName() {
	case srcWorkers:
		return "worker"
	case srcWorkflows:
		return "workflow"
	case srcExecutions:
		return "execution"
	case srcRuns:
		return "workflow run"
	case srcSchedules:
		return "schedule"
	}
	return "row"
}

// mcpModalHost is the shell surface that owns the MCP + skills modal — the panel the GUI imports
// into the worker page.
type mcpModalHost interface {
	OpenWorkerMCPModal(workerID, name string) tea.Cmd
}

// mcpModalHost resolves the shell, when it offers the modal.
func (m *Model) mcpModalHost() (mcpModalHost, bool) {
	h, ok := m.Shell().(mcpModalHost)
	return h, ok
}

// actionByKey returns the action bound to a key for the focused row.
func (m *Model) actionByKey(key string) (kit2.Action, bool) {
	for _, a := range m.actionsForSelection() {
		if a.Key == key {
			return a, true
		}
	}
	return kit2.Action{}, false
}

// unavailableReason explains why a write chord has nothing to run on the focused row — but ONLY for
// the chords that source actually binds.
func (m *Model) unavailableReason(key string) string {
	switch m.ActiveSourceName() {
	case srcRuns:
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		state := strings.ToLower(it.Meta)
		switch key {
		case keyRetryRun:
			return "retry applies to a FAILED run — " + it.ID + " is " + state
		case keyForceProgress:
			return "force-progress applies to a WEDGED (running) run — " + it.ID + " is " + state
		}
		return ""
	case srcExecutions:
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		switch key {
		case keyCancel:
			return "execution " + it.ID + " is not live (" + strings.ToLower(it.Meta) + ") — there is nothing to cancel"
		case keyInterject:
			return "interjection needs a LIVE (running) execution — " + it.ID + " is " + strings.ToLower(it.Meta)
		}
		return ""

	case srcSchedules:
		// The ONE write chord on this pane is the delete, and its applicability is view-dependent: a
		// finished row may have no bound work item, so there is nothing to remove a schedule from.
		if key != keySchedDelete {
			return ""
		}
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		if m.sched.view() == schedFinished && m.sched.itemFor(it.ID) == "" {
			return "this run has no bound work item, so it has no schedule to remove"
		}
		return ""

	case srcWorkers:
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		status := workerStatusOf(it.Meta)
		switch key {
		case keyDeprecate:
			return "deprecate applies to a PUBLISHED worker — " + it.Title + " is " + status
		case keyDelete:
			return "this worker cannot be deleted — the pane offers no delete for it (retired workers are kept)"
		}
		return ""
	case srcWorkflows:
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		switch key {
		case keyDeprecateWf:
			return "deprecate applies to a PUBLISHED workflow — " + it.Title + " is " + workflowStatusOf(it.Meta)
		case keyDelete:
			return "this workflow cannot be deleted — the pane offers no delete for it"
		}
		return ""
	}
	return ""
}

// refuse records a local (pre-RPC) refusal in the status line.
func (m *Model) refuse(why string) tea.Cmd {
	m.notice = why
	return nil
}

// openAction opens the confirmation dialog for an action that needs one, or
// runs it immediately.
func (m *Model) openAction(a kit2.Action) tea.Cmd {
	if !a.NeedsConfirm() {
		return m.runAction(a)
	}
	d := kit2.Confirm(a.Label, a.Confirm, a.Label)
	d.Danger = a.Danger
	m.Open = d
	pending := a
	m.pending = &pending
	m.OnDialog = func(choice string) tea.Cmd {
		pa := m.pending
		m.pending = nil
		m.OnDialog = nil
		if pa == nil || choice == "" {
			m.notice = "cancelled"
			return nil
		}
		return m.runAction(*pa)
	}
	return nil
}

func (m *Model) runAction(a kit2.Action) tea.Cmd {
	m.notice = a.Label + " …"
	return m.Mutate(mutate.Request{
		Name:     a.Label,
		Source:   a.Source,
		Apply:    a.Apply,
		Rollback: a.Rollback,
		Do:       a.Do,
	})
}

// --- write RPCs ------------------------------------------------------------

func (m *Model) rpcCancelExecution(ctx context.Context, id string) error {
	_, err := m.cl.Executions.CancelExecution(ctx, connect.NewRequest(&apiv1.CancelExecutionRequest{
		Id:     id,
		Reason: cancelReason,
	}))
	return err
}

// defaultDeleteExecution is the plain wire form of the Executions pane's delete.
func (m *Model) defaultDeleteExecution(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Executions == nil {
		return errors.New("no execution client")
	}
	_, err := m.cl.Executions.DeleteExecution(ctx, connect.NewRequest(&apiv1.DeleteExecutionRequest{Id: id}))
	return err
}

func (m *Model) rpcRetryFailedRun(ctx context.Context, runID string) error {
	_, err := m.cl.Workflows.RetryFailedWorkflowRun(ctx, connect.NewRequest(&apiv1.RetryFailedWorkflowRunRequest{RunId: runID}))
	return err
}

// rpcAbortRun ends a LIVE run through AbortWorkflow — the same RPC the GUI's Stop button uses
// (useAbortWorkflow -> workflowClient.abortWorkflow).
func (m *Model) rpcAbortRun(ctx context.Context, runID string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client — cannot abort the run")
	}
	_, err := m.cl.Workflows.AbortWorkflow(ctx, connect.NewRequest(&apiv1.AbortWorkflowRequest{
		RunId:  runID,
		Reason: abortReason,
	}))
	return err
}

func (m *Model) rpcForceProgressRun(ctx context.Context, runID string) error {
	_, err := m.cl.Workflows.ForceProgressWorkflowRun(ctx, connect.NewRequest(&apiv1.ForceProgressWorkflowRunRequest{RunId: runID}))
	return err
}

// defaultDeleteRun is the plain wire form of the Runs pane's delete.
func (m *Model) defaultDeleteRun(ctx context.Context, runID string) error {
	if m.cl == nil || m.cl.Workflows == nil {
		return errors.New("no workflow client")
	}
	_, err := m.cl.Workflows.DeleteWorkflowRun(ctx, connect.NewRequest(&apiv1.DeleteWorkflowRunRequest{RunId: runID}))
	return err
}

func (m *Model) rpcSendMessage(ctx context.Context, execID, message string) error {
	_, err := m.cl.Executions.SendExecutionMessage(ctx, connect.NewRequest(&apiv1.SendExecutionMessageRequest{
		ExecutionId: execID,
		Message:     message,
	}))
	return err
}

// --- helpers ---------------------------------------------------------------

// isLiveExecution reports whether an execution list row is live (cancellable / interjectable).
//
// IT TAKES THE ROW'S `Meta`, WHICH IS "<status> · <worker>" — not a bare status. That is the bug this
// comment exists for: the switch below compared the WHOLE string against a list of bare statuses, so
// "running · Quick Software Engineer" matched nothing and the function returned false for every live
// execution. Six call sites passed a row meta and ALL SIX were wrong in the same way:
//
//   - `c: cancel` was never offered on a running execution (the action was gated on this);
//   - the transcript's composer always said "Ask a follow-up" instead of "Message the worker mid-run"
//     and took the follow-up path for a run that was still executing.
//
// The status is the part BEFORE the separator (executionListMeta builds `meta` as the lowered status
// plus " · " plus the worker name), so the status is read the same way workerStatusOf reads it — up to
// the first space. A caller that hands a BARE status still works, which is deliberate: the two
// conventions must not have to agree.
func isLiveExecution(meta string) bool {
	switch statusOfMeta(meta) {
	case "running", "healthy", "stalled", "unhealthy", "dispatching", "queued", "starting", "terminating":
		return true
	}
	return false
}

// statusOfMeta reads the status out of a row's meta string, which is "<status> · <worker>" when a worker
// name is known and a bare status when it is not.
func statusOfMeta(meta string) string {
	m := strings.ToLower(strings.TrimSpace(meta))
	if i := strings.IndexByte(m, ' '); i > 0 {
		return m[:i]
	}
	return m
}

// errNeedForm is returned if an action's direct path is used without its form
// (defensive: handleActionKey always opens the form first).
func errNeedForm(what string) error {
	return fmt.Errorf("%s needs a form — this action is not runnable directly", what)
}

// HintLine is the screen's key cheat-sheet.
func (m *Model) HintLine() string {
	switch m.ActiveSourceName() {
	case srcExecutions:
		// THE DELETE CHORD IS NAMED, and the mark state is stated when there is one — the same reason the
		// Workers pane names it.
		if n := m.markableCount(); n > 0 {
			return theme.HintText.Render(keyDelete + ": delete " + fmt.Sprint(n) + " selected " +
				theme.DetailKey.Render("·") + " esc: clear " + theme.DetailKey.Render("·") +
				" space: mark · ←/→: pane · r: refresh")
		}
		return theme.HintText.Render("c: cancel (confirm) " + theme.DetailKey.Render("·") +
			" " + keyDelete + ": delete (confirm) " + theme.DetailKey.Render("·") +
			" i: interject · space: mark for bulk · enter: live session · ←/→: pane · r: refresh")
	case srcRuns:
		// The delete chord is named, and the mark state is stated when there is one.
		if n := m.markableCount(); n > 0 {
			return theme.HintText.Render(keyDelete + ": delete " + fmt.Sprint(n) + " selected " +
				theme.DetailKey.Render("·") + " esc: clear " + theme.DetailKey.Render("·") +
				" space: mark · ←/→: pane · r: refresh")
		}
		// THE ABORT CHORD IS ADVERTISED ON A LIVE RUN, AND ONLY THERE.
		if m.runIsLive() {
			return theme.HintText.Render(keyAbortRun + ": abort run (confirm) " + theme.DetailKey.Render("·") +
				" t: retry failed run " + theme.DetailKey.Render("·") +
				" p: force-progress (confirm) " + theme.DetailKey.Render("·") +
				" " + keyDelete + ": delete (confirm) · space: mark for bulk · enter: step runs + diagnosis · r: refresh")
		}
		return theme.HintText.Render("t: retry failed run (confirm) " + theme.DetailKey.Render("·") +
			" p: force-progress wedged run (confirm) " + theme.DetailKey.Render("·") +
			" " + keyDelete + ": delete (confirm) · space: mark for bulk · enter: step runs + diagnosis · r: refresh")
	case srcSchedules:
		return m.scheduleHint()

	case srcWorkers:
		// THE DELETE CHORD AND THE BULK SET-MODEL CHORD ARE NAMED HERE, and the mark state is stated,
		// because the composer is the row the operator reads.
		if n := m.markableCount(); n > 0 {
			return theme.HintText.Render(m.markHint(n) +
				keyDelete + ": delete " + fmt.Sprint(n) + " " + theme.DetailKey.Render("·") +
				" " + keyBulkSetModel + ": set model " + theme.DetailKey.Render("·") +
				" esc: clear " + theme.DetailKey.Render("·") + " ↑↓: move · r: refresh")
		}
		return theme.HintText.Render(
			"n: new " + theme.DetailKey.Render("·") + " e: edit " + theme.DetailKey.Render("·") +
				" m: MCP + skills " + theme.DetailKey.Render("·") +
				" V: edit version (prompt/config) " + theme.DetailKey.Render("·") +
				" p: publish " + theme.DetailKey.Render("·") + " a: set active version " + theme.DetailKey.Render("·") +
				" u: deprecate " + theme.DetailKey.Render("·") + " C: categorize " + theme.DetailKey.Render("·") +
				" " + keyDelete + ": delete " + theme.DetailKey.Render("·") +
				" space: mark for bulk " + theme.DetailKey.Render("·") + " enter: versions · r: refresh")
	case srcWorkflows:
		if m.flowEditing {
			// Inside the mode the flow IS the editing surface, so the cheat-sheet is the
			// mode's own keys — the pane's list actions are not what the operator is doing.
			return theme.HintText.Render("↑↓: step " + theme.DetailKey.Render("·") +
				" enter: edit step " + theme.DetailKey.Render("·") +
				" a: add step " + theme.DetailKey.Render("·") +
				" x: remove step " + theme.DetailKey.Render("·") +
				" E: rename workflow " + theme.DetailKey.Render("·") +
				" esc: done " + theme.DetailKey.Render("·") + " r: refresh")
		}
		// The Workflows pane used to advertise NO delete at all — the chord existed (`shift+x`) and was
		// simply not written down, which is how it stayed undiscoverable.
		if n := m.markableCount(); n > 0 {
			return theme.HintText.Render(m.markHint(n) +
				keyDelete + ": delete " + fmt.Sprint(n) + " " + theme.DetailKey.Render("·") +
				" esc: clear " + theme.DetailKey.Render("·") + " ↑↓: move · r: refresh")
		}
		return theme.HintText.Render("e: edit workflow (steps) " + theme.DetailKey.Render("·") +
			" V: new version " + theme.DetailKey.Render("·") +
			" n: new workflow " + theme.DetailKey.Render("·") +
			" E: rename " + theme.DetailKey.Render("·") +
			" p: publish " + theme.DetailKey.Render("·") + " u: deprecate " + theme.DetailKey.Render("·") +
			" C: categorize " + theme.DetailKey.Render("·") +
			" " + keyDelete + ": delete " + theme.DetailKey.Render("·") +
			" space: mark for bulk " + theme.DetailKey.Render("·") +
			" enter: flow view " + theme.DetailKey.Render("·") + " r: refresh")
	}
	return theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · r: refresh")
}

// markHint states the selection, LEFT of the chords that act on it: the operator needs the count
// before they need the keys.
func (m *Model) markHint(n int) string {
	return fmt.Sprintf("%d marked", n) + " " + theme.DetailKey.Render("·") + " "
}

// runIsLive reports whether the RUNS pane's focused row is a live run — the same meta the row draws
// and the same test the action list uses.
func (m *Model) runIsLive() bool {
	it, ok := m.ActiveItem()
	if !ok {
		return false
	}
	return strings.ToLower(it.Meta) == "running"
}

// AbortRunHint is the composer's probe for the ABORT chord: the run-level gesture, named only while
// the Runs pane holds a live run. IT ANSWERS FOR THE RUNS PANE ONLY.
func (m *Model) AbortRunHint() string {
	if m.ActiveSourceName() != srcRuns {
		return ""
	}
	if m.runIsLive() {
		return keyAbortRun + " abort run"
	}
	return ""
}
