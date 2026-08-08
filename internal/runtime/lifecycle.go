package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
)

// devTenantID mirrors the single-dev-tenant assumption used by the
// reconcilers (docs/03 §1). Multi-tenant scheduling arrives with auth.
const devTenantID = "tnt_dev"

// Lifecycle decides WHEN workflow runtime containers are created and
// reaped based on workflow run state. It talks to the host-side daemon
// through runtime.Client. A nil client (no daemon socket — headless
// `orchicon serve`) makes every operation a no-op so the control plane
// degrades to in-process execution.
type Lifecycle struct {
	client *Client
	pool   *db.Pool
	log    *slog.Logger
}

// NewLifecycle creates a workflow runtime lifecycle. client may be nil
// to disable per-workflow runtime containers.
func NewLifecycle(client *Client, pool *db.Pool, log *slog.Logger) *Lifecycle {
	return &Lifecycle{client: client, pool: pool, log: log}
}

// Enabled reports whether a daemon is configured.
func (l *Lifecycle) Enabled() bool { return l.client != nil }

// EnsureForRun creates the runtime container for a workflow run
// (idempotent). The run's project directory is mounted if the project
// has one; the project's and run's work item's context_files paths that
// lie OUTSIDE project_dir are additionally bind-mounted (paths under
// project_dir are already covered by the project-dir mount). Otherwise
// the container is created with just the standard home mounts (opencode
// config/auth, git identity). Executions dispatch into this container
// for the whole lifetime of the run.
func (l *Lifecycle) EnsureForRun(ctx context.Context, run db.WorkflowRunRow) error {
	if l.client == nil {
		return nil
	}
	if !l.client.Ready(ctx) {
		return fmt.Errorf("runtime daemon not reachable")
	}
	var mounts []MountSpec
	projectDir := ""
	if run.ProjectID != "" {
		ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
		if err != nil {
			return fmt.Errorf("ensure runtime: begin tx: %w", err)
		}
		project, gerr := db.GetProject(ctx, ttx.Tx, run.TenantID, run.ProjectID)
		if gerr == nil {
			projectDir = project.ProjectDir
			if projectDir != "" {
				mounts = append(mounts, MountSpec{Source: projectDir, Dest: projectDir})
			}
			// Project context files/directories outside project_dir.
			mounts = append(mounts, contextMounts(project.ContextFiles, projectDir)...)
			// The run's bound work item's context files/directories.
			if run.WorkItemID != "" {
				if wi, werr := db.GetWorkItem(ctx, ttx.Tx, run.TenantID, run.WorkItemID); werr == nil {
					mounts = append(mounts, contextMounts(wi.ContextFiles, projectDir)...)
				}
			}
		}
		_ = ttx.Rollback(ctx)
		if gerr != nil && gerr != db.ErrNotFound {
			return fmt.Errorf("ensure runtime: get project: %w", gerr)
		}
	}
	// Resolve the image: the run's runtime_image (captured at run start)
	// or the daemon default (empty -> base). Empty means the daemon's
	// configured base image.
	image := run.RuntimeImage
	if _, err := l.client.Create(ctx, CreateRequest{WorkflowID: run.ID, Image: image, Mounts: mounts}); err != nil {
		return fmt.Errorf("ensure runtime for run %s: %w", run.ID, err)
	}
	l.log.Info("workflow runtime ensured", "run", run.ID, "project", run.ProjectID, "image", image, "mounts", len(mounts))
	return nil
}

// contextMounts converts a context_files JSONB payload into Docker
// bind-mount specs, skipping paths that lie under projectDir (already
// covered by the project-dir mount) and any path that isn't absolute.
// A context path may be a file or a directory; individual file
// bind-mounts are legal in Docker, so the same shape works for both.
func contextMounts(ctxFiles []byte, projectDir string) []MountSpec {
	var out []MountSpec
	if len(ctxFiles) == 0 {
		return out
	}
	var files []string
	if err := json.Unmarshal(ctxFiles, &files); err != nil {
		return out
	}
	for _, f := range files {
		resolved := strings.TrimSpace(f)
		if !filepath.IsAbs(resolved) {
			continue
		}
		// Paths under project_dir are already mounted via the project-dir
		// mount — don't double-mount them.
		if projectDir != "" {
			rel, err := filepath.Rel(projectDir, resolved)
			if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				continue
			}
		}
		out = append(out, MountSpec{Source: resolved, Dest: resolved})
	}
	return out
}

// ReapForRun removes the runtime container for a run that reached a
// terminal state (completed/failed/aborted). Idempotent.
func (l *Lifecycle) ReapForRun(ctx context.Context, runID string) error {
	if l.client == nil {
		return nil
	}
	if err := l.client.Kill(ctx, runID); err != nil {
		l.log.Warn("reap runtime failed", "run", runID, "error", err)
		return err
	}
	l.log.Info("workflow runtime reaped", "run", runID)
	return nil
}

// Adopt reconciles the daemon's runtime containers with the set of
// active (pending/running/paused) workflow runs: it kills containers
// whose run is no longer active (orphans — e.g. after a plane crash or a
// run aborted while the plane was down) and ensures containers exist for
// every active run. Called once at boot.
func (l *Lifecycle) Adopt(ctx context.Context) error {
	if l.client == nil {
		return nil
	}
	containers, err := l.client.List(ctx)
	if err != nil {
		return fmt.Errorf("adopt: list runtimes: %w", err)
	}
	ttx, err := l.pool.BeginTenantTx(ctx, devTenantID)
	if err != nil {
		return fmt.Errorf("adopt: begin tx: %w", err)
	}
	runs, err := db.ListPendingWorkflowRuns(ctx, ttx.Tx, devTenantID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("adopt: list runs: %w", err)
	}

	active := make(map[string]bool, len(runs))
	for _, run := range runs {
		active[run.ID] = true
	}
	for _, name := range containers {
		id := strings.TrimPrefix(name, "orchicon-runtime-")
		if !active[id] {
			l.log.Warn("reaping orphan runtime container", "container", name)
			if err := l.client.Kill(ctx, id); err != nil {
				l.log.Warn("adopt: reap orphan failed", "container", name, "error", err)
			}
		}
	}
	for _, run := range runs {
		if err := l.EnsureForRun(ctx, run); err != nil {
			l.log.Warn("adopt: ensure runtime failed", "run", run.ID, "error", err)
		}
	}
	return nil
}
