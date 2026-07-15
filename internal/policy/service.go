// Package policy implements the PolicyService Connect handler
// (docs/07_API_Specification.md §3.5) and the Rego-based Policy Engine.
//
// The service is the API-layer boundary: validate + sanitize inputs,
// resolve the tenant, perform the mutation + outbox enqueue in one
// transaction, enforce the Policy lifecycle (draft → published →
// superseded) and versioning model (docs/02 §2.5). The Engine performs
// Rego evaluation.
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxNameLen         = 500
	maxVersionNoteLen  = 1 << 14
	maxRegoModuleLen   = 1 << 20 // 1 MiB
	maxQueryLen        = 1000
	maxInputLen        = 1 << 20
	maxReasonLen       = 1000
)

// Service implements the PolicyService Connect handler.
type Service struct {
	pool       *db.Pool
	log        *slog.Logger
	engine     *Engine
	subscriber eventbus.Subscriber
	apiv1connect.UnimplementedPolicyServiceHandler
}

var _ apiv1connect.PolicyServiceHandler = (*Service)(nil)

// NewService constructs a PolicyService handler.
func NewService(pool *db.Pool, log *slog.Logger, engine *Engine, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, log: log, engine: engine, subscriber: sub}
}

// CreatePolicy creates a new Policy in draft state with its first draft
// version (docs/02 §2.5).
func (s *Service) CreatePolicy(ctx context.Context, req *connect.Request[apiv1.CreatePolicyRequest]) (*connect.Response[apiv1.CreatePolicyResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name, err := validateName(msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	decisionPoint := decisionPointToDomain(msg.DecisionPoint)
	if decisionPoint == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("decision_point must be specified"))
	}
	scope := scopeToDomain(msg.Scope)
	if scope == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("scope must be specified"))
	}
	effect := effectToDomain(msg.Effect)
	if effect == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("effect must be specified"))
	}
	regoModule, err := validateTextField(msg.RegoModule, maxRegoModuleLen, "rego_module")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	query, err := validateTextField(msg.Query, maxQueryLen, "query")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	scopeRef, err := validateTextField(msg.ScopeRef, maxNameLen, "scope_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	versionNote, err := validateTextField(msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	policyID := db.NewID()
	versionID := db.NewID()

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	policyRow := db.PolicyRow{ID: policyID, TenantID: tenantID, Name: name, Status: domain.PolicyDraft}
	created, err := db.CreatePolicy(ctx, ttx.Tx, policyRow)
	if err != nil {
		return nil, mapDBError(err)
	}
	versionRow := db.PolicyVersionRow{
		ID:            versionID,
		TenantID:      tenantID,
		PolicyID:      policyID,
		Version:       1,
		VersionNote:   versionNote,
		Status:        domain.PolicyVersionDraft,
		DecisionPoint: decisionPoint,
		Scope:         scope,
		ScopeRef:      scopeRef,
		Effect:        effect,
		RegoModule:     regoModule,
		Query:         query,
	}
	createdVersion, err := db.CreatePolicyVersion(ctx, ttx.Tx, versionRow)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueuePolicyLifecycleEvent(ctx, ttx.Tx, domain.PolicyEventPublished, created, createdVersion); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("policy created", "id", created.ID, "tenant", tenantID, "name", name)
	return connect.NewResponse(&apiv1.CreatePolicyResponse{
		Policy: policyRowToProto(created),
		Version: versionRowToProto(createdVersion),
	}), nil
}

// PublishPolicy publishes the draft version, compiling the Rego module
// (docs/02 §2.5).
func (s *Service) PublishPolicy(ctx context.Context, req *connect.Request[apiv1.PublishPolicyRequest]) (*connect.Response[apiv1.PublishPolicyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.PolicyId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetPolicy(ctx, ttx.Tx, tenantID, req.Msg.PolicyId)
	if err != nil {
		return nil, mapDBError(err)
	}
	latest, err := db.GetLatestPolicyVersion(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, false)
	if err != nil {
		return nil, mapDBError(err)
	}
	if latest.Status != domain.PolicyVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("latest version (v%d) is not draft", latest.Version))
	}
	// Compile the Rego module before publishing (reject malformed Rego).
	if err := CompileModule(ctx, req.Msg.PolicyId, latest.RegoModule, latest.Query); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("rego compile error: %w", err))
	}
	published, err := db.PublishPolicyVersion(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdatePolicyCurrentVersion(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, current.Version, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueuePolicyLifecycleEvent(ctx, ttx.Tx, domain.PolicyEventPublished, updated, published); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("policy version published", "id", updated.ID, "version", published.Version)
	return connect.NewResponse(&apiv1.PublishPolicyResponse{
		Policy: policyRowToProto(updated),
		Version: versionRowToProto(published),
	}), nil
}

// SupersedePolicy transitions a published Policy to superseded.
func (s *Service) SupersedePolicy(ctx context.Context, req *connect.Request[apiv1.SupersedePolicyRequest]) (*connect.Response[apiv1.SupersedePolicyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.PolicyId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetPolicy(ctx, ttx.Tx, tenantID, req.Msg.PolicyId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.PolicyPublished {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("policy must be published to supersede (status=%s)", current.Status))
	}
	updated, err := db.UpdatePolicyStatus(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, current.Version, domain.PolicySuperseded)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueuePolicyLifecycleEvent(ctx, ttx.Tx, domain.PolicyEventSuperseded, updated, db.PolicyVersionRow{}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.SupersedePolicyResponse{Policy: policyRowToProto(updated)}), nil
}

// GetPolicy returns a single policy header + latest published version.
func (s *Service) GetPolicy(ctx context.Context, req *connect.Request[apiv1.GetPolicyRequest]) (*connect.Response[apiv1.GetPolicyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	p, err := db.GetPolicy(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.GetPolicyResponse{Policy: policyRowToProto(p)}
	if p.CurrentVersion > 0 {
		if v, err := db.GetLatestPolicyVersion(ctx, ttx.Tx, tenantID, req.Msg.Id, true); err == nil {
			resp.LatestVersion = versionRowToProto(v)
		}
	}
	return connect.NewResponse(resp), nil
}

// ListPolicies returns a page of policies.
func (s *Service) ListPolicies(ctx context.Context, req *connect.Request[apiv1.ListPoliciesRequest]) (*connect.Response[apiv1.ListPoliciesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListPoliciesFilter{TenantID: tenantID, PageSize: int(req.Msg.PageSize), AfterID: req.Msg.PageToken}
	if req.Msg.Status != nil {
		f.Status = policyStatusFromProto(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	policies, err := db.ListPolicies(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListPoliciesResponse{}
	for _, p := range policies {
		resp.Policies = append(resp.Policies, policyRowToProto(p))
	}
	if len(policies) > 0 {
		resp.NextPageToken = policies[len(policies)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ListPolicyVersions returns all versions of a policy, newest first.
func (s *Service) ListPolicyVersions(ctx context.Context, req *connect.Request[apiv1.ListPolicyVersionsRequest]) (*connect.Response[apiv1.ListPolicyVersionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.PolicyId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	versions, err := db.ListPolicyVersions(ctx, ttx.Tx, tenantID, req.Msg.PolicyId)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListPolicyVersionsResponse{}
	for _, v := range versions {
		resp.Versions = append(resp.Versions, versionRowToProto(v))
	}
	return connect.NewResponse(resp), nil
}

// UpdatePolicyVersion saves edits to a draft version's Rego module.
func (s *Service) UpdatePolicyVersion(ctx context.Context, req *connect.Request[apiv1.UpdatePolicyVersionRequest]) (*connect.Response[apiv1.UpdatePolicyVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.PolicyId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy_id must not be empty"))
	}
	regoModule, err := validateTextField(req.Msg.RegoModule, maxRegoModuleLen, "rego_module")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	query, err := validateTextField(req.Msg.Query, maxQueryLen, "query")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	scopeRef, err := validateTextField(req.Msg.ScopeRef, maxNameLen, "scope_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	versionNote, err := validateTextField(req.Msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	dp := decisionPointToDomain(req.Msg.DecisionPoint)
	sc := scopeToDomain(req.Msg.Scope)
	eff := effectToDomain(req.Msg.Effect)

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	latest, err := db.GetLatestPolicyVersion(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, false)
	if err != nil {
		return nil, mapDBError(err)
	}
	if latest.Status != domain.PolicyVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("latest version (v%d) is not draft", latest.Version))
	}
	updated, err := db.UpdatePolicyVersion(ctx, ttx.Tx, tenantID, req.Msg.PolicyId, latest.Version, db.UpdatePolicyVersionFields{
		DecisionPoint: &dp, Scope: &sc, ScopeRef: &scopeRef, Effect: &eff,
		RegoModule: &regoModule, Query: &query, VersionNote: &versionNote,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.UpdatePolicyVersionResponse{Version: versionRowToProto(updated)}), nil
}

// EvaluatePolicy runs a dry-run evaluation (docs/07 §3.5).
func (s *Service) EvaluatePolicy(ctx context.Context, req *connect.Request[apiv1.EvaluatePolicyRequest]) (*connect.Response[apiv1.EvaluatePolicyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	dp := decisionPointToDomain(req.Msg.DecisionPoint)
	if dp == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("decision_point must be specified"))
	}
	inputStr, err := validateTextField(req.Msg.Input, maxInputLen, "input")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	var input any
	if inputStr != "" {
		if err := json.Unmarshal([]byte(inputStr), &input); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("input must be valid JSON: %w", err))
		}
	}
	var dec Decision
	if req.Msg.PolicyId != "" {
		dec, err = s.engine.EvaluateVersion(ctx, tenantID, req.Msg.PolicyId, int(req.Msg.PolicyVersion), input)
	} else {
		dec, err = s.engine.Evaluate(ctx, tenantID, dp, req.Msg.TargetType, req.Msg.TargetId, input)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.EvaluatePolicyResponse{
		Effect:       effectToProto(dec.Effect),
		PolicyId:     dec.PolicyID,
		PolicyVersion: int32(dec.PolicyVersion),
		Result:       string(dec.Result),
		Trace:        string(dec.Trace),
		Error:        dec.Err,
	}), nil
}

// ExplainDecision returns the Rego trace for a past decision
// (docs/07 §3.5).
func (s *Service) ExplainDecision(ctx context.Context, req *connect.Request[apiv1.ExplainDecisionRequest]) (*connect.Response[apiv1.ExplainDecisionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	var d db.PolicyDecisionRow
	if req.Msg.DecisionId != "" {
		d, err = db.GetPolicyDecision(ctx, ttx.Tx, tenantID, req.Msg.DecisionId)
	} else if req.Msg.TraceId != "" {
		d, err = db.GetPolicyDecisionByTrace(ctx, ttx.Tx, tenantID, req.Msg.TraceId)
	} else {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("decision_id or trace_id required"))
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.ExplainDecisionResponse{Decision: decisionRowToProto(d)}), nil
}

// ListDecisions returns a page of recorded decisions (the decision log).
func (s *Service) ListDecisions(ctx context.Context, req *connect.Request[apiv1.ListDecisionsRequest]) (*connect.Response[apiv1.ListDecisionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListPolicyDecisionsFilter{
		TenantID: tenantID, TargetType: req.Msg.TargetType, TargetID: req.Msg.TargetId,
		PolicyID: req.Msg.PolicyId, PageSize: int(req.Msg.PageSize), AfterID: req.Msg.PageToken,
	}
	if req.Msg.DecisionPoint != nil {
		f.DecisionPoint = decisionPointToDomain(*req.Msg.DecisionPoint)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	decisions, err := db.ListPolicyDecisions(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListDecisionsResponse{}
	for _, d := range decisions {
		resp.Decisions = append(resp.Decisions, decisionRowToProto(d))
	}
	if len(decisions) > 0 {
		resp.NextPageToken = decisions[len(decisions)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// GetDecision returns a single decision by id.
func (s *Service) GetDecision(ctx context.Context, req *connect.Request[apiv1.GetDecisionRequest]) (*connect.Response[apiv1.GetDecisionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	d, err := db.GetPolicyDecision(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetDecisionResponse{Decision: decisionRowToProto(d)}), nil
}

// --- helpers + mappers -----------------------------------------------------

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	return name, nil
}

func validateTextField(s string, max int, field string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fmt.Errorf("%s must be at most %d characters", field, max)
	}
	return s, nil
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("policy not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

func enqueuePolicyLifecycleEvent(ctx context.Context, tx pgx.Tx, eventType string, p db.PolicyRow, v db.PolicyVersionRow) error {
	evt := map[string]any{
		"event_type": eventType, "tenant_id": p.TenantID, "policy_id": p.ID,
		"status": p.Status, "occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if v.ID != "" {
		evt["version"] = v.Version
	}
	payload, _ := json.Marshal(evt)
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID: p.TenantID, EventType: eventType, AggregateType: "policy",
		AggregateID: p.ID, Payload: payload, OccurredAt: time.Now().UTC(),
	})
}

func decisionPointToDomain(dp apiv1.DecisionPoint) string {
	switch dp {
	case apiv1.DecisionPoint_DECISION_POINT_ADMISSION:
		return domain.DecisionPointAdmission
	case apiv1.DecisionPoint_DECISION_POINT_DISPATCH:
		return domain.DecisionPointDispatch
	case apiv1.DecisionPoint_DECISION_POINT_BUDGET:
		return domain.DecisionPointBudget
	case apiv1.DecisionPoint_DECISION_POINT_APPROVAL:
		return domain.DecisionPointApproval
	case apiv1.DecisionPoint_DECISION_POINT_RECOVERY:
		return domain.DecisionPointRecovery
	case apiv1.DecisionPoint_DECISION_POINT_COMPLETION:
		return domain.DecisionPointCompletion
	}
	return ""
}

func scopeToDomain(s apiv1.PolicyScope) string {
	switch s {
	case apiv1.PolicyScope_POLICY_SCOPE_TENANT:
		return domain.PolicyScopeTenant
	case apiv1.PolicyScope_POLICY_SCOPE_PROJECT:
		return domain.PolicyScopeProject
	case apiv1.PolicyScope_POLICY_SCOPE_WORKER:
		return domain.PolicyScopeWorker
	case apiv1.PolicyScope_POLICY_SCOPE_TASK:
		return domain.PolicyScopeTask
	}
	return ""
}

func effectToDomain(e apiv1.PolicyEffect) string {
	switch e {
	case apiv1.PolicyEffect_POLICY_EFFECT_ALLOW:
		return domain.PolicyEffectAllow
	case apiv1.PolicyEffect_POLICY_EFFECT_DENY:
		return domain.PolicyEffectDeny
	case apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_APPROVAL:
		return domain.PolicyEffectRequireApproval
	case apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_REVIEW:
		return domain.PolicyEffectRequireReview
	}
	return ""
}

func policyStatusToProto(s string) apiv1.PolicyStatus {
	switch s {
	case domain.PolicyDraft:
		return apiv1.PolicyStatus_POLICY_STATUS_DRAFT
	case domain.PolicyPublished:
		return apiv1.PolicyStatus_POLICY_STATUS_PUBLISHED
	case domain.PolicySuperseded:
		return apiv1.PolicyStatus_POLICY_STATUS_SUPERSEDED
	}
	return apiv1.PolicyStatus_POLICY_STATUS_UNSPECIFIED
}

func policyStatusFromProto(s apiv1.PolicyStatus) string {
	switch s {
	case apiv1.PolicyStatus_POLICY_STATUS_DRAFT:
		return domain.PolicyDraft
	case apiv1.PolicyStatus_POLICY_STATUS_PUBLISHED:
		return domain.PolicyPublished
	case apiv1.PolicyStatus_POLICY_STATUS_SUPERSEDED:
		return domain.PolicySuperseded
	}
	return ""
}

func policyVersionStatusToProto(s string) apiv1.PolicyVersionStatus {
	switch s {
	case domain.PolicyVersionDraft:
		return apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT
	case domain.PolicyVersionPublished:
		return apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED
	case domain.PolicyVersionSuperseded:
		return apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_SUPERSEDED
	}
	return apiv1.PolicyVersionStatus_POLICY_VERSION_STATUS_UNSPECIFIED
}

func decisionPointToProto(s string) apiv1.DecisionPoint {
	switch s {
	case domain.DecisionPointAdmission:
		return apiv1.DecisionPoint_DECISION_POINT_ADMISSION
	case domain.DecisionPointDispatch:
		return apiv1.DecisionPoint_DECISION_POINT_DISPATCH
	case domain.DecisionPointBudget:
		return apiv1.DecisionPoint_DECISION_POINT_BUDGET
	case domain.DecisionPointApproval:
		return apiv1.DecisionPoint_DECISION_POINT_APPROVAL
	case domain.DecisionPointRecovery:
		return apiv1.DecisionPoint_DECISION_POINT_RECOVERY
	case domain.DecisionPointCompletion:
		return apiv1.DecisionPoint_DECISION_POINT_COMPLETION
	}
	return apiv1.DecisionPoint_DECISION_POINT_UNSPECIFIED
}

func scopeToProto(s string) apiv1.PolicyScope {
	switch s {
	case domain.PolicyScopeTenant:
		return apiv1.PolicyScope_POLICY_SCOPE_TENANT
	case domain.PolicyScopeProject:
		return apiv1.PolicyScope_POLICY_SCOPE_PROJECT
	case domain.PolicyScopeWorker:
		return apiv1.PolicyScope_POLICY_SCOPE_WORKER
	case domain.PolicyScopeTask:
		return apiv1.PolicyScope_POLICY_SCOPE_TASK
	}
	return apiv1.PolicyScope_POLICY_SCOPE_UNSPECIFIED
}

func effectToProto(e string) apiv1.PolicyEffect {
	switch e {
	case domain.PolicyEffectAllow:
		return apiv1.PolicyEffect_POLICY_EFFECT_ALLOW
	case domain.PolicyEffectDeny:
		return apiv1.PolicyEffect_POLICY_EFFECT_DENY
	case domain.PolicyEffectRequireApproval:
		return apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_APPROVAL
	case domain.PolicyEffectRequireReview:
		return apiv1.PolicyEffect_POLICY_EFFECT_REQUIRE_REVIEW
	}
	return apiv1.PolicyEffect_POLICY_EFFECT_UNSPECIFIED
}

func policyRowToProto(p db.PolicyRow) *apiv1.Policy {
	return &apiv1.Policy{
		Id: p.ID, TenantId: p.TenantID, Name: p.Name,
		Status: policyStatusToProto(p.Status), CurrentVersion: int32(p.CurrentVersion),
		Version: int32(p.Version), CreatedAt: timestamppb.New(p.CreatedAt),
		UpdatedAt: timestamppb.New(p.UpdatedAt),
	}
}

func versionRowToProto(v db.PolicyVersionRow) *apiv1.PolicyVersion {
	pv := &apiv1.PolicyVersion{
		Id: v.ID, TenantId: v.TenantID, PolicyId: v.PolicyID, Version: int32(v.Version),
		VersionNote: v.VersionNote, Status: policyVersionStatusToProto(v.Status),
		DecisionPoint: decisionPointToProto(v.DecisionPoint), Scope: scopeToProto(v.Scope),
		ScopeRef: v.ScopeRef, Effect: effectToProto(v.Effect),
		RegoModule: v.RegoModule, Query: v.Query, CreatedAt: timestamppb.New(v.CreatedAt),
	}
	if v.PublishedAt != nil {
		pv.PublishedAt = timestamppb.New(*v.PublishedAt)
	}
	return pv
}

func decisionRowToProto(d db.PolicyDecisionRow) *apiv1.PolicyDecision {
	return &apiv1.PolicyDecision{
		Id: d.ID, TenantId: d.TenantID, PolicyId: d.PolicyID, PolicyVersion: int32(d.PolicyVersion),
		DecisionPoint: d.DecisionPoint, Effect: effectToProto(d.Effect), Scope: d.Scope,
		ScopeRef: d.ScopeRef, TargetType: d.TargetType, TargetId: d.TargetID,
		ActorType: d.ActorType, ActorId: d.ActorID, Input: string(d.Input),
		Result: string(d.Result), Trace: string(d.Trace), TraceId: d.TraceID,
		Error: d.Error, OccurredAt: timestamppb.New(d.OccurredAt),
	}
}
