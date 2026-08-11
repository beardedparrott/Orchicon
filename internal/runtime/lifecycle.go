package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// devTenantID mirrors the single-dev-tenant assumption used by the
// reconcilers (docs/03 §1). Multi-tenant scheduling arrives with auth.
const devTenantID = "tnt_dev"

// Lifecycle decides WHEN workflow runtime containers are created and
// reaped based on workflow run state, and gates execution dispatch on the
// container's opencode serve being proven usable. It talks to the
// host-side daemon through runtime.Client. A nil client (no daemon socket
// — headless `orchicon serve`) makes every operation a no-op so the
// control plane degrades to in-process execution.
type Lifecycle struct {
	client      *Client
	pool        *db.Pool
	log         *slog.Logger
	serveConfig string // OPENCODE_CONFIG_CONTENT for the container's serve
}

// NewLifecycle creates a workflow runtime lifecycle. client may be nil to
// disable per-workflow runtime containers. serveConfig is the
// OPENCODE_CONFIG_CONTENT the container's opencode serve boots with (built
// by the opencode package — permission rules only, user MCPs omitted).
func NewLifecycle(client *Client, pool *db.Pool, log *slog.Logger, serveConfig string) *Lifecycle {
	return &Lifecycle{client: client, pool: pool, log: log, serveConfig: serveConfig}
}

// Enabled reports whether a daemon is configured.
func (l *Lifecycle) Enabled() bool { return l.client != nil }

// buildCreateRequest computes the create request for a run: the project
// directory mount (if the project has one) plus the project's and run's
// bound work item's context_files paths that lie OUTSIDE project_dir
// (paths under project_dir are covered by the project-dir mount), and the
// resolved runtime image. Every container is created with the serve
// config so the opencode serve warms up at create time, not at first
// dispatch. Shared by EnsureForRun and EnsureServing.
func (l *Lifecycle) buildCreateRequest(ctx context.Context, run db.WorkflowRunRow) (CreateRequest, error) {
	req := CreateRequest{
		WorkflowID:  run.ID,
		Image:       run.RuntimeImage,
		ServeConfig: l.serveConfig,
	}
	if run.ProjectID == "" {
		return req, nil
	}
	ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		return CreateRequest{}, fmt.Errorf("ensure runtime: begin tx: %w", err)
	}
	project, gerr := db.GetProject(ctx, ttx.Tx, run.TenantID, run.ProjectID)
	if gerr == nil {
		projectDir := project.ProjectDir
		if projectDir != "" {
			req.Mounts = append(req.Mounts, MountSpec{Source: projectDir, Dest: projectDir})
			req.ProjectDir = projectDir
		}
		// Project context files/directories outside project_dir.
		req.Mounts = append(req.Mounts, contextMounts(project.ContextFiles, projectDir)...)
		// The run's bound work item's context files/directories.
		if run.WorkItemID != "" {
			if wi, werr := db.GetWorkItem(ctx, ttx.Tx, run.TenantID, run.WorkItemID); werr == nil {
				req.Mounts = append(req.Mounts, contextMounts(wi.ContextFiles, projectDir)...)
			}
		}
	}
	_ = ttx.Rollback(ctx)
	if gerr != nil && gerr != db.ErrNotFound {
		return CreateRequest{}, fmt.Errorf("ensure runtime: get project: %w", gerr)
	}
	return req, nil
}

// EnsureForRun creates the runtime container for a workflow run
// (idempotent) with its opencode serve warmed at create time. Executions
// dispatch into this container for the whole lifetime of the run.
func (l *Lifecycle) EnsureForRun(ctx context.Context, run db.WorkflowRunRow) error {
	if l.client == nil {
		return nil
	}
	if !l.client.Ready(ctx) {
		return fmt.Errorf("runtime daemon not reachable")
	}
	req, err := l.buildCreateRequest(ctx, run)
	if err != nil {
		return err
	}
	if _, err := l.client.Create(ctx, req); err != nil {
		return fmt.Errorf("ensure runtime for run %s: %w", run.ID, err)
	}
	l.log.Info("workflow runtime ensured", "run", run.ID, "project", run.ProjectID, "image", req.Image, "mounts", len(req.Mounts))
	return nil
}

// EnsureServing is the workflow run-start gate. It ensures the run's
// runtime container exists with its opencode serve brought up, then blocks
// until the serve is proven USABLE (L1: /global/health AND a real
// session-create round-trip), bounded by ORCHICON_RUNTIME_SERVE_READY_TIMEOUT
// (default 120s). The WorkflowReconciler must NOT dispatch any execution for
// the run until this returns nil — a cold-starting serve that would
// previously fail the first dispatch's 30s window now gets the full window
// at run start, off the dispatch hot path.
func (l *Lifecycle) EnsureServing(ctx context.Context, run db.WorkflowRunRow) error {
	if l.client == nil {
		return nil
	}
	if !l.client.Ready(ctx) {
		return fmt.Errorf("runtime daemon not reachable")
	}
	req, err := l.buildCreateRequest(ctx, run)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(serveReadyTimeout())
	var lastErr error
	for {
		resp, cerr := l.client.Create(ctx, req)
		if cerr == nil && resp.ServePort != 0 && resp.ServePassword != "" {
			base := resp.ServeURL
			if base == "" {
				base = fmt.Sprintf("http://127.0.0.1:%d", resp.ServePort)
			}
			if serveUsableAt(base, resp.ServePassword) {
				l.log.Info("workflow runtime serve usable", "run", run.ID, "serve", base)
				return nil
			}
			lastErr = fmt.Errorf("serve %s not usable yet", base)
		} else if cerr != nil {
			lastErr = cerr
		} else {
			lastErr = fmt.Errorf("serve not yet published")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("runtime opencode serve failed to become usable for run %s: %w", run.ID, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// serveReadyTimeout resolves the run-start serve gate window. Overridable
// via ORCHICON_RUNTIME_SERVE_READY_TIMEOUT (duration string); default 120s.
func serveReadyTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_RUNTIME_SERVE_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 120 * time.Second
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

// Adopt ensures a runtime lease exists for every active (pending/running)
// workflow run at boot. The warm pool owns container lifecycle cleanup
// (idle-reap + a wholesale reset at daemon start), so adopt only ensures
// leases for active runs after a plane/daemon restart — the run-start gate
// and the adapter's self-heal cover the rest.
func (l *Lifecycle) Adopt(ctx context.Context) error {
	if l.client == nil {
		return nil
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
	for _, run := range runs {
		if err := l.EnsureForRun(ctx, run); err != nil {
			l.log.Warn("adopt: ensure runtime failed", "run", run.ID, "error", err)
		}
	}
	return nil
}
