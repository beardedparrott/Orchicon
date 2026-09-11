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

// editLoadedMsg carries the policy version an edit form is prefilled from.
type editLoadedMsg struct {
	policyID string
	version  *apiv1.PolicyVersion
	err      error
}

// versionsLoadedMsg carries a policy's version list for the picker.
type versionsLoadedMsg struct {
	policyID string
	versions []*apiv1.PolicyVersion
	err      error
}

// policyValues is the validated policy-editor payload (the enum fields are
// parsed before the write is issued).
type policyValues struct {
	name, scopeRef, query, versionNote, rego string
	dp                                       apiv1.DecisionPoint
	scope                                    apiv1.PolicyScope
	effect                                   apiv1.PolicyEffect
}

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
		switch k.String() {
		case "n":
			return true, m.beginPolicyCreate()
		case "e":
			return true, m.beginPolicyEdit()
		case "p":
			return true, m.beginPolicyPublish()
		case "v":
			return true, m.beginPolicyVersions()
		}
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

// --- policies --------------------------------------------------------------

func (m *Model) beginPolicyCreate() tea.Cmd {
	if m.inFlight != "" {
		return nil
	}
	m.actRef = ""
	m.ov = newForm("New policy",
		"rego/opa module — the body is sent whole (no silent truncation)",
		"policy-create",
		&ovField{Key: "name", Label: "name"},
		&ovField{Key: "decision_point", Label: "decision point (admission|dispatch|budget|approval|recovery|completion)"},
		&ovField{Key: "scope", Label: "scope (tenant|project|worker|task)", Value: "tenant"},
		&ovField{Key: "scope_ref", Label: "scope ref (project_id / worker_id, empty for tenant)"},
		&ovField{Key: "effect", Label: "effect (allow|deny|require_approval|require_review)", Value: "deny"},
		&ovField{Key: "query", Label: "query (default data.<pkg>.allow)"},
		&ovField{Key: "version_note", Label: "version note"},
		&ovField{Key: "rego_module", Label: "rego module", Multi: true})
	return nil
}

func (m *Model) beginPolicyEdit() tea.Cmd {
	id := m.activeID()
	if id == "" {
		return m.refuse("no policy selected")
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = id
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		resp, err := cl.Policies.ListPolicyVersions(ctx, connect.NewRequest(&apiv1.ListPolicyVersionsRequest{PolicyId: id}))
		if err != nil {
			return editLoadedMsg{policyID: id, err: err}
		}
		vs := resp.Msg.GetVersions()
		if len(vs) == 0 {
			return editLoadedMsg{policyID: id, err: fmt.Errorf("policy %s has no version to edit", id)}
		}
		// Prefer the draft (the only mutable version); else the newest.
		pick := vs[0]
		for _, v := range vs {
			if v.GetStatus() == apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT {
				pick = v
				break
			}
		}
		return editLoadedMsg{policyID: id, version: pick}
	}
}

func (m *Model) beginPolicyPublish() tea.Cmd {
	id := m.activeID()
	if id == "" {
		return m.refuse("no policy selected")
	}
	if m.inFlight != "" {
		return nil
	}
	m.actRef = id
	m.ov = newConfirm("Publish policy "+id,
		"the draft version is compiled into the active bundle and becomes immutable.",
		"policy-publish")
	return nil
}

func (m *Model) beginPolicyVersions() tea.Cmd {
	id := m.activeID()
	if id == "" {
		return m.refuse("no policy selected")
	}
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		resp, err := cl.Policies.ListPolicyVersions(ctx, connect.NewRequest(&apiv1.ListPolicyVersionsRequest{PolicyId: id}))
		if err != nil {
			return versionsLoadedMsg{policyID: id, err: err}
		}
		return versionsLoadedMsg{policyID: id, versions: resp.Msg.GetVersions()}
	}
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

func (m *Model) cmdCreatePolicy(pv policyValues) tea.Cmd {
	m.beginWrite("policy-create")
	cl := m.cl
	tenant := m.tenantID
	return m.Base.Mutate(mutate.Request{
		Name:   "create policy " + pv.name,
		Source: "policies",
		Do: func(ctx context.Context) error {
			_, err := cl.Policies.CreatePolicy(ctx, connect.NewRequest(&apiv1.CreatePolicyRequest{
				TenantId:      tenant,
				Name:          pv.name,
				DecisionPoint: pv.dp,
				Scope:         pv.scope,
				ScopeRef:      pv.scopeRef,
				Effect:        pv.effect,
				RegoModule:    pv.rego,
				Query:         pv.query,
				VersionNote:   pv.versionNote,
			}))
			return err
		},
	})
}

func (m *Model) cmdUpdatePolicyVersion(policyID string, pv policyValues) tea.Cmd {
	m.beginWrite("policy-edit")
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   "save policy " + policyID,
		Source: "policies",
		Do: func(ctx context.Context) error {
			_, err := cl.Policies.UpdatePolicyVersion(ctx, connect.NewRequest(&apiv1.UpdatePolicyVersionRequest{
				PolicyId:      policyID,
				DecisionPoint: pv.dp,
				Scope:         pv.scope,
				ScopeRef:      pv.scopeRef,
				Effect:        pv.effect,
				RegoModule:    pv.rego,
				Query:         pv.query,
				VersionNote:   pv.versionNote,
			}))
			return err
		},
	})
}

func (m *Model) cmdPublishPolicy(policyID string) tea.Cmd {
	m.beginWrite("policy-publish")
	cl := m.cl
	return m.Base.Mutate(mutate.Request{
		Name:   "publish policy " + policyID,
		Source: "policies",
		Do: func(ctx context.Context) error {
			_, err := cl.Policies.PublishPolicy(ctx, connect.NewRequest(&apiv1.PublishPolicyRequest{PolicyId: policyID}))
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
func (m *Model) selectOverlayItem() tea.Cmd {
	o := m.ov
	if o == nil || o.cursor < 0 || o.cursor >= len(o.items) {
		return nil
	}
	if o.action != "policy-versions" {
		m.ov = nil
		return nil
	}
	v := m.versions[o.items[o.cursor].ID]
	if v == nil {
		return nil
	}
	body := "v" + fmt.Sprint(v.GetVersion()) + "  " + versionStatusName(v.GetStatus()) + "\n" +
		"decision point: " + decisionPointName(v.GetDecisionPoint()) + "\n" +
		"scope: " + scopeName(v.GetScope()) + " " + v.GetScopeRef() + "\n" +
		"effect: " + effectName(v.GetEffect()) + "\n" +
		"query: " + v.GetQuery() + "\n" +
		"note: " + v.GetVersionNote() + "\n\n" +
		v.GetRegoModule()
	m.ov = newViewer("Policy "+m.policyFor+" version v"+fmt.Sprint(v.GetVersion()), "rego module (verbatim)", body)
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
	var pv policyValues
	// Enum fields parse up-front: a bad value is refused locally (shown in
	// the overlay AND the dock) instead of sending a half-formed write.
	if o.action == "policy-create" || o.action == "policy-edit" {
		if strings.TrimSpace(vals["name"]) == "" && o.action == "policy-create" {
			o.err = "name is required"
			m.lastErr = "policy: " + o.err
			m.Fail("policy: " + o.err)
			return nil
		}
		dp, err := parseDecisionPoint(vals["decision_point"])
		if err != nil {
			o.err = err.Error()
			m.lastErr = o.err
			m.Fail(o.err)
			return nil
		}
		sc, err := parseScope(vals["scope"])
		if err != nil {
			o.err = err.Error()
			m.lastErr = o.err
			m.Fail(o.err)
			return nil
		}
		ef, err := parseEffect(vals["effect"])
		if err != nil {
			o.err = err.Error()
			m.lastErr = o.err
			m.Fail(o.err)
			return nil
		}
		if strings.TrimSpace(vals["rego_module"]) == "" {
			o.err = "the rego module must not be empty"
			m.lastErr = o.err
			m.Fail(o.err)
			return nil
		}
		pv = policyValues{
			name:        vals["name"],
			scopeRef:    vals["scope_ref"],
			query:       vals["query"],
			versionNote: vals["version_note"],
			rego:        vals["rego_module"],
			dp:          dp,
			scope:       sc,
			effect:      ef,
		}
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
	case "policy-create":
		return m.cmdCreatePolicy(pv)
	case "policy-edit":
		return m.cmdUpdatePolicyVersion(m.actRef, pv)
	case "policy-publish":
		return m.cmdPublishPolicy(m.actRef)
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

// --- async results ---------------------------------------------------------

func (m *Model) handleEditLoaded(msg editLoadedMsg) tea.Cmd {
	if msg.err != nil {
		m.lastErr = msg.err.Error()
		m.Fail(msg.err.Error())
		return nil
	}
	v := msg.version
	m.ov = newForm("Edit policy "+msg.policyID+" (v"+fmt.Sprint(v.GetVersion())+")",
		"only a draft version is mutable — publishing makes it immutable",
		"policy-edit",
		&ovField{Key: "decision_point", Label: "decision point", Value: decisionPointName(v.GetDecisionPoint())},
		&ovField{Key: "scope", Label: "scope", Value: scopeName(v.GetScope())},
		&ovField{Key: "scope_ref", Label: "scope ref", Value: v.GetScopeRef()},
		&ovField{Key: "effect", Label: "effect", Value: effectName(v.GetEffect())},
		&ovField{Key: "query", Label: "query", Value: v.GetQuery()},
		&ovField{Key: "version_note", Label: "version note", Value: v.GetVersionNote()},
		&ovField{Key: "rego_module", Label: "rego module", Multi: true, Value: v.GetRegoModule()})
	return nil
}

func (m *Model) handleVersionsLoaded(msg versionsLoadedMsg) tea.Cmd {
	if msg.err != nil {
		m.lastErr = msg.err.Error()
		m.Fail(msg.err.Error())
		return nil
	}
	m.policyFor = msg.policyID
	m.versions = map[string]*apiv1.PolicyVersion{}
	items := make([]ovItem, 0, len(msg.versions))
	for _, v := range msg.versions {
		id := fmt.Sprint(v.GetVersion())
		m.versions[id] = v
		items = append(items, ovItem{
			Label: "v" + id,
			ID:    id,
			Meta:  versionStatusName(v.GetStatus()) + "  " + v.GetVersionNote(),
		})
	}
	m.ov = newPicker("Policy "+msg.policyID+" versions",
		"enter inspects a version's rego body (verbatim — no truncation)",
		"policy-versions", items)
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

func parseDecisionPoint(s string) (apiv1.DecisionPoint, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "admission":
		return apiv1.DecisionPoint_DECISION_POINT_ADMISSION, nil
	case "dispatch":
		return apiv1.DecisionPoint_DECISION_POINT_DISPATCH, nil
	case "budget":
		return apiv1.DecisionPoint_DECISION_POINT_BUDGET, nil
	case "approval":
		return apiv1.DecisionPoint_DECISION_POINT_APPROVAL, nil
	case "recovery":
		return apiv1.DecisionPoint_DECISION_POINT_RECOVERY, nil
	case "completion":
		return apiv1.DecisionPoint_DECISION_POINT_COMPLETION, nil
	}
	return 0, fmt.Errorf("unknown decision point %q (admission|dispatch|budget|approval|recovery|completion)", s)
}

func decisionPointName(v apiv1.DecisionPoint) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "DECISION_POINT_"))
}

func parseScope(s string) (apiv1.PolicyScope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "tenant":
		return apiv1.PolicyScope_POLICY_SCOPE_TENANT, nil
	case "project":
		return apiv1.PolicyScope_POLICY_SCOPE_PROJECT, nil
	case "worker":
		return apiv1.PolicyScope_POLICY_SCOPE_WORKER, nil
	case "task":
		return apiv1.PolicyScope_POLICY_SCOPE_TASK, nil
	}
	return 0, fmt.Errorf("unknown scope %q (tenant|project|worker|task)", s)
}

func scopeName(v apiv1.PolicyScope) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "POLICY_SCOPE_"))
}

func parseEffect(s string) (apiv1.PolicyEffect, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return apiv1.PolicyEffect_POLICY_EFFECT_ALLOW, nil
	case "deny":
		return apiv1.PolicyEffect_POLICY_EFFECT_DENY, nil
	case "require_approval":
		return apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_APPROVAL, nil
	case "require_review":
		return apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_REVIEW, nil
	}
	return 0, fmt.Errorf("unknown effect %q (allow|deny|require_approval|require_review)", s)
}

func effectName(v apiv1.PolicyEffect) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "POLICY_EFFECT_"))
}

func versionStatusName(v apiv1.PolicyVersionStatus) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "POLICY_VERSION_STATUS_"))
}

func policyStatusName(v apiv1.PolicyStatus) string {
	return strings.ToLower(strings.TrimPrefix(v.String(), "POLICY_STATUS_"))
}
