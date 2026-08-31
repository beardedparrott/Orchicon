package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// devTenantID mirrors the single-dev-tenant assumption used by the
// reconcilers (docs/03 §1). Multi-tenant scheduling arrives with auth.
const devTenantID = "tnt_dev"

// worktreeDirName mirrors the WorktreeReconciler's namespace under
// project_dir where per-run worktrees are provisioned
// (internal/scheduler/worktree_reconciler.go). The runtime layer must know
// it so the composite worktree MCP tools can be pointed at the run's
// worktree instead of the project root.
const worktreeDirName = ".orchicon-worktrees"

// runWorktreeBase resolves the base directory the composite worktree MCP
// tools (batch_read/batch_grep/batch_write) resolve relative paths against
// for a run's container serve. For a git-backed project the run gets a
// provisioned worktree at the deterministic path
// <projectDir>/.orchicon-worktrees/<runID> and executions run inside it —
// so the batch tools MUST resolve there, never at the project root (a
// project-root base silently writes into the main checkout, violating
// worktree hygiene). Non-repo projects run in place at projectDir. Mirrors
// the WorktreeReconciler's isInsideWorkTree decision so the baked base
// matches the execution cwd.
func runWorktreeBase(ctx context.Context, projectDir, runID string) string {
	if projectDir == "" {
		return ""
	}
	if isInsideWorkTree(ctx, projectDir) {
		return filepath.Join(projectDir, worktreeDirName, runID)
	}
	return projectDir
}

// isInsideWorkTree mirrors the WorktreeReconciler's git check
// (`git rev-parse --is-inside-work-tree` exits 0 and prints "true"). A
// non-zero exit or "false" output means not a git repo / not a work tree —
// the non-repo skip signal that keeps the execution in place at projectDir.
// A missing git binary or nonexistent dir is treated as non-repo (the run
// proceeds in place), matching the reconciler.
func isInsideWorkTree(ctx context.Context, projectDir string) bool {
	if fi, err := os.Stat(projectDir); err != nil || !fi.IsDir() {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", projectDir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

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
	// The sandbox-scoped Orchicon MCP is registered only for dev images
	// (which boot the sandbox plane against a sandbox DB); the plane-channel
	// Orchicon MCP (`orchicon-plane`) is registered on EVERY image when the
	// run's worker role grants it (planeEnv non-empty) — plane access is
	// role-gated, never image-gated. The project dir is threaded through
	// because the composite worktree batch tools (batch_read/grep/write) need
	// a base directory to resolve against — the project is mounted
	// in-container at the same absolute path.
	serveConfigFor func(image, projectDir, workflowRunID string, planeEnv map[string]string) string
	// secretsKEK is the plane-resolved 32-byte KEK for tenant secrets
	// (resolved at server construction; nil disables secret injection).
	secretsKEK []byte
}

// NewLifecycle creates a workflow runtime lifecycle. client may be nil to
// disable per-workflow runtime containers. serveConfigFor builds the
// OPENCODE_CONFIG_CONTENT the container's opencode serve boots with from
// the run's runtime image tag (permission rules for every image, the
// sandbox-scoped Orchicon MCP for dev images, and the plane-channel
// Orchicon MCP on any image when the run's worker role grants it — built
// by the opencode package).
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
	// Serve config is built last so the composite worktree batch tools
	// (batch_read/grep/write) resolve against the RUN'S WORKTREE, not the
	// project root: the worker's session cwd is the run worktree
	// (executionDir), so a batch tool resolving relative paths against the
	// project root silently writes into the main checkout — the worktree
	// hygiene violation observed in automation runs (research/ pollution in
	// the develop checkout). The run worktree path is deterministic
	// (<projectDir>/.orchicon-worktrees/<runID>) for git-backed projects
	// (the WorktreeReconciler provisions exactly that path); non-repo
	// projects run in place at the project dir. Mirror the reconciler's
	// isInsideWorkTree decision so the baked base matches the eventual
	// execution cwd.
	req.ServeConfig = l.serveConfigFor(run.RuntimeImage, runWorktreeBase(ctx, projectDir, run.ID), run.ID, planeEnv)
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
// runtime container. Runtime containers share the docker bridge with the
// plane, so the plane must advertise an address that is reachable from a
// bridge container — NOT the host's published port.
//
// The host's published port (e.g. prod's 0.0.0.0:8091->8080) is only
// reachable from outside the bridge: a bridge container dialing the gateway
// IP + published port (172.17.0.1:8091) is dropped by the docker hairpin
// NAT, so the plane-channel MCP sidecar times out on every call. The plane
// container's OWN bridge IP + internal port 8080 is reachable directly on
// the bridge (the same "direct container-IP access, no published port"
// model the serve path already uses — see daemon.go createContainer).
//
// Resolution order:
//  1. ORCHICON_PLANE_PUBLIC_URL, if set (operator override wins).
//  2. Container mode (ORCHICON_CONTAINER_MODE=1): the plane's own container
//     IP + internal port 8080 — identical for dev and prod (both listen on
//     8080 internally), and correct regardless of the published port.
//  3. Host mode: the docker bridge gateway + default dev mapping
//     (172.17.0.1:8080), where the runtime container reaches the host plane
//     via the gateway.
func planePublicURL() string {
	if v := os.Getenv("ORCHICON_PLANE_PUBLIC_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if os.Getenv("ORCHICON_CONTAINER_MODE") == "1" {
		if ip := containerIPAddress(); ip != "" {
			return "http://" + ip + ":8080"
		}
	}
	return "http://172.17.0.1:8080"
}

// containerIPAddress resolves the current container's bridge IP. Docker
// sets the container hostname to the container ID and writes
// "<ip> <hostname>" into /etc/hosts, so reading the hostname's entry is the
// most reliable cross-platform resolution (no `hostname -i` dependency).
// Returns "" when it cannot be resolved (host mode, or /etc/hosts absent).
func containerIPAddress() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return ""
	}
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return ""
	}
	return parseHostsIP(string(data), hostname)
}

// parseHostsIP extracts the IP for a given hostname from an /etc/hosts body
// (Docker writes "<ip> <hostname>" as the first field pair). Returns "" when
// the hostname has no entry.
func parseHostsIP(body, hostname string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == hostname {
			return fields[0]
		}
	}
	return ""
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
		// Workflow-driven runs (a recurring item bound to a template
		// workflow) carry their workers on the run's STEPS, not on the work
		// item — resolve the first published, role-bound step worker so the
		// plane channel is available there too. Deny-by-default remains: no
		// qualifying step worker → no channel.
		ref.WorkerID = l.resolveWorkflowStepWorker(ctx, ttx.Tx, run)
		if ref.WorkerID == "" {
			return nil, nil
		}
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
	// The API key's identity_id must reference a REAL identities row — the
	// audit trail stamps every write with the caller's identity via the
	// identities_actor_identity_fk, and a worker ID is not an identity. Stamp
	// writes with a per-run SERVICE identity instead (subject "run:<runID>",
	// idempotently provisioned via GetOrCreateIdentity so concurrent steps
	// share one row). Entitlements are unchanged — ResolveApiKey authorizes
	// from the KEY's scopes alone and never unions identity roles, so this
	// is audit attribution, not privilege widening.
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, run.TenantID, automationIdentitySubject(run.ID), "Automation run "+run.ID, "service")
	if err != nil {
		return nil, fmt.Errorf("plane credential: ensure automation identity: %w", err)
	}
	plaintext, prefix, hash := auth.GenerateApiKey()
	expiresAt := time.Now().Add(planeTokenTTL())
	if _, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{
		TenantID:   run.TenantID,
		IdentityID: ident.ID,
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
	env := map[string]string{
		"ORCHICON_PLANE_URL":           planePublicURL(),
		"ORCHICON_PLANE_TOKEN":         plaintext,
		"ORCHICON_MCP_TENANT_ID":       run.TenantID,
		"ORCHICON_MCP_WORKFLOW_RUN_ID": run.ID,
	}
	// Relay the run's run_context JSONB (the automation provenance block a
	// recurring fire writes at fire time) so the plane-channel registry can
	// stamp created work items with spawned_by/spawned_by_run_id and land
	// outputs_mode=idea creates in IDEA state — feature 4.1 AC2 on the plane
	// surface. It rides the same trusted mint path as the token itself: the
	// control plane injects it, the sidecar relays it verbatim on creates.
	// A bare run ID is not a run_context (ProvenanceFromRunContext parses
	// JSON) — sending one instead of this made every plane-path idea spawn
	// silently land as a plain pending item.
	if len(run.RunContext) > 0 {
		env["ORCHICON_RUN_CONTEXT"] = string(run.RunContext)
	}
	return env, nil
}

// automationIdentitySubject returns the identities.subject for a run's
// per-run automation service identity — the stable (tenant, subject) key
// that GetOrCreateIdentity provisions the plane-channel API key against.
// The key's identity_id must reference a REAL identities row: the audit
// trail stamps every write with actor_identity_id, which is FK'd to
// identities — a worker ID is not an identity and fails writes with
// SQLSTATE 23503 (audit_events_actor_identity_fk).
func automationIdentitySubject(runID string) string { return "run:" + runID }

// resolveWorkflowStepWorker returns the first PUBLISHED, role-bound worker
// referenced by any step of the run's workflow version, or "" when the run
// is not a workflow run or no step worker qualifies (deny-by-default).
//
// The run's container and opencode serve are shared across all steps, so a
// single run-level credential is minted for the first qualifying step
// worker and the whole run speaks with that role's entitlements. Mixed-role
// workflows (steps with different roles) are not yet supported — the first
// qualifying worker's role wins and steps with other roles get no plane
// channel; per-step minting is a follow-up.
func (l *Lifecycle) resolveWorkflowStepWorker(ctx context.Context, tx pgx.Tx, run db.WorkflowRunRow) string {
	if run.WorkflowID == "" {
		return ""
	}
	wv, err := db.GetWorkflowVersion(ctx, tx, run.TenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		l.log.Warn("plane credential: workflow version lookup failed", "run", run.ID, "workflow", run.WorkflowID, "version", run.WorkflowVersion, "error", err)
		return ""
	}
	steps, err := workflow.ParseSteps(wv.Steps)
	if err != nil {
		l.log.Warn("plane credential: workflow steps parse failed", "run", run.ID, "workflow", run.WorkflowID, "error", err)
		return ""
	}
	for _, s := range steps {
		if s.Ref == "" {
			continue
		}
		w, werr := db.GetWorker(ctx, tx, run.TenantID, s.Ref)
		if werr != nil {
			continue
		}
		if w.Status == "published" && w.RoleRef != "" {
			return w.ID
		}
	}
	return ""
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
