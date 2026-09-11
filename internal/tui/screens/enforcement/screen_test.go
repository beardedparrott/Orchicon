package enforcement

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// fakePlane is one Connect handler set serving Policy + Approval + Recovery
// (the three services the enforcement screen writes to). It records every
// write request and mutates its own list state so the screen's post-write
// reconciliation is observable.
type fakePlane struct {
	apiv1connect.UnimplementedPolicyServiceHandler
	apiv1connect.UnimplementedApprovalServiceHandler
	apiv1connect.UnimplementedRecoveryServiceHandler

	mu         sync.Mutex
	approvals  []*apiv1.ApprovalItem
	recoveries []*apiv1.RecoveryExecution
	plan       *apiv1.ContinuationPlan
	policies   []*apiv1.Policy
	versions   []*apiv1.PolicyVersion
	decisions  []*apiv1.PolicyDecision

	approveReqs   []*apiv1.ApproveStepRequest
	createReqs    []*apiv1.CreatePolicyRequest
	updateReqs    []*apiv1.UpdatePolicyVersionRequest
	publishReqs   []*apiv1.PublishPolicyRequest
	approvePlan   []*apiv1.ApproveContinuationPlanRequest
	rejectPlan    []*apiv1.RejectContinuationPlanRequest
	cancelReqs    []*apiv1.CancelRecoveryRequest
	succeedReqs   []*apiv1.MarkTaskSucceededRequest
	versionLists  int
	approveErr    error
	createErr     error
	updateErr     error
	publishErr    error
	approveErrStr string
}

func (f *fakePlane) ListPendingStepApprovals(ctx context.Context, req *connect.Request[apiv1.ListPendingStepApprovalsRequest]) (*connect.Response[apiv1.ListPendingStepApprovalsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&apiv1.ListPendingStepApprovalsResponse{
		Items: append([]*apiv1.ApprovalItem{}, f.approvals...),
	}), nil
}

func (f *fakePlane) ApproveStep(ctx context.Context, req *connect.Request[apiv1.ApproveStepRequest]) (*connect.Response[apiv1.ApproveStepResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approveReqs = append(f.approveReqs, req.Msg)
	if f.approveErr != nil {
		return nil, f.approveErr
	}
	// The step is resolved: it leaves the pending list (reconciliation).
	kept := make([]*apiv1.ApprovalItem, 0, len(f.approvals))
	for _, a := range f.approvals {
		if a.GetStepRunId() != req.Msg.GetStepRunId() {
			kept = append(kept, a)
		}
	}
	f.approvals = kept
	return connect.NewResponse(&apiv1.ApproveStepResponse{}), nil
}

func (f *fakePlane) ListPolicies(ctx context.Context, req *connect.Request[apiv1.ListPoliciesRequest]) (*connect.Response[apiv1.ListPoliciesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&apiv1.ListPoliciesResponse{
		Policies: append([]*apiv1.Policy{}, f.policies...),
	}), nil
}

func (f *fakePlane) GetPolicy(ctx context.Context, req *connect.Request[apiv1.GetPolicyRequest]) (*connect.Response[apiv1.GetPolicyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.policies {
		if p.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&apiv1.GetPolicyResponse{Policy: p}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such policy"))
}

func (f *fakePlane) ListPolicyVersions(ctx context.Context, req *connect.Request[apiv1.ListPolicyVersionsRequest]) (*connect.Response[apiv1.ListPolicyVersionsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versionLists++
	return connect.NewResponse(&apiv1.ListPolicyVersionsResponse{
		Versions: append([]*apiv1.PolicyVersion{}, f.versions...),
	}), nil
}

func (f *fakePlane) CreatePolicy(ctx context.Context, req *connect.Request[apiv1.CreatePolicyRequest]) (*connect.Response[apiv1.CreatePolicyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createReqs = append(f.createReqs, req.Msg)
	if f.createErr != nil {
		return nil, f.createErr
	}
	p := &apiv1.Policy{Id: "pol-new", Name: req.Msg.GetName(), Status: apiv1.PolicyStatus_POLICY_STATUS_DRAFT}
	f.policies = append(f.policies, p)
	return connect.NewResponse(&apiv1.CreatePolicyResponse{
		Policy:  p,
		Version: &apiv1.PolicyVersion{PolicyId: p.GetId(), Version: 1, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT, RegoModule: req.Msg.GetRegoModule()},
	}), nil
}

func (f *fakePlane) UpdatePolicyVersion(ctx context.Context, req *connect.Request[apiv1.UpdatePolicyVersionRequest]) (*connect.Response[apiv1.UpdatePolicyVersionResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateReqs = append(f.updateReqs, req.Msg)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	// Saving a draft mutates it (the version inspector must see the save).
	for _, v := range f.versions {
		if v.GetPolicyId() == req.Msg.GetPolicyId() && v.GetStatus() == apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT {
			v.RegoModule = req.Msg.GetRegoModule()
			v.Query = req.Msg.GetQuery()
			v.Effect = req.Msg.GetEffect()
			v.DecisionPoint = req.Msg.GetDecisionPoint()
			v.Scope = req.Msg.GetScope()
			v.ScopeRef = req.Msg.GetScopeRef()
		}
	}
	return connect.NewResponse(&apiv1.UpdatePolicyVersionResponse{
		Version: &apiv1.PolicyVersion{PolicyId: req.Msg.GetPolicyId(), Version: 3, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT, RegoModule: req.Msg.GetRegoModule()},
	}), nil
}

func (f *fakePlane) PublishPolicy(ctx context.Context, req *connect.Request[apiv1.PublishPolicyRequest]) (*connect.Response[apiv1.PublishPolicyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishReqs = append(f.publishReqs, req.Msg)
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	for _, p := range f.policies {
		if p.GetId() == req.Msg.GetPolicyId() {
			p.Status = apiv1.PolicyStatus_POLICY_STATUS_PUBLISHED
			p.CurrentVersion = 3
		}
	}
	return connect.NewResponse(&apiv1.PublishPolicyResponse{
		Policy:  &apiv1.Policy{Id: req.Msg.GetPolicyId(), Status: apiv1.PolicyStatus_POLICY_STATUS_PUBLISHED, CurrentVersion: 3},
		Version: &apiv1.PolicyVersion{PolicyId: req.Msg.GetPolicyId(), Version: 3, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED},
	}), nil
}

func (f *fakePlane) ListDecisions(ctx context.Context, req *connect.Request[apiv1.ListDecisionsRequest]) (*connect.Response[apiv1.ListDecisionsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*apiv1.PolicyDecision
	for _, d := range f.decisions {
		if req.Msg.GetTargetId() != "" && d.GetTargetId() != req.Msg.GetTargetId() {
			continue
		}
		out = append(out, d)
	}
	return connect.NewResponse(&apiv1.ListDecisionsResponse{Decisions: out}), nil
}

func (f *fakePlane) GetDecision(ctx context.Context, req *connect.Request[apiv1.GetDecisionRequest]) (*connect.Response[apiv1.GetDecisionResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.decisions {
		if d.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&apiv1.GetDecisionResponse{Decision: d}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such decision"))
}

func (f *fakePlane) ListRecoveries(ctx context.Context, req *connect.Request[apiv1.ListRecoveriesRequest]) (*connect.Response[apiv1.ListRecoveriesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&apiv1.ListRecoveriesResponse{
		Recoveries: append([]*apiv1.RecoveryExecution{}, f.recoveries...),
	}), nil
}

func (f *fakePlane) GetRecovery(ctx context.Context, req *connect.Request[apiv1.GetRecoveryRequest]) (*connect.Response[apiv1.GetRecoveryResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.recoveries {
		if r.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&apiv1.GetRecoveryResponse{Recovery: r}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such recovery"))
}

func (f *fakePlane) GetContinuationPlan(ctx context.Context, req *connect.Request[apiv1.GetContinuationPlanRequest]) (*connect.Response[apiv1.GetContinuationPlanResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&apiv1.GetContinuationPlanResponse{Plan: f.plan}), nil
}

func (f *fakePlane) ApproveContinuationPlan(ctx context.Context, req *connect.Request[apiv1.ApproveContinuationPlanRequest]) (*connect.Response[apiv1.ApproveContinuationPlanResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvePlan = append(f.approvePlan, req.Msg)
	if f.approveErr != nil {
		return nil, f.approveErr
	}
	return connect.NewResponse(&apiv1.ApproveContinuationPlanResponse{}), nil
}

func (f *fakePlane) RejectContinuationPlan(ctx context.Context, req *connect.Request[apiv1.RejectContinuationPlanRequest]) (*connect.Response[apiv1.RejectContinuationPlanResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectPlan = append(f.rejectPlan, req.Msg)
	return connect.NewResponse(&apiv1.RejectContinuationPlanResponse{}), nil
}

func (f *fakePlane) CancelRecovery(ctx context.Context, req *connect.Request[apiv1.CancelRecoveryRequest]) (*connect.Response[apiv1.CancelRecoveryResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelReqs = append(f.cancelReqs, req.Msg)
	return connect.NewResponse(&apiv1.CancelRecoveryResponse{}), nil
}

func (f *fakePlane) MarkTaskSucceeded(ctx context.Context, req *connect.Request[apiv1.MarkTaskSucceededRequest]) (*connect.Response[apiv1.MarkTaskSucceededResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeedReqs = append(f.succeedReqs, req.Msg)
	return connect.NewResponse(&apiv1.MarkTaskSucceededResponse{TaskId: req.Msg.GetTaskId(), Status: "succeeded"}), nil
}

// stubShell records the dock surface the screen pushes into.
type stubShell struct {
	notice string
	err    string
}

func (s *stubShell) EnforcementDockNotice(t string) { s.notice = t }
func (s *stubShell) EnforcementDockError(t string)  { s.err = t }

func newHarness(t *testing.T, f *fakePlane) (*Model, *stubShell) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewPolicyServiceHandler(f))
	mux.Handle(apiv1connect.NewApprovalServiceHandler(f))
	mux.Handle(apiv1connect.NewRecoveryServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL, Timeout: 5 * time.Second}, srv.Client())
	m := New(cl, subs.NewRegistry(), "")
	sh := &stubShell{}
	m.SetShell(sh)
	m.SetSize(160, 44)
	return m, sh
}

// runCmd drains a tea.Cmd graph into the model (cmd → msg → Update → cmd…).
func runCmd(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmd(t, m, c)
		}
		return
	}
	_, next := m.Update(msg)
	runCmd(t, m, next)
}

func typeKey(s string) tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func enterKey() tea.KeyMsg           { return tea.KeyMsg{Type: tea.KeyEnter} }
func escKey() tea.KeyMsg             { return tea.KeyMsg{Type: tea.KeyEsc} }
func ctrlSKey() tea.KeyMsg           { return tea.KeyMsg{Type: tea.KeyCtrlS} }
func downKey() tea.KeyMsg            { return tea.KeyMsg{Type: tea.KeyDown} }
func loadAll(t *testing.T, m *Model) { t.Helper(); runCmd(t, m, m.Load()) }

func sampleApproval(id string) *apiv1.ApprovalItem {
	return &apiv1.ApprovalItem{
		StepRunId:          id,
		WorkflowName:       "wf-release",
		ProjectName:        "orchicon",
		WorkItemName:       "wi-7",
		UpstreamWorker:     "eng-1",
		UpstreamSummary:    "implemented the enforcement writes",
		TouchedFiles:       []string{"internal/tui/app.go"},
		AcceptanceCriteria: "approve/reject asserted",
		Status:             "pending",
		Reason:             "policy requires a human decision",
		AttachmentNames:    []string{"diff.patch"},
	}
}

// TestApprovalApproveRejectReconcile is acceptance criterion 1: approve and
// reject a pending step approval from the TUI (RPC asserted), the list
// reconciles afterwards, and the detail carries the upstream + policy
// context.
func TestApprovalApproveRejectReconcile(t *testing.T) {
	f := &fakePlane{
		approvals: []*apiv1.ApprovalItem{sampleApproval("sr-1"), sampleApproval("sr-2")},
		decisions: []*apiv1.PolicyDecision{{
			Id: "dec-1", PolicyId: "pol-9", PolicyVersion: 2,
			DecisionPoint: "approval", Effect: apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_APPROVAL,
			TargetType: "step_run", TargetId: "sr-1",
		}},
	}
	m, sh := newHarness(t, f)
	m.SelectSource("approvals")
	loadAll(t, m)

	if _, ok := m.Base.SourceItem("approvals", "sr-1"); !ok {
		t.Fatal("sr-1 must be listed after load")
	}

	// The detail carries the approval context AND the policy decision.
	_, fields, body, err := m.detail(context.Background(), "approvals", "sr-1")
	if err != nil {
		t.Fatalf("approval detail: %v", err)
	}
	joined := ""
	for _, fl := range fields {
		joined += fl.Key + "=" + fl.Value + "\n"
	}
	for _, want := range []string{"upstream worker=eng-1", "reason=policy requires a human decision", "attachments=diff.patch"} {
		if !strings.Contains(joined, want) {
			t.Errorf("approval detail missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(body, "policy context:") || !strings.Contains(body, "require_approval  policy pol-9") {
		t.Errorf("approval detail must surface the policy context, got:\n%s", body)
	}

	if m.ClaimsKeys() {
		t.Fatal("no overlay is open — the screen must not claim the keyboard")
	}

	// Approve (form-gated by the reason prompt).
	if _, cmd := m.Update(typeKey("a")); cmd != nil {
		t.Fatalf("opening the approve form must not issue a write")
	}
	if !m.ClaimsKeys() || m.ov.action != "approval-approve" {
		t.Fatalf("approve must open its reason form, got %+v", m.ov)
	}
	if _, cmd := m.Update(typeKey("looks good")); cmd != nil {
		t.Fatalf("typing must not issue a write")
	}
	_, cmd := m.Update(enterKey())
	runCmd(t, m, cmd)

	if len(f.approveReqs) != 1 {
		t.Fatalf("ApproveStep calls = %d, want 1", len(f.approveReqs))
	}
	if got := f.approveReqs[0]; !got.GetApproved() || got.GetStepRunId() != "sr-1" || got.GetReason() != "looks good" {
		t.Fatalf("ApproveStep request = %+v", got)
	}
	if m.inFlight != "" || m.ov != nil {
		t.Fatalf("the write must clear inFlight/close the form (inFlight=%q ov=%v)", m.inFlight, m.ov)
	}
	if sh.err != "" {
		t.Fatalf("a successful approve must not set a dock error: %q", sh.err)
	}
	if !strings.Contains(sh.notice, "approved step approval sr-1") {
		t.Fatalf("dock notice = %q, want the success notice", sh.notice)
	}
	// Reconciliation: the resolved step left the list.
	if _, ok := m.Base.SourceItem("approvals", "sr-1"); ok {
		t.Fatal("the approvals list must reconcile — sr-1 is resolved")
	}
	if _, ok := m.Base.SourceItem("approvals", "sr-2"); !ok {
		t.Fatal("the untouched approval must survive the reconcile")
	}

	// Reject: empty reason is refused locally (never a silent no-op).
	m.Base.SelectItem("approvals", "sr-2")
	m.Update(typeKey("x"))
	if m.ov == nil || m.ov.action != "approval-reject" {
		t.Fatalf("reject must open its reason form, got %+v", m.ov)
	}
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.approveReqs) != 1 {
		t.Fatalf("an empty rejection reason must NOT reach the plane (calls=%d)", len(f.approveReqs))
	}
	if m.ov == nil || m.ov.err == "" {
		t.Fatal("an empty rejection reason must be refused with a visible reason")
	}
	if sh.err == "" {
		t.Fatal("the local refusal must surface in the dock")
	}
	// Reject for real.
	m.Update(typeKey("not shippable"))
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.approveReqs) != 2 {
		t.Fatalf("ApproveStep calls = %d, want 2", len(f.approveReqs))
	}
	if got := f.approveReqs[1]; got.GetApproved() || got.GetReason() != "not shippable" {
		t.Fatalf("reject request = %+v", got)
	}
	if !strings.Contains(sh.notice, "rejected step approval sr-2") {
		t.Fatalf("dock notice = %q", sh.notice)
	}
}

// TestApprovalWriteDisabledWhileInFlight pins the idempotence guard: a
// second press while the first write is in flight must not issue a second
// RPC.
func TestApprovalWriteDisabledWhileInFlight(t *testing.T) {
	f := &fakePlane{approvals: []*apiv1.ApprovalItem{sampleApproval("sr-1")}}
	m, _ := newHarness(t, f)
	m.SelectSource("approvals")
	loadAll(t, m)

	m.Update(typeKey("a"))
	_, cmd := m.Update(enterKey()) // submit; the RPC is now in flight
	if m.inFlight == "" {
		t.Fatal("submitting must mark the action in flight")
	}
	m.Update(typeKey("a")) // double-press
	if m.ov != nil {
		t.Fatal("a double-press while in flight must not open a second write")
	}
	runCmd(t, m, cmd)
	if len(f.approveReqs) != 1 {
		t.Fatalf("ApproveStep calls = %d, want exactly 1 (double-press safe)", len(f.approveReqs))
	}
}

// TestPolicyCreateEditPublishVersions is acceptance criterion 2: create,
// edit, publish, list versions and inspect a version body — every RPC
// asserted, the Rego body sent whole.
func TestPolicyCreateEditPublishVersions(t *testing.T) {
	const draftRego = "package orchicon.policy.pol1\n\nimport future.keywords\n\nallow {\n  input.action == \"merge\"\n  not input.draft\n}\n"
	const publishedRego = "package orchicon.policy.pol1\n\nallow = false\n"
	f := &fakePlane{
		policies: []*apiv1.Policy{{Id: "pol-1", Name: "gate", Status: apiv1.PolicyStatus_POLICY_STATUS_PUBLISHED, CurrentVersion: 2}},
		versions: []*apiv1.PolicyVersion{
			{PolicyId: "pol-1", Version: 3, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT, RegoModule: draftRego, Effect: apiv1.PolicyEffect_POLICY_EFFECT_DENY, DecisionPoint: apiv1.DecisionPoint_DECISION_POINT_APPROVAL, Scope: apiv1.PolicyScope_POLICY_SCOPE_TENANT, Query: "data.orchicon.policy.pol1.allow", VersionNote: "tighten"},
			{PolicyId: "pol-1", Version: 2, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED, RegoModule: publishedRego},
			{PolicyId: "pol-1", Version: 1, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_SUPERSEDED, RegoModule: publishedRego},
		},
	}
	m, sh := newHarness(t, f)
	m.SelectSource("policies")
	loadAll(t, m)

	// --- create -----------------------------------------------------------
	m.Update(typeKey("n"))
	if m.ov == nil || m.ov.action != "policy-create" {
		t.Fatalf("n must open the create form, got %+v", m.ov)
	}
	m.ov.setValue("name", "merge-gate")
	m.ov.setValue("decision_point", "admission")
	m.ov.setValue("scope", "project")
	m.ov.setValue("scope_ref", "prj-1")
	m.ov.setValue("effect", "require_approval")
	m.ov.setValue("query", "data.orchicon.policy.mergegate.allow")
	m.ov.setValue("rego_module", draftRego)
	_, cmd := m.Update(ctrlSKey())
	runCmd(t, m, cmd)

	if len(f.createReqs) != 1 {
		t.Fatalf("CreatePolicy calls = %d, want 1", len(f.createReqs))
	}
	got := f.createReqs[0]
	if got.GetName() != "merge-gate" ||
		got.GetDecisionPoint() != apiv1.DecisionPoint_DECISION_POINT_ADMISSION ||
		got.GetScope() != apiv1.PolicyScope_POLICY_SCOPE_PROJECT ||
		got.GetScopeRef() != "prj-1" ||
		got.GetEffect() != apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_APPROVAL ||
		got.GetQuery() != "data.orchicon.policy.mergegate.allow" {
		t.Fatalf("CreatePolicy request = %+v", got)
	}
	if got.GetRegoModule() != draftRego {
		t.Fatalf("the Rego body must be sent WHOLE (no truncation):\n%q", got.GetRegoModule())
	}
	if !strings.Contains(sh.notice, "created policy") {
		t.Fatalf("dock notice = %q", sh.notice)
	}
	// Reconciliation: the new policy is in the list.
	if _, ok := m.Base.SourceItem("policies", "pol-new"); !ok {
		t.Fatal("the policies list must reconcile after CreatePolicy")
	}

	// --- edit (draft prefill) --------------------------------------------
	m.Base.SelectItem("policies", "pol-1")
	if _, cmd := m.Update(typeKey("e")); cmd == nil {
		t.Fatal("e must fetch the draft version to prefill the editor")
	} else {
		runCmd(t, m, cmd)
	}
	if m.ov == nil || m.ov.action != "policy-edit" {
		t.Fatalf("e must open the edit form, got %+v", m.ov)
	}
	if m.ov.value("rego_module") != draftRego {
		t.Fatalf("the editor must prefill the DRAFT rego verbatim, got %q", m.ov.value("rego_module"))
	}
	if m.ov.value("decision_point") != "approval" || m.ov.value("effect") != "deny" {
		t.Fatalf("the editor must prefill the enums, got %q / %q", m.ov.value("decision_point"), m.ov.value("effect"))
	}
	edited := draftRego + "\nbudget_ok { input.budget < 1000 }\n"
	m.ov.setValue("rego_module", edited)
	_, cmd = m.Update(ctrlSKey())
	runCmd(t, m, cmd)
	if len(f.updateReqs) != 1 {
		t.Fatalf("UpdatePolicyVersion calls = %d, want 1", len(f.updateReqs))
	}
	if u := f.updateReqs[0]; u.GetPolicyId() != "pol-1" || u.GetRegoModule() != edited {
		t.Fatalf("UpdatePolicyVersion request = %+v", u)
	}

	// --- publish (Confirm) ------------------------------------------------
	m.Update(typeKey("p"))
	if m.ov == nil || m.ov.kind != ovConfirm {
		t.Fatalf("p must open a Confirm overlay, got %+v", m.ov)
	}
	m.Update(typeKey("n")) // cancel must not write
	if len(f.publishReqs) != 0 {
		t.Fatal("cancelling the Confirm must not publish")
	}
	m.Update(typeKey("p"))
	_, cmd = m.Update(typeKey("y"))
	runCmd(t, m, cmd)
	if len(f.publishReqs) != 1 || f.publishReqs[0].GetPolicyId() != "pol-1" {
		t.Fatalf("PublishPolicy calls = %+v", f.publishReqs)
	}

	// --- list versions + inspect a body -----------------------------------
	m.Base.SelectItem("policies", "pol-1")
	_, cmd = m.Update(typeKey("v"))
	runCmd(t, m, cmd)
	if m.ov == nil || m.ov.kind != ovPicker || m.ov.action != "policy-versions" {
		t.Fatalf("v must open the versions picker, got %+v", m.ov)
	}
	if len(m.ov.items) != 3 {
		t.Fatalf("the picker must list every version, got %d", len(m.ov.items))
	}
	if f.versionLists < 2 {
		t.Fatalf("ListPolicyVersions calls = %d, want the versions listed", f.versionLists)
	}
	// Row 0 is the draft (v3) — inspect it; the body is the rego verbatim.
	m.Update(enterKey())
	if m.ov == nil || m.ov.kind != ovViewer {
		t.Fatalf("enter must open the version body viewer, got %+v", m.ov)
	}
	if !strings.Contains(m.ov.body, "budget_ok { input.budget < 1000 }") || !strings.Contains(m.ov.body, "package orchicon.policy.pol1") {
		t.Fatalf("the inspector must show the version's rego body verbatim:\n%s", m.ov.body)
	}
	m.Update(escKey())
	if m.ov != nil {
		t.Fatal("esc must close the viewer")
	}
}

// TestVersionsPickerRowsAreSelectable pins that a version row other than the
// first is inspectable (the picker cursor really moves).
func TestVersionsPickerRowsAreSelectable(t *testing.T) {
	const v2Rego = "package p\nallow { false }\n"
	f := &fakePlane{
		policies: []*apiv1.Policy{{Id: "pol-1", Name: "gate"}},
		versions: []*apiv1.PolicyVersion{
			{PolicyId: "pol-1", Version: 2, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED, RegoModule: v2Rego},
			{PolicyId: "pol-1", Version: 1, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_SUPERSEDED, RegoModule: "package p\nallow { true }\n"},
		},
	}
	m, _ := newHarness(t, f)
	m.SelectSource("policies")
	loadAll(t, m)
	_, cmd := m.Update(typeKey("v"))
	runCmd(t, m, cmd)
	m.Update(downKey())
	m.Update(enterKey())
	if m.ov == nil || m.ov.kind != ovViewer {
		t.Fatalf("expected a viewer, got %+v", m.ov)
	}
	if !strings.Contains(m.ov.body, "allow { true }") {
		t.Fatalf("row 1 (v1) body must be shown, got:\n%s", m.ov.body)
	}
}

// TestRefusalsSurfaceVerbatimInDock is acceptance criterion 4: a plane
// refusal reaches the dock verbatim — never a silent no-op — for both a
// policy create and an approval decision.
func TestRefusalsSurfaceVerbatimInDock(t *testing.T) {
	const planeMsg = "policy denied: rego compile failed at line 3: unexpected token"
	f := &fakePlane{
		policies:   []*apiv1.Policy{{Id: "pol-1", Name: "gate"}},
		versions:   []*apiv1.PolicyVersion{{PolicyId: "pol-1", Version: 1, Status: apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT, RegoModule: "package p\n"}},
		approvals:  []*apiv1.ApprovalItem{sampleApproval("sr-1")},
		createErr:  connect.NewError(connect.CodeInvalidArgument, errors.New(planeMsg)),
		approveErr: connect.NewError(connect.CodePermissionDenied, errors.New("policy requires a higher role to approve this step")),
	}
	m, sh := newHarness(t, f)

	// policy create refusal
	m.SelectSource("policies")
	loadAll(t, m)
	m.Update(typeKey("n"))
	m.ov.setValue("name", "bad")
	m.ov.setValue("decision_point", "admission")
	m.ov.setValue("effect", "deny")
	m.ov.setValue("rego_module", "package p\n!boom\n")
	_, cmd := m.Update(ctrlSKey())
	runCmd(t, m, cmd)
	if !strings.Contains(sh.err, planeMsg) {
		t.Fatalf("the plane's refusal must reach the dock verbatim, got %q", sh.err)
	}
	if m.inFlight != "" {
		t.Fatalf("the in-flight guard must clear on refusal (inFlight=%q)", m.inFlight)
	}
	if m.lastErr == "" {
		t.Fatal("the refusal must also be visible in the screen")
	}
	// Retryable: the operator can open the form again.
	m.Update(typeKey("n"))
	if m.ov == nil {
		t.Fatal("a refused write must leave the action usable again")
	}
	m.Update(escKey())

	// approval refusal
	f.createErr = nil
	m.SelectSource("approvals")
	loadAll(t, m)
	sh.err = ""
	m.Update(typeKey("a"))
	_, cmd = m.Update(typeKey("yes please"))
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if !strings.Contains(sh.err, "policy requires a higher role to approve this step") {
		t.Fatalf("the approval refusal must reach the dock verbatim, got %q", sh.err)
	}
	if len(f.approveReqs) != 1 {
		t.Fatalf("ApproveStep calls = %d, want 1", len(f.approveReqs))
	}
	// The refused step is STILL pending (no silent resolution).
	if _, ok := m.Base.SourceItem("approvals", "sr-1"); !ok {
		t.Fatal("a refused approval must keep the step pending")
	}
}

// TestRecoveryActionsConfirmGated is acceptance criterion 3: the recovery
// action surface is executable from the detail view, guarded by Confirm.
func TestRecoveryActionsConfirmGated(t *testing.T) {
	f := &fakePlane{
		recoveries: []*apiv1.RecoveryExecution{{
			Id: "rec-1", TaskId: "wi-1", ProjectId: "prj-1", Status: apiv1.RecoveryStatus_RECOVERY_STATUS_BLOCKED,
			Level: apiv1.RecoveryLevel_RECOVERY_LEVEL_L3, CurrentStep: "plan", TriggerReason: "budget",
			NeedsHumanApproval: true, Summary: "captured context", BudgetTokensUsed: 10, BudgetTokensLimit: 100,
		}},
		plan: &apiv1.ContinuationPlan{
			Id: "plan-1", RecoveryId: "rec-1", Version: 1, Status: apiv1.PlanStatus_PLAN_STATUS_PENDING,
			ContextSummary: "resume from the merge step", Remaining: `["run tests"]`,
		},
	}
	m, sh := newHarness(t, f)
	m.SelectSource("recoveries")
	loadAll(t, m)

	// The detail surfaces the available actions.
	_, fields, body, err := m.detail(context.Background(), "recoveries", "rec-1")
	if err != nil {
		t.Fatalf("recovery detail: %v", err)
	}
	acts := ""
	for _, fl := range fields {
		if fl.Key == "actions" {
			acts = fl.Value
		}
	}
	for _, want := range []string{"a approve plan", "x reject plan", "c cancel recovery", "m mark task succeeded"} {
		if !strings.Contains(acts, want) {
			t.Errorf("recovery actions = %q, want it to include %q", acts, want)
		}
	}
	if !strings.Contains(body, "continuation plan v1") || !strings.Contains(body, "resume from the merge step") {
		t.Errorf("the recovery detail must show the pending plan, got:\n%s", body)
	}

	// cancel — Confirm-gated: the key opens Confirm, only y writes.
	m.Update(typeKey("c"))
	if m.ov == nil || m.ov.kind != ovConfirm {
		t.Fatalf("c must open a Confirm overlay, got %+v", m.ov)
	}
	if len(f.cancelReqs) != 0 {
		t.Fatal("opening Confirm must not write")
	}
	m.Update(escKey())
	if len(f.cancelReqs) != 0 {
		t.Fatal("esc must cancel the guarded action")
	}
	m.Update(typeKey("c"))
	_, cmd := m.Update(typeKey("y"))
	runCmd(t, m, cmd)
	if len(f.cancelReqs) != 1 || f.cancelReqs[0].GetRecoveryId() != "rec-1" {
		t.Fatalf("CancelRecovery calls = %+v", f.cancelReqs)
	}

	// approve plan — guarded twice: the form gates, and the plan must be
	// re-confirmed. Approve writes only on submit.
	m.Update(typeKey("a"))
	if m.ov == nil || m.ov.action != "recovery-approve-plan" {
		t.Fatalf("a must open the plan-approve form, got %+v", m.ov)
	}
	if len(f.approvePlan) != 0 {
		t.Fatal("opening the form must not write")
	}
	m.ov.setValue("actor", "op@example.com")
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.approvePlan) != 1 || f.approvePlan[0].GetRecoveryId() != "rec-1" || f.approvePlan[0].GetActor() != "op@example.com" {
		t.Fatalf("ApproveContinuationPlan calls = %+v", f.approvePlan)
	}

	// reject plan — the reason is required.
	m.Update(typeKey("x"))
	if m.ov == nil || m.ov.action != "recovery-reject-plan" {
		t.Fatalf("x must open the plan-reject form, got %+v", m.ov)
	}
	m.Update(enterKey()) // actor → advance
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.rejectPlan) != 0 {
		t.Fatal("an empty rejection reason must not reach the plane")
	}
	if m.ov == nil || m.ov.err == "" || sh.err == "" {
		t.Fatal("the missing reason must be refused visibly (overlay + dock)")
	}
	m.ov.setValue("reason", "unsafe to resume")
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.rejectPlan) != 1 || f.rejectPlan[0].GetReason() != "unsafe to resume" {
		t.Fatalf("RejectContinuationPlan calls = %+v", f.rejectPlan)
	}

	// mark task succeeded — the human completion path.
	m.Update(typeKey("m"))
	if m.ov == nil || m.ov.action != "recovery-mark-succeeded" {
		t.Fatalf("m must open the mark-succeeded form, got %+v", m.ov)
	}
	m.ov.setValue("reason", "acceptance verified by hand")
	_, cmd = m.Update(enterKey())
	runCmd(t, m, cmd)
	if len(f.succeedReqs) != 1 {
		t.Fatalf("MarkTaskSucceeded calls = %+v", f.succeedReqs)
	}
	if got := f.succeedReqs[0]; got.GetTaskId() != "wi-1" || got.GetActorType() != "human" || got.GetReason() != "acceptance verified by hand" {
		t.Fatalf("MarkTaskSucceeded request = %+v", got)
	}
	if !strings.Contains(sh.notice, "marked task wi-1 succeeded") {
		t.Fatalf("dock notice = %q", sh.notice)
	}
}

// TestRecoveryUnavailableActionIsNeverSilent pins the "no silent no-op"
// rule for the *plane's* action surface: an action the plane does not allow
// (no pending plan / terminal status) is refused with the reason in the dock.
func TestRecoveryUnavailableActionIsNeverSilent(t *testing.T) {
	f := &fakePlane{recoveries: []*apiv1.RecoveryExecution{{
		Id: "rec-1", TaskId: "wi-1", Status: apiv1.RecoveryStatus_RECOVERY_STATUS_RESUMED, CurrentStep: "resume",
	}}}
	m, sh := newHarness(t, f)
	m.SelectSource("recoveries")
	loadAll(t, m)
	m.Update(typeKey("a"))
	if m.ov != nil {
		t.Fatal("no pending plan → the approve-plan action must not open")
	}
	if sh.err == "" || !strings.Contains(sh.err, "no pending continuation plan") {
		t.Fatalf("dock error = %q, want the reason surfaced", sh.err)
	}
	sh.err = ""
	m.Update(typeKey("c")) // resumed → not cancellable
	if sh.err == "" || !strings.Contains(sh.err, "no longer be cancelled") {
		t.Fatalf("dock error = %q, want the terminal-status refusal", sh.err)
	}
	if len(f.cancelReqs) != 0 {
		t.Fatal("a terminal recovery must not be cancelled")
	}
}

// TestPolicyEnumValidationRefusesLocally pins that a bad enum never reaches
// the plane and is surfaced (never a silent no-op).
func TestPolicyEnumValidationRefusesLocally(t *testing.T) {
	f := &fakePlane{}
	m, sh := newHarness(t, f)
	m.SelectSource("policies")
	loadAll(t, m)
	m.Update(typeKey("n"))
	m.ov.setValue("name", "x")
	m.ov.setValue("decision_point", "nonsense")
	m.ov.setValue("effect", "deny")
	m.ov.setValue("rego_module", "package p\n")
	_, cmd := m.Update(ctrlSKey())
	runCmd(t, m, cmd)
	if len(f.createReqs) != 0 {
		t.Fatal("an unparseable decision point must not be sent")
	}
	if sh.err == "" || !strings.Contains(sh.err, "unknown decision point") {
		t.Fatalf("dock error = %q, want the validation refusal", sh.err)
	}
}

// TestFormTypingSurvivesGlobalChords pins ClaimsKeys: while a form is open the
// screen claims every key, so a Rego body containing the global chords
// (q / d / y / ?) is typed verbatim.
func TestFormTypingSurvivesGlobalChords(t *testing.T) {
	f := &fakePlane{}
	m, _ := newHarness(t, f)
	m.SelectSource("policies")
	loadAll(t, m)
	if m.ClaimsKeys() {
		t.Fatal("no overlay → no claim")
	}
	m.Update(typeKey("n"))
	if !m.ClaimsKeys() {
		t.Fatal("an open form must claim the keyboard")
	}
	const body = "package p\n# q d y ? \nallow { input.draft == false }\n"
	m.ov.setValue("rego_module", body)
	if got := m.ov.value("rego_module"); got != body {
		t.Fatalf("the multiline body must round-trip untouched, got %q", got)
	}
	// Typing into the multiline field inserts runes verbatim (no truncation).
	m.ov.active = len(m.ov.fields) - 1
	m.ov.setValue("rego_module", "")
	m.Update(typeKey("d"))
	m.Update(typeKey("q"))
	if got := m.ov.value("rego_module"); got != "dq" {
		t.Fatalf("global-chord letters must land in the field, got %q", got)
	}
	// Enter inserts a newline in a multiline field; ctrl+s submits.
	m.Update(enterKey())
	if !strings.Contains(m.ov.value("rego_module"), "\n") {
		t.Fatal("enter must insert a newline in a multiline field")
	}
}

// TestViewRendersOverlaysAndHints keeps the rendering paths honest (the frame
// must never drop the overlay or its hints).
func TestViewRendersOverlaysAndHints(t *testing.T) {
	f := &fakePlane{
		approvals: []*apiv1.ApprovalItem{sampleApproval("sr-1")},
		recoveries: []*apiv1.RecoveryExecution{{
			Id: "rec-1", TaskId: "wi-1", Status: apiv1.RecoveryStatus_RECOVERY_STATUS_RUNNING, CurrentStep: "review",
		}},
	}
	m, _ := newHarness(t, f)

	m.SelectSource("policies")
	loadAll(t, m)
	if out := m.View(); !strings.Contains(out, "n: new policy") {
		t.Errorf("the policies hint must advertise the write chords:\n%s", out)
	}
	m.Update(typeKey("n"))
	if out := m.View(); !strings.Contains(out, "rego module") {
		t.Errorf("the open create form must render its fields:\n%s", out)
	}
	m.Update(escKey())

	m.SelectSource("approvals")
	loadAll(t, m)
	m.Update(typeKey("x"))
	if out := m.View(); !strings.Contains(out, "reject") {
		t.Errorf("the reject form must render:\n%s", out)
	}
	m.Update(escKey())

	m.SelectSource("recoveries")
	loadAll(t, m)
	m.Update(typeKey("c"))
	if out := m.View(); !strings.Contains(out, "y / enter: confirm") {
		t.Errorf("the Confirm overlay must render its key contract:\n%s", out)
	}
	m.Update(escKey())

	// A long Rego body must render whole in the viewer (never truncated by
	// the overlay itself).
	long := strings.Repeat("allow { input.x == ", 3) + "1 }\n"
	m.SelectSource("policies")
	loadAll(t, m)
	m.ov = newViewer("t", "h", long)
	if out := m.View(); !strings.Contains(out, long[:20]) {
		t.Errorf("the viewer must render the body verbatim:\n%s", out)
	}
}
