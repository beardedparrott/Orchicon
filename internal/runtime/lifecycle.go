package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
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
	client *Client
	pool   *db.Pool
	log    *slog.Logger
	// serveConfigFor builds the OPENCODE_CONFIG_CONTENT for a run's
	// container serve from its runtime image tag and in-container project dir.
	// The Orchicon MCP is registered only for dev images (which boot the
	// sandbox plane against a sandbox DB); base/gui images get a config
	// identical to today. The project dir is threaded through because the
	// composite worktree batch tools (batch_read/grep/write) need a base
	// directory to resolve against — the project is mounted in-container at
	// the same absolute path.
serveConfigFor func(image, projectDir, workflowRunID string, planeEnv map[string]string) string
	// secretsKEK is the plane-resolved 32-byte KEK for tenant secrets
	// (resolved at server construction; nil disables secret injection).
	secretsKEK []byte
}

// NewLifecycle creates a workflow runtime lifecycle. client may be nil to
// disable per-workflow runtime containers. serveConfigFor builds the
// OPENCODE_CONFIG_CONTENT the container's opencode serve boots with from
// the run's runtime image tag (permission rules only for base/gui images,
// plus the sandbox-scoped Orchicon MCP for dev images — built by the
// opencode package).
func NewLifecycle(client *Client, pool *db.Pool, log *slog.Logger, serveConfigFor func(image, projectDir, workflowRunID string, planeEnv map[string]string) string, secretsKEK []byte) *Lifecycle {
	return &Lifecycle{client: client, pool: pool, log: log, serveConfigFor: serveConfigFor, secretsKEK: secretsKEK}
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
		WorkflowID: run.ID,
		Image:      run.RuntimeImage,
	}
	var projectDir string
	if run.ProjectID != "" {
		ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
		if err != nil {
			return CreateRequest{}, fmt.Errorf("ensure runtime: begin tx: %w", err)
		}
		project, gerr := db.GetProject(ctx, ttx.Tx, run.TenantID, run.ProjectID)
		if gerr == nil {
			projectDir = project.ProjectDir
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
	}
	// Plane channel: when the run's worker is a published worker with a
	// role_ref, mint a short-lived, role-scoped API key and pass it to the
	// serve config + container env so the `orchicon-plane` MCP channel is
	// available (deny-by-default — planeEnv is nil otherwise). The plaintext
	// is never stored; only the hash persists, and the key expires with the
	// TTL.
	planeEnv, merr := l.mintPlaneCredential(ctx, run)
	if merr != nil {
		l.log.Warn("plane credential mint failed", "run", run.ID, "error", merr)
	}
	if planeEnv != nil {
		if req.Secrets == nil {
			req.Secrets = map[string]string{}
		}
		for k, v := range planeEnv {
			req.Secrets[k] = v
		}
	}
	// Serve config is built last so the in-container project dir is available
	// for the composite worktree batch tools (batch_read/grep/write).
	req.ServeConfig = l.serveConfigFor(run.RuntimeImage, projectDir, run.ID, planeEnv)
	// Secrets: decrypt per-work-item selection and inject as container env.
	// KEK is plane-only — resolved once at server construction (env override
	// or the per-instance data-dir key); the daemon blindly injects -e (same
	// pattern as GH_TOKEN).
	if run.WorkItemID != "" && len(l.secretsKEK) == 32 {
		// Reuse the tx already opened above if still in scope; open a fresh one for secrets.
		ttx2, terr := l.pool.BeginTenantTx(ctx, run.TenantID)
		if terr == nil {
			defer ttx2.Rollback(ctx)
			if wi, werr := db.GetWorkItem(ctx, ttx2.Tx, run.TenantID, run.WorkItemID); werr == nil {
				var ids []string
				if len(wi.SecretIDs) > 0 {
					_ = json.Unmarshal(wi.SecretIDs, &ids)
				}
				if len(ids) > 0 {
					m, merr := db.BatchGetSecrets(ctx, ttx2.Tx, run.TenantID, ids)
					if merr == nil && len(m) == len(ids) {
						if req.Secrets == nil {
							req.Secrets = map[string]string{}
						}
						for _, id := range ids {
							row := m[id]
							if pt, derr := secretcrypto.Decrypt(row.Ciphertext, l.secretsKEK); derr == nil {
								req.Secrets[row.Name] = string(pt)
							}
						}
					}
				}
			}
			_ = ttx2.Commit(ctx)
		}
	}
	return req, nil
}

// planeTokenTTL resolves the plane credential lifetime. Overridable via
// ORCHICON_PLANE_TOKEN_TTL (duration string); default 24h — long enough to
// cover the run's container lifetime (including pool reset recreation) but
// short enough that a leaked token is useless well before it matters.
func planeTokenTTL() time.Duration {
	if v := os.Getenv("ORCHICON_PLANE_TOKEN_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 24 * time.Hour
}

// planePublicURL is the plane's API base URL as reachable from inside a
// runtime container (the docker bridge gateway + the instance's published
// HTTP port). The container has no route to the plane's internal
// localhost, so the operator sets ORCHICON_PLANE_PUBLIC_URL when the
// published port differs from the default dev mapping (:8080 → prod
// publishes :8091).
func planePublicURL() string {
	if v := os.Getenv("ORCHICON_PLANE_PUBLIC_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://172.17.0.1:8080"
}

// mintPlaneCredential resolves the run's bound worker and, when it is a
// PUBLISHED worker with a role_ref (deny-by-default), mints a short-lived
// API key scoped to the role's entitlements. Returns the env vars the serve
// config + container need for the `orchicon-plane` MCP channel, or nil when
// no plane access is granted. The plaintext key is returned once and never
// persisted — only its SHA-256 hash is stored (internal/auth/apikeys.go).
func (l *Lifecycle) mintPlaneCredential(ctx context.Context, run db.WorkflowRunRow) (map[string]string, error) {
	if run.WorkItemID == "" {
		return nil, nil
	}
	ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		return nil, fmt.Errorf("plane credential: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	wi, err := db.GetWorkItem(ctx, ttx.Tx, run.TenantID, run.WorkItemID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("plane credential: get work item: %w", err)
	}
	var ref struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if len(wi.AssignedWorkerRef) > 0 {
		_ = json.Unmarshal(wi.AssignedWorkerRef, &ref)
	}
	if ref.WorkerID == "" {
		return nil, nil
	}
	worker, err := db.GetWorker(ctx, ttx.Tx, run.TenantID, ref.WorkerID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("plane credential: get worker: %w", err)
	}
	// Draft/deprecated workers are inert: no plane access. Role_ref is the
	// explicit opt-in (deny-by-default).
	if worker.Status != "published" || worker.RoleRef == "" {
		return nil, nil
	}
	role, err := db.GetRole(ctx, ttx.Tx, run.TenantID, worker.RoleRef)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			l.log.Warn("plane credential: role missing for worker", "worker", worker.ID, "role", worker.RoleRef)
			return nil, nil
		}
		return nil, fmt.Errorf("plane credential: get role: %w", err)
	}
	plaintext, prefix, hash := auth.GenerateApiKey()
	expiresAt := time.Now().Add(planeTokenTTL())
	if _, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{
		TenantID:   run.TenantID,
		IdentityID: worker.ID,
		Name:       "automation:run:" + run.ID,
		KeyPrefix:  prefix,
		KeyHash:    hash,
		Scopes:     role.Entitlements,
		Status:     "active",
		ExpiresAt:  &expiresAt,
	}); err != nil {
		return nil, fmt.Errorf("plane credential: mint key: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("plane credential: commit: %w", err)
	}
	l.log.Info("plane credential minted", "run", run.ID, "worker", worker.ID, "role", worker.RoleRef, "scopes", len(role.Entitlements))
	return map[string]string{
		"ORCHICON_PLANE_URL":         planePublicURL(),
		"ORCHICON_PLANE_TOKEN":       plaintext,
		"ORCHICON_MCP_TENANT_ID":     run.TenantID,
		"ORCHICON_MCP_WORKFLOW_RUN_ID": run.ID,
	}, nil
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
// (default 120s). When the container's image boots the sandbox plane
// (dev images; CreateResponse.PlaneURL set), the gate ALSO requires the
// plane's /healthz to answer on the container bridge IP — a half-initialized
// plane can't pass, so no execution dispatches against a run whose sandbox
// environment isn't ready. The WorkflowReconciler must NOT dispatch any
// execution for the run until this returns nil — a cold-starting serve that
// would previously fail the first dispatch's 30s window now gets the full
// window at run start, off the dispatch hot path.
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
			serveOK := serveUsableAt(base, resp.ServePassword)
			planeOK := true
			if resp.PlaneURL != "" {
				planeOK = planeHealthyAt(resp.PlaneURL)
			}
			if serveOK && planeOK {
				l.log.Info("workflow runtime serve usable", "run", run.ID, "serve", base, "plane", resp.PlaneURL)
				return nil
			}
			lastErr = fmt.Errorf("serve %s not usable yet (plane %s: %v)", base, resp.PlaneURL, planeOK)
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

// planeHealthyAt reports whether the run's sandbox plane answers /healthz
// at the given base URL (the container bridge IP :8080). The handler only
// mounts once the plane has finished constructing, so a half-initialized
// plane can't pass. No auth: the plane lives inside an isolated runtime
// container reachable only at the bridge IP.
func planeHealthyAt(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
