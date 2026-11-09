package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// devTenantID mirrors the single-dev-tenant assumption used by the
// reconcilers (docs/03 §1). Multi-tenant scheduling arrives with auth.
const devTenantID = "tnt_dev"

// opencodeAdapterKind is the adapter kind whose executions need the in-container opencode serve.
const opencodeAdapterKind = "opencode"

// It is NEVER written on new runs: imageForRun always persists the resolved image.
const runtimeNoServeImage = "no-serve"

// NoServeImage is the exported legacy sentinel; see runtimeNoServeImage.
const NoServeImage = runtimeNoServeImage

// worktreeDirName mirrors the WorktreeReconciler's namespace under project_dir where per-run
// worktrees are provisioned (internal/scheduler/worktree_reconciler.go).
const worktreeDirName = ".orchicon-worktrees"

// For a git-backed project the run gets a provisioned worktree at the deterministic path
// <projectDir>/.orchicon-worktrees/<runID> and executions run inside it.
func runWorktreeBase(ctx context.Context, projectDir, runID string) string {
	if projectDir == "" {
		return ""
	}
	if isInsideWorkTree(ctx, projectDir) {
		return filepath.Join(projectDir, worktreeDirName, runID)
	}
	return projectDir
}

// isInsideWorkTree mirrors the WorktreeReconciler's git check (`git rev-parse
// --is-inside-work-tree` exits 0 and prints "true").
func isInsideWorkTree(ctx context.Context, projectDir string) bool {
	if fi, err := os.Stat(projectDir); err != nil || !fi.IsDir() {
		return false
	}
	cmd := procwin.CommandContext(ctx, "git", "-C", projectDir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// Lifecycle decides WHEN workflow runtime containers are created and reaped based on workflow run
// state, and gates execution dispatch on the container's opencode serve being proven usable.
type Lifecycle struct {
	client *Client
	pool   *db.Pool
	log    *slog.Logger
	// serveConfigFor builds the OPENCODE_CONFIG_CONTENT for a run's container serve from its runtime
	// image tag, its in-container project dir.
	serveConfigFor func(image, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution, providers []ProviderConfig, workers []adapter.WorkerAgent) string
	// secretsKEK is the plane-resolved 32-byte KEK for tenant secrets
	// (resolved at server construction; nil disables secret injection).
	secretsKEK []byte
	// scopeResolver resolves the RUN's MCP union for the container bake (the same resolver the
	// native/claude bridges use).
	scopeResolver mcpclient.ScopeResolver

	// containerProviders resolves the TENANT's providers as a runtime container must see them: base
	// URLs transposed off the host's loopback (a container's own 127.0.0.1 is itself.
	containerProviders func(ctx context.Context, tenantID string) []ProviderConfig

	// planeCredMu guards planeCredCache.
	planeCredMu sync.Mutex
	// WHY IT MUST EXIST: mintPlaneCredential MINTS A FRESH RANDOM API KEY on every call.
	planeCredCache map[string]map[string]string
}

// NewLifecycle creates a workflow runtime lifecycle.
func NewLifecycle(client *Client, pool *db.Pool, log *slog.Logger, serveConfigFor func(image, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution, providers []ProviderConfig, workers []adapter.WorkerAgent) string, secretsKEK []byte) *Lifecycle {
	return &Lifecycle{client: client, pool: pool, log: log, serveConfigFor: serveConfigFor, secretsKEK: secretsKEK}
}

// SetScopeResolver injects the MCP scope resolver the run-level union is computed from.
func (l *Lifecycle) SetScopeResolver(r mcpclient.ScopeResolver) { l.scopeResolver = r }

// SetContainerProviders injects the container-locality provider resolver (see the
// field's note for why the Lifecycle, not the adapter, owns it).
func (l *Lifecycle) SetContainerProviders(fn func(ctx context.Context, tenantID string) []ProviderConfig) {
	l.containerProviders = fn
}

// IT IS COMPUTED FROM THE RUN, NEVER THE EXECUTING STEP.
func (l *Lifecycle) resolveRunUnion(ctx context.Context, run db.WorkflowRunRow) mcpclient.Resolution {
	if l.scopeResolver == nil {
		return mcpclient.Resolution{}
	}
	res, err := l.scopeResolver.ResolveScope(
		tenant.WithID(ctx, run.TenantID),
		mcpclient.ScopeRef{Kind: mcpclient.ScopeRun, RunID: run.ID})
	if err != nil {
		l.log.Warn("run MCP union unresolved — the container serve will carry built-ins only",
			"run", run.ID, "error", err)
		return mcpclient.Resolution{}
	}
	// Expand ${SECRET_NAME} in place, dropping any server whose secret cannot
	// resolve (a serve hangs on a server it cannot connect to).
	if len(l.secretsKEK) == 32 {
		for i := 0; i < len(res.Servers); i++ {
			env, hdr, serr := mcpsettings.ResolveSecretRefs(ctx, l.pool, l.secretsKEK,
				run.TenantID, res.Servers[i].Spec.Env, res.Servers[i].Spec.Headers)
			if serr != nil {
				l.log.Warn("run MCP server dropped — its secret did not resolve (a serve hangs on an unreachable server)",
					"run", run.ID, "server", res.Servers[i].Spec.ID,
					"scope", res.Servers[i].FromID, "error", serr)
				res.Servers = append(res.Servers[:i], res.Servers[i+1:]...)
				i--
				continue
			}
			res.Servers[i].Spec.Env, res.Servers[i].Spec.Headers = env, hdr
		}
	}
	// AC 4: log the union's provenance ONCE, plane-side.
	l.log.Info("run MCP union resolved", "run", run.ID,
		"servers", mcpclient.ProvenanceString(res.Servers), "skills", len(res.Skills))
	return res
}

// COMPUTED FROM THE RUN, NEVER THE EXECUTING STEP — the same rule the MCP union follows, for the
// same reason: the config is applied ONCE at container creation.
func (l *Lifecycle) resolveRunWorkers(ctx context.Context, run db.WorkflowRunRow) []adapter.WorkerAgent {
	if run.WorkflowID == "" {
		return nil
	}
	ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		l.log.Warn("run agent workers: begin tx failed — the serve will carry no per-worker agents", "run", run.ID, "error", err)
		return nil
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	wv, err := db.GetWorkflowVersion(ctx, ttx.Tx, run.TenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		l.log.Warn("run agent workers: workflow version lookup failed", "run", run.ID, "error", err)
		return nil
	}
	steps, err := workflow.ParseSteps(wv.Steps)
	if err != nil {
		l.log.Warn("run agent workers: workflow steps parse failed", "run", run.ID, "error", err)
		return nil
	}
	resolved := adapter.ResolveRunSteps(ctx, ttx.Tx, run.TenantID, steps)

	// The prompt's runtime-environment block is built from the RUN's image and the runtime mode,
	// mirroring the composite: scheduler.runEffectiveImage resolves the same value for a run-driven.
	image := strings.TrimSpace(run.RuntimeImage)
	if image == "" {
		image = db.RuntimeBaseImage()
	}
	mode := db.ExecutionModeRuntime

	seen := make(map[string]struct{}, len(resolved))
	out := make([]adapter.WorkerAgent, 0, len(resolved))
	for _, r := range resolved {
		if strings.TrimSpace(r.WorkerID) == "" {
			continue
		}
		name := adapter.WorkerAgentName(r.WorkerID)
		if _, dup := seen[name]; dup {
			continue
		}
		prompt := db.AgentSystemPrompt(r.Version, image, mode)
		if strings.TrimSpace(prompt) == "" {
			l.log.Warn("run agent workers: worker resolved to an empty prompt — omitted rather than emitted empty",
				"run", run.ID, "worker", r.WorkerID)
			continue
		}
		seen[name] = struct{}{}
		out = append(out, adapter.WorkerAgent{Name: name, Prompt: prompt, Model: agentModelForServe(r.ModelRef)})
	}
	// Step order is the workflow's own order, so the result is already
	// deterministic — it feeds the config bytes and therefore the pool key.
	if len(out) > 0 {
		l.log.Info("run agent workers resolved", "run", run.ID, "agents", len(out))
	}
	return out
}

// This is what the opencode adapter's self-heal path asks for instead of rebuilding the config from
// the executing step: the container's config is applied once at creation.
func (l *Lifecycle) RunServeConfig(ctx context.Context, runID string) (string, bool) {
	ttx, err := l.pool.BeginTenantTx(ctx, devTenantID)
	if err != nil {
		l.log.Warn("run serve config: begin tx", "run", runID, "error", err)
		return "", false
	}
	defer func() { _ = ttx.Rollback(ctx) }()
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, devTenantID, runID)
	if err != nil {
		l.log.Warn("run serve config: get run", "run", runID, "error", err)
		return "", false
	}
	req, err := l.buildCreateRequest(ctx, run)
	if err != nil {
		l.log.Warn("run serve config: build create request", "run", runID, "error", err)
		return "", false
	}
	if req.ServeConfig == "" {
		return "", false
	}
	return req.ServeConfig, true
}

// runContainerProviders resolves the tenant's providers for a container bake, or nil
// when no resolver is wired (the pre-fix behaviour: no provider block).
func (l *Lifecycle) runContainerProviders(ctx context.Context, tenantID string) []ProviderConfig {
	if l.containerProviders == nil {
		return nil
	}
	return l.containerProviders(ctx, tenantID)
}

// Enabled reports whether a daemon is configured.
func (l *Lifecycle) Enabled() bool { return l.client != nil }

// buildCreateRequest computes the create request for a run: the project directory mount (if the
// project has one) plus the project's and run's bound work item's context_files paths that lie.
func (l *Lifecycle) buildCreateRequest(ctx context.Context, run db.WorkflowRunRow) (CreateRequest, error) {
	req := CreateRequest{
		WorkflowID: run.ID,
		Image:      run.RuntimeImage,
	}
	// Effective git strategy (workflow > project > "local") resolved up front so the daemon can gate
	// the push credential for a "none" run.
	strategyTx, serr := l.pool.BeginTenantTx(ctx, run.TenantID)
	if serr == nil {
		req.GitStrategy = db.EffectiveGitStrategy(ctx, strategyTx.Tx, run.TenantID, run.WorkflowID, run.ProjectID)
		_ = strategyTx.Rollback(ctx)
	} else {
		req.GitStrategy = db.DefaultGitStrategy
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
				// ONCE. The reported argv carried the project bind TWICE:
				//
				//	-v /mnt/c/projects/weatherapp:/mnt/c/projects/weatherapp
				//	-v /mnt/c/projects/weatherapp:/mnt/c/projects/weatherapp
				//
				// which is not fatal but is pure noise in the vector an operator has to read to debug
				// anything — and a duplicate here means the mount list is assembled from more than one
				// place, so the NEXT duplicate could be two different sources for one destination, which
				// the engine resolves by ORDER. dedupeMounts makes the list a set, keeping the first.
				req.Mounts = dedupeMounts(append(req.Mounts, MountSpec{Source: projectDir, Dest: projectDir}))
				req.ProjectDir = projectDir
			}
			// Project context files/directories outside project_dir.
			req.Mounts = dedupeMounts(append(req.Mounts, contextMounts(project.ContextFiles, projectDir)...))
			// The run's bound work item's context files/directories.
			if run.WorkItemID != "" {
				if wi, werr := db.GetWorkItem(ctx, ttx.Tx, run.TenantID, run.WorkItemID); werr == nil {
					req.Mounts = dedupeMounts(append(req.Mounts, contextMounts(wi.ContextFiles, projectDir)...))
				}
			}
		}
		_ = ttx.Rollback(ctx)
		if gerr != nil && gerr != db.ErrNotFound {
			return CreateRequest{}, fmt.Errorf("ensure runtime: get project: %w", gerr)
		}
	}
	// Plane channel: when the run's worker is a published worker with a role_ref, mint a short-lived,
	// role-scoped API key and pass it to the serve config + container env so the `orchicon-plane` MCP.
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
	// Serve config is built last so the composite worktree batch tools (batch_read/grep/write) resolve
	// against the RUN'S WORKTREE, not the project root.
	demand := l.adapterDemandFor(ctx, run)
	req.AdapterKinds = demand.Kinds()
	if demand.Has(opencodeAdapterKind) {
		req.ServeConfig = l.serveConfigFor(run.RuntimeImage, runWorktreeBase(ctx, projectDir, run.ID), run.ID, planeEnv,
			l.resolveRunUnion(ctx, run), l.runContainerProviders(ctx, run.TenantID), l.resolveRunWorkers(ctx, run))
	}
	// Secrets: decrypt per-work-item selection and inject as container env.
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

// planeTokenTTL resolves the plane credential lifetime.
func planeTokenTTL() time.Duration {
	if v := os.Getenv("ORCHICON_PLANE_TOKEN_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 24 * time.Hour
}

// planeHTTPPort is the port THIS instance's plane listens on, read from ORCHICON_HTTP_ADDR (default
// 8080).
func planeHTTPPort() int {
	if addr := os.Getenv("ORCHICON_HTTP_ADDR"); addr != "" {
		if _, portStr, err := net.SplitHostPort(addr); err == nil {
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p <= 65535 {
				return p
			}
		}
	}
	return 8080
}

// planePublicURL is the plane's API base URL as reachable from inside a
// runtime container.
//
// Runtime containers share the docker bridge with the plane, so the plane
// must advertise an address a bridge container can dial — NOT the host's
// published port. A published port (e.g. prod's 0.0.0.0:8091->8080) is only
// reachable from outside the bridge: a bridge container dialing the gateway
// IP + published port is dropped by the docker hairpin NAT, which is why the
// plane-channel MCP sidecar times out on every call. Hence the plane
// advertises either its own container bridge IP (container residency) or the
// docker bridge interface address it binds on the host (host residency).
//
// The port is always THIS instance's HTTP port (planeHTTPPort()) — never a
// literal 8080.
//
// Resolution order:
//  1. ORCHICON_PLANE_PUBLIC_URL, if set (operator override wins). The
//     launcher sets it PER INSTANCE; a globally exported value would point
//     one instance's workers at the other instance's plane.
//  2. Container residency (ORCHICON_CONTAINER_MODE=1): the plane
//     container's own bridge IP (containerIPAddress) + this instance's
//     port. Identical to the pre-migration behavior — a containerized plane
//     keeps working unchanged.
//  3. Host residency with ORCHICON_HTTP_EXTRA_BIND: that bind's host (the
//     docker bridge address the host plane listens on) + this instance's
//     port.
//  4. Host residency without it (a manual `orchicon serve`): the stable
//     name host.docker.internal + this instance's port. Every runtime
//     container is created with
//     `--add-host=host.docker.internal:host-gateway` (daemon.go), so the
//     name resolves to the host regardless of bridge subnet — the hardcoded
//     172.17.0.1 no longer has to be right. NOTE: this fallback depends on
//     that create arg; if it is ever dropped, the fallback must become the
//     extra bind host only.
func planePublicURL() string {
	if v := os.Getenv("ORCHICON_PLANE_PUBLIC_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	port := strconv.Itoa(planeHTTPPort())
	if os.Getenv("ORCHICON_CONTAINER_MODE") == "1" {
		if ip := containerIPAddress(); ip != "" {
			return "http://" + ip + ":" + port
		}
		if containerResidencyNeedsOverride() {
			// Podman (typically rootless, and always across the podman-machine VM boundary on macOS/Windows)
			// gives a container no bridge IP and has no 172.17.0.1 gateway.
			return ""
		}
		// Docker container mode with an unresolvable own IP: keep the historical gateway fallback rather
		// than emitting an empty URL (docker always writes /etc/hosts, so this is a guard, not a path).
		return "http://172.17.0.1:" + port
	}
	if b := os.Getenv("ORCHICON_HTTP_EXTRA_BIND"); b != "" {
		if host, _, err := net.SplitHostPort(b); err == nil && host != "" {
			return "http://" + host + ":" + port
		}
	}
	// Host-resident plane reached by name: host.docker.internal is the single spelling for BOTH engines
	// because the daemon maps it at create time (Engine.CreateBaseArgs emits `--add-host.
	return "http://host.docker.internal:" + port
}

// hostsFilePath is the file containerIPAddress reads.
var hostsFilePath = "/etc/hosts"

// containerIPAddress resolves the current container's bridge IP.
func containerIPAddress() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return ""
	}
	data, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return ""
	}
	return parseHostsIP(string(data), hostname)
}

// parseHostsIP extracts the IP for a given hostname from an /etc/hosts body (Docker writes "<ip>
// <hostname>" as the first field pair).
func parseHostsIP(body, hostname string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == hostname {
			return fields[0]
		}
	}
	return ""
}

// mintPlaneCredential resolves the run's bound worker and, when it is a PUBLISHED worker with a
// role_ref (deny-by-default), mints a short-lived API key scoped to the role's entitlements.
func (l *Lifecycle) mintPlaneCredential(ctx context.Context, run db.WorkflowRunRow) (map[string]string, error) {
	if run.WorkItemID == "" {
		return nil, nil
	}
	// Fast path: a memoized credential for this run is what the run-start
	// container was baked with, so a self-heal build MUST reuse it verbatim.
	l.planeCredMu.Lock()
	if env, ok := l.planeCredCache[run.ID]; ok {
		l.planeCredMu.Unlock()
		return env, nil
	}
	l.planeCredMu.Unlock()
	env, err := l.mintPlaneCredentialUncached(ctx, run)
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, nil
	}
	l.planeCredMu.Lock()
	if l.planeCredCache == nil {
		l.planeCredCache = map[string]map[string]string{}
	}
	// Another concurrent build may have won the race; first occurrence wins so
	// every caller sees the SAME credential.
	if existing, ok := l.planeCredCache[run.ID]; ok {
		l.planeCredMu.Unlock()
		return existing, nil
	}
	l.planeCredCache[run.ID] = env
	l.planeCredMu.Unlock()
	return env, nil
}

// planeURLForRun returns the plane URL this instance hands to a run's credential, failing LOUDLY
// when it cannot be derived.
func planeURLForRun() (string, error) {
	u := planePublicURL()
	if u == "" {
		return "", fmt.Errorf("plane credential: this instance's plane URL is not derivable for a container-resident plane under %s (no bridge IP inside the container) — set ORCHICON_PLANE_PUBLIC_URL to the address this instance's workers can dial", planEngineNameForError())
	}
	return u, nil
}

// mintPlaneCredentialUncached performs the actual mint (see mintPlaneCredential,
// its sole caller, for the memoization contract).
func (l *Lifecycle) mintPlaneCredentialUncached(ctx context.Context, run db.WorkflowRunRow) (map[string]string, error) {
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
		// Workflow-driven runs (a recurring item bound to a template workflow) carry their workers on the
		// run's STEPS, not on the work item — resolve the first published.
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
	// Resolve the plane URL BEFORE anything is written.
	planeURL, err := planeURLForRun()
	if err != nil {
		return nil, err
	}
	// The API key's identity_id must reference a REAL identities row — the audit trail stamps every
	// write with the caller's identity via the identities_actor_identity_fk.
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
		"ORCHICON_PLANE_URL":           planeURL,
		"ORCHICON_PLANE_TOKEN":         plaintext,
		"ORCHICON_MCP_TENANT_ID":       run.TenantID,
		"ORCHICON_MCP_WORKFLOW_RUN_ID": run.ID,
	}
	// Relay the run's run_context JSONB (the automation provenance block a recurring fire writes at
	// fire time) so the plane-channel registry can stamp created work items.
	if len(run.RunContext) > 0 {
		env["ORCHICON_RUN_CONTEXT"] = string(run.RunContext)
	}
	return env, nil
}

// automationIdentitySubject returns the identities.subject for a run's per-run automation service
// identity — the stable (tenant.
func automationIdentitySubject(runID string) string { return "run:" + runID }

// adapterDemandFor resolves the run's BOOT PROFILE — the set of adapter kinds its step workers
// resolve to — through the ONE shared demand-set primitive (adapter.AdapterDemandSet, Task C).
func (l *Lifecycle) adapterDemandFor(ctx context.Context, run db.WorkflowRunRow) adapter.DemandSet {
	// adapter.AdapterDemandSet("") is the conservative default: a ref that
	// yields no kind contributes the default adapter kind (opencode).
	conservative := adapter.AdapterDemandSet("")
	if run.WorkflowID == "" {
		return conservative
	}
	ttx, err := l.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		l.log.Warn("boot profile: begin tx failed — assuming adapter demand", "run", run.ID, "error", err)
		return conservative
	}
	defer ttx.Rollback(ctx)
	wv, err := db.GetWorkflowVersion(ctx, ttx.Tx, run.TenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		l.log.Warn("boot profile: workflow version lookup failed", "run", run.ID, "workflow", run.WorkflowID, "error", err)
		return conservative
	}
	steps, err := workflow.ParseSteps(wv.Steps)
	if err != nil {
		l.log.Warn("boot profile: workflow steps parse failed", "run", run.ID, "error", err)
		return conservative
	}
	var refs []string
	for _, s := range steps {
		switch s.Kind {
		case domain.StepKindTask, domain.StepKindApproval:
		default:
			continue // no worker ref → no adapter → no demand
		}
		if s.Ref == "" {
			continue
		}
		var modelRef string
		if s.WorkerVersion > 0 {
			if v, verr := db.GetWorkerVersionByNumber(ctx, ttx.Tx, run.TenantID, s.Ref, s.WorkerVersion); verr == nil {
				modelRef = v.ModelRef
			}
		}
		if modelRef == "" {
			if v, verr := db.GetLatestWorkerVersion(ctx, ttx.Tx, run.TenantID, s.Ref, true); verr == nil {
				modelRef = v.ModelRef
			}
		}
		refs = append(refs, modelRef)
	}
	return adapter.AdapterDemandSet(refs...)
}

// resolveWorkflowStepWorker returns the first PUBLISHED, role-bound worker referenced by any step
// of the run's workflow version.
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

// ServeDependent implements the scheduler.RuntimeLifecycle capability probe: the only
// serve-dependent adapter kind today is "opencode" (the in-container opencode serve).
func (l *Lifecycle) ServeDependent(kind string) bool {
	if kind == "" {
		kind = opencodeAdapterKind
	}
	return kind == opencodeAdapterKind
}

// EnsureForRun creates the runtime container for a workflow run (idempotent) with its opencode
// serve warmed at create time.
func (l *Lifecycle) EnsureForRun(ctx context.Context, run db.WorkflowRunRow) error {
	if l.client == nil {
		return fmt.Errorf("runtime daemon not reachable — this run resolves to runtime mode and no container can be created: switch the project (or this work item's execution_mode) to local, or start the runtime daemon")
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

// EnsureServing is the workflow run-start gate.
func (l *Lifecycle) EnsureServing(ctx context.Context, run db.WorkflowRunRow, needsServe bool) error {
	if run.RuntimeImage == runtimeNoServeImage {
		return nil
	}
	if !needsServe {
		return nil
	}
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
			// A MISSING CONTAINER BINARY IS NOT GOING TO APPEAR.
			if errors.Is(cerr, ErrNoContainerBinary) {
				return fmt.Errorf("runtime container cannot be created for run %s and retrying cannot fix "+
					"it: %w", run.ID, cerr)
			}
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

// planeHealthyAt reports whether the run's sandbox plane answers /healthz at the given base URL
// (the container bridge IP :8080).
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

// contextMounts converts a context_files JSONB payload into Docker bind-mount specs, skipping paths
// that lie under projectDir (already covered by the project-dir mount) and any path that isn't.
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

// ImagePresent reports whether a runtime image exists LOCALLY on the daemon's host — the presence
// probe the reconciler's dispatch fail-fast uses.
func (l *Lifecycle) ImagePresent(ctx context.Context, ref string) (bool, error) {
	if l == nil || l.client == nil {
		return false, errors.New("runtime lifecycle: no runtime daemon configured")
	}
	return l.client.ImagePresent(ctx, ref)
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
	// Evict the memoized plane credential: a re-created container must mint a
	// fresh, unexpired key rather than reuse a token from the reaped one.
	l.planeCredMu.Lock()
	delete(l.planeCredCache, runID)
	l.planeCredMu.Unlock()
	l.log.Info("workflow runtime reaped", "run", runID)
	return nil
}

// Adopt ensures a runtime lease exists for every active (pending/running) workflow run at boot.
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
		if run.RuntimeImage == runtimeNoServeImage {
			continue // legacy no-serve row: no container to adopt
		}
		// A LOCAL-MODE RUN HAS NO CONTAINER TO ADOPT, and asking for one both fails and MISLEADS.
		//
		// REPORTED from Windows, exactly:
		//
		//	msg="adopt: ensure runtime failed" error="...runtime daemon: mount source outside allowed
		//	    roots: C:\projects\weatherapp ..."
		//
		// for a run whose project was execution_mode=local. The run had not asked for a container; this
		// boot-time sweep asked on its behalf, could not mount the project, and logged a warning that
		// reads as a container misconfiguration rather than "a local run was wrongly treated as
		// containerised". It sent an operator hunting a mount-policy problem that did not exist.
		//
		// THE RECONCILER ALREADY GETS THIS RIGHT (shouldEnsureRunFor / runLocalMode), and adopt is the
		// path that disagreed with it. One rule, both callers.
		if l.runIsLocal(ctx, run) {
			continue
		}
		if err := l.EnsureForRun(ctx, run); err != nil {
			l.log.Warn("adopt: ensure runtime failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

// runIsLocal reports whether a run's EFFECTIVE execution mode is local, so the boot-time adopt
// sweep does not containerise a run that explicitly opted out.
func (l *Lifecycle) runIsLocal(ctx context.Context, run db.WorkflowRunRow) bool {
	if l.pool == nil {
		return false
	}
	ttx, err := l.pool.BeginTenantTx(ctx, devTenantID)
	if err != nil {
		return false
	}
	defer func() { _ = ttx.Rollback(ctx) }()

	var itemMode *string
	if strings.TrimSpace(run.WorkItemID) != "" {
		if w, werr := db.GetWorkItem(ctx, ttx.Tx, devTenantID, run.WorkItemID); werr == nil {
			itemMode = w.ExecutionMode
		}
	}
	return db.EffectiveExecutionMode(ctx, ttx.Tx, devTenantID, itemMode, run.ProjectID) == db.ExecutionModeLocal
}

// agentModelForServe renders a worker's model ref in the `provider/model` form an opencode CONFIG
// takes, or "" when it cannot be rendered. THE ADAPTER SEGMENT MUST BE STRIPPED.
func agentModelForServe(modelRef string) string {
	provider, model, ok := adapter.SplitForServe(modelRef)
	if !ok || provider == "" || model == "" {
		return ""
	}
	return provider + "/" + model
}

// dedupeMounts removes duplicate bind specs, keeping the FIRST occurrence of each (source,
// destination) pair.
func dedupeMounts(in []MountSpec) []MountSpec {
	if len(in) < 2 {
		return in
	}
	seen := make(map[MountSpec]struct{}, len(in))
	out := in[:0]
	for _, m := range in {
		key := MountSpec{Source: m.Source, Dest: m.Dest}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	return out
}
