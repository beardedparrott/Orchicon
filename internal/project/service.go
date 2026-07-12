package project

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
	"github.com/jackc/pgx/v5"
)

// Service implements the ProjectService Connect handler
// (apiv1connect.ProjectServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3); the relay publishes it to NATS asynchronously.
type Service struct {
	pool *db.Pool
	log  *slog.Logger
	apiv1connect.UnimplementedProjectServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.ProjectServiceHandler = (*Service)(nil)

// New constructs a ProjectService handler.
func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// CreateProject validates input, inserts the project, and enqueues a
// project.created event — all in one tenant-scoped transaction.
func (s *Service) CreateProject(ctx context.Context, req *connect.Request[apiv1.CreateProjectRequest]) (*connect.Response[apiv1.CreateProjectResponse], error) {
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
	goals, err := validateGoals(msg.Goals)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	row := db.ProjectRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		Name:     name,
		Slug:     slug,
		Status:   domain.ProjectDrafting,
		Goals:    goals,
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	created, err := db.CreateProject(ctx, ttx.Tx, row)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.created", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project created", "id", created.ID, "tenant", tenantID, "slug", slug)
	return connect.NewResponse(&apiv1.CreateProjectResponse{Project: rowToProto(created)}), nil
}

// GetProject returns a single project by id within the tenant scope.
func (s *Service) GetProject(ctx context.Context, req *connect.Request[apiv1.GetProjectRequest]) (*connect.Response[apiv1.GetProjectResponse], error) {
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
	p, err := db.GetProject(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetProjectResponse{Project: rowToProto(p)}), nil
}

// ListProjects returns a page of projects for the tenant. Deleted
// projects are excluded by default. Pagination is cursor-based on ULID
// id ordering (docs/07 §5.2).
func (s *Service) ListProjects(ctx context.Context, req *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	projects, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
		TenantID:        tenantID,
		ExcludeStatuses: []string{domain.ProjectDeleted},
		PageSize:        int(req.Msg.PageSize),
		AfterID:         req.Msg.PageToken,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListProjectsResponse{}
	for _, p := range projects {
		resp.Projects = append(resp.Projects, rowToProto(p))
	}
	if len(projects) > 0 {
		resp.NextPageToken = projects[len(projects)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// UpdateProject applies a partial update with optimistic concurrency.
// Only name and goals are mutable; slug is immutable after creation
// (it appears in external references).
func (s *Service) UpdateProject(ctx context.Context, req *connect.Request[apiv1.UpdateProjectRequest]) (*connect.Response[apiv1.UpdateProjectResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	var fields db.UpdateProjectFields
	if msg.Name != nil {
		name, err := validateName(*msg.Name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Name = &name
	}
	if msg.Goals != nil {
		goals, err := validateGoals(*msg.Goals)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Goals = &goals
	}
	if fields.Name == nil && fields.Goals == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("at least one field must be set"))
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Read current version for optimistic concurrency.
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	updated, err := db.UpdateProject(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.updated", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project updated", "id", updated.ID, "version", updated.Version)
	return connect.NewResponse(&apiv1.UpdateProjectResponse{Project: rowToProto(updated)}), nil
}

// ArchiveProject transitions a project to archived status.
func (s *Service) ArchiveProject(ctx context.Context, req *connect.Request[apiv1.ArchiveProjectRequest]) (*connect.Response[apiv1.ArchiveProjectResponse], error) {
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
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	archived, err := db.ArchiveProject(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.archived", archived); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project archived", "id", archived.ID)
	return connect.NewResponse(&apiv1.ArchiveProjectResponse{Project: rowToProto(archived)}), nil
}

// PauseProject is a lifecycle transition. It reuses the update path to
// set status=paused. (Full lifecycle handling arrives with the
// reconciler framework in Phase 3.)
func (s *Service) PauseProject(ctx context.Context, req *connect.Request[apiv1.PauseProjectRequest]) (*connect.Response[apiv1.PauseProjectResponse], error) {
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
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Direct status CAS to paused; version bump is handled by the query.
	// tenant_id is in the WHERE clause as the primary isolation layer.
	const q = `UPDATE projects SET status = 'paused', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at`
	var p db.ProjectRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pause project: %w", err))
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.paused", p); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project paused", "id", p.ID)
	return connect.NewResponse(&apiv1.PauseProjectResponse{Project: rowToProto(p)}), nil
}

// StreamProjectEvents is not yet implemented; it arrives with the
// realtime + infrastructure phase (Phase 3) when the useStream hook and
// NATS fan-out are built.
func (s *Service) StreamProjectEvents(ctx context.Context, req *connect.Request[apiv1.StreamProjectEventsRequest], stream *connect.ServerStream[apiv1.StreamProjectEventsResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("StreamProjectEvents arrives in Phase 3"))
}

// enqueueProjectEvent builds a ProjectEvent envelope, encodes it as JSON,
// and enqueues it in the outbox within the current transaction. The
// relay publishes it to NATS asynchronously.
func enqueueProjectEvent(ctx context.Context, tx pgx.Tx, eventType string, p db.ProjectRow) error {
	payload, err := buildEventPayload(eventType, p)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      p.TenantID,
		EventType:     eventType,
		AggregateType: "project",
		AggregateID:   p.ID,
		AggregateVer:  p.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// buildEventPayload returns the JSON-encoded ProjectEvent envelope that
// gets stored in the outbox and published to NATS.
func buildEventPayload(eventType string, p db.ProjectRow) ([]byte, error) {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       p.TenantID,
		"project_id":      p.ID,
		"aggregate_type":  "project",
		"aggregate_id":    p.ID,
		"aggregate_version": p.Version,
		"status":          p.Status,
		"name":            p.Name,
		"slug":            p.Slug,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal event payload: %w", err)
	}
	return b, nil
}

// mapDBError translates a data-access error into a Connect error code.
// ErrNotFound → NotFound; everything else is Internal (the data-access
// layer does not surface business-logic errors — those are caught at the
// service layer before the query runs).
func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}
