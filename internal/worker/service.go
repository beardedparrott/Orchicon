package worker

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
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
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

// SetAdapterKinds wires the Dispatcher's registered adapter kinds for
// explicit-adapter-input validation (ADR-0005 D2). Called by the server
// layer after construction; the package-level seam mirrors
// SetModelRefRegistry.
func (s *Service) SetAdapterKinds(fn func() []string) {
	SetAdapterKinds(fn)
}

// SetCustomProviderIDs wires the tenant custom-provider source for
// model-ref validation (ADR-0006 D6). Called by the server layer after
// construction; the package-level seam mirrors SetAdapterKinds.
func (s *Service) SetCustomProviderIDs(fn func(ctx context.Context, tenantID string) ([]string, error)) {
	SetCustomProviderIDs(fn)
}

// CreateWorker validates input, inserts the worker header + its first
// draft version, and enqueues a worker.created event — all in one
// tenant-scoped transaction. The transactional create lives in
// CreateWorkerTx (create.go) so the AskOrchicon tool path shares the
// exact same implementation (one create core, no drift).
func (s *Service) CreateWorker(ctx context.Context, req *connect.Request[apiv1.CreateWorkerRequest]) (*connect.Response[apiv1.CreateWorkerResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	in := CreateWorkerInput{
		TenantID:            tenantID,
		Name:                msg.Name,
		Slug:                msg.Slug,
		Description:         msg.Description,
		Purpose:             msg.Purpose,
		RoleRef:             msg.RoleRef,
		VersionNote:         msg.VersionNote,
		RuntimeRef:          msg.RuntimeRef,
		Adapter:             msg.Adapter,
		ModelRef:            msg.ModelRef,
		Role:                msg.Role,
		Skills:              msg.Skills,
		Behavior:            msg.Behavior,
		AgentsMD:            msg.AgentsMd,
		SystemPrompt:        msg.SystemPrompt,
		ContextSources:      msg.ContextSources,
		Permissions:         msg.Permissions,
		GatedTools:          msg.GatedTools,
		BudgetOverrides:     msg.BudgetOverrides,
		Labels:              msg.Labels,
		ExecutionPolicyRef:  msg.ExecutionPolicyRef,
		ConcurrencyLimit:    int(msg.ConcurrencyLimit),
		RecoveryWorkflowRef: msg.RecoveryWorkflowRef,
	}
	if err := ValidateCreateWorkerInput(&in); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// A role binding must reference a role that exists in this tenant.
	if in.RoleRef != "" {
		if _, err := db.GetRole(ctx, ttx.Tx, tenantID, in.RoleRef); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("role_ref: role %q not found in tenant", in.RoleRef))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	created, createdVersion, err := CreateWorkerTx(ctx, ttx.Tx, in)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker created", "id", created.ID, "tenant", tenantID, "slug", created.Slug)
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.published", "worker", updated.ID,
		audit.SnapshotStatus(current.Status), audit.Snapshot(workerVersionAuditSnapshot(published))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.published: %w", err))
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.deprecated", "worker", updated.ID,
		audit.SnapshotStatus(current.Status), audit.SnapshotStatus(updated.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.deprecated: %w", err))
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.retired", "worker", updated.ID,
		audit.SnapshotStatus(current.Status), audit.SnapshotStatus(updated.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.retired: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker retired", "id", updated.ID)
	return connect.NewResponse(&apiv1.RetireWorkerResponse{Worker: workerRowToProto(updated)}), nil
}

// UpdateWorker updates the mutable header fields of a draft Worker.
func (s *Service) UpdateWorker(ctx context.Context, req *connect.Request[apiv1.UpdateWorkerRequest]) (*connect.Response[apiv1.UpdateWorkerResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker id must not be empty"))
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("begin tx: %w", err))
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}

	var fields db.UpdateWorkerFields
	if msg.Name != "" {
		name, err := validateName(msg.Name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Name = &name
	}
	if msg.Description != "" {
		desc, err := validateTextField(msg.Description, maxDescLen, "description")
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Description = &desc
	}
	if msg.Purpose != "" {
		purpose, err := validateTextField(msg.Purpose, maxPurposeLen, "purpose")
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Purpose = &purpose
	}
	// The role binding is the only header field editable on a published
	// worker: it lives on the header (not the version) and is what gates
	// plane access. name/description/purpose stay draft-only.
	if msg.RoleRef != nil {
		roleRef := msg.GetRoleRef()
		if current.Status != domain.WorkerDraft && (msg.Name != "" || msg.Description != "" || msg.Purpose != "") {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("only the role binding can be changed on a published worker"))
		}
		if roleRef != "" {
			if _, err := db.GetRole(ctx, ttx.Tx, tenantID, roleRef); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("role_ref: role %q not found in tenant", roleRef))
				}
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
		fields.RoleRef = &roleRef
	}

	updated, err := db.UpdateWorker(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.updated", "worker", updated.ID,
		audit.Snapshot(workerAuditSnapshot(current)), audit.Snapshot(workerAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.updated: %w", err))
	}

	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	s.log.Info("worker updated", "id", updated.ID)
	return connect.NewResponse(&apiv1.UpdateWorkerResponse{Worker: workerRowToProto(updated)}), nil
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
	current, err := db.GetWorker(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteWorker(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.deleted", "worker", current.ID,
		audit.Snapshot(workerAuditSnapshot(current)), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker deleted", "id", req.Msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteWorkerResponse{}), nil
}

// DeleteWorkerVersion deletes a single draft worker version. Published
// versions are immutable and cannot be deleted.
func (s *Service) DeleteWorkerVersion(ctx context.Context, req *connect.Request[apiv1.DeleteWorkerVersionRequest]) (*connect.Response[apiv1.DeleteWorkerVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" || req.Msg.VersionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id and version_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkerVersionByID(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, req.Msg.VersionId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, req.Msg.VersionId); err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.version_deleted", "worker", req.Msg.WorkerId,
		audit.Snapshot(workerVersionAuditSnapshot(current)), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.version_deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.DeleteWorkerVersionResponse{}), nil
}

// SetActiveWorkerVersion sets a published version as the worker's current version.
func (s *Service) SetActiveWorkerVersion(ctx context.Context, req *connect.Request[apiv1.SetActiveWorkerVersionRequest]) (*connect.Response[apiv1.SetActiveWorkerVersionResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.WorkerId == "" || req.Msg.Version <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_id and version are required"))
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
	if _, err := db.SetActiveWorkerVersion(ctx, ttx.Tx, tenantID, req.Msg.WorkerId, current.Version, int(req.Msg.Version)); err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.active_version_set", "worker", current.ID,
		audit.Snapshot(workerAuditSnapshot(current)),
		audit.Snapshot(map[string]any{"current_version": int(req.Msg.Version)})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.active_version_set: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("active worker version set", "worker", req.Msg.WorkerId, "version", req.Msg.Version)
	return connect.NewResponse(&apiv1.SetActiveWorkerVersionResponse{}), nil
}

// RevertWorkerVersionToDraft moves a published version back to draft for editing.
func (s *Service) RevertWorkerVersionToDraft(ctx context.Context, req *connect.Request[apiv1.RevertWorkerVersionToDraftRequest]) (*connect.Response[apiv1.RevertWorkerVersionToDraftResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.VersionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("version_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	// The request only carries the version_id; resolve the owning worker
	// for the audit target. RevertWorkerVersionToDraft updates by version
	// id only.
	workerID, err := db.GetWorkerIDForVersion(ctx, ttx.Tx, tenantID, req.Msg.VersionId)
	if err != nil {
		return nil, mapDBError(err)
	}
	current, err := db.GetWorkerVersionByID(ctx, ttx.Tx, tenantID, workerID, req.Msg.VersionId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.RevertWorkerVersionToDraft(ctx, ttx.Tx, tenantID, req.Msg.VersionId); err != nil {
		return nil, mapDBError(err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.version_reverted_to_draft", "worker", workerID,
		audit.Snapshot(workerVersionAuditSnapshot(current)),
		audit.Snapshot(map[string]any{"status": domain.WorkerVersionDraft})); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.version_reverted_to_draft: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	return connect.NewResponse(&apiv1.RevertWorkerVersionToDraftResponse{}), nil
}

// maxBulkUpdateWorkerModel is the upper bound on ids per BulkUpdateWorkerModel
// call; mirrors BatchDeleteExecutions (internal/execution/service.go).
const maxBulkUpdateWorkerModel = 100

// BulkUpdateWorkerModel sets model_ref on every requested Worker and
// publishes the affected version in a single round trip. It mirrors the
// manual edit-then-republish flow:
//   - if the worker has a draft version, model_ref is updated in place
//     and the version is published;
//   - if the worker is published with no draft, the latest published
//     version is reverted to draft, model_ref is set, and it is republished.
//
// Version numbers do NOT advance (no fork); current_version follows the
// latest published version via UpdateWorkerCurrentVersion (same CAS as
// PublishWorkerVersion). Per-worker logic runs in its own tenant
// transaction so a failure in one row does not roll back the others —
// the contract is partial success. Each mutation writes a
// `worker.published` outbox row (one per affected worker) and a
// `worker.bulk_model_updated` audit row; the batch as a whole emits one
// `worker.bulk_model_updated_batch` audit row with the summary counts.
func (s *Service) BulkUpdateWorkerModel(ctx context.Context, req *connect.Request[apiv1.BulkUpdateWorkerModelRequest]) (*connect.Response[apiv1.BulkUpdateWorkerModelResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(req.Msg.WorkerIds) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_ids must not be empty"))
	}
	if len(req.Msg.WorkerIds) > maxBulkUpdateWorkerModel {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("max %d workers per batch", maxBulkUpdateWorkerModel))
	}
	// Structural bounds only here: grammar validation is per-worker inside
	// applyModelChange (the identical-ref no-op must be judged against each
	// worker's CURRENT ref — a batch re-save of workers that already carry
	// a legacy ref is a no-op for them, not a validation failure).
	modelRef := strings.TrimSpace(req.Msg.ModelRef)
	if modelRef == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model_ref must not be empty"))
	}
	if utf8.RuneCountInString(modelRef) > maxNameLen {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("model_ref must be at most %d characters", maxNameLen))
	}

	resp := &apiv1.BulkUpdateWorkerModelResponse{}
	for _, id := range req.Msg.WorkerIds {
		if id == "" {
			continue
		}
		result, err := s.bulkUpdateOneWorker(ctx, tenantID, id, modelRef)
		if err != nil {
			// Per-worker logic never returns a hard error — it converts
			// failures into an Error outcome. This branch is a defensive
			// fallback so a future change cannot accidentally abort the
			// whole batch.
			s.log.Warn("bulk_update_worker_model: per-worker outcome errored", "worker_id", id, "error", err)
			resp.Results = append(resp.Results, &apiv1.BulkUpdateWorkerModelResult{
				WorkerId: id,
				Outcome: &apiv1.BulkUpdateWorkerModelResult_Error{
					Error: &apiv1.BulkUpdateWorkerModelError{Message: err.Error()},
				},
			})
			resp.ErrorCount++
			continue
		}
		resp.Results = append(resp.Results, result)
		switch result.Outcome.(type) {
		case *apiv1.BulkUpdateWorkerModelResult_Updated:
			resp.UpdatedCount++
		case *apiv1.BulkUpdateWorkerModelResult_Skipped:
			resp.SkippedCount++
		case *apiv1.BulkUpdateWorkerModelResult_Error:
			resp.ErrorCount++
		}
	}

	if err := s.recordBulkAudit(ctx, tenantID, modelRef, req.Msg.WorkerIds, resp); err != nil {
		s.log.Warn("bulk_update_worker_model: batch audit row failed", "error", err)
	}

	s.log.Info("bulk_update_worker_model completed",
		"total", len(resp.Results),
		"updated", resp.UpdatedCount,
		"skipped", resp.SkippedCount,
		"errors", resp.ErrorCount,
		"model_ref", modelRef)
	return connect.NewResponse(resp), nil
}

// bulkUpdateOneWorker runs the per-worker update-or-revert-and-publish
// flow in its own tenant transaction. Failures are converted into
// per-worker outcomes (Skipped / Error) so the batch keeps going.
func (s *Service) bulkUpdateOneWorker(ctx context.Context, tenantID, workerID, modelRef string) (*apiv1.BulkUpdateWorkerModelResult, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	worker, err := db.GetWorker(ctx, ttx.Tx, tenantID, workerID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return skippedOutcome(workerID, apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NOT_FOUND), nil
		}
		return nil, fmt.Errorf("get worker: %w", err)
	}
	switch worker.Status {
	case domain.WorkerDeprecated:
		return skippedOutcome(workerID, apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_DEPRECATED), nil
	case domain.WorkerRetired:
		return skippedOutcome(workerID, apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_RETIRED), nil
	}

	latest, err := db.GetLatestWorkerVersion(ctx, ttx.Tx, tenantID, workerID, false)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return skippedOutcome(workerID, apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NO_PUBLISHED_VERSION), nil
		}
		return nil, fmt.Errorf("get latest worker version: %w", err)
	}

	updatedVer, err := s.applyModelChange(ctx, ttx.Tx, worker, latest, modelRef)
	if err != nil {
		return nil, err
	}

	if err := ttx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &apiv1.BulkUpdateWorkerModelResult{
		WorkerId: workerID,
		Outcome: &apiv1.BulkUpdateWorkerModelResult_Updated{
			Updated: &apiv1.BulkUpdateWorkerModelUpdated{
				Version:  int32(updatedVer.Version),
				ModelRef: updatedVer.ModelRef,
			},
		},
	}, nil
}

// applyModelChange applies the per-version update+publish flow:
//   - draft: set model_ref on the row in place, then publish.
//   - published: revert to draft, set model_ref, then republish.
//
// In both branches the version number is preserved and UpdateWorkerCurrentVersion
// is called so current_version follows the latest published version (mirrors
// PublishWorkerVersion).
func (s *Service) applyModelChange(ctx context.Context, tx pgx.Tx, worker db.WorkerRow, latest db.WorkerVersionRow, modelRef string) (db.WorkerVersionRow, error) {
	// Validation is per-worker here (not at the bulk request level): an
	// IDENTICAL re-save is a pure no-op — no grammar re-validation (legacy
	// refs re-save cleanly, ADR-0004 D5), no adapter-change gate. A
	// CHANGED ref is fully validated as a fresh selection.
	if strings.TrimSpace(latest.ModelRef) != strings.TrimSpace(modelRef) {
		if _, err := validateModelRef(ctx, worker.TenantID, modelRef); err != nil {
			return db.WorkerVersionRow{}, err
		}
		// ADR-0005 D4: an adapter CHANGE must land on a provider/model pair
		// valid for the NEW adapter. A violation becomes this worker's Error
		// outcome (the batch keeps going — partial-success contract).
		if err := validateAdapterChange(ctx, worker.TenantID, latest.ModelRef, modelRef); err != nil {
			return db.WorkerVersionRow{}, err
		}
	}
	var (
		before    db.WorkerVersionRow
		after     db.WorkerVersionRow
		auditVer  int
		eventType string
	)

	switch latest.Status {
	case domain.WorkerVersionDraft:
		before = latest
		merged := latest
		merged.ModelRef = modelRef
		updated, err := db.UpdateDraftVersion(ctx, tx, merged)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("update draft version: %w", err)
		}
		published, err := db.PublishWorkerVersion(ctx, tx, worker.TenantID, worker.ID, updated.Version)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("publish draft version: %w", err)
		}
		updatedWorker, err := db.UpdateWorkerCurrentVersion(ctx, tx, worker.TenantID, worker.ID, worker.Version, published.Version)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("update worker current_version: %w", err)
		}
		after = published
		auditVer = published.Version
		eventType = "worker.published"
		if err := enqueueWorkerEvent(ctx, tx, eventType, updatedWorker, published); err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("enqueue %s: %w", eventType, err)
		}
	case domain.WorkerVersionPublished:
		before = latest
		if err := db.RevertWorkerVersionToDraft(ctx, tx, worker.TenantID, latest.ID); err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("revert published version: %w", err)
		}
		// Re-read the (now-draft) row so the audit and outbox payloads
		// reflect the post-revert state.
		reverted, err := db.GetWorkerVersionByID(ctx, tx, worker.TenantID, worker.ID, latest.ID)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("re-read reverted version: %w", err)
		}
		merged := reverted
		merged.ModelRef = modelRef
		updatedDraft, err := db.UpdateDraftVersion(ctx, tx, merged)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("update reverted draft: %w", err)
		}
		published, err := db.PublishWorkerVersion(ctx, tx, worker.TenantID, worker.ID, updatedDraft.Version)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("republish: %w", err)
		}
		updatedWorker, err := db.UpdateWorkerCurrentVersion(ctx, tx, worker.TenantID, worker.ID, worker.Version, published.Version)
		if err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("update worker current_version: %w", err)
		}
		after = published
		auditVer = published.Version
		eventType = "worker.published"
		if err := enqueueWorkerEvent(ctx, tx, eventType, updatedWorker, published); err != nil {
			return db.WorkerVersionRow{}, fmt.Errorf("enqueue %s: %w", eventType, err)
		}
	default:
		return db.WorkerVersionRow{}, fmt.Errorf("unsupported latest version status %q for bulk update", latest.Status)
	}

	// Per-mutation audit row. Use the action suffix that matches the
	// single-worker publish flow so the granular trail is comparable.
	if err := recordAudit(ctx, tx, worker.TenantID, "worker.bulk_model_updated", "worker", worker.ID,
		audit.Snapshot(workerVersionAuditSnapshot(before)),
		audit.Snapshot(workerVersionAuditSnapshot(after))); err != nil {
		return db.WorkerVersionRow{}, fmt.Errorf("audit worker.bulk_model_updated: %w", err)
	}
	_ = auditVer
	return after, nil
}

// skippedOutcome is a small constructor that packages the per-worker
// Skipped outcome with its reason.
func skippedOutcome(workerID string, reason apiv1.BulkUpdateWorkerModelSkipReason) *apiv1.BulkUpdateWorkerModelResult {
	return &apiv1.BulkUpdateWorkerModelResult{
		WorkerId: workerID,
		Outcome: &apiv1.BulkUpdateWorkerModelResult_Skipped{
			Skipped: &apiv1.BulkUpdateWorkerModelSkipped{Reason: reason},
		},
	}
}

// recordBulkAudit writes a single audit row that ties the batch together.
// It runs in its own (very small) tenant transaction so a failure here
// never rolls back the per-worker work — the per-mutation audit rows
// already form the granular trail.
func (s *Service) recordBulkAudit(ctx context.Context, tenantID, modelRef string, ids []string, resp *apiv1.BulkUpdateWorkerModelResponse) error {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin audit tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.bulk_model_updated_batch", "worker", tenantID,
		nil,
		audit.Snapshot(map[string]any{
			"model_ref":     modelRef,
			"count":         len(ids),
			"updated_count": resp.UpdatedCount,
			"skipped_count": resp.SkippedCount,
			"error_count":   resp.ErrorCount,
		})); err != nil {
		return fmt.Errorf("audit worker.bulk_model_updated_batch: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit audit tx: %w", err)
	}
	return nil
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
	rows, err := db.ListWorkersWithActiveVersion(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkersResponse{}
	for _, r := range rows {
		item := &apiv1.WorkerListItem{
			Worker:              workerRowToProto(r.WorkerRow),
			ActiveModelRef:      r.ActiveModelRef,
			ActiveVersionStatus: workerVersionStatusToProto(r.ActiveVersionStatus),
			ActiveAdapter:       adapterKindOf(r.ActiveModelRef),
		}
		resp.Items = append(resp.Items, item)
		// Keep deprecated workers populated for wire-compat during rollout.
		resp.Workers = append(resp.Workers, item.Worker)
	}
	if len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].ID
	}
	// Enrich with worker categories (each response only carries its own target_type set).
	if cats, err := db.ListCategories(ctx, ttx.Tx, tenantID, "worker"); err == nil {
		for _, c := range cats {
			resp.Categories = append(resp.Categories, &apiv1.Category{Id: c.ID, TenantId: c.TenantID, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER, Name: c.Name, Description: c.Description, Slug: c.Slug, SortOrder: int32(c.SortOrder)})
		}
		if assigns, err := db.ListAssignments(ctx, ttx.Tx, tenantID, "worker"); err == nil {
			for _, a := range assigns {
				resp.Assignments = append(resp.Assignments, &apiv1.CategoryAssignment{EntityId: a.EntityID, CategoryId: a.CategoryID, TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER})
			}
		}
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

// GetWorkerVersion returns a single version by its id.
func (s *Service) GetWorkerVersion(ctx context.Context, req *connect.Request[apiv1.GetWorkerVersionRequest]) (*connect.Response[apiv1.GetWorkerVersionResponse], error) {
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
	v, err := db.GetWorkerVersionByID(ctx, ttx.Tx, tenantID, "", req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetWorkerVersionResponse{Version: versionRowToProto(v)}), nil
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
	if msg.ModelRef != nil || msg.Adapter != nil {
		// ADR-0005 D2: the explicit adapter input is a consistency
		// affordance — it must be a registered kind and must agree with
		// the resulting ref's adapter segment. It stays EMPTY unless the
		// caller explicitly set it (a model_ref-only change — the picker's
		// save path — never does), so the agreement check below only fires
		// on an explicit selection and a ref-only adapter change is judged
		// solely by the adapter-change validation above.
		var adapterSel string
		if msg.Adapter != nil {
			sel, err := validateAdapterInput(*msg.Adapter)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			adapterSel = sel
		}
		if msg.ModelRef != nil {
			// Identical re-save is a no-op (validateModelRefForUpdate);
			// a changed ref is fully validated, then adapter-change
			// validation runs against the merged value.
			modelRef, err := validateModelRefForUpdate(ctx, tenantID, current.ModelRef, *msg.ModelRef)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			merged.ModelRef = modelRef
			// ADR-0005 D4: an adapter CHANGE (parsed segment differs from
			// the current ref's) must land on a provider/model pair valid
			// for the NEW adapter — provider known for the kind, model
			// non-empty (the parser enforces segment non-emptiness).
			// Unchanged-adapter re-saves keep the ADR-0004 D5 semantics
			// verbatim.
			if err := validateAdapterChange(ctx, tenantID, current.ModelRef, merged.ModelRef); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
		// Agreement runs against the MERGED ref so an explicit adapter
		// with an unset ref checks against the version's current ref, and
		// a set+set pair checks against the incoming pair. An empty
		// adapter (input not sent) is a no-op — the ref alone defines the
		// selection.
		if err := validateAdapterRefAgreement(adapterSel, merged.ModelRef); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if msg.SystemPrompt != nil {
		merged.SystemPrompt = *msg.SystemPrompt
	}
	if msg.Role != nil {
		merged.Role = *msg.Role
	}
	if msg.Skills != nil {
		merged.Skills = *msg.Skills
	}
	if msg.Behavior != nil {
		merged.Behavior = *msg.Behavior
	}
	if msg.AgentsMd != nil {
		merged.AgentsMD = *msg.AgentsMd
	}
	// Structured fields are the source of truth: when any changed, recompose
	// the stored system_prompt from them so the DB column matches dispatch.
	// A legacy client that only sends system_prompt leaves the structured
	// fields untouched and the raw prompt is stored as-is.
	if msg.Role != nil || msg.Skills != nil || msg.Behavior != nil || msg.AgentsMd != nil {
		if merged.Role != "" || merged.Skills != "" || merged.Behavior != "" || merged.AgentsMD != "" {
			merged.SystemPrompt = composeWorkerPrompt(merged)
		} else if msg.SystemPrompt != nil {
			merged.SystemPrompt = *msg.SystemPrompt
		}
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.version_updated", "worker", msg.WorkerId,
		audit.Snapshot(workerVersionAuditSnapshot(current)), audit.Snapshot(workerVersionAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.version_updated: %w", err))
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
		Role:                source.Role,
		Skills:              source.Skills,
		Behavior:            source.Behavior,
		AgentsMD:            source.AgentsMD,
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
	if msg.ModelRef != nil || msg.Adapter != nil {
		// ADR-0005 D2: explicit adapter input must be a registered kind and
		// agree with the resulting ref's adapter segment (the source ref
		// when model_ref is unset). It stays EMPTY unless the caller
		// explicitly set it, so a model_ref-only adapter change — the
		// picker's save path — is judged solely by validateAdapterChange.
		var adapterSel string
		if msg.Adapter != nil {
			sel, err := validateAdapterInput(*msg.Adapter)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			adapterSel = sel
		}
		if msg.ModelRef != nil {
			modelRef, err := validateModelRefForUpdate(ctx, tenantID, source.ModelRef, *msg.ModelRef)
			if err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			newVer.ModelRef = modelRef
			// ADR-0005 D4: adapter-change validation against the source
			// version's ref (same contract as UpdateWorkerVersion).
			if err := validateAdapterChange(ctx, tenantID, source.ModelRef, newVer.ModelRef); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
		// Agreement runs against the MERGED ref (the source ref when
		// model_ref is unset). An empty adapter (input not sent) is a
		// no-op — the ref alone defines the selection.
		if err := validateAdapterRefAgreement(adapterSel, newVer.ModelRef); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if msg.SystemPrompt != nil {
		newVer.SystemPrompt = *msg.SystemPrompt
	}
	if msg.Role != nil {
		newVer.Role = *msg.Role
	}
	if msg.Skills != nil {
		newVer.Skills = *msg.Skills
	}
	if msg.Behavior != nil {
		newVer.Behavior = *msg.Behavior
	}
	if msg.AgentsMd != nil {
		newVer.AgentsMD = *msg.AgentsMd
	}
	if msg.Role != nil || msg.Skills != nil || msg.Behavior != nil || msg.AgentsMd != nil {
		if newVer.Role != "" || newVer.Skills != "" || newVer.Behavior != "" || newVer.AgentsMD != "" {
			newVer.SystemPrompt = composeWorkerPrompt(newVer)
		} else if msg.SystemPrompt != nil {
			newVer.SystemPrompt = *msg.SystemPrompt
		}
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.version_created", "worker", msg.WorkerId,
		nil, audit.Snapshot(workerVersionAuditSnapshot(created))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.version_created: %w", err))
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
	if acquired {
		if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.edit_lock_acquired", "worker", req.Msg.WorkerId,
			nil, audit.Snapshot(map[string]any{"held_by": lock.HeldBy, "expires_at": lock.ExpiresAt})); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.edit_lock_acquired: %w", err))
		}
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "worker.edit_lock_released", "worker", req.Msg.WorkerId,
		audit.Snapshot(map[string]any{"held_by": actor}), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit worker.edit_lock_released: %w", err))
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

// recordAudit writes an actor-based audit row in the caller's tx,
// resolving the actor from the request context. Must be called in the
// same transaction as the mutation so the row commits atomically.
func recordAudit(ctx context.Context, tx pgx.Tx, tenantID, action, targetType, targetID string, before, after json.RawMessage) error {
	e := auth.ActorFromContext(ctx)
	if e.TenantID == "" {
		e.TenantID = tenantID
	}
	e.Action = action
	e.TargetType = targetType
	e.TargetID = targetID
	e.Before = before
	e.After = after
	return audit.Record(ctx, tx, e)
}

// workerAuditSnapshot is the non-secret projection of a worker header
// for the audit trail.
func workerAuditSnapshot(w db.WorkerRow) map[string]any {
	return map[string]any{
		"id":       w.ID,
		"name":     w.Name,
		"slug":     w.Slug,
		"status":   w.Status,
		"role_ref": w.RoleRef,
		"version":  w.Version,
	}
}

// workerVersionAuditSnapshot is the non-secret projection of a worker
// version for the audit trail. The composed system_prompt can embed
// secrets (workers carry repo secrets); exclude it from snapshots.
func workerVersionAuditSnapshot(v db.WorkerVersionRow) map[string]any {
	return map[string]any{
		"id":        v.ID,
		"worker_id": v.WorkerID,
		"version":   v.Version,
		"status":    v.Status,
		"model_ref": v.ModelRef,
	}
}

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

// uniqueSlug returns a tenant-unique slug for the requested slug, appending
// "-2", "-3", ... until the workers_tenant_slug_idx constraint would accept
// it. Guards clones and re-created workers against slug collisions.
func uniqueSlug(ctx context.Context, tx pgx.Tx, tenantID, slug string) (string, error) {
	if slug == "" {
		return "", nil
	}
	exists, err := db.WorkerSlugExists(ctx, tx, tenantID, slug)
	if err != nil {
		return "", err
	}
	if !exists {
		return slug, nil
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", slug, i)
		exists, err := db.WorkerSlugExists(ctx, tx, tenantID, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not allocate a unique slug for %q", slug)
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
		RoleRef:        w.RoleRef,
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
		Id:                  v.ID,
		WorkerId:            v.WorkerID,
		Version:             int32(v.Version),
		VersionNote:         v.VersionNote,
		Status:              workerVersionStatusToProto(v.Status),
		RuntimeRef:          v.RuntimeRef,
		ModelRef:            v.ModelRef,
		Adapter:             adapterKindOf(v.ModelRef),
		SystemPrompt:        composeWorkerPrompt(v),
		Role:                v.Role,
		Skills:              v.Skills,
		Behavior:            v.Behavior,
		AgentsMd:            v.AgentsMD,
		ContextSources:      string(v.ContextSources),
		Permissions:         string(v.Permissions),
		GatedTools:          string(v.GatedTools),
		BudgetOverrides:     string(v.BudgetOverrides),
		ExecutionPolicyRef:  v.ExecutionPolicyRef,
		ConcurrencyLimit:    int32(v.ConcurrencyLimit),
		RecoveryWorkflowRef: v.RecoveryWorkflowRef,
		Labels:              string(v.Labels),
		CreatedAt:           timestamppb.New(v.CreatedAt),
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
