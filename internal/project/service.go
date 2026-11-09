package project

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
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the ProjectService Connect handler (apiv1connect.ProjectServiceHandler).
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
	// execution_mode defaults to local when the caller sends no mode; an
	// unknown value is rejected at this boundary rather than coerced.
	executionMode, err := executionModeFromProto(msg.GetExecutionMode(), db.DefaultExecutionMode)
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
		GitStrategy: func() string {
			if fd := msg.ProtoReflect().Descriptor().Fields().ByName("git_strategy"); fd != nil && msg.ProtoReflect().Has(fd) {
				switch msg.ProtoReflect().Get(fd).Enum() {
				case 1:
					return "local"
				case 2:
					return "pr"
				case 3:
					return "none"
				}
			}
			// fallback: check goals for __git_strategy (when proto not yet regenerated)
			if len(msg.Goals) > 0 {
				for _, gf := range msg.Goals {
					if gf.Key == "__git_strategy" {
						if gf.Value == "local" || gf.Value == "pr" || gf.Value == "none" {
							return gf.Value
						}
					}
				}
			}
			return "local"
		}(),
		// Project-level runtime default (auto-fills work items on create).
		// Empty = inherit tenant/base.
		DefaultRuntimeImage: func() *string {
			if v := strings.TrimSpace(msg.GetDefaultRuntimeImage()); v != "" {
				return &v
			}
			return nil
		}(),
		ExecutionMode: executionMode,
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.created", "project", created.ID, nil, audit.Snapshot(projectAuditSnapshot(created))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.created: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.ensureProjectGitState(ctx, created)
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

// ListProjects returns a page of projects for the tenant with optional search, status filter, and
// sort.
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
	if msg.SkillFiles != nil {
		files := msg.SkillFiles.Files
		if err := validateContextFiles(files); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filesJSON, err := contextFilesToJSON(files)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		fields.SkillFiles = &filesJSON
	}
	if msg.MaxConcurrentRuns != nil {
		limit := int(*msg.MaxConcurrentRuns)
		if limit < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("max_concurrent_runs must be >= 0"))
		}
		fields.MaxConcurrentRuns = &limit
	}
	// SETTING A PROJECT TO LOCAL CLEARS ITS DEFAULT RUNTIME IMAGE — the same rule the work item
	// follows, and for the same reason: a local run executes on the host.
	clearImageForLocal := false
	if msg.ExecutionMode != nil {
		if m, merr := executionModeFromProto(*msg.ExecutionMode, ""); merr == nil && m == db.ExecutionModeLocal {
			clearImageForLocal = true
		}
	}
	if clearImageForLocal {
		empty := ""
		fields.DefaultRuntimeImage = &empty
	}
	if msg.DefaultRuntimeImage != nil && !clearImageForLocal {
		v := strings.TrimSpace(*msg.DefaultRuntimeImage)
		fields.DefaultRuntimeImage = &v
	}
	if msg.ExecutionMode != nil {
		// A present mode is explicit: UNSPECIFIED or an unknown enum value is a client bug, rejected with
		// the shared message.
		m, err := executionModeFromProto(*msg.ExecutionMode, "")
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.ExecutionMode = &m
	}
	if fd := msg.ProtoReflect().Descriptor().Fields().ByName("git_strategy"); fd != nil && msg.ProtoReflect().Has(fd) {
		enumVal := msg.ProtoReflect().Get(fd).Enum()
		var s string
		switch enumVal {
		case 1:
			s = "local"
		case 2:
			s = "pr"
		case 3:
			s = "none"
		default:
			s = "local"
		}
		fields.GitStrategy = &s
	} else if msg.Goals != nil {
		for _, gf := range msg.Goals.Fields {
			if gf.Key == "__git_strategy" && (gf.Value == "local" || gf.Value == "pr" || gf.Value == "none") {
				fields.GitStrategy = &gf.Value
				// remove the pseudo-goal from goals so it doesn't persist as a real goal
				filtered := make([]*apiv1.GoalField, 0, len(msg.Goals.Fields))
				for _, g := range msg.Goals.Fields {
					if g.Key != "__git_strategy" {
						filtered = append(filtered, g)
					}
				}
				if len(filtered) != len(msg.Goals.Fields) {
					goals, _ := convertGoalsToJSON(filtered)
					fields.Goals = &goals
				}
				break
			}
		}
	}
	if fields.Name == nil && fields.Slug == nil && fields.Goals == nil && fields.ProjectDir == nil && fields.ContextFiles == nil && fields.SkillFiles == nil && fields.MaxConcurrentRuns == nil && fields.GitStrategy == nil && fields.DefaultRuntimeImage == nil && fields.ExecutionMode == nil {
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
	// Context files must live inside the (effective) project directory — the only path guaranteed to be
	// mounted where workers run.
	if msg.ContextFiles != nil {
		effDir := current.ProjectDir
		if fields.ProjectDir != nil {
			effDir = *fields.ProjectDir
		}
		if err := contextfiles.ValidateWithin(msg.ContextFiles.Files, effDir); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	// Skill files are held to the SAME boundary as context files, and for the same reason: the project
	// directory is the only directory mounted into a worker's container.
	if msg.SkillFiles != nil {
		effDir := current.ProjectDir
		if fields.ProjectDir != nil {
			effDir = *fields.ProjectDir
		}
		if err := contextfiles.ValidateWithin(msg.SkillFiles.Files, effDir); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	updated, err := db.UpdateProject(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueProjectEvent(ctx, ttx.Tx, "project.updated", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.updated", "project", updated.ID,
		audit.Snapshot(projectAuditSnapshot(current)), audit.Snapshot(projectAuditSnapshot(updated))); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.updated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	// A SAVED PROJECT IS RE-CHECKED, not only a new one.
	s.ensureProjectGitState(ctx, updated)
	s.log.Info("project updated", "id", updated.ID, "version", updated.Version)
	// Project dir / context files changed → refresh the container mount
	// manifest immediately (the periodic writer is a safety net).
	if fields.ProjectDir != nil || fields.ContextFiles != nil || fields.SkillFiles != nil {
		notifyProjectChanged()
	}
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.archived", "project", archived.ID,
		audit.SnapshotStatus(current.Status), audit.SnapshotStatus(archived.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.archived: %w", err))
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
	current, err := db.GetProject(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.DeleteProject(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.deleted", "project", current.ID,
		audit.Snapshot(projectAuditSnapshot(current)), nil); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.deleted: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project deleted", "id", req.Msg.Id, "tenant", tenantID)
	return connect.NewResponse(&apiv1.DeleteProjectResponse{}), nil
}

// PauseProject is a lifecycle transition.
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
			project_dir, context_files, skill_files, max_concurrent_runs`
	var p db.ProjectRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.SkillFiles, &p.MaxConcurrentRuns,
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.paused", "project", p.ID,
		audit.SnapshotStatus(current.Status), audit.SnapshotStatus(p.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.paused: %w", err))
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
			project_dir, context_files, skill_files, max_concurrent_runs`
	var p db.ProjectRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.SkillFiles, &p.MaxConcurrentRuns,
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
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.activated", "project", p.ID,
		audit.SnapshotStatus(current.Status), audit.SnapshotStatus(p.Status)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit project.activated: %w", err))
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("project activated", "id", p.ID)
	return connect.NewResponse(&apiv1.ActivateProjectResponse{Project: rowToProto(p)}), nil
}

// StreamProjectEvents is the server-stream RPC that fans out project events from NATS to connected
// clients (docs/07 §4, docs/10 §4).
func (s *Service) StreamProjectEvents(ctx context.Context, req *connect.Request[apiv1.StreamProjectEventsRequest], stream *connect.ServerStream[apiv1.StreamProjectEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}

	// Subject filter: project events only.
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

// parseProjectEvent decodes the JSON event payload from the outbox/NATS into a ProjectEvent proto
// message.
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

// enqueueProjectEvent builds a ProjectEvent envelope, encodes it as JSON, and enqueues it in the
// outbox within the current transaction.
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
		"event_type":        eventType,
		"tenant_id":         p.TenantID,
		"project_id":        p.ID,
		"aggregate_type":    "project",
		"aggregate_id":      p.ID,
		"aggregate_version": p.Version,
		"status":            p.Status,
		"name":              p.Name,
		"slug":              p.Slug,
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal event payload: %w", err)
	}
	return b, nil
}

// recordAudit writes an actor-based audit row in the caller's tx, resolving the actor from the
// request context.
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

// projectAuditSnapshot is the non-secret projection of a project row for the audit trail.
func projectAuditSnapshot(p db.ProjectRow) map[string]any {
	return map[string]any{
		"id":                    p.ID,
		"name":                  p.Name,
		"slug":                  p.Slug,
		"status":                p.Status,
		"version":               p.Version,
		"default_runtime_image": stringOrEmptyAudit(p.DefaultRuntimeImage),
		"execution_mode":        p.ExecutionMode,
	}
}

// stringOrEmptyAudit dereferences a nullable string for audit snapshots.
func stringOrEmptyAudit(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// mapDBError translates a data-access error into a Connect error code.
func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// executionModeFromProto maps a wire execution mode to the stored string.
func executionModeFromProto(v apiv1.ExecutionMode, unset string) (string, error) {
	switch v {
	case apiv1.ExecutionMode_EXECUTION_MODE_LOCAL:
		return db.ExecutionModeLocal, nil
	case apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME:
		return db.ExecutionModeRuntime, nil
	case apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED:
		if unset != "" {
			return unset, nil
		}
	}
	return "", errors.New(db.ExecutionModeInvalidMsg)
}

// ensureProjectGitState runs the git-state setup for a saved project and REPORTS what it did.
func (s *Service) ensureProjectGitState(ctx context.Context, proj db.ProjectRow) {
	rep := EnsureGitState(ctx, proj.ProjectDir, proj.GitStrategy)

	// THE LOG ALWAYS GETS THE DETAIL. It is the durable diagnostic — the transcript notice below is capped to a
	// sentence so it stays readable in a conversation.
	switch {
	case rep.GitMissing:
		s.log.Error("project: git is not installed on the control-plane host — nothing about this project's "+
			"repository could be checked, and its git-backed runs cannot start",
			"id", proj.ID, "project_dir", proj.ProjectDir, "strategy", proj.GitStrategy, "error", rep.Err)
	case rep.Err != nil:
		s.log.Warn("project: git setup could not complete — the project was saved, and its git-backed runs "+
			"will fail until the repository is usable",
			"id", proj.ID, "project_dir", proj.ProjectDir, "strategy", proj.GitStrategy, "error", rep.Err)
	case rep.Warning != "":
		s.log.Warn("project: git setup left a condition that breaks what this strategy promises",
			"id", proj.ID, "project_dir", proj.ProjectDir, "strategy", proj.GitStrategy,
			"warning", rep.Warning, "done", rep.Done)
	case rep.Needed:
		s.log.Info("project: git state set up for git-backed runs",
			"id", proj.ID, "project_dir", proj.ProjectDir, "strategy", proj.GitStrategy, "done", rep.Done)
	default:
		s.log.Debug("project: git state already usable", "id", proj.ID, "detail", rep.Skipped)
		return
	}

	// AND THE OPERATOR IS TOLD, IN THE TRANSCRIPT, WHENEVER IT MATTERS.
	if rep.Loud() {
		msg := "Git setup for this project could not finish, and its git-backed runs will not work as chosen: " +
			rep.Summary()
		if err := s.enqueueProjectNotice(ctx, proj, msg); err != nil {
			s.log.Warn("project: git setup notice was not recorded — the outcome stands, but the project's event "+
				"stream will not show it", "id", proj.ID, "error", err)
		}
		return
	}
	if !rep.Needed {
		return
	}
	// A successful setup is reported too — but as the ordinary, informational record it is.
	msg := "Git setup for this project: " + rep.Summary()
	if err := s.enqueueProjectNotice(ctx, proj, msg); err != nil {
		s.log.Warn("project: git setup notice was not recorded — the setup stands, but the project's event "+
			"stream will not show it", "id", proj.ID, "error", err)
	}
}

// enqueueProjectNotice writes one project-scoped event carrying a notice, in its own transaction.
func (s *Service) enqueueProjectNotice(ctx context.Context, proj db.ProjectRow, msg string) error {
	tenantID := proj.TenantID
	if tenantID == "" || proj.ID == "" {
		return nil
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	if err := enqueueProjectEventWithNotice(ctx, ttx.Tx, "project.git_setup", proj, msg); err != nil {
		return err
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "project.git_setup", "project", proj.ID, nil,
		audit.Snapshot(map[string]any{"detail": msg})); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// enqueueProjectEventWithNotice is enqueueProjectEvent with an extra top-level "notice" field in the payload,
// for events whose whole content is something the operator needs to read.
func enqueueProjectEventWithNotice(ctx context.Context, tx pgx.Tx, eventType string, p db.ProjectRow, notice string) error {
	payload, err := buildEventPayload(eventType, p)
	if err != nil {
		return err
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return err
	}
	envelope["notice"] = notice
	payload, err = json.Marshal(envelope)
	if err != nil {
		return err
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      p.TenantID,
		EventType:     eventType,
		AggregateType: "project",
		AggregateID:   p.ID,
		AggregateVer:  p.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}
