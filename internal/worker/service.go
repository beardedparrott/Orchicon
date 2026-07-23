package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the WorkerService Connect handler
// (apiv1connect.WorkerServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3); the relay publishes it to NATS asynchronously.
//
// The Worker lifecycle (docs/05 §4) is enforced here:
//   - CreateWorker creates a Worker in draft state with its first draft
//     version.
//   - PublishWorkerVersion transitions a draft version to published
//     (immutable) and the Worker to published.
//   - DeprecateWorker transitions a published Worker to deprecated.
//   - RetireWorker transitions a deprecated Worker to retired.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedWorkerServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.WorkerServiceHandler = (*Service)(nil)

// New constructs a WorkerService handler.
func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// CreateWorker validates input, inserts the worker header + its first
// draft version, and enqueues a worker.created event — all in one
// tenant-scoped transaction.
func (s *Service) CreateWorker(ctx context.Context, req *connect.Request[apiv1.CreateWorkerRequest]) (*connect.Response[apiv1.CreateWorkerResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	name, err := validateName(msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	slug, err := normalizeSlug(msg.Slug, name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	description, err := validateTextField(msg.Description, maxDescLen, "description")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	purpose, err := validateTextField(msg.Purpose, maxPurposeLen, "purpose")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	versionNote, err := validateTextField(msg.VersionNote, maxVersionNoteLen, "version_note")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	runtimeRef, err := validateTextField(msg.RuntimeRef, maxNameLen, "runtime_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	modelRef, err := validateTextField(msg.ModelRef, maxNameLen, "model_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	systemPrompt, err := validateTextField(msg.SystemPrompt, maxPromptLen, "system_prompt")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	executionPolicyRef, err := validateTextField(msg.ExecutionPolicyRef, maxNameLen, "execution_policy_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	recoveryWorkflowRef, err := validateTextField(msg.RecoveryWorkflowRef, maxNameLen, "recovery_workflow_ref")
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	contextSources, err := validateJSONField(msg.ContextSources, "[]", "context_sources", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	permissions, err := validateJSONField(msg.Permissions, "{}", "permissions", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	gatedTools, err := validateJSONField(msg.GatedTools, "[]", "gated_tools", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	budgetOverrides, err := validateJSONField(msg.BudgetOverrides, "{}", "budget_overrides", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	labels, err := validateJSONField(msg.Labels, "{}", "labels", maxJSONFieldLen)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	concurrencyLimit := int(msg.ConcurrencyLimit)
	if concurrencyLimit < 0 {
		concurrencyLimit = 0
	}

	workerID := db.NewID()
	versionID := db.NewID()

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	workerRow := db.WorkerRow{
		ID:             workerID,
		TenantID:       tenantID,
		Name:           name,
		Slug:           slug,
		Description:    description,
		Purpose:        purpose,
		Status:         domain.WorkerDraft,
		CurrentVersion: 0,
		CreatedBy:      "", // populated when auth lands (Phase 9)
	}
	created, err := db.CreateWorker(ctx, ttx.Tx, workerRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	// First version is always version 1, in draft state (docs/05 §4).
	versionRow := db.WorkerVersionRow{
		ID:                 versionID,
		TenantID:           tenantID,
		WorkerID:           workerID,
		Version:            1,
		VersionNote:        versionNote,
		Status:             domain.WorkerVersionDraft,
		RuntimeRef:         runtimeRef,
		ModelRef:           modelRef,
		SystemPrompt:       systemPrompt,
		ContextSources:     contextSources,
		Permissions:        permissions,
		GatedTools:         gatedTools,
		BudgetOverrides:    budgetOverrides,
		ExecutionPolicyRef: executionPolicyRef,
		ConcurrencyLimit:   concurrencyLimit,
		RecoveryWorkflowRef: recoveryWorkflowRef,
		Labels:             labels,
	}
	createdVersion, err := db.CreateWorkerVersion(ctx, ttx.Tx, versionRow)
	if err != nil {
		return nil, mapDBError(err)
	}

	if err := enqueueWorkerEvent(ctx, ttx.Tx, "worker.created", created, createdVersion); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker created", "id", created.ID, "tenant", tenantID, "slug", slug)
	return connect.NewResponse(&apiv1.CreateWorkerResponse{
		Worker:  workerRowToProto(created),
		Version: versionRowToProto(createdVersion),
	}), nil
}
// making it dispatchable (docs/05 §4). The version becomes immutable
// and the Worker transitions to published.
func (s *Service) PublishWorkerVersion(ctx context.Context, req *connect.Request[apiv1.PublishWorkerVersionRequest]) (*connect.Response[apiv1.PublishWorkerVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.WorkerId)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Publish the latest draft version.
	latest, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, false)
	if err != nil {
		return nil, mapDBError(err)
	}
	if latest.Status != domain.WorkerVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("latest version (v%d) is not draft (status=%s)", latest.Version, latest.Status))
	}
	published, err := db.PublishWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdateWorkerCurrentVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, current.Version, latest.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkerEvent(ctx, ttx.Tx, "worker.published", updated, published); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker version published", "id", updated.ID, "version", published.Version)
	return connect.NewResponse(&apiv1.PublishWorkerVersionResponse{
		Worker:  workerRowToProto(updated),
		Version: versionRowToProto(published),
	}), nil
}

// DeprecateWorker transitions a published Worker to deprecated (docs/05
// §4). Still dispatchable for in-flight Workflows; no new Workflows may
// bind.
func (s *Service) DeprecateWorker(ctx context.Context, req *connect.Request[apiv1.DeprecateWorkerRequest]) (*connect.Response[apiv1.DeprecateWorkerResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.WorkerId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.WorkerPublished {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("worker must be published to deprecate (status=%s)", current.Status))
	}
	updated, err := db.UpdateWorkerStatus(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, current.Version, domain.WorkerDeprecated)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Deprecate the current published version too (per-version state).
	if _, err := db.DeprecateWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, current.CurrentVersion); err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, mapDBError(err)
	}
	// Re-fetch the deprecated version for the event payload.
	deprecatedVer, _ := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, false)
	if err := enqueueWorkerEvent(ctx, ttx.Tx, "worker.deprecated", updated, deprecatedVer); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker deprecated", "id", updated.ID)
	return connect.NewResponse(&apiv1.DeprecateWorkerResponse{Worker: workerRowToProto(updated)}), nil
}

// RetireWorker transitions a deprecated Worker to retired (docs/05 §4).
// No new dispatches; in-flight executions run to completion.
func (s *Service) RetireWorker(ctx context.Context, req *connect.Request[apiv1.RetireWorkerRequest]) (*connect.Response[apiv1.RetireWorkerResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.WorkerId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.WorkerDeprecated {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("worker must be deprecated to retire (status=%s)", current.Status))
	}
	// docs/05 §4: "A Worker may be retired only when zero active
	// executions pin its latest published version." Enforcement of the
	// active-execution check arrives with the scheduler (Phase 5); for
	// now the lifecycle transition is gated on status only.
	updated, err := db.UpdateWorkerStatus(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, current.Version, domain.WorkerRetired)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkerEvent(ctx, ttx.Tx, "worker.retired", updated, db.WorkerVersionRow{}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker retired", "id", updated.ID)
	return connect.NewResponse(&apiv1.RetireWorkerResponse{Worker: workerRowToProto(updated)}), nil
}

// DeleteWorker hard-deletes a Worker and all its versions (cascade).
func (s *Service) DeleteWorker(ctx context.Context, req *connect.Request[apiv1.DeleteWorkerRequest]) (*connect.Response[apiv1.DeleteWorkerResponse], error) {
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
	if _, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteWorker(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker deleted", "id", req.Msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteWorkerResponse{}), nil
}

// GetWorker returns a single worker header by id, with its latest
// published version (if any).
func (s *Service) GetWorker(ctx context.Context, req *connect.Request[apiv1.GetWorkerRequest]) (*connect.Response[apiv1.GetWorkerResponse], error) {
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
	w, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.GetWorkerResponse{Worker: workerRowToProto(w)}
	// Include the latest version (draft or published) so the frontend can
	// show the edit/publish buttons for draft workers (docs/05 §4).
	if v, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.Id, false); err == nil {
		resp.LatestVersion = versionRowToProto(v)
	}
	return connect.NewResponse(resp), nil
}

// ListWorkers returns a page of workers for the tenant. Pagination is
// cursor-based on ULID id ordering (docs/07 §5.2).
func (s *Service) ListWorkers(ctx context.Context, req *connect.Request[apiv1.ListWorkersRequest]) (*connect.Response[apiv1.ListWorkersResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListWorkersFilter{
		TenantID:  tenantID,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
		Search:    req.Msg.Search,
		SortBy:    req.Msg.SortBy,
		SortOrder: req.Msg.SortOrder,
	}
	if req.Msg.Status != nil {
		f.Status = workerStatusFromProto(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	workers, err := db.ListWorkers(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkersResponse{}
	for _, w := range workers {
		resp.Workers = append(resp.Workers, workerRowToProto(w))
	}
	if len(workers) > 0 {
		resp.NextPageToken = workers[len(workers)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// ListWorkerVersions returns all versions of a worker, newest first.
func (s *Service) ListWorkerVersions(ctx context.Context, req *connect.Request[apiv1.ListWorkerVersionsRequest]) (*connect.Response[apiv1.ListWorkerVersionsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	versions, err := db.ListWorkerVersions(ctx, ttx.Tx, tenantID, req.Msg.WorkerId)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkerVersionsResponse{}
	for _, v := range versions {
		resp.Versions = append(resp.Versions, versionRowToProto(v))
	}
	return connect.NewResponse(resp), nil
}

// UpdateWorkerVersion updates the mutable fields of a draft WorkerVersion.
// Only versions with status='draft' may be updated; published versions are
// immutable. The service reads the current version first, then applies
// only the fields set in the request, then writes back the merged row.
func (s *Service) UpdateWorkerVersion(ctx context.Context, req *connect.Request[apiv1.UpdateWorkerVersionRequest]) (*connect.Response[apiv1.UpdateWorkerVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	msg := req.Msg
	if msg.WorkerId == "" || msg.VersionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id and version_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Fetch the existing version to confirm it exists and is draft.
	current, err := db.GetWorkerVersionByID(ctx, ttx.Tx, tenantID, msg.WorkerId, msg.VersionId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if current.Status != domain.WorkerVersionDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("version %s status is %q, must be 'draft' to update", msg.VersionId, current.Status))
	}

	// Build merged row: apply only non-nil proto fields over current.
	merged := current
	if msg.RuntimeRef != nil {
		merged.RuntimeRef = *msg.RuntimeRef
	}
	if msg.ModelRef != nil {
		merged.ModelRef = *msg.ModelRef
	}
	if msg.SystemPrompt != nil {
		merged.SystemPrompt = *msg.SystemPrompt
	}
	if msg.ContextSources != nil {
		merged.ContextSources = []byte(*msg.ContextSources)
	}
	if msg.Permissions != nil {
		merged.Permissions = []byte(*msg.Permissions)
	}
	if msg.GatedTools != nil {
		merged.GatedTools = []byte(*msg.GatedTools)
	}
	if msg.BudgetOverrides != nil {
		merged.BudgetOverrides = []byte(*msg.BudgetOverrides)
	}
	if msg.ExecutionPolicyRef != nil {
		merged.ExecutionPolicyRef = *msg.ExecutionPolicyRef
	}
	if msg.ConcurrencyLimit != nil {
		merged.ConcurrencyLimit = int(*msg.ConcurrencyLimit)
	}
	if msg.RecoveryWorkflowRef != nil {
		merged.RecoveryWorkflowRef = *msg.RecoveryWorkflowRef
	}
	if msg.Labels != nil {
		merged.Labels = []byte(*msg.Labels)
	}
	if msg.VersionNote != nil {
		merged.VersionNote = *msg.VersionNote
	}

	updated, err := db.UpdateDraftVersion(ctx, ttx.Tx, merged)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker version updated", "worker_id", msg.WorkerId, "version_id", msg.VersionId, "version", updated.Version)
	return connect.NewResponse(&apiv1.UpdateWorkerVersionResponse{
		Version: versionRowToProto(updated),
	}), nil
}

// CreateWorkerVersion creates a new draft version for a Worker, copying
// fields from the latest published version. The new version starts as a
// draft. Optional fields in the request override the source values.
func (s *Service) CreateWorkerVersion(ctx context.Context, req *connect.Request[apiv1.CreateWorkerVersionRequest]) (*connect.Response[apiv1.CreateWorkerVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	msg := req.Msg
	if msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Fetch the worker to confirm it exists.
	if _, err := db.GetWorker(ctx, ttx.Tx, tenantID, msg.WorkerId); err != nil {
		return nil, mapDBError(err)
	}

	// Get the latest published version as the source template.
	source, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, msg.WorkerId, true)
	if err != nil {
		return nil, mapDBError(err)
	}

	// Compute the next version number.
	nextVer, err := db.NextWorkerVersionNumber(ctx, ttx.Tx, tenantID, msg.WorkerId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Build the new draft row, applying any request overrides.
	newVer := db.WorkerVersionRow{
		ID:                  db.NewID(),
		TenantID:            tenantID,
		WorkerID:            msg.WorkerId,
		Version:             nextVer,
		Status:              domain.WorkerVersionDraft,
		RuntimeRef:          source.RuntimeRef,
		ModelRef:            source.ModelRef,
		SystemPrompt:        source.SystemPrompt,
		ContextSources:      source.ContextSources,
		Permissions:         source.Permissions,
		GatedTools:          source.GatedTools,
		BudgetOverrides:     source.BudgetOverrides,
		ExecutionPolicyRef:  source.ExecutionPolicyRef,
		ConcurrencyLimit:    source.ConcurrencyLimit,
		RecoveryWorkflowRef: source.RecoveryWorkflowRef,
		Labels:              source.Labels,
	}
	if msg.RuntimeRef != nil {
		newVer.RuntimeRef = *msg.RuntimeRef
	}
	if msg.ModelRef != nil {
		newVer.ModelRef = *msg.ModelRef
	}
	if msg.SystemPrompt != nil {
		newVer.SystemPrompt = *msg.SystemPrompt
	}
	if msg.ContextSources != nil {
		newVer.ContextSources = []byte(*msg.ContextSources)
	}
	if msg.Permissions != nil {
		newVer.Permissions = []byte(*msg.Permissions)
	}
	if msg.GatedTools != nil {
		newVer.GatedTools = []byte(*msg.GatedTools)
	}
	if msg.BudgetOverrides != nil {
		newVer.BudgetOverrides = []byte(*msg.BudgetOverrides)
	}
	if msg.ExecutionPolicyRef != nil {
		newVer.ExecutionPolicyRef = *msg.ExecutionPolicyRef
	}
	if msg.ConcurrencyLimit != nil {
		newVer.ConcurrencyLimit = int(*msg.ConcurrencyLimit)
	}
	if msg.RecoveryWorkflowRef != nil {
		newVer.RecoveryWorkflowRef = *msg.RecoveryWorkflowRef
	}
	if msg.Labels != nil {
		newVer.Labels = []byte(*msg.Labels)
	}
	if msg.VersionNote != nil {
		newVer.VersionNote = *msg.VersionNote
	}

	created, err := db.CreateWorkerVersion(ctx, ttx.Tx, newVer)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create worker version: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker version created", "worker_id", msg.WorkerId, "version", nextVer)
	return connect.NewResponse(&apiv1.CreateWorkerVersionResponse{
		Version: versionRowToProto(created),
	}), nil
}

// AcquireEditLock acquires an exclusive edit lock on a Worker for the
// visual editor (docs/07 §3.3). Returns acquired=false if already held
// by another actor.
func (s *Service) AcquireEditLock(ctx context.Context, req *connect.Request[apiv1.AcquireEditLockRequest]) (*connect.Response[apiv1.AcquireEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
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
	lock, acquired, err := db.AcquireEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, domain.EditLockResourceWorker, actor, db.DefaultEditLockTTL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	resp := &apiv1.AcquireEditLockResponse{Acquired: acquired}
	if acquired || lock.HeldBy != "" {
		resp.Lock = lockRowToProto(lock)
	}
	return connect.NewResponse(resp), nil
}

// ReleaseEditLock releases a held edit lock. Only the actor that holds
// the lock may release it.
func (s *Service) ReleaseEditLock(ctx context.Context, req *connect.Request[apiv1.ReleaseEditLockRequest]) (*connect.Response[apiv1.ReleaseEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
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
	if err := db.ReleaseEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, domain.EditLockResourceWorker, actor); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.ReleaseEditLockResponse{}), nil
}

// GetEditLock returns the current edit lock state for a Worker, if any
// unexpired lock exists.
func (s *Service) GetEditLock(ctx context.Context, req *connect.Request[apiv1.GetEditLockRequest]) (*connect.Response[apiv1.GetEditLockResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	lock, err := db.GetEditLock(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, domain.EditLockResourceWorker)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return connect.NewResponse(&apiv1.GetEditLockResponse{}), nil
		}
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetEditLockResponse{Lock: lockRowToProto(lock)}), nil
}

// --- helpers ---------------------------------------------------------------

// enqueueWorkerEvent builds a worker event envelope, encodes it as
// JSON, and enqueues it in the outbox within the current transaction
// (mirrors internal/project/service.go).
func enqueueWorkerEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkerRow, v db.WorkerVersionRow) error {
	payload, err := buildEventPayload(eventType, w, v)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      w.TenantID,
		EventType:     eventType,
		AggregateType: "worker",
		AggregateID:   w.ID,
		AggregateVer:  w.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// buildEventPayload returns the JSON-encoded worker event envelope.
func buildEventPayload(eventType string, w db.WorkerRow, v db.WorkerVersionRow) ([]byte, error) {
	evt := map[string]any{
		"event_type":        eventType,
		"tenant_id":         w.TenantID,
		"worker_id":         w.ID,
		"aggregate_type":    "worker",
		"aggregate_id":      w.ID,
		"aggregate_version": w.Version,
		"status":            w.Status,
		"name":              w.Name,
		"slug":              w.Slug,
		"current_version":   w.CurrentVersion,
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	if v.WorkerID != "" {
		evt["version"] = v.Version
		evt["version_status"] = v.Status
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal event payload: %w", err)
	}
	return b, nil
}

// mapDBError translates a data-access error into a Connect error code
// (mirrors internal/project/service.go).
func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("worker not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// workerStatusToProto maps a domain status string to the proto enum
// value (docs/05 §4).
func workerStatusToProto(status string) apiv1.WorkerStatus {
	switch status {
	case domain.WorkerDraft:
		return apiv1.WorkerStatus_WORKER_STATUS_DRAFT
	case domain.WorkerPublished:
		return apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED
	case domain.WorkerDeprecated:
		return apiv1.WorkerStatus_WORKER_STATUS_DEPRECATED
	case domain.WorkerRetired:
		return apiv1.WorkerStatus_WORKER_STATUS_RETIRED
	default:
		return apiv1.WorkerStatus_WORKER_STATUS_UNSPECIFIED
	}
}

// workerStatusFromProto maps a proto enum value to the domain status
// string.
func workerStatusFromProto(status apiv1.WorkerStatus) string {
	switch status {
	case apiv1.WorkerStatus_WORKER_STATUS_DRAFT:
		return domain.WorkerDraft
	case apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED:
		return domain.WorkerPublished
	case apiv1.WorkerStatus_WORKER_STATUS_DEPRECATED:
		return domain.WorkerDeprecated
	case apiv1.WorkerStatus_WORKER_STATUS_RETIRED:
		return domain.WorkerRetired
	default:
		return ""
	}
}

// workerVersionStatusToProto maps a domain version status string to the
// proto enum value.
func workerVersionStatusToProto(status string) apiv1.WorkerVersionStatus {
	switch status {
	case domain.WorkerVersionDraft:
		return apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_DRAFT
	case domain.WorkerVersionPublished:
		return apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED
	case domain.WorkerVersionDeprecated:
		return apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_DEPRECATED
	default:
		return apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_UNSPECIFIED
	}
}

// workerRowToProto maps a db.WorkerRow to the generated proto Worker.
func workerRowToProto(w db.WorkerRow) *apiv1.Worker {
	return &apiv1.Worker{
		Id:             w.ID,
		TenantId:       w.TenantID,
		Name:           w.Name,
		Slug:           w.Slug,
		Description:    w.Description,
		Purpose:        w.Purpose,
		Status:         workerStatusToProto(w.Status),
		CurrentVersion: int32(w.CurrentVersion),
		CreatedBy:      w.CreatedBy,
		Version:        int32(w.Version),
		CreatedAt:      timestamppb.New(w.CreatedAt),
		UpdatedAt:      timestamppb.New(w.UpdatedAt),
	}
}

// versionRowToProto maps a db.WorkerVersionRow to the generated proto
// WorkerVersion. JSON []byte fields are converted to strings (the proto
// uses string for JSON-typed fields).
func versionRowToProto(v db.WorkerVersionRow) *apiv1.WorkerVersion {
	pv := &apiv1.WorkerVersion{
		Id:                 v.ID,
		WorkerId:           v.WorkerID,
		Version:            int32(v.Version),
		VersionNote:        v.VersionNote,
		Status:             workerVersionStatusToProto(v.Status),
		RuntimeRef:         v.RuntimeRef,
		ModelRef:           v.ModelRef,
		SystemPrompt:       composeWorkerPrompt(v),
		ContextSources:     string(v.ContextSources),
		Permissions:        string(v.Permissions),
		GatedTools:         string(v.GatedTools),
		BudgetOverrides:    string(v.BudgetOverrides),
		ExecutionPolicyRef:  v.ExecutionPolicyRef,
		ConcurrencyLimit:   int32(v.ConcurrencyLimit),
		RecoveryWorkflowRef: v.RecoveryWorkflowRef,
		Labels:             string(v.Labels),
		CreatedAt:          timestamppb.New(v.CreatedAt),
	}
	if v.PublishedAt != nil {
		pv.PublishedAt = timestamppb.New(*v.PublishedAt)
	}
	return pv
}

// composeWorkerPrompt builds the system prompt for the proto response
// from the worker's four structured fields. Falls back to v.SystemPrompt.
func composeWorkerPrompt(v db.WorkerVersionRow) string {
	if v.Role == "" && v.Skills == "" && v.Behavior == "" && v.AgentsMD == "" {
		return v.SystemPrompt
	}
	var parts []string
	add := func(heading, content string) {
		c := strings.TrimSpace(content)
		if c == "" {
			return
		}
		parts = append(parts, "# "+heading+"\n\n"+c)
	}
	add("Role", v.Role)
	add("Skills", v.Skills)
	add("Behavior", v.Behavior)
	add("AGENTS.md", v.AgentsMD)
	return strings.Join(parts, "\n\n")
}

// lockRowToProto maps a db.EditLockRow to the generated proto EditLock.
func lockRowToProto(l db.EditLockRow) *apiv1.EditLock {
	return &apiv1.EditLock{
		ResourceId: l.ResourceID,
		HeldBy:     l.HeldBy,
		AcquiredAt: timestamppb.New(l.AcquiredAt),
		ExpiresAt:  timestamppb.New(l.ExpiresAt),
	}
}
