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
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the ProjectService Connect handler
// (apiv1connect.ProjectServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3); the relay publishes it to NATS asynchronously.
type Service struct {
	pool       *db.Pool
	log        *slog.Logger
	subscriber eventbus.Subscriber
	apiv1connect.UnimplementedProjectServiceHandler
}

// Compile-time assertion that Service satisfies the handler interface.
var _ apiv1connect.ProjectServiceHandler = (*Service)(nil)

// New constructs a ProjectService handler.
func New(pool *db.Pool, log *slog.Logger, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, log: log, subscriber: sub}
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
	goals, err := convertGoalsToJSON(msg.Goals)
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

// ListProjects returns a page of projects for the tenant with optional
// search, status filter, and sort. Deleted projects are excluded by
// default. Pagination is cursor-based on ULID id ordering (docs/07 §5.2).
func (s *Service) ListProjects(ctx context.Context, req *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := db.ListProjectsFilter{
		TenantID:        tenantID,
		ExcludeStatuses: []string{domain.ProjectDeleted},
		PageSize:        int(req.Msg.PageSize),
		AfterID:         req.Msg.PageToken,
		Search:          req.Msg.Search,
		SortBy:          req.Msg.SortBy,
		SortOrder:       req.Msg.SortOrder,
	}
	if req.Msg.Status != nil {
		f.Status = statusFromProto(*req.Msg.Status)
	} else {
		f.ExcludeStatuses = []string{domain.ProjectDeleted}
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	projects, err := db.ListProjects(ctx, ttx.Tx, f)
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
// Supports name, slug, goals, project_dir, and context_files.
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
	if msg.Slug != nil {
		slug, err := validateSlug(*msg.Slug)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Slug = &slug
	}
	if msg.Goals != nil {
		goals, err := convertGoalsToJSON(msg.Goals.Fields)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Goals = &goals
	}
	if msg.ProjectDir != nil {
		dir, err := validateProjectDir(*msg.ProjectDir)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.ProjectDir = &dir
	}
	if msg.ContextFiles != nil {
		files := msg.ContextFiles.Files
		if err := validateContextFiles(files); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filesJSON, err := contextFilesToJSON(files)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		fields.ContextFiles = &filesJSON
	}
	if fields.Name == nil && fields.Slug == nil && fields.Goals == nil && fields.ProjectDir == nil && fields.ContextFiles == nil {
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

// DeleteProject hard-deletes a project and cascades to owned entities
// (work items, workflows, workflow versions, runs, step runs).
func (s *Service) DeleteProject(ctx context.Context, req *connect.Request[apiv1.DeleteProjectRequest]) (*connect.Response[apiv1.DeleteProjectResponse], error) {
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
	// Check project exists before deleting
	if _, err := db.GetProject(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteProject(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project deleted", "id", req.Msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteProjectResponse{}), nil
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
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at,
			project_dir, context_files`
	var p db.ProjectRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles,
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

// ActivateProject transitions a drafting project to active status.
// CAS on version to prevent lost updates.
func (s *Service) ActivateProject(ctx context.Context, req *connect.Request[apiv1.ActivateProjectRequest]) (*connect.Response[apiv1.ActivateProjectResponse], error) {
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
	const q = `UPDATE projects SET status = 'active', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3 AND status = 'drafting'
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at,
			project_dir, context_files`
	var p db.ProjectRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the version was stale or the project is not drafting.
		if current.Status != domain.ProjectDrafting {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("project status is %q, must be 'drafting' to activate", current.Status))
		}
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("activate project: %w", err))
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.activated", p); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project activated", "id", p.ID)
	return connect.NewResponse(&apiv1.ActivateProjectResponse{Project: rowToProto(p)}), nil
}

// StreamProjectEvents is the server-stream RPC that fans out project
// events from NATS to connected clients (docs/07 §4, docs/10 §4). It
// subscribes to the orchicon.events.project.* subject filter and streams
// each event as a StreamProjectEventsResponse. If from_sequence is
// provided, the consumer resumes from that JetStream sequence
// (docs/07 §4: resume after reconnect).
//
// The stream stays open until the client disconnects (context
// cancelled) or the subscriber is unavailable. When NATS is down, the
// RPC returns Unavailable (docs/08 §8: "frontend realtime degrades —
// reconnect on recovery").
func (s *Service) StreamProjectEvents(ctx context.Context, req *connect.Request[apiv1.StreamProjectEventsRequest], stream *connect.ServerStream[apiv1.StreamProjectEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}

	// Subject filter: project events only. If project_id is specified,
	// the client filters further on its side (the NATS subject does
	// not encode project_id in v0.1 — events carry it in the payload).
	filter := "orchicon.events.project.>"

	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}

	ch, err := s.subscriber.Subscribe(ctx, filter, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to project events: %w", err))
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				// Channel closed — subscriber shut down. Return nil so
				// the client can reconnect with backoff (docs/10 §4).
				return nil
			}
			evt, err := parseProjectEvent(msg.Data)
			if err != nil {
				s.log.Warn("failed to parse project event", "subject", msg.Subject, "error", err)
				continue
			}
			resp := &apiv1.StreamProjectEventsResponse{
				Event:    evt,
				Sequence: int64(msg.Seq),
			}
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// ListProjectFiles returns the immediate children of a directory.
// When dir_path is set, lists that directory directly (filesystem
// browsing mode). Otherwise uses the project's project_dir + subpath.
func (s *Service) ListProjectFiles(ctx context.Context, req *connect.Request[apiv1.ListProjectFilesRequest]) (*connect.Response[apiv1.ListProjectFilesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var rootDir string

	// dir_path bypasses project lookup — direct filesystem browse mode.
	if req.Msg.DirPath != "" {
		rootDir, err = validateProjectDir(req.Msg.DirPath)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	} else {
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
		if p.ProjectDir == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("project_dir is not set on this project"))
		}
		if err := ttx.Commit(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
		}
		rootDir = p.ProjectDir
	}

	parentPath, dirName, entries, err := listDirectory(rootDir, req.Msg.Subpath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list directory: %w", err))
	}
	return connect.NewResponse(&apiv1.ListProjectFilesResponse{
		ParentPath: parentPath,
		DirName:    dirName,
		Entries:    entries,
	}), nil
}

// parseProjectEvent decodes the JSON event payload from the outbox/NATS
// into a ProjectEvent proto message. The payload is the envelope written
// by buildEventPayload.
func parseProjectEvent(data []byte) (*apiv1.ProjectEvent, error) {
	var env struct {
		EventType    string `json:"event_type"`
		TenantID     string `json:"tenant_id"`
		ProjectID    string `json:"project_id"`
		AggregateVer int    `json:"aggregate_version"`
		Status       string `json:"status"`
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		OccurredAt   string `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse project event: %w", err)
	}
	evt := &apiv1.ProjectEvent{
		EventId:   "", // event_id is the outbox row id, not in the payload
		EventType: env.EventType,
		TenantId:  env.TenantID,
		ProjectId: env.ProjectID,
		Payload:   data,
	}
	if t, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err == nil {
		evt.OccurredAt = timestamppb.New(t)
	}
	return evt, nil
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
