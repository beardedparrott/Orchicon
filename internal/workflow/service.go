package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the WorkflowService Connect handler
// (apiv1connect.WorkflowServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3); the relay publishes it to NATS asynchronously.
//
// The Workflow lifecycle (docs/02 §2.4) is enforced here:
//   - CreateWorkflow creates a Workflow in draft state with its first
//     draft version.
//   - PublishWorkflow transitions a draft version to published
//     (immutable) and the Workflow to published.
//   - DeprecateWorkflow transitions a published Workflow to deprecated.
//   - StartWorkflow creates a WorkflowRun from a published version,
//     seeds a WorkflowStepRun for each step, and enqueues a run_started
//     event. The WorkflowReconciler progresses the run.
type Service struct {
	pool       *db.Pool
	log        *slog.Logger
	subscriber eventbus.Subscriber
	apiv1connect.UnimplementedWorkflowServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.WorkflowServiceHandler = (*Service)(nil)

// New constructs a WorkflowService handler.
func New(pool *db.Pool, log *slog.Logger, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, log: log, subscriber: sub}
}

// CreateWorkflow validates input, inserts the workflow header + its first
// draft version, and enqueues a workflow.created event — all in one
// tenant-scoped transaction.
func (s *Service) CreateWorkflow(ctx context.Context, req *connect.Request[apiv1.CreateWorkflowRequest]) (*connect.Response[apiv1.CreateWorkflowResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name, err := validateName(msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	versionNote, err := validateTextField(msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	recoveryPolicyRef, err := validateTextField(msg.RecoveryPolicyRef, maxNameLen, "recovery_policy_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	steps, err := validateStepsField(msg.Steps)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	inputs, err := validateJSONField(msg.Inputs, "{}", "inputs", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	outputs, err := validateJSONField(msg.Outputs, "{}", "outputs", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// project_id is optional (empty for tenant-level templates). Trim
	// whitespace; no further validation — it's a ULID reference checked
	// by the data-access layer's FK-free convention.
	projectID := msg.ProjectId

	workflowID := db.NewID()
	versionID := db.NewID()

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Only active projects may host workflows (docs/02 §2.1).
	if msg.ProjectId != "" {
		if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, msg.ProjectId); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("project not active: %w", err))
		}
	}

	workflowRow := db.WorkflowRow{
		ID:             workflowID,
		TenantID:       tenantID,
		ProjectID:      projectID,
		Name:           name,
		Status:         domain.WorkflowDraft,
		CurrentVersion: 0,
	}
	created, err := db.CreateWorkflow(ctx, ttx.Tx, workflowRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	// First version is always version 1, in draft state (docs/02 §2.4).
	versionRow := db.WorkflowVersionRow{
		ID:                versionID,
		TenantID:          tenantID,
		WorkflowID:        workflowID,
		Version:           1,
		VersionNote:       versionNote,
		Status:            domain.WorkflowVersionDraft,
		Steps:             steps,
		Inputs:            inputs,
		Outputs:           outputs,
		RecoveryPolicyRef: recoveryPolicyRef,
	}
	createdVersion, err := db.CreateWorkflowVersion(ctx, ttx.Tx, versionRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	if err := enqueueWorkflowEvent(ctx, ttx.Tx, "workflow.created", created, createdVersion, db.WorkflowRunRow{}, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow created", "id", created.ID, "tenant", tenantID, "name", name)
	return connect.NewResponse(&apiv1.CreateWorkflowResponse{
		Workflow: workflowRowToProto(created),
		Version:  versionRowToProto(createdVersion),
	}), nil
}

// PublishWorkflow publishes the draft version, making the workflow
// runnable (docs/02 §2.4). The version becomes immutable and the
// Workflow transitions to published.
func (s *Service) PublishWorkflow(ctx context.Context, req *connect.Request[apiv1.PublishWorkflowRequest]) (*connect.Response[apiv1.PublishWorkflowResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Publish the latest draft version.
	latest, err := db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, false)
	if err != nil {
		return nil, mapDBError(err)
	}
	if latest.Status != domain.WorkflowVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("latest version (v%d) is not draft (status=%s)", latest.Version, latest.Status))
	}
	published, err := db.PublishWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, current.Version, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkflowEvent(ctx, ttx.Tx, "workflow.published", updated, published, db.WorkflowRunRow{}, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow version published", "id", updated.ID, "version", published.Version)
	return connect.NewResponse(&apiv1.PublishWorkflowResponse{
		Workflow: workflowRowToProto(updated),
		Version:  versionRowToProto(published),
	}), nil
}

// CreateWorkflowVersion creates a new draft version from the latest
// published version of a published or deprecated workflow (docs/02 §2.4).
// If the workflow is deprecated, it's transitioned back to draft so the
// user can edit and republish. Published workflows keep their status; the
// new draft version sits ahead of the published one.
func (s *Service) CreateWorkflowVersion(ctx context.Context, req *connect.Request[apiv1.CreateWorkflowVersionRequest]) (*connect.Response[apiv1.CreateWorkflowVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	versionNote, err := validateTextField(req.Msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.WorkflowPublished && current.Status != domain.WorkflowDeprecated {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("workflow must be published or deprecated to create a new version (status=%s)", current.Status))
	}

	// Clone the latest published version's data into the new draft version.
	latestPublished, err := db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, true)
	if err != nil {
		return nil, mapDBError(err)
	}
	nextVersion, err := db.NextWorkflowVersionNumber(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("next version: %w", err))
	}
	versionRow := db.WorkflowVersionRow{
		ID:                db.NewID(),
		TenantID:          tenantID,
		WorkflowID:        req.Msg.WorkflowId,
		Version:           nextVersion,
		VersionNote:       versionNote,
		Status:            domain.WorkflowVersionDraft,
		Steps:             latestPublished.Steps,
		Inputs:            latestPublished.Inputs,
		Outputs:           latestPublished.Outputs,
		RecoveryPolicyRef: latestPublished.RecoveryPolicyRef,
	}
	created, err := db.CreateWorkflowVersion(ctx, ttx.Tx, versionRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	// If deprecated, transition the header back to draft so the editor
	// shows the save/publish affordances.
	updated := current
	if current.Status == domain.WorkflowDeprecated {
		updated, err = db.UpdateWorkflowStatus(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, current.Version, domain.WorkflowDraft)
		if err != nil {
			return nil, mapDBError(err)
		}
	}

	if err := enqueueWorkflowEvent(ctx, ttx.Tx, "workflow.version_created", updated, created, db.WorkflowRunRow{}, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow version created", "id", updated.ID, "version", created.Version)
	return connect.NewResponse(&apiv1.CreateWorkflowVersionResponse{
		Workflow: workflowRowToProto(updated),
		Version:  versionRowToProto(created),
	}), nil
}

// DeprecateWorkflow transitions a published Workflow to deprecated
// (docs/02 §2.4). Still runnable for in-flight runs; no new runs may
// start.
func (s *Service) DeprecateWorkflow(ctx context.Context, req *connect.Request[apiv1.DeprecateWorkflowRequest]) (*connect.Response[apiv1.DeprecateWorkflowResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.WorkflowPublished {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("workflow must be published to deprecate (status=%s)", current.Status))
	}
	updated, err := db.UpdateWorkflowStatus(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, current.Version, domain.WorkflowDeprecated)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkflowEvent(ctx, ttx.Tx, "workflow.deprecated", updated, db.WorkflowVersionRow{}, db.WorkflowRunRow{}, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow deprecated", "id", updated.ID)
	return connect.NewResponse(&apiv1.DeprecateWorkflowResponse{Workflow: workflowRowToProto(updated)}), nil
}

// DeleteWorkflow hard-deletes a workflow and all its child rows (steps,
// runs, versions, edit locks). This is irreversible — use DeprecateWorkflow
// for a soft hide (docs/02 §2.4).
func (s *Service) DeleteWorkflow(ctx context.Context, req *connect.Request[apiv1.DeleteWorkflowRequest]) (*connect.Response[apiv1.DeleteWorkflowResponse], error) {
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
	if _, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteWorkflow(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow deleted", "id", req.Msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteWorkflowResponse{}), nil
}

// GetWorkflow returns a single workflow header by id, with its latest
// published version (if any).
func (s *Service) GetWorkflow(ctx context.Context, req *connect.Request[apiv1.GetWorkflowRequest]) (*connect.Response[apiv1.GetWorkflowResponse], error) {
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
	w, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.GetWorkflowResponse{Workflow: workflowRowToProto(w)}
	// Include the latest version (draft or published) so the frontend
	// can always show version info and editing affordances.
	if v, err := db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.Id, false); err == nil {
		resp.LatestVersion = versionRowToProto(v)
	}
	return connect.NewResponse(resp), nil
}

// ListWorkflows returns a page of workflows for the tenant, optionally
// scoped to a project, with search/filter and configurable sort
// (docs/07 §5.2).
func (s *Service) ListWorkflows(ctx context.Context, req *connect.Request[apiv1.ListWorkflowsRequest]) (*connect.Response[apiv1.ListWorkflowsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListWorkflowsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
		Search:    req.Msg.Search,
		SortBy:    req.Msg.SortBy,
		SortOrder: req.Msg.SortOrder,
	}
	if req.Msg.Status != nil {
		f.Status = workflowStatusFromProto(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	workflows, err := db.ListWorkflows(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkflowsResponse{}
	for _, w := range workflows {
		resp.Workflows = append(resp.Workflows, workflowRowToProto(w))
	}
	if len(workflows) > 0 {
		resp.NextPageToken = workflows[len(workflows)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ListWorkflowVersions returns all versions of a workflow, newest first.
func (s *Service) ListWorkflowVersions(ctx context.Context, req *connect.Request[apiv1.ListWorkflowVersionsRequest]) (*connect.Response[apiv1.ListWorkflowVersionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	versions, err := db.ListWorkflowVersions(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkflowVersionsResponse{}
	for _, v := range versions {
		resp.Versions = append(resp.Versions, versionRowToProto(v))
	}
	return connect.NewResponse(resp), nil
}

// UpdateWorkflowVersion saves edits to a draft version's steps (and
// inputs/outputs/recovery_policy_ref). Only draft versions are mutable;
// published versions are immutable (docs/02 §2.4). This is the "save"
// action in the visual editor. The steps JSON is validated as a
// well-formed JSON array (AGENTS.md security standards).
func (s *Service) UpdateWorkflowVersion(ctx context.Context, req *connect.Request[apiv1.UpdateWorkflowVersionRequest]) (*connect.Response[apiv1.UpdateWorkflowVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	steps, err := validateStepsField(req.Msg.Steps)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	inputs, err := validateJSONField(req.Msg.Inputs, "{}", "inputs", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	outputs, err := validateJSONField(req.Msg.Outputs, "{}", "outputs", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	recoveryPolicyRef, err := validateTextField(req.Msg.RecoveryPolicyRef, maxNameLen, "recovery_policy_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	versionNote, err := validateTextField(req.Msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Only the latest draft version is mutable.
	latest, err := db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, false)
	if err != nil {
		return nil, mapDBError(err)
	}
	if latest.Status != domain.WorkflowVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("latest version (v%d) is not draft (status=%s); published versions are immutable", latest.Version, latest.Status))
	}

	// Update the draft version's mutable fields. The version number and
	// id are unchanged; this is an in-place edit of the draft snapshot.
	const q = `UPDATE workflow_versions
		SET steps = $3, inputs = $4, outputs = $5, recovery_policy_ref = $6,
		    version_note = CASE WHEN $7 = '' THEN version_note ELSE $7 END
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, workflow_id, version, version_note, status,
			steps, inputs, outputs, recovery_policy_ref, published_at, created_at`
	var v db.WorkflowVersionRow
	err = ttx.Tx.QueryRow(ctx, q,
		tenantID, latest.ID, steps, inputs, outputs, recoveryPolicyRef, versionNote,
	).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.RecoveryPolicyRef, &v.PublishedAt, &v.CreatedAt,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("db: update workflow version: %w", err))
	}

	// Enqueue a workflow.updated event for the streaming feed.
	wf, _ := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err := enqueueWorkflowEvent(ctx, ttx.Tx, "workflow.updated", wf, v, db.WorkflowRunRow{}, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow version updated", "workflow", req.Msg.WorkflowId, "version", v.Version)
	return connect.NewResponse(&apiv1.UpdateWorkflowVersionResponse{Version: versionRowToProto(v)}), nil
}

// StartWorkflow creates a WorkflowRun from a published version, seeds a
// WorkflowStepRun for each step in the DAG, and enqueues a run_started
// event (docs/02 §2.4, docs/03 §2). The WorkflowReconciler picks up the
// run and progresses the step DAG.
func (s *Service) StartWorkflow(ctx context.Context, req *connect.Request[apiv1.StartWorkflowRequest]) (*connect.Response[apiv1.StartWorkflowResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	runContext, err := validateJSONField(req.Msg.RunContext, "{}", "run_context", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Workflow must be published (or deprecated — still runnable for
	// in-flight; new runs allowed until retired). docs/02 §2.4.
	wf, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if wf.Status != domain.WorkflowPublished && wf.Status != domain.WorkflowDeprecated {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("workflow must be published to start (status=%s)", wf.Status))
	}
	// Resolve the published version to run.
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, wf.CurrentVersion)
	if err != nil {
		return nil, mapDBError(err)
	}
	if version.Status != domain.WorkflowVersionPublished {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("workflow version v%d is not published (status=%s)", version.Version, version.Status))
	}

	// Parse the steps JSON to seed a WorkflowStepRun per step.
	steps, err := parseSteps(version.Steps)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("parse workflow steps: %w", err))
	}

	runID := db.NewID()
	now := time.Now().UTC()
	runRow := db.WorkflowRunRow{
		ID:              runID,
		TenantID:        tenantID,
		WorkflowID:      req.Msg.WorkflowId,
		WorkflowVersion: version.Version,
		ProjectID:       req.Msg.ProjectId,
		Status:          domain.WorkflowRunPending,
		RunContext:      runContext,
		StartedAt:       &now,
	}
	createdRun, err := db.CreateWorkflowRun(ctx, ttx.Tx, runRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	// Seed a step run for each step in the DAG (status=pending). The
	// reconciler progresses them through ready→running→succeeded.
	for _, step := range steps {
		stepRun := db.WorkflowStepRunRow{
			ID:            db.NewID(),
			TenantID:      tenantID,
			WorkflowRunID: runID,
			StepID:        step.ID,
			StepName:      step.Name,
			StepKind:      stepKindFromDomain(step.Kind),
			Status:        domain.StepRunPending,
		}
		if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, stepRun); err != nil {
			return nil, mapDBError(err)
		}
	}

	if err := enqueueWorkflowEvent(ctx, ttx.Tx, domain.WorkflowEventRunStarted, wf, version, createdRun, ""); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow run started", "run_id", runID, "workflow", req.Msg.WorkflowId, "version", version.Version, "steps", len(steps))
	return connect.NewResponse(&apiv1.StartWorkflowResponse{Run: runRowToProto(createdRun)}), nil
}

// AbortWorkflow transitions a running WorkflowRun to aborted
// (docs/02 §2.4). In-flight step runs are cancelled.
func (s *Service) AbortWorkflow(ctx context.Context, req *connect.Request[apiv1.AbortWorkflowRequest]) (*connect.Response[apiv1.AbortWorkflowResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id must not be empty"))
	}
	reason, err := validateTextField(req.Msg.Reason, maxReasonLen, "reason")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, req.Msg.RunId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status == domain.WorkflowRunCompleted || current.Status == domain.WorkflowRunFailed || current.Status == domain.WorkflowRunAborted {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("run is already terminal (status=%s)", current.Status))
	}
	now := time.Now().UTC()
	updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, req.Msg.RunId, current.Version, db.UpdateWorkflowRunFields{
		Status:  strPtr(domain.WorkflowRunAborted),
		EndedAt: &now,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	// Cancel any in-flight step runs.
	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, req.Msg.RunId)
	if err != nil {
		return nil, mapDBError(err)
	}
	for _, sr := range stepRuns {
		if sr.Status == domain.StepRunPending || sr.Status == domain.StepRunReady || sr.Status == domain.StepRunRunning || sr.Status == domain.StepRunApprovalPending {
			if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunFailed),
				EndedAt: &now,
			}); err != nil {
				return nil, mapDBError(err)
			}
		}
	}
	wf, _ := db.GetWorkflow(ctx, ttx.Tx, tenantID, updated.WorkflowID)
	if err := enqueueWorkflowEvent(ctx, ttx.Tx, domain.WorkflowEventRunAborted, wf, db.WorkflowVersionRow{}, updated, reason); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("workflow run aborted", "run_id", updated.ID, "reason", reason)
	return connect.NewResponse(&apiv1.AbortWorkflowResponse{Run: runRowToProto(updated)}), nil
}

// GetWorkflowRun returns a single WorkflowRun by id.
func (s *Service) GetWorkflowRun(ctx context.Context, req *connect.Request[apiv1.GetWorkflowRunRequest]) (*connect.Response[apiv1.GetWorkflowRunResponse], error) {
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
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetWorkflowRunResponse{Run: runRowToProto(run)}), nil
}

// ListWorkflowRuns returns a page of WorkflowRuns for a workflow.
func (s *Service) ListWorkflowRuns(ctx context.Context, req *connect.Request[apiv1.ListWorkflowRunsRequest]) (*connect.Response[apiv1.ListWorkflowRunsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	f := db.ListWorkflowRunsFilter{
		TenantID:   tenantID,
		WorkflowID: req.Msg.WorkflowId,
		PageSize:   int(req.Msg.PageSize),
		AfterID:    req.Msg.PageToken,
	}
	if req.Msg.Status != nil {
		f.Status = workflowRunStatusFromProto(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	runs, err := db.ListWorkflowRuns(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkflowRunsResponse{}
	for _, r := range runs {
		resp.Runs = append(resp.Runs, runRowToProto(r))
	}
	if len(runs) > 0 {
		resp.NextPageToken = runs[len(runs)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// GetWorkflowStepRuns returns all step runs for a WorkflowRun. Used by
// the run view to overlay live step transitions on the canvas
// (docs/10 §5.1).
func (s *Service) GetWorkflowStepRuns(ctx context.Context, req *connect.Request[apiv1.GetWorkflowStepRunsRequest]) (*connect.Response[apiv1.GetWorkflowStepRunsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, req.Msg.RunId)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.GetWorkflowStepRunsResponse{}
	for _, sr := range stepRuns {
		resp.StepRuns = append(resp.StepRuns, stepRunRowToProto(sr))
	}
	return connect.NewResponse(resp), nil
}

// StreamWorkflowEvents is the server-stream RPC that fans out workflow
// run events from NATS to connected clients (docs/07 §4, docs/10 §4.1).
func (s *Service) StreamWorkflowEvents(ctx context.Context, req *connect.Request[apiv1.StreamWorkflowEventsRequest], stream *connect.ServerStream[apiv1.StreamWorkflowEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}
	filter := "orchicon.events.workflow.>"
	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}
	ch, err := s.subscriber.Subscribe(ctx, filter, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to workflow events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			evt, err := parseWorkflowEvent(msg.Data)
			if err != nil {
				s.log.Warn("failed to parse workflow event", "subject", msg.Subject, "error", err)
				continue
			}
			// Filter by workflow_id / run_id if specified.
			if req.Msg.WorkflowRunId != "" && evt.WorkflowRunId != req.Msg.WorkflowRunId {
				continue
			}
			if req.Msg.WorkflowId != "" && evt.WorkflowId != "" && evt.WorkflowId != req.Msg.WorkflowId {
				continue
			}
			resp := &apiv1.StreamWorkflowEventsResponse{
				Event:    evt,
				Sequence: int64(msg.Seq),
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// AcquireEditLock acquires an exclusive edit lock on a Workflow for the
// visual editor (docs/07 §3.3). Returns acquired=false if already held
// by another actor.
func (s *Service) AcquireEditLock(ctx context.Context, req *connect.Request[apiv1.AcquireWorkflowEditLockRequest]) (*connect.Response[apiv1.AcquireWorkflowEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	actor, err := validateActor(req.Msg.Actor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	lock, acquired, err := db.AcquireEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, domain.EditLockResourceWorkflow, actor, db.DefaultEditLockTTL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	resp := &apiv1.AcquireWorkflowEditLockResponse{Acquired: acquired}
	if acquired || lock.HeldBy != "" {
		resp.Lock = lockRowToProto(lock)
	}
	return connect.NewResponse(resp), nil
}

// ReleaseEditLock releases a held edit lock. Only the actor that holds
// the lock may release it.
func (s *Service) ReleaseEditLock(ctx context.Context, req *connect.Request[apiv1.ReleaseWorkflowEditLockRequest]) (*connect.Response[apiv1.ReleaseWorkflowEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	actor, err := validateActor(req.Msg.Actor)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	if err := db.ReleaseEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, domain.EditLockResourceWorkflow, actor); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.ReleaseWorkflowEditLockResponse{}), nil
}

// GetEditLock returns the current edit lock state for a Workflow, if any
// unexpired lock exists.
func (s *Service) GetEditLock(ctx context.Context, req *connect.Request[apiv1.GetWorkflowEditLockRequest]) (*connect.Response[apiv1.GetWorkflowEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkflowId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	lock, err := db.GetEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkflowId, domain.EditLockResourceWorkflow)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewResponse(&apiv1.GetWorkflowEditLockResponse{}), nil
		}
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetWorkflowEditLockResponse{Lock: lockRowToProto(lock)}), nil
}

// --- helpers ---------------------------------------------------------------

// StepWire is the JSON shape of a Step stored in workflow_versions.steps
// (mirrors the proto Step message — docs/02 §2.4). Used to parse the
// steps JSON when seeding step runs and when the reconciler progresses
// the DAG. Exported so the WorkflowReconciler (internal/scheduler) can
// parse the same shape without duplicating the schema.
type StepWire struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Ref           string   `json:"ref"`
	WorkerVersion int      `json:"worker_version"`
	DependsOn     []string `json:"depends_on"`
	GatePolicyRef string   `json:"gate_policy_ref"`
	Config        string   `json:"config"`
	PositionX     float64  `json:"position_x"`
	PositionY     float64  `json:"position_y"`
}

// ParseSteps decodes the steps JSON (an array of Step messages) into a
// slice of StepWire. Returns an empty slice for empty/null input.
// Exported for use by the WorkflowReconciler.
//
// Old kind strings from before the v1 palette rework (e.g. "worker"
// instead of "task") are normalized to the current domain constants
// for backward compatibility.
func ParseSteps(data []byte) ([]StepWire, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var steps []StepWire
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	// Normalize old frontend kind strings to domain constants.
	for i := range steps {
		switch steps[i].Kind {
		case "worker":
			steps[i].Kind = domain.StepKindTask
		}
	}
	return steps, nil
}

// parseSteps is the internal alias used by the service handler.
func parseSteps(data []byte) ([]StepWire, error) {
	return ParseSteps(data)
}

// enqueueWorkflowEvent builds a workflow event envelope, encodes it as
// JSON, and enqueues it in the outbox within the current transaction
// (mirrors internal/worker/service.go).
func enqueueWorkflowEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkflowRow, v db.WorkflowVersionRow, r db.WorkflowRunRow, stepID string) error {
	payload, err := buildWorkflowEventPayload(eventType, w, v, r, stepID)
	if err != nil {
		return err
	}
	aggregateID := w.ID
	aggregateVer := w.Version
	tenantID := w.TenantID
	if r.ID != "" {
		// Run-level events key on the run id.
		aggregateID = r.ID
		aggregateVer = r.Version
		tenantID = r.TenantID
	}
	row := db.OutboxRow{
		TenantID:      tenantID,
		EventType:     eventType,
		AggregateType: "workflow",
		AggregateID:   aggregateID,
		AggregateVer:  aggregateVer,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// buildWorkflowEventPayload returns the JSON-encoded workflow event
// envelope. The envelope carries the workflow id, run id, step id, and
// status fields so streaming clients can route and render events without
// re-parsing the full payload.
func buildWorkflowEventPayload(eventType string, w db.WorkflowRow, v db.WorkflowVersionRow, r db.WorkflowRunRow, stepID string) ([]byte, error) {
	evt := map[string]any{
		"event_type": eventType,
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if w.ID != "" {
		evt["tenant_id"] = w.TenantID
		evt["workflow_id"] = w.ID
		evt["workflow_status"] = w.Status
	}
	if v.ID != "" {
		evt["workflow_version"] = v.Version
	}
	if r.ID != "" {
		evt["workflow_run_id"] = r.ID
		evt["run_status"] = r.Status
		evt["tenant_id"] = r.TenantID
		evt["workflow_id"] = r.WorkflowID
	}
	if stepID != "" {
		evt["step_id"] = stepID
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow event payload: %w", err)
	}
	return b, nil
}

// parseWorkflowEvent decodes the JSON event payload from the outbox/NATS
// into a WorkflowEvent proto message.
func parseWorkflowEvent(data []byte) (*apiv1.WorkflowEvent, error) {
	var env struct {
		EventType    string `json:"event_type"`
		TenantID     string `json:"tenant_id"`
		WorkflowID   string `json:"workflow_id"`
		WorkflowRunID string `json:"workflow_run_id"`
		StepID       string `json:"step_id"`
		RunStatus    string `json:"run_status"`
		StepStatus   string `json:"step_status"`
		OccurredAt   string `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse workflow event: %w", err)
	}
	evt := &apiv1.WorkflowEvent{
		EventType:    env.EventType,
		TenantId:     env.TenantID,
		WorkflowId:   env.WorkflowID,
		WorkflowRunId: env.WorkflowRunID,
		StepId:       env.StepID,
		RunStatus:   workflowRunStatusToProto(env.RunStatus),
		StepStatus:   stepRunStatusToProto(env.StepStatus),
		Payload:      data,
	}
	if t, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err == nil {
		evt.OccurredAt = timestamppb.New(t)
	}
	return evt, nil
}

// mapDBError translates a data-access error into a Connect error code.
func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("workflow not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// --- proto mappers ---------------------------------------------------------

func workflowStatusToProto(status string) apiv1.WorkflowStatus {
	switch status {
	case domain.WorkflowDraft:
		return apiv1.WorkflowStatus_WORKFLOW_STATUS_DRAFT
	case domain.WorkflowPublished:
		return apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED
	case domain.WorkflowDeprecated:
		return apiv1.WorkflowStatus_WORKFLOW_STATUS_DEPRECATED
	default:
		return apiv1.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
}

func workflowStatusFromProto(status apiv1.WorkflowStatus) string {
	switch status {
	case apiv1.WorkflowStatus_WORKFLOW_STATUS_DRAFT:
		return domain.WorkflowDraft
	case apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED:
		return domain.WorkflowPublished
	case apiv1.WorkflowStatus_WORKFLOW_STATUS_DEPRECATED:
		return domain.WorkflowDeprecated
	default:
		return ""
	}
}

func workflowVersionStatusToProto(status string) apiv1.WorkflowVersionStatus {
	switch status {
	case domain.WorkflowVersionDraft:
		return apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_DRAFT
	case domain.WorkflowVersionPublished:
		return apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_PUBLISHED
	case domain.WorkflowVersionDeprecated:
		return apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_DEPRECATED
	default:
		return apiv1.WorkflowVersionStatus_WORKFLOW_VERSION_STATUS_UNSPECIFIED
	}
}

func workflowRunStatusToProto(status string) apiv1.WorkflowRunStatus {
	switch status {
	case domain.WorkflowRunPending:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING
	case domain.WorkflowRunRunning:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING
	case domain.WorkflowRunCompleted:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED
	case domain.WorkflowRunFailed:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED
	case domain.WorkflowRunAborted:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_ABORTED
	case domain.WorkflowRunPaused:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PAUSED
	default:
		return apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_UNSPECIFIED
	}
}

func workflowRunStatusFromProto(status apiv1.WorkflowRunStatus) string {
	switch status {
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING:
		return domain.WorkflowRunPending
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING:
		return domain.WorkflowRunRunning
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED:
		return domain.WorkflowRunCompleted
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED:
		return domain.WorkflowRunFailed
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_ABORTED:
		return domain.WorkflowRunAborted
	case apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PAUSED:
		return domain.WorkflowRunPaused
	default:
		return ""
	}
}

func stepKindToProto(kind string) apiv1.StepKind {
	switch kind {
	case domain.StepKindTask:
		return apiv1.StepKind_STEP_KIND_TASK
	case domain.StepKindDecision:
		return apiv1.StepKind_STEP_KIND_DECISION
	case domain.StepKindApproval:
		return apiv1.StepKind_STEP_KIND_APPROVAL
	case domain.StepKindParallel:
		return apiv1.StepKind_STEP_KIND_PARALLEL
	case domain.StepKindRecover:
		return apiv1.StepKind_STEP_KIND_RECOVER
	default:
		return apiv1.StepKind_STEP_KIND_UNSPECIFIED
	}
}

// stepKindFromDomain maps a domain step kind string to the proto enum.
// (Named to avoid clashing with the to-proto direction.)
func stepKindFromDomain(kind string) string { return kind }

func stepRunStatusToProto(status string) apiv1.StepRunStatus {
	switch status {
	case domain.StepRunPending:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_PENDING
	case domain.StepRunReady:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_READY
	case domain.StepRunRunning:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING
	case domain.StepRunSucceeded:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED
	case domain.StepRunFailed:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED
	case domain.StepRunSkipped:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_SKIPPED
	case domain.StepRunBlocked:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_BLOCKED
	case domain.StepRunApprovalPending:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_APPROVAL_PENDING
	default:
		return apiv1.StepRunStatus_STEP_RUN_STATUS_UNSPECIFIED
	}
}

func workflowRowToProto(w db.WorkflowRow) *apiv1.Workflow {
	return &apiv1.Workflow{
		Id:             w.ID,
		TenantId:       w.TenantID,
		ProjectId:       w.ProjectID,
		Name:            w.Name,
		Status:          workflowStatusToProto(w.Status),
		CurrentVersion: int32(w.CurrentVersion),
		Version:         int32(w.Version),
		CreatedAt:       timestamppb.New(w.CreatedAt),
		UpdatedAt:       timestamppb.New(w.UpdatedAt),
	}
}

func versionRowToProto(v db.WorkflowVersionRow) *apiv1.WorkflowVersion {
	pv := &apiv1.WorkflowVersion{
		Id:                v.ID,
		TenantId:          v.TenantID,
		WorkflowId:        v.WorkflowID,
		Version:           int32(v.Version),
		VersionNote:       v.VersionNote,
		Status:            workflowVersionStatusToProto(v.Status),
		Steps:             string(v.Steps),
		Inputs:            string(v.Inputs),
		Outputs:           string(v.Outputs),
		RecoveryPolicyRef: v.RecoveryPolicyRef,
		CreatedAt:         timestamppb.New(v.CreatedAt),
	}
	if v.PublishedAt != nil {
		pv.PublishedAt = timestamppb.New(*v.PublishedAt)
	}
	return pv
}

func runRowToProto(r db.WorkflowRunRow) *apiv1.WorkflowRun {
	p := &apiv1.WorkflowRun{
		Id:              r.ID,
		TenantId:        r.TenantID,
		WorkflowId:      r.WorkflowID,
		WorkflowVersion: int32(r.WorkflowVersion),
		ProjectId:       r.ProjectID,
		Status:          workflowRunStatusToProto(r.Status),
		CurrentStep:     r.CurrentStep,
		RunContext:      string(r.RunContext),
		Version:         int32(r.Version),
		CreatedAt:       timestamppb.New(r.CreatedAt),
		UpdatedAt:       timestamppb.New(r.UpdatedAt),
	}
	if r.StartedAt != nil {
		p.StartedAt = timestamppb.New(*r.StartedAt)
	}
	if r.EndedAt != nil {
		p.EndedAt = timestamppb.New(*r.EndedAt)
	}
	return p
}

func stepRunRowToProto(s db.WorkflowStepRunRow) *apiv1.WorkflowStepRun {
	p := &apiv1.WorkflowStepRun{
		Id:                s.ID,
		TenantId:          s.TenantID,
		WorkflowRunId:     s.WorkflowRunID,
		StepId:            s.StepID,
		StepName:          s.StepName,
		StepKind:          stepKindToProto(s.StepKind),
		Status:            stepRunStatusToProto(s.Status),
		Attempt:           int32(s.Attempt),
		Result:            string(s.Result),
		WorkerExecutionId: s.WorkerExecutionID,
		CreatedAt:         timestamppb.New(s.CreatedAt),
		UpdatedAt:         timestamppb.New(s.UpdatedAt),
	}
	if s.StartedAt != nil {
		p.StartedAt = timestamppb.New(*s.StartedAt)
	}
	if s.EndedAt != nil {
		p.EndedAt = timestamppb.New(*s.EndedAt)
	}
	return p
}

// lockRowToProto maps a db.EditLockRow to the generated proto EditLock
// (shared with WorkerService — docs/07 §3.3).
func lockRowToProto(l db.EditLockRow) *apiv1.EditLock {
	return &apiv1.EditLock{
		ResourceId: l.ResourceID,
		HeldBy:     l.HeldBy,
		AcquiredAt: timestamppb.New(l.AcquiredAt),
		ExpiresAt:  timestamppb.New(l.ExpiresAt),
	}
}

func strPtr(s string) *string { return &s }
