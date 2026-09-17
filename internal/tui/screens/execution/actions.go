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
	// srcSchedules is the Schedules pane: upcoming / running / finished, cycled with `v`. The
	// operator: "I noticed I don't see any 'Schedules' section in the TUI under Executions. We
	// need to implement this in the TUI."
	srcSchedules = "schedules"
)

// cancelReason is the audit reason recorded on a TUI-initiated cancel.
const cancelReason = "cancelled from the orch TUI"

// Write chords (matched by name in handleActionKey).
//
// The worker CRUD chords deliberately AVOID d/D: the shell's global routes run
// BEFORE the screen sees a key, and d/D is the diff-pane toggle there — so a
// "deprecate" on D could never fire while the content pane has focus. `u` (for
// unpublish) carries it instead, and the pair is documented in HintLine.
const (
	keyCancel        = "c"
	keyInterject     = "i"
	keyRetryRun      = "t"
	keyForceProgress = "p"
	keySetModel      = "m"
	// keyBulkSetModel sets the model for EVERY marked worker at once — the TUI's counterpart to the
	// GUI's BulkChangeWorkerModelDialog, and the one thing a per-worker form cannot do (you cannot edit
	// ten forms at once).
	//
	// SHIFT+M, because lowercase `m` is the retired single-worker chord — retired on purpose, since the
	// model is a FIELD on the Edit and Version forms now, and re-binding it here would quietly bring back
	// the second way of doing the same thing that was removed.
	keyBulkSetModel = "M"
	// Worker CRUD (Item 6). Each opens a form or a confirm; none writes from
	// Update directly.
	keyNewWorker   = "n"
	keyEditWorker  = "e"
	keyEditVersion = "V"
	keyPublish     = "p"
	keySetActive   = "a"
	keyDeprecate   = "u"
	// keyDelete is THE delete chord, for BOTH panes and for both single and bulk deletes. It is one
	// constant with one value because the operator asked for exactly that: *"let's make those consistent
	// across the board please with 'ctrl+x' for single and bulk on both"*, after finding Workers on `x`,
	// Workflows on `shift+x`, and the bulk variants on whatever their pane happened to use.
	//
	// TWO SEPARATE CONSTANTS SET TO THE SAME LITERAL WOULD DRIFT AGAIN — that is how this happened the
	// first time — so the Workflows pane reads this one too. `ctrl+x` is also free of the STEP EDITOR's
	// `x` (remove step), which the old `X`/`x` pair had to be arranged around; that collision is simply
	// gone now.
	keyDelete = "ctrl+x"
	// keyCategorize ASSIGNS the selected item to a grouping (worker / workflow / conversation
	// categories). A CAPITAL, following this screen's own convention that a capital is the OTHER act on
	// the same pane (`E` renames a workflow header, `V` edits a version, `M` sets the model on a
	// selection) — and because every lowercase letter with a mnemonic is taken here: `c` is cancel, `g`
	// goes to the run, `t` retries.
	//
	// It opens the SHELL's assign-or-create modal (see tui/categories.go): the category list is the
	// shell's cache and the modal is shell-hosted, so the screen hands the intent over rather than
	// owning a second implementation of it.
	keyCategorize = "C"
	// Workflow lifecycle. The SAME chords reuse handles the same verbs on a
	// different pane (n/e/p/u/x), and both sets are scoped to their own source —
	// `p` is ALSO force-progress on a run, which is only safe because the
	// workflow and worker form-openers dispatch solely while their pane has focus.
	//
	// Workflow lifecycle. `e` is the WORKFLOW edit MODE — the operator: "when someone
	// hits 'e' to edit a workflow, they are going to think they are editing the entire
	// workflow and all its steps at once, not in pieces." Inside that mode `enter`
	// edits the selected step, `a` adds and `x` removes, so the step chords are no
	// longer separate top-level commands sitting outside a normal edit view. `E`
	// renames the workflow header (a different act from editing its steps); delete is
	// the shared `ctrl+x` (see keyDelete). Both sets are scoped to the Workflows
	// pane, which is also what keeps `p` usable as force-progress on a run.
	keyNewWorkflow  = "n"
	keyEditWorkflow = "E"
	keyPublishWf    = "p"
	keyDeprecateWf  = "u"
	// Delete is keyDelete — ONE chord for both panes (see its declaration).
	// WORKFLOW EDIT MODE. `e` opens it from the Workflows pane; inside it the step
	// cursor and the step chords are live. `a` is the operator's suggested key for
	// adding ("Not sure how we can handle adding a step in this mode, maybe we can
	// keep it 'a'"); `-` stays bound because it was the previous chord.
	keyFlowEdit       = "e"
	keyFlowEditStep   = "enter"
	keyFlowAddStep    = "a"
	keyFlowAddStepAlt = "-"
	keyFlowRemoveStep = "x"
	keyFlowExit       = "esc"
	// Retained so the flow cursor keys read the same as before.
	keyStepUp   = "up"
	keyStepDown = "down"
	// SCHEDULES chords. `v` cycles the view, exactly as it does on the Work Items pane
	// (tree/archive), so one pane has one way to change what it is showing. `g` is the
	// operator's "key that takes you to the workflow run", and `x` is the delete — whose
	// MEANING is per-view (cancel an upcoming/running schedule, or remove the schedule from a
	// finished row's item), matching the GUI's two different buttons.
	// keySchedView / keyGoToRun / keySchedDelete are the Schedules pane's chords: the lens cycle,
	// the jump to the run, and the delete. (The Execution detail's message box needs NO chord — it
	// is a POSITION in the transcript, reached by walking down past the last block:
	// execution_blocks.go.)
	keySchedView   = "v"
	keyGoToRun     = "g"
	keySchedDelete = "x"
)

// The RETIRED chords. `f` was the follow-up box AND the base's pager; `i` was the interject box.
// Both are now positions in the transcript rather than keys (execution_blocks.go), and these
// constants exist only so the screen can EXPLAIN where the box went instead of going silent on an
// operator who learned them. See handleActionKey's stub.
const (
	keyFollowUpLegacy  = "f"
	keyInterjectLegacy = "i"
)

// DropKeyClaim releases the screen's key claim so the focus chord can return the
// operator to the composer (see kit2.Base.DropKeyClaim) — without it an open
// form would swallow ctrl+g and the next letters would run actions instead of
// being typed.
func (m *Model) DropKeyClaim() { m.Base.DropKeyClaim() }

// ClaimsKeys reports whether the screen owns every key right now (an open
// interjection form, the confirm dialog, or the inline detail editor). The shell
// consults it before its own routes so a typed character is never stolen.
func (m *Model) ClaimsKeys() bool {
	return m.form != nil || m.Open != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// FormOpen reports whether a FORM is open — modal OR inline details-pane editor,
// plus the model picker (a modal layered over a form). While one is up, Tab moves
// through the form's FIELDS rather than the tab ring.
func (m *Model) FormOpen() bool {
	return m.form != nil || m.modelPicker != nil || m.Base.EditingDetail()
}

// OwnsTab reports that Tab is a move INSIDE this pane, not a move of the shell's
// tab ring: on the Executions detail, Tab toggles between the TRANSCRIPT and the
// MESSAGE BOX.
//
// The operator: "Once we have focus on an execution detail pane, we should allow
// tab to tab between the Execution details and the chat prompt. Currently once you
// go into a chat in an execution you are locked in it and can't get out."
//
// The lock-in was real and had two causes. The chat CLAIMED Tab and then did
// nothing with it (this screen's composer treated "tab" as "nothing below me"), so
// the key vanished; and when the shell won the race instead, it moved the TOP MENU
// while the caret stayed in the chat — which reads as being stuck, because typing
// still went into the chat. Esc and up always worked, but nothing advertised them
// and Tab is the gesture the whole rest of the app uses to move between regions.
//
// Scoped to the DETAIL with an execution selected, so the list keeps the shell's
// Tab (walking the ring) and this explains a toggle that is only real in the pane.
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
	// The SHARED bulk rule comes first, for the source that offers bulk actions: more than one
	// marked row means the operator is operating on a SELECTION, not on the row the cursor is
	// on, so the single-row actions are replaced rather than mixed with it.
	if m.ActiveSourceName() == srcSchedules {
		if ids := m.Base.BulkIDs(); len(ids) > 0 {
			return m.scheduleDeleteActions(ids)
		}
	}
	// WORKERS and WORKFLOWS take the same rule: more than one marked row means the operator is operating on
	// a SELECTION, so the single-row actions are replaced rather than mixed with it. Without this branch,
	// marking rows on these panes highlighted them and did nothing else.
	if m.ActiveSourceName() == srcWorkers {
		if ids := m.markableIDs(); len(ids) >= kit2.BulkThreshold {
			acts := m.entityBulkActions(srcWorkers, keyDelete, "worker", ids, m.rpcDeleteWorker)
			// The worker-only bulk act. It opens a MODAL rather than writing, so its Do refuses the way
			// every other form-opening action on this screen does (see errNeedForm) — the key path
			// intercepts `M` first and opens the picker, and a CLICK on the bar label gets the refusal
			// instead of a silent nothing.
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
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case srcExecutions:
		id := item.ID
		// CANCEL is offered only for a LIVE execution — cancelling something already finished is
		// not a thing. But the bar is not empty for a finished one, and the MESSAGE BOX is why:
		// the composer works on a finished execution too (it asks a follow-up), which is the
		// normal thing to open and exactly what the operator reported not finding.
		//
		// The box is a POSITION in the transcript rather than a chord (execution_blocks.go): walk
		// down past the last block and you are in it. So the bar advertises it as an instruction
		// rather than as a key — a key here would be a second way to reach a box the operator
		// already reaches by pressing down, which is how the old `f`/`i` pair became unintuitive.
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
			// The box, advertised as the gesture that reaches it.
			//
			// No Key either: the box is a POSITION in the transcript (down past the last block), so
			// a key would be a second route to something the operator already reaches by pressing
			// down — which is how the old `f`/`i` pair became unintuitive. The label IS the gesture.
			kit2.Action{
				Label: "↓ past the last block: message box", Source: srcExecutions,
				Do: func(context.Context) error {
					return errNeedForm("message box — walk down past the last block")
				},
			},
			// The COLLAPSE gesture, on the same principle: the operator needs to know it exists before
			// trying it on a 200-line tool dump.
			//
			// It carries NO Key: `enter` is handled by the transcript cursor itself
			// (execution_blocks.go), and claiming it here as an ACTION would put it in the
			// action-by-key table, where it would fire on ANY focused execution row — including
			// while the cursor is on a prose block, which is exactly the "enter did nothing"
			// experience this item exists to remove. The bar is a LABEL; the key belongs to the
			// cursor.
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
		return acts

	case srcSchedules:
		// The single-row delete. The BULK case is handled before the switch, because a
		// selection replaces the cursor-row actions rather than joining them.
		return m.scheduleDeleteActions([]string{item.ID})

	case srcWorkers:
		// Item 6: the full CRUD surface. Form-opening actions carry a Do that
		// refuses by name (handleActionKey opens the form first, so it is never
		// reached); only the direct writes appear in the footer as runnable.
		//
		// There is deliberately NO "set model" action. The operator: "the edit page
		// of a worker should also have the model selector. No need to have a
		// separate 'm' option to set models that way." The model is a field on the
		// edit form (and on the version editor and the create form), so a separate
		// chord was a second, competing path to the same field.
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
			// publish names the version it will ship (handleActionKey loads the
			// trail first and refuses when there is no draft), so the operator is
			// never guessing what a publish does.
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
		// The workflow lifecycle. Same shape as the Workers pane: form-opening
		// actions carry a Do that refuses by name (handleActionKey opens the form
		// first); the direct writes are confirm-gated.
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
//
// The editor is pointed at the version the pane SHOWS (published, else newest), so
// what you edit is what you see. When that version is not a draft, saving creates
// one — published versions are immutable, so step editing implies a draft (the
// server rejects anything else).
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

// handleFlowKeys drives the WORKFLOW EDIT MODE.
//
// The operator's model: "I want to hit 'e' and edit all steps and add steps, delete steps,
// etc. IN THIS SCREEN. I want the FULL visual editing to occur ... I see you can move up
// and down with the arrow keys to select a step. I think that was at least in the right
// direction. Though I think 'e' should be edit workflow mode ... then you can move the
// arrow keys down, then hit 'enter' on the specific step to edit a step."
//
// So the mode is EXPLICIT. Outside it the flow view is a READ-ONLY view and this handler
// claims only the key that opens the mode — the previous shape claimed the step chords
// whenever a flow was on screen, which is exactly what made "edit step", "add step" and
// "remove step" read as top-level commands living outside any edit view. handled is false
// when the key belongs to the navigation layer.
//
// It runs BEFORE handleActionKey because the mode owns `e`, `x` and now `enter`, all of
// which mean something else to the pane and the shell.
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
		// ENTER edits the step the cursor is on — the operator's explicit ask. It must be
		// claimed here, ahead of kit2's own `enter: detail`, or the mode would reload the
		// pane's detail instead of opening the step.
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
		m.endFlowEdit()
		return nil, true
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

// endFlowEdit leaves the mode and reports it.
func (m *Model) endFlowEdit() {
	if !m.flowEditing {
		return
	}
	m.flowEditing = false
	m.notice = "finished editing " + m.stepWorkflowName
	m.paintFlow()
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
	// THE RUNS STEP FLOW. The vertical keys walk the run's STEPS and `enter` jumps to the
	// highlighted step's execution — the operator's "if you hit enter on a particular step
	// (whether it's complete or still running), it should take you to the execution".
	//
	// ENTER IS A TWO-STAGE GESTURE, which is the operator's stated model and now the shipped one:
	// "You should have to hit enter FIRST on the workflow run, THEN it moves to the detail pane and
	// then from there enter should select steps."
	//
	//   list focused  + enter  →  move the keyboard into the DETAIL pane (the base does this)
	//   detail focused + enter →  jump to the cursor step's execution
	//
	// A previous revision claimed enter unconditionally while a flow was drawn, on the reasoning
	// that every row advertises "enter → execution". That reasoning was wrong about which enter the
	// row was advertising: the rows describe what enter does ONCE THE DETAIL HAS THE FOCUS, and the
	// first enter is what GIVES it the focus. Claiming the key from the list also broke the list,
	// because enter on a run row is how you open the run at all.
	//
	// So the vertical keys and enter share one rule — the detail must hold the focus — because on
	// this pane they are all "act on the step cursor" gestures, and the step cursor is the detail's.
	// When there is NO flow to act on, enter falls through to the base even with the detail focused,
	// so the generic "activate the selected row" gesture is untouched. The repaint is local — the
	// rows are cached from the detail fetch — so moving the cursor costs no round trip.
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
	// The SCHEDULES pane's own chords (view cycle, jump to the run) run first, scoped to its
	// pane: `v` and `g` mean nothing elsewhere on this screen, and scoping keeps a future
	// binding elsewhere from silently stealing them here.
	if m.ActiveSourceName() == srcSchedules {
		if cmd, handled := m.handleSchedulesKeys(kstr); handled {
			return cmd, true
		}
	}
	// CATEGORY ROWS FIRST, for the two panes that group their rows. `e` and `x` are the panes' OWN item
	// keys, and on a folder they must manage the GROUPING (rename / delete) rather than open a form
	// against a synthetic id. It runs ahead of the flow editor too, so `x` on a folder cannot remove a
	// workflow step.
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
		case keyFlowEdit:
			// `e` on the Workflows pane is the WORKFLOW EDIT MODE, not a step chord:
			// "when someone hits 'e' to edit a workflow, they are going to think they are
			// editing the entire workflow and all its steps at once, not in pieces". The
			// per-step chords (enter/a/x) live inside the mode.
			//
			// This is only reached when the mode is OFF or when no flow is loaded — with the
			// mode on, handleFlowKeys handles `e`... which it does not, so the mode's own
			// keys are the ones listed in beginFlowEdit. Guarded here so pressing `e` inside
			// the mode does not re-enter (a no-op) and does not fall through to a header form.
			if m.flowEditing {
				return nil, true
			}
			if m.stepWorkflowID == "" || m.Base.DetailID() == "" {
				return m.refuse("select a workflow first"), true
			}
			return m.beginFlowEdit(), true
		}
	}
	// Worker CRUD chords open FORMS (or start a load that opens one), so they are
	// dispatched before the generic action lookup — but ONLY while the Workers
	// pane is focused. That scoping matters: `p` is also force-progress on a run,
	// and an unscoped switch would hijack it and refuse with a worker message.
	if m.ActiveSourceName() == srcWorkers {
		if kstr == keyCategorize {
			return m.categorizeSelected(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER), true
		}
		switch kstr {
		case keyNewWorker:
			// Item 3: worker editing happens IN THE DETAILS PANE, like work items —
			// not in a modal. A modal covers the list and the detail it is editing;
			// the pane keeps both visible and is the established pattern on Work.
			//
			// The form needs the tenant's ROLES (the plane-role picker is one of its
			// fields), so creating is a LOAD-then-open like every other worker form
			// rather than a synchronous open: the empty worker id is the create
			// signal. Nothing is written until the operator saves.
			return m.beginWorkerOp("", opCreateWorker), true
		case keyEditWorker, keyEditVersion, keyPublish, keySetActive:
			it, ok := m.ActiveItem()
			if !ok {
				return m.refuse("select a worker first"), true
			}
			// A CATEGORY ROW IS NOT A WORKER. These panes nest their rows under their grouping, and the
			// parent row carries a synthetic id — so an op aimed at it would hit the server with an id it
			// has never heard of and fail with something unrelated to what the operator did.
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
		// BULK SET MODEL. The mechanism is the same ONE the mark/delete path uses — a selection above the
		// shared threshold replaces the single-row actions — and this is the act with no single-row
		// equivalent to replace: a per-worker model edit is a FORM FIELD (e / V), which is why lowercase
		// `m` was retired. Chaining ten forms is not a feature, so the bulk case is its own modal.
		//
		// SCOPED TO THE WORKERS PANE, exactly like every other worker chord here (n/e/V/p/a/u). The
		// Workflows pane has its own model story (a ref per version, edited through V), and the
		// executions and runs panes have none at all — so the key falls through there rather than
		// opening a WORKER picker over a workflow.
		ids := m.markableIDs()
		if len(ids) < kit2.BulkThreshold {
			// Refuse with the two REAL routes rather than a bare "no": an operator pressing M with
			// nothing marked needs to know both that marking is the bulk gesture and that one worker's
			// model lives on its form.
			if len(ids) == 1 {
				return m.refuse("one worker is not a bulk selection — press e to edit it, or mark two or more with space"), true
			}
			return m.refuse("mark two or more workers first (space marks), then " + keyBulkSetModel + " sets their model together"), true
		}
		return m.beginBulkSetModel(ids), true
	}
	if kstr == keySetModel {
		// The chord is gone (the model is a form field now), but a conversation —
		// or a muscle memory — may still send it. Explain rather than no-op.
		return m.refuse("the model is a field on the Edit form (e) and the version editor (V) — open one and choose it there, or press " + keyBulkSetModel + " to set it for several marked workers"), true
	}
	if a, ok := m.actionByKey(kstr); ok {
		return m.openAction(a), true
	}
	if why := m.unavailableReason(kstr); why != "" {
		return m.refuse(why), true
	}
	return nil, false
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

// unavailableReason explains why a write chord has nothing to run on the focused
// row — but ONLY for the chords that source actually binds.
//
// It used to answer for ANY key, and that made the pane UNUSABLE: handleActionKey
// treats a non-empty reason as "handled", so on the Executions and Workers panes
// every key was consumed and explained — including the arrow keys. That is the
// operator's "Executions and Workers will not allow you to use up/down when you
// select their submenu. They are locked."
//
// A source with no chord bound to the pressed key must return "", so the key
// falls through to the screen (which is what moves the list).
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
		// The ONE write chord on this pane is the delete, and its applicability is
		// view-dependent: a finished row may have no bound work item, so there is nothing to
		// remove a schedule from.
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

func (m *Model) rpcRetryFailedRun(ctx context.Context, runID string) error {
	_, err := m.cl.Workflows.RetryFailedWorkflowRun(ctx, connect.NewRequest(&apiv1.RetryFailedWorkflowRunRequest{RunId: runID}))
	return err
}

func (m *Model) rpcForceProgressRun(ctx context.Context, runID string) error {
	_, err := m.cl.Workflows.ForceProgressWorkflowRun(ctx, connect.NewRequest(&apiv1.ForceProgressWorkflowRunRequest{RunId: runID}))
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

// isLiveExecution reports whether a status string is a live (cancellable /
// interjectable) execution.
func isLiveExecution(status string) bool {
	switch strings.ToLower(status) {
	case "running", "healthy", "stalled", "unhealthy", "dispatching", "queued", "starting", "terminating":
		return true
	}
	return false
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
		return theme.HintText.Render("c: cancel (confirm) · i: interject · enter: live session · ←/→: pane · r: refresh")
	case srcRuns:
		return theme.HintText.Render("t: retry failed run (confirm) · p: force-progress wedged run (confirm) · enter: step runs + diagnosis · r: refresh")
	case srcSchedules:
		return m.scheduleHint()

	case srcWorkers:
		// THE DELETE CHORD AND THE BULK SET-MODEL CHORD ARE NAMED HERE, and the mark state is stated,
		// because the composer is the row the operator reads. It used to advertise `x: delete` — a chord
		// that no longer exists anywhere — and said nothing about what marking rows does, so a bulk
		// delete was invisible until it was already selected.
		if n := m.Base.MarkCount(); n > 0 {
			return theme.HintText.Render(m.markHint(n) +
				keyDelete + ": delete " + fmt.Sprint(n) + " " + theme.DetailKey.Render("·") +
				" " + keyBulkSetModel + ": set model " + theme.DetailKey.Render("·") +
				" esc: clear " + theme.DetailKey.Render("·") + " ↑↓: move · r: refresh")
		}
		return theme.HintText.Render(
			"n: new " + theme.DetailKey.Render("·") + " e: edit " + theme.DetailKey.Render("·") +
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
		if n := m.Base.MarkCount(); n > 0 {
			return theme.HintText.Render(m.markHint(n) +
				keyDelete + ": delete " + fmt.Sprint(n) + " " + theme.DetailKey.Render("·") +
				" esc: clear " + theme.DetailKey.Render("·") + " ↑↓: move · r: refresh")
		}
		return theme.HintText.Render("e: edit workflow (steps) " + theme.DetailKey.Render("·") +
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

// markHint states the selection, LEFT of the chords that act on it: the operator needs the count before
// they need the keys, and the count is what tells them the delete is about to hit more than the row under
// the cursor.
func (m *Model) markHint(n int) string {
	return fmt.Sprintf("%d marked", n) + " " + theme.DetailKey.Render("·") + " "
}
