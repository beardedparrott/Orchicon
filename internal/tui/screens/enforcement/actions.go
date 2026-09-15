// actions.go — the enforcement screen's WRITE paths. Every mutation the
// GUI exposes lands here: ApproveStep (approve / reject a pending step
// approval), CreatePolicy / UpdatePolicyVersion / PublishPolicy, and the
// recovery action surface (ApproveContinuationPlan / RejectContinuationPlan
// / CancelRecovery / MarkTaskSucceeded).
//
// Discipline the acceptance criteria pin:
//   - a write is issued ONCE per confirm (inFlight disables the action
//     until the RPC returns — a double-press can never double-write);
//   - the plane's refusal is surfaced VERBATIM in the dock, never a
//     silent no-op;
//   - the mutated source reconciles (re-fetched) after the write lands.
package enforcement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
)

// rpcTimeout bounds each write RPC (the plane answers in-band).
const rpcTimeout = 20 * time.Second

// NOTE: the POLICY EDITOR (create/edit/publish/version-inspect) was removed from this screen
// when policies became a "coming soon" placeholder. The operator: "the form is weird and I
// have never tested it ... just say 'Coming soon...' for now."
//
// It is deleted rather than parked because a form nobody can reach is the same class of thing
// as a knob nothing honours: it invites someone to re-wire an unsettled design instead of
// designing it. The plane-side capability is untouched — ListPolicies / CreatePolicy /
// UpdatePolicyVersion / PublishPolicy / ListPolicyVersions all remain in the generated client,
// and the Rego expression that parses into these types is in git history at the commit that
// removed the editor.
//
// What remains here is the APPROVALS and RECOVERIES write surface, both of which the operator
// uses.

// --- action keys -----------------------------------------------------------

// handleActionKey dispatches the screen's write chords for the focused
// source. It returns handled=false for every other key so Base keeps
// owning navigation. While a write is in flight the chords are inert
// (idempotent-safe on double-press).
func (m *Model) handleActionKey(k tea.KeyMsg) (bool, tea.Cmd) {
	if m.ov != nil {
		return false, nil // the overlay owns every key while it is open
	}
	switch m.Base.ActiveSourceName() {
	case "approvals":
		switch k.String() {
		case "a":
			return true, m.beginApprove(true)
		case "x":
			return true, m.beginApprove(false)
		}
	case "policies":
		// NO chords. The pane is a "coming soon" placeholder: it lists nothing, so every
		// write chord would have nothing to act on. An unbound key falls through to the
		// shared navigation layer, which is the honest behaviour for an inert pane.
		return false, nil
	case "recoveries":
		switch k.String() {
		case "a":
			return true, m.beginPlanDecision(true)
		case "x":
			return true, m.beginPlanDecision(false)
		case "c":
			return true, m.beginRecoveryCancel()
		case "m":
			return true, m.beginMarkSucceeded()
		}
	}
	return false, nil
}

// refuse records a local (pre-RPC) refusal and surfaces it in the dock —
// an action that cannot run is never a silent no-op.
func (m *Model) refuse(why string) tea.Cmd {
	m.lastErr = why
	m.lastOK = ""
	m.Fail(why)
	return nil
}

// --- approvals -------------------------------------------------------------

func (m *Model) beginApprove(approved bool) tea.Cmd {
	id := m.activeID()
	if id == "" {
		return m.refuse("no approval selected")
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = id
	if approved {
		m.ov = newForm("Approve step "+id,
			"the plane writes this to .orchicon/<run_id>/summary for downstream workers",
			"approval-approve",
			&ovField{Key: "reason", Label: "reason (optional)"})
	} else {
		m.ov = newForm("Reject step "+id,
			"the step still succeeds — the rejection reason travels as data for a LOOP_DECISION",
			"approval-reject",
			&ovField{Key: "reason", Label: "reason (required)"})
	}
	return nil
}

// --- recoveries ------------------------------------------------------------

// recoveryActionAvailable reports whether the plane's action surface allows
// act on the recovery currently selected (from the cached recovery + the
// continuation-plan status the detail fetch recorded).
func (m *Model) recoveryActionAvailable(r *apiv1.RecoveryExecution, act string) error {
	if r == nil {
		return fmt.Errorf("no recovery selected")
	}
	switch act {
	case "plan":
		switch m.planStatus[r.GetId()] {
		case apiv1.PlanStatus_PLAN_STATUS_PENDING:
			return nil
		default:
			return fmt.Errorf("recovery %s has no pending continuation plan to decide", r.GetId())
		}
	case "cancel":
		switch r.GetStatus() {
		case apiv1.RecoveryStatus_RECOVERY_STATUS_PENDING,
			apiv1.RecoveryStatus_RECOVERY_STATUS_RUNNING,
			apiv1.RecoveryStatus_RECOVERY_STATUS_ESCALATED,
			apiv1.RecoveryStatus_RECOVERY_STATUS_BLOCKED:
			return nil
		default:
			return fmt.Errorf("recovery %s is %s — it can no longer be cancelled", r.GetId(), strings.ToLower(r.GetStatus().String()))
		}
	case "succeed":
		if r.GetTaskId() == "" {
			return fmt.Errorf("recovery %s has no affected task to mark succeeded", r.GetId())
		}
		return nil
	}
	return fmt.Errorf("unknown recovery action %q", act)
}

func (m *Model) selectedRecovery() *apiv1.RecoveryExecution {
	return m.recoveries[m.activeID()]
}

func (m *Model) beginPlanDecision(approve bool) tea.Cmd {
	id := m.activeID()
	r := m.selectedRecovery()
	if err := m.recoveryActionAvailable(r, "plan"); err != nil {
		return m.refuse(err.Error())
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = id
	if approve {
		m.ov = newForm("Approve continuation plan",
			"approving resumes recovery (docs/06 §7) — guarded by Confirm",
			"recovery-approve-plan",
			&ovField{Key: "actor", Label: "actor (who approves)"})
	} else {
		m.ov = newForm("Reject continuation plan",
			"rejecting fails/escalates the recovery (docs/06 §8) — guarded by Confirm",
			"recovery-reject-plan",
			&ovField{Key: "actor", Label: "actor (who rejects)"},
			&ovField{Key: "reason", Label: "reason (required)"})
	}
	return nil
}

func (m *Model) beginRecoveryCancel() tea.Cmd {
	r := m.selectedRecovery()
	if err := m.recoveryActionAvailable(r, "cancel"); err != nil {
		return m.refuse(err.Error())
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = r.GetId()
	m.ov = newConfirm("Cancel recovery "+r.GetId(),
		"the affected task returns to its prior state (docs/06 §3).",
		"recovery-cancel")
	return nil
}

func (m *Model) beginMarkSucceeded() tea.Cmd {
	r := m.selectedRecovery()
	if err := m.recoveryActionAvailable(r, "succeed"); err != nil {
		return m.refuse(err.Error())
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = r.GetId()
	m.ov = newForm("Mark task succeeded",
		"task "+r.GetTaskId()+" — the human completion path records an audit event (docs/02 §4 #2)",
		"recovery-mark-succeeded",
		&ovField{Key: "reason", Label: "reason (why the task is complete)"})
	return nil
}

// --- write RPCs ------------------------------------------------------------
//
// Every write goes through kit2's ONE mutation executor (m.Base.Mutate): the
// RPC runs off the update loop, the dock reports progress/failure, and the
// affected list reconciles on success. The plane's refusal text reaches the
// dock VERBATIM inside the failure message (never a silent no-op), and the
// in-flight guard makes a double-press a no-op.

// beginWrite marks a write in flight (the action chords are inert until the
// executor's Result lands) and clears the previous error.
func (m *Model) beginWrite(action string) { m.inFlight = action; m.lastErr = "" }

func (m *Model) cmdApproveStep(stepID string, approved bool, reason string) tea.Cmd {
	m.beginWrite("approval")
	verb := "approve"
	if !approved {
		verb = "reject"
	}
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   verb + " step approval " + stepID,
		Source: "approvals",
		Do: func(ctx context.Context) error {
			_, err := cl.Approvals.ApproveStep(ctx, connect.NewRequest(&apiv1.ApproveStepRequest{
				StepRunId: stepID,
				Approved:  approved,
				Reason:    reason,
			}))
			return err
		},
	})
}

func (m *Model) cmdApprovePlan(recoveryID, actor string) tea.Cmd {
	m.beginWrite("recovery-approve-plan")
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   "approve continuation plan for recovery " + recoveryID,
		Source: "recoveries",
		Do: func(ctx context.Context) error {
			_, err := cl.Recovery.ApproveContinuationPlan(ctx, connect.NewRequest(&apiv1.ApproveContinuationPlanRequest{
				RecoveryId: recoveryID,
				Actor:      actor,
			}))
			return err
		},
	})
}

func (m *Model) cmdRejectPlan(recoveryID, actor, reason string) tea.Cmd {
	m.beginWrite("recovery-reject-plan")
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   "reject continuation plan for recovery " + recoveryID,
		Source: "recoveries",
		Do: func(ctx context.Context) error {
			_, err := cl.Recovery.RejectContinuationPlan(ctx, connect.NewRequest(&apiv1.RejectContinuationPlanRequest{
				RecoveryId: recoveryID,
				Actor:      actor,
				Reason:     reason,
			}))
			return err
		},
	})
}

func (m *Model) cmdCancelRecovery(recoveryID, reason string) tea.Cmd {
	m.beginWrite("recovery-cancel")
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   "cancel recovery " + recoveryID,
		Source: "recoveries",
		Do: func(ctx context.Context) error {
			_, err := cl.Recovery.CancelRecovery(ctx, connect.NewRequest(&apiv1.CancelRecoveryRequest{
				RecoveryId: recoveryID,
				Reason:     reason,
			}))
			return err
		},
	})
}

func (m *Model) cmdMarkSucceeded(recoveryID, taskID, actor, reason string) tea.Cmd {
	m.beginWrite("recovery-mark-succeeded")
	cl := m.cl
	tenant := m.tenantID
	return m.Base.Mutate(mutate.Request{
		Name:   "mark task " + taskID,
		Source: "recoveries",
		Do: func(ctx context.Context) error {
			_, err := cl.Recovery.MarkTaskSucceeded(ctx, connect.NewRequest(&apiv1.MarkTaskSucceededRequest{
				TenantId:  tenant,
				TaskId:    taskID,
				ActorType: "human",
				ActorId:   actor,
				Reason:    reason,
			}))
			return err
		},
	})
}

// --- overlay key handling --------------------------------------------------

// handleOverlayKey routes a key into the open overlay. Returns the cmd the
// submit produced (nil for pure editing keys).
func (m *Model) handleOverlayKey(k tea.KeyMsg) tea.Cmd {
	o := m.ov
	if o == nil {
		return nil
	}
	if m.inFlight != "" {
		return nil // a write is in flight: the overlay is inert
	}
	switch o.kind {
	case ovConfirm:
		switch k.String() {
		case "y", "Y", "enter":
			return m.submitOverlay()
		case "n", "N", "esc":
			m.ov = nil
		}
		return nil

	case ovViewer:
		switch k.String() {
		case "esc", "enter":
			m.ov = nil
		case "up", "k":
			if o.scroll > 0 {
				o.scroll--
			}
		case "down", "j":
			o.scroll++
		}
		return nil

	case ovPicker:
		switch k.String() {
		case "esc":
			m.ov = nil
		case "up", "k":
			if o.cursor > 0 {
				o.cursor--
			}
		case "down", "j":
			if o.cursor < len(o.items)-1 {
				o.cursor++
			}
		case "enter":
			return m.selectOverlayItem()
		}
		return nil

	case ovForm:
		return m.handleFormKey(k)
	}
	return nil
}

func (m *Model) handleFormKey(k tea.KeyMsg) tea.Cmd {
	o := m.ov
	f := o.fields[o.active]
	switch k.String() {
	case "esc":
		m.ov = nil
		return nil
	case "ctrl+s":
		return m.submitOverlay()
	case "tab":
		o.active = (o.active + 1) % len(o.fields)
		return nil
	case "shift+tab":
		o.active = (o.active - 1 + len(o.fields)) % len(o.fields)
		return nil
	case "enter":
		if f.Multi {
			f.Value += "\n"
			return nil
		}
		if o.active == len(o.fields)-1 {
			return m.submitOverlay()
		}
		o.active++
		return nil
	case "backspace":
		r := []rune(f.Value)
		if len(r) > 0 {
			f.Value = string(r[:len(r)-1])
		}
		return nil
	case "space":
		f.Value += " "
		return nil
	}
	if k.Type == tea.KeyRunes && len(k.Runes) > 0 {
		f.Value += string(k.Runes)
	}
	return nil
}

// selectOverlayItem opens the inspector for the highlighted picker row.
//
// The only picker this screen still opens is the recovery-plan actor list, which has no
// inspector: closing is the whole action. (The policy VERSIONS picker that used to inspect a
// Rego body went with the policy editor — see the note at the top of this file.)
func (m *Model) selectOverlayItem() tea.Cmd {
	m.ov = nil
	return nil
}

// submitOverlay validates the open form/confirm and issues its write.
func (m *Model) submitOverlay() tea.Cmd {
	o := m.ov
	if o == nil {
		return nil
	}
	vals := map[string]string{}
	for _, f := range o.fields {
		vals[f.Key] = f.Value
	}
	if o.action == "approval-reject" && strings.TrimSpace(vals["reason"]) == "" {
		o.err = "a rejection reason is required (it is written to .orchicon/<run_id>/summary)"
		m.lastErr = o.err
		m.Fail(o.err)
		return nil
	}
	if o.action == "recovery-reject-plan" && strings.TrimSpace(vals["reason"]) == "" {
		o.err = "a rejection reason is required"
		m.lastErr = o.err
		m.Fail(o.err)
		return nil
	}

	m.ov = nil
	m.inFlight = o.action
	m.lastErr = ""
	switch o.action {
	case "approval-approve":
		return m.cmdApproveStep(m.actRef, true, vals["reason"])
	case "approval-reject":
		return m.cmdApproveStep(m.actRef, false, vals["reason"])
	case "recovery-approve-plan":
		return m.cmdApprovePlan(m.actRef, vals["actor"])
	case "recovery-reject-plan":
		return m.cmdRejectPlan(m.actRef, vals["actor"], vals["reason"])
	case "recovery-cancel":
		return m.cmdCancelRecovery(m.actRef, vals["reason"])
	case "recovery-mark-succeeded":
		r := m.recoveries[m.actRef]
		task := ""
		if r != nil {
			task = r.GetTaskId()
		}
		return m.cmdMarkSucceeded(m.actRef, task, vals["actor"], vals["reason"])
	}
	m.inFlight = ""
	return nil
}

// --- dock surface (mutate.Sink) --------------------------------------------

// Progress / Fail / Notice implement mutate.Sink: the executor's feedback
// lands in the composer dock through the shell's hooks. A refusal is passed
// through VERBATIM — an approval/policy decision the plane rejects is never a
// silent no-op.
func (m *Model) Progress(msg string) {
	m.lastOK = msg
	m.lastErr = ""
	if d, ok := m.Shell().(interface{ DockNotice(string) }); ok && d != nil {
		d.DockNotice(msg)
	}
}

func (m *Model) Fail(msg string) {
	m.lastErr = msg
	m.lastOK = ""
	if d, ok := m.Shell().(interface{ DockError(string) }); ok && d != nil {
		d.DockError(msg)
	}
}

func (m *Model) Notice(msg string) {
	m.lastOK = msg
	m.lastErr = ""
	if d, ok := m.Shell().(interface{ DockNotice(string) }); ok && d != nil {
		d.DockNotice(msg)
	}
}

// --- enum helpers ----------------------------------------------------------
//
// Only the EFFECT name survives: the approvals detail renders the policy decisions recorded
// against a step run, and needs to print an effect. The parse/name helpers for decision
// point, scope and version status belonged to the policy editor, which is gone.

func effectName(v apiv1.PolicyEffect) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "POLICY_EFFECT_"))
}
