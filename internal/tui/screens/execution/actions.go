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
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Source names (also the slash-command slugs the shell generates).
const (
	srcExecutions = "executions"
	srcRuns       = "runs"
)

// cancelReason is the audit reason recorded on a TUI-initiated cancel.
const cancelReason = "cancelled from the orch TUI"

// Write chords (matched by name in handleActionKey).
const (
	keyCancel        = "c"
	keyInterject     = "i"
	keyRetryRun      = "t"
	keyForceProgress = "p"
)

// ClaimsKeys reports whether the screen owns every key right now (an open
// interjection form or the confirm dialog). The shell consults it before its
// own routes so a typed character is never stolen.
func (m *Model) ClaimsKeys() bool { return m.form != nil || m.Open != nil }

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
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case srcExecutions:
		id := item.ID
		if !isLiveExecution(item.Meta) {
			return nil
		}
		return []kit2.Action{
			{
				Label: "cancel", Key: keyCancel, Danger: true, Source: srcExecutions,
				Confirm: "Cancel execution " + id + "?\n" +
					"The execution transitions to TERMINATED and its worker session is stopped; " +
					"in-flight tool calls are abandoned (the reason is recorded as " + strconv.Quote(cancelReason) + ").",
				Do: func(ctx context.Context) error { return m.rpcCancelExecution(ctx, id) },
			},
			{
				Label: "interject", Key: keyInterject, Source: srcExecutions,
				// The message is collected by a form — handleActionKey opens it.
				Do: func(context.Context) error { return errNeedForm("interject") },
			},
		}

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
	}
	return nil
}

// handleActionKey dispatches a write chord for the focused source. handled
// is false when the key belongs to the shared navigation layer.
func (m *Model) handleActionKey(kstr string) (tea.Cmd, bool) {
	if kstr == keyInterject {
		if m.ActiveSourceName() != srcExecutions {
			return m.refuse("interjection applies to a running execution — focus the Executions pane"), true
		}
		it, ok := m.ActiveItem()
		if !ok || !isLiveExecution(it.Meta) {
			return m.refuse("interjection needs a LIVE (running) execution — this one is not running"), true
		}
		return m.beginInterject(it.ID), true
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

// unavailableReason explains why a write chord has nothing to run on the
// focused row (never a silent no-op).
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
	case srcExecutions:
		it, ok := m.ActiveItem()
		if !ok {
			return ""
		}
		return "execution " + it.ID + " is not live (" + strings.ToLower(it.Meta) + ") — there is nothing to cancel or interject into"
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

// beginInterject opens the interjection form: a message injected into the
// LIVE worker session (no new execution / work item / workflow state).
func (m *Model) beginInterject(execID string) tea.Cmd {
	f := kit2.NewForm("Interject into "+execID,
		kit2.FieldSpec{Name: "message", Label: "Message", Kind: kit2.KTextArea, Required: true,
			Placeholder: "steer the running worker — the reply streams back into the session"})
	f.Focused = true
	f.Width = 66
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		msg := strings.TrimSpace(v["message"])
		id := execID
		return m.Mutate(mutate.Request{
			Name:   "interject into " + id,
			Source: srcExecutions,
			Do: func(ctx context.Context) error {
				return m.rpcSendMessage(ctx, id, msg)
			},
		}), nil
	}
	m.form = f
	m.notice = ""
	return nil
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
		return theme.HintText.Render("c: cancel (confirm) · i: interject · enter: live session · ←/→: pane · f: more pages · r: refresh")
	case srcRuns:
		return theme.HintText.Render("t: retry failed run (confirm) · p: force-progress wedged run (confirm) · enter: step runs + diagnosis · r: refresh")
	}
	return theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh")
}
