// Package opencode implements the OpenCode adapter bridge — the single
// session-transport bridge that drives worker executions as persistent
// opencode sessions on a serve instance (docs/04_Runtime_Adapter_SDK.md
// §6).
//
// Transport strategy (docs/04 §6.0): every execution runs as a persistent
// opencode session created + driven over the serve HTTP+SSE API
// (session_run.go). The legacy one-shot `opencode run` subprocess path
// was REMOVED: a run that cannot get a session fails loudly
// (failed_to_start → workflow recovery) instead of silently degrading to
// a second, inferior transport. When OpenCode ships a stable IPC API, the
// adapter swaps its internals to an IPC client; the gRPC contract and
// control plane are unaffected.
//
// The adapter MUST NOT advertise capabilities the CLI surface cannot
// honestly deliver (docs/04 §6.2). v0.1 advertises a reduced
// capability set.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// Adapter is the OpenCode adapter bridge. It implements
// scheduler.AdapterBridge. Every execution runs as a persistent opencode
// session driven through a serve's HTTP+SSE API (session_run.go); the
// legacy one-shot `opencode run` subprocess transport was removed.
type Adapter struct {
	log *slog.Logger
	mu  sync.Mutex

	// sessions tracks live session-transport executions (execution_id →
	// runner). It is the routing table for SendExecutionMessage and makes
	// IsExecutionActive report session executions as alive until the plane
	// restarts (an empty map after boot = every session execution orphaned
	// and correctly reported dead).
	sessions map[string]*sessionRun

	// host is the always-on host opencode serve for the in-process
	// (local) execution population. Session-transport only.
	host *HostServe

	// rt is the workflow runtime daemon client. When non-nil AND an
	// execution carries a RuntimeWorkflowID, the adapter reaches that
	// workflow's runtime container serve for the session. Nil keeps
	// everything in-process (headless serve).
	rt *runtime.Client

	// usageRecorder records LLM usage (Postgres dual-write + OTel
	// metrics) on each step_finish event carrying tokens + cost
	// (docs/04 §6.1, docs/08 §5.2). Injected by the server; nil =
	// usage is not recorded (telemetry loss never blocks control flow
	// — docs/08 §8 invariant #5).
	usageRecorder UsageRecorderFunc

	// sessionStore persists the durable per-execution session transcript
	// (execution_session_parts). Injected by the server; nil = the
	// transcript is not recorded (e.g. tests).
	sessionStore SessionStoreFunc

	// consecutiveSessionErrors counts back-to-back model-layer session
	// failures across executions (guarded by mu). When it reaches
	// sessionErrorRecycleThreshold, the adapter recycles the affected
	// workflow's runtime container so the next dispatch gets a FRESH
	// serve — the observed wedge was a serve that kept answering health
	// (so the health watchdog never fired) but whose model turns failed
	// instantly, poisoning every auto-retry. Reset to zero on any
	// non-error progress (a successful step / tool call / message).
	consecutiveSessionErrors int

	// infraModelTurnRecycles counts runtime-container recycles performed
	// for the infra-model-turn class (a model call that couldn't reach the
	// model API at the socket/transport layer) within a SINGLE dispatch —
	// the per-dispatch repair budget for the immediate recycle path. Reset
	// on any non-error progress so a healthy step never inherits a prior
	// dispatch's spent budget.
	infraModelTurnRecycles int
}

// SessionStoreFunc persists transcript entries for one execution. The
// implementation owns the tenant transaction. It is the opencode alias
// for the scheduler contract type (scheduler.SessionStoreFunc) so the
// adapter reads identically to how it did before the contract move.
type SessionStoreFunc = scheduler.SessionStoreFunc

// SetRuntimeClient injects the workflow runtime daemon client. When set,
// executions with a RuntimeWorkflowID dispatch into that workflow's
// runtime container; without it the adapter runs in-process. It is the
// opencode implementation of scheduler.ConfigurableBridge.
func (a *Adapter) SetRuntimeClient(rt *runtime.Client) { a.rt = rt }

// SetHostServe injects the always-on host opencode serve manager. When
// set AND sessions are enabled, local (in-process) executions run as
// persistent sessions on it. Nil means no host serve is available — such
// executions fail fast (the one-shot subprocess path was removed). It is
// the opencode implementation of scheduler.ConfigurableBridge.
func (a *Adapter) SetHostServe(hs *HostServe) { a.host = hs }

// SendExecutionMessage routes a mid-run human message into a live session
// execution. It does NOT create a new execution, work item, or workflow
// state — the message joins the session's existing turn queue and the
// reply streams back through the normal execution event stream. Returns
// an error when the execution has no live session (already finished or
// unknown execution).
func (a *Adapter) SendExecutionMessage(ctx context.Context, execID, message string) error {
	a.mu.Lock()
	r := a.sessions[execID]
	a.mu.Unlock()
	if r == nil {
		return fmt.Errorf("execution %s has no live session (not running on the session transport)", execID)
	}
	if err := r.client.SendMessage(ctx, r.sessionID, r.system, r.modelRef, message); err != nil {
		return fmt.Errorf("send execution message: %w", err)
	}
	r.bumpPending()
	r.recordHumanMessage(message)
	a.log.Info("mid-run execution message sent", "execution", execID, "session", r.sessionID)
	return nil
}

// AbortExecution tears down a running execution's live opencode session so the
// model stops generating immediately. It is invoked when a human cancels an
// execution: the execution row is already transitioned to terminated, and this
// is what actually stops the token spend (without it the session keeps turning
// in the background — the "terminated but still active" runaway). It cancels
// the session's subscription context (ending the run loop) and aborts the
// opencode session on the serve. Unknown/finished executions are a no-op.
func (a *Adapter) AbortExecution(ctx context.Context, execID, reason string) error {
	a.mu.Lock()
	r := a.sessions[execID]
	a.mu.Unlock()
	if r == nil {
		a.log.Debug("abort execution: no live session", "execution", execID)
		return nil
	}
	a.log.Info("aborting execution session", "execution", execID, "session", r.sessionID, "reason", reason)
	// Cancel the run loop's subscription context first so the runner unwinds
	// (its defer cleans up the sessions registry), then abort the opencode
	// session so the model's current turn stops streaming.
	r.subCancel()
	if err := r.client.Abort(ctx, r.sessionID); err != nil {
		a.log.Warn("abort session failed", "execution", execID, "error", err)
	}
	return nil
}

// sessionsEnabled reports whether the session transport is enabled for an
// execution. The global kill-switch ORCHICON_OPCODE_SESSION_TRANSPORT=0
// disables it everywhere — with the one-shot path removed, a disabled
// transport means executions FAIL (fail-fast) rather than degrading.
func (a *Adapter) sessionsEnabled(manifest scheduler.ExecutionManifest) bool {
	return os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0"
}

// executionDir resolves the worker's working directory: the run's provisioned
// worktree path when set, else the project dir. The worktree lives under the
// project dir (.orchicon-worktrees/<runID>), so it is covered by the
// project-dir mount and passes the project-dir containment checks.
func executionDir(m scheduler.ExecutionManifest) string {
	if m.WorktreePath != "" {
		return m.WorktreePath
	}
	return m.ProjectDir
}

// sessionClientFor resolves the SessionClient for an execution: the
// per-container serve for workflow-run executions (ensuring the container
// exists + is serving), or the host serve for the in-process population.
// Returns nil when no serve is available — the caller fails the execution
// (the legacy one-shot fallback was removed).
func (a *Adapter) sessionClientFor(ctx context.Context, manifest scheduler.ExecutionManifest) *SessionClient {
	if a.rt != nil && manifest.RuntimeWorkflowID != "" {
		// The composite worktree MCP tools resolve relative paths against
		// ORCHICON_MCP_WORKTREE_DIR, which must be the execution's working
		// directory — the run worktree when provisioned, else the project
		// dir — NOT the project root. Passing the project root here (the
		// pre-fix behavior) made batch_write land in the main checkout
		// instead of the run worktree (worktree hygiene violation).
		// executionDir mirrors the reconciler's cwd resolution, so the
		// serve config baked for a self-healed/recreated container carries
		// the same base the run-start gate uses.
		resp, err := a.rt.Create(ctx, runtime.CreateRequest{
			WorkflowID:  manifest.RuntimeWorkflowID,
			Image:       manifest.RuntimeImage,
			Mounts:      projectMount(manifest.ProjectDir),
			ServeConfig: RuntimeServeConfig(manifest.RuntimeImage, executionDir(manifest), manifest.RuntimeWorkflowID, nil),
			ProjectDir:  manifest.ProjectDir,
		})
		if err != nil {
			a.log.Warn("session transport: ensure runtime container failed",
				"run", manifest.RuntimeWorkflowID, "execution", manifest.ExecutionID, "error", err)
			return nil
		}
		if resp.ServePort == 0 || resp.ServePassword == "" {
			return nil
		}
		// The daemon returns the plane-reachable base URL (the docker
		// bridge gateway, reachable from a containerized plane). Fall back
		// to loopback for host-plane deployments.
		baseURL := resp.ServeURL
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", resp.ServePort)
		}
		return NewSessionClient(baseURL, resp.ServePassword, executionDir(manifest))
	}
	if a.host != nil {
		return a.host.Client()
	}
	return nil
}

// RuntimeServeConfig builds the OPENCODE_CONFIG_CONTENT for a runtime
// container's serve given its runtime image tag: the permission rules only,
// with the operator's MCP servers omitted — a SERVE eagerly connects to
// every configured MCP server at startup, and the operator's entries (an
// `orchicon` MCP that docker execs, a local Playwright MCP) cannot run
// inside the sandbox, which would hang the serve (the one-shot run path
// tolerates MCP failures and keeps them). Worker system prompts ride the
// per-message `system` field instead.
//
// For DEV images (which boot the sandbox plane — Postgres + NATS +
// `orchicon serve` in-container), the serve ALSO registers the built-in
// Orchicon MCP against the sandbox DB: the `orchicon mcp` sidecar is
// pointed at the container-local Postgres via the entry's environment map
// (ORCHICON_POSTGRES_DSN), so workers get the `orchicon_*` tools natively
// against their own sandbox — never the host plane's DB. Base/gui images
// get no MCP (no sandbox plane), behavior identical to today.
func RuntimeServeConfig(imageTag, projectDir, workflowRunID string, planeEnv map[string]string) string {
	opts := ConfigOptions{
		AgentName:    workerAgent,
		AgentPrompt:  workerAgentPrompt,
		DefaultAgent: workerAgent,
		ModelRef:     "",
		SkipUserMCP:  true,
	}
	if runtime.IsDevImageTag(imageTag) {
		opts.TenantID = serveTenantID()
		opts.OrchiconMCP = true
		opts.MCPEnv = map[string]string{"ORCHICON_POSTGRES_DSN": runtime.SandboxPostgresDSN}
		// The sandbox worker's Orchicon MCP sidecar is told its workflow run so
		// it can inject the run_context into create calls and stamp recurring-
		// fire provenance (feature 4.1, AC2).
		if workflowRunID != "" {
			opts.MCPEnv["ORCHICON_MCP_WORKFLOW_RUN_ID"] = workflowRunID
		}
	}
	// Plane channel: when the runtime lifecycle minted a role-scoped worker
	// credential for this run, register the `orchicon-plane` MCP server so
	// the worker gets `orchicon_plane_*` tools against the REAL instance.
	// Orthogonal to the sandbox channel (dev images register `orchicon_*`
	// against the in-container sandbox DB) — a role-bound worker on a dev
	// image gets both.
	if len(planeEnv) > 0 {
		opts.PlaneMCP = true
		opts.PlaneMCPEnv = planeEnv
	}
	// The orchicon binary is bind-mounted read-only at /usr/local/bin/orchicon
	// in EVERY runtime container (the runtime daemon's own executable,
	// daemon.go:450) — never baked into the image. So the MCP sidecars always
	// run from there, regardless of image tag.
	opts.MCPBinaryPath = runtimeContainerBinaryPath

	// Composite worktree tools (batch_read / batch_grep / batch_write) are LIVE
	// for ALL runtime images, not just dev. The worktree MCP sidecar is
	// DB-less and runs from the daemon's bind-mounted binary, so it works on
	// the base/gui images too — it does not need the sandbox plane. The worker
	// is handed the batch tools and opencode's built-in read/grep are denied
	// (see config.go permissionRules), so it is forced onto the batch tools —
	// the whole point: fewer turns, less re-sent context. WorktreeDir is the
	// in-container project dir (the runtime daemon starts the serve with cwd
	// == project dir); ORCHICON_WORKTREE_DIR overrides it, and the serve
	// process cwd is the last-resort fallback. CompositeTools is only set when
	// a worktree dir resolves, so the batch MCP is always registered alongside
	// the read/grep deny (no lockout).
	opts.WorktreeDir = projectDir
	if opts.WorktreeDir == "" {
		opts.WorktreeDir = os.Getenv("ORCHICON_WORKTREE_DIR")
	}
	if opts.WorktreeDir == "" {
		if wd, werr := os.Getwd(); werr == nil {
			opts.WorktreeDir = wd
		}
	}
	opts.CompositeTools = opts.WorktreeDir != ""
	return BuildConfigContent(opts)
}

// startViaSession runs an execution through a persistent opencode session
// on the given serve. Returns nil once the execution completes (OnResult
// fired); a non-nil error means the session transport could not be set up
// — the caller surfaces it as a failed execution (no one-shot fallback).
func (a *Adapter) startViaSession(ctx context.Context, procCtx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks, client *SessionClient, modelRef string) error {
	if client == nil {
		return fmt.Errorf("no opencode serve available")
	}
	runner := &sessionRun{
		a:         a,
		parentCtx: ctx,
		procCtx:   procCtx,
		execRow:   execRow,
		manifest:  manifest,
		callbacks: callbacks,
		client:    client,
		modelRef:  modelRef,
		system:    executionSystemPrompt(manifest),
		done:      make(chan struct{}),
		stats:     &execStreamState{},
		// Unified warn→escalate→abort ladder. The spend accumulator starts
		// empty; the merged budget is parsed once (limits + warning schedule).
		budget:     &budgetAccumulator{},
		budgetSpec: parseBudgetSpec(manifest.Budgets),
		startedAt:  time.Now(),
	}
	return runner.run()
}

// executionSystemPrompt returns the per-session system prompt for an
// execution. When the execution runs inside a workflow runtime container
// with the composite worktree tools enabled (a runtime container with a
// project dir), it appends the batch-tool discipline so the worker is
// steered to `batch_read`/`batch_grep`/`batch_write` instead of the granular
// built-in tools — the whole point of the composite-tools feature (fewer
// turns, less re-sent context). Host-serve executions (RuntimeWorkflowID
// empty) have no composite tools and get the bare worker system prompt.
func executionSystemPrompt(manifest scheduler.ExecutionManifest) string {
	sp := manifest.SystemPrompt
	if manifest.WorktreePath != "" {
		sp += worktreePathDiscipline
	}
	if manifest.RuntimeWorkflowID != "" && manifest.ProjectDir != "" {
		sp += batchToolsDiscipline
	}
	return sp
}

// batchToolsDiscipline steers the worker to the composite context-efficient
// file tools. It is phrased defensively ("if available") so a worker whose
// runtime does not expose them falls back to the built-ins without error, and
// it explicitly forbids the granular tools so the model does not keep reaching
// for read/grep in batches (the conflicting behaviour the old guidance caused).
const worktreePathDiscipline = "\n\nAll file reads/writes must use paths relative to the current worktree; the main checkout is not accessible.\n"

const batchToolsDiscipline = "\n\n# Tool discipline (composite worktree tools)\n" +
	"Use the composite file tools for ALL file access:\n" +
	"- `batch_read` reads several files or a whole directory in ONE call.\n" +
	"- `batch_grep` searches several patterns across the tree in ONE call.\n" +
	"- `batch_write` applies several create/overwrite/edit/append writes in ONE atomic call.\n" +
	"- Do NOT use `read`, `grep`, `glob`, `write`, or `edit` for file access when the batch tools are available — they are fallback-only.\n" +
	"- Never re-read a file whose content is already in context — every extra tool call re-sends the whole conversation.\n"

// IsExecutionActive reports whether an in-process execution subprocess is
// still tracked as running. Used by the execution-liveness reaper to
// detect executions orphaned by a control-plane restart (a fresh boot has
// an empty sessions registry, so every previously-running execution is
// correctly reported dead). Session-transport executions are tracked in
// the sessions registry and report active while their runner lives.
func (a *Adapter) IsExecutionActive(execID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[execID]
	return ok
}

// UsageRecord is the usage sample the adapter emits on step_finish
// (docs/04 §6.1 step_finish carries tokens + cost). It is the opencode
// alias for the scheduler contract type (scheduler.UsageRecord) — the
// server copies it field-for-field into the gateway's input so the
// gateway never branches on provider.
type UsageRecord = scheduler.UsageRecord

// UsageRecorderFunc records a usage sample. Decoupled from the
// aigateway package via a function type so the adapter has no import
// dependency on the gateway (docs/04 §6.0: adapter is a thin bridge). It
// is the opencode alias for the scheduler contract type.
type UsageRecorderFunc = scheduler.UsageRecorderFunc

// SetUsageRecorder injects the usage recording callback. The server
// constructs it from the aigateway.UsageRecorder. It is the opencode
// implementation of scheduler.ConfigurableBridge.
func (a *Adapter) SetUsageRecorder(fn UsageRecorderFunc) { a.usageRecorder = fn }

// SetSessionStore injects the durable transcript writer. The server wraps
// db.AppendExecutionSessionParts in a tenant transaction. Nil = the
// session transcript is not persisted. It is the opencode implementation
// of scheduler.ConfigurableBridge.
func (a *Adapter) SetSessionStore(fn SessionStoreFunc) { a.sessionStore = fn }

// workerAgent is the opencode agent name the adapter injects the worker's
// composed system prompt under (selected with --agent).
const workerAgent = "orchicon-worker"

// workerAgentPrompt is the MINIMAL system prompt registered for the
// orchicon-worker agent. It deliberately carries ONLY a tool inventory and
// tool-call discipline — the worker's actual identity/task/context rides
// Orchicon's own per-message `system` field. The point is to REPLACE
// opencode's large built-in `build` agent prompt (which the default agent
// would otherwise inject into every turn) with this short shell, cutting
// per-turn tokens. The tool list restores the "which tool fits which job"
// guidance opencode's build prompt previously supplied, without its
// verbosity.
const workerAgentPrompt = "You are an autonomous coding agent.\n\n" +
	"File access tools (use these for all reading, searching, and writing):\n" +
	"- `batch_read` — read several files or a whole directory in ONE call\n" +
	"- `batch_grep` — search several patterns across the tree in ONE call\n" +
	"- `batch_write` — apply several create/overwrite/edit/append writes in ONE atomic call\n\n" +
	"Other tools:\n" +
	"- `glob` — find files by pattern\n" +
	"- `bash` — run a shell command in the project\n" +
	"- `todowrite` — maintain the live task-progress list (emit it every turn)\n" +
	"- `webfetch` — fetch web content from a URL\n" +
	"- `websearch` — search the web (use only if needed)\n" +
	"- `skill` — load a skill's instructions\n" +
	"- `orchicon_*` — Orchicon platform tools: projects, work items, workers, workflows, executions, policies, runtime images, usage, settings, and the project-directory list/read tools.\n\n" +
	"Discipline — read carefully:\n" +
	"- Do NOT use `read`, `grep`, `write`, or `edit` for file access; they are disabled in favor of the batch tools. Use `glob` only to find paths, never to read.\n" +
	"- Never re-read a file whose content is already in context — every extra tool call re-sends the whole conversation.\n" +
	"- Bundle independent reads/searches/writes into ONE batch call; never split related work across many micro calls.\n" +
	"- Prefer the fewest tool calls that complete the task."

// runtimeContainerBinaryPath is where the runtime daemon bind-mounts its
// own executable in every runtime container (internal/runtime/daemon.go).
// The sandbox Orchicon MCP sidecar spawns there, never at the plane's
// (host) executable path.
const runtimeContainerBinaryPath = "/usr/local/bin/orchicon"

// New creates an OpenCode adapter bridge.
func New(log *slog.Logger) *Adapter {
	return &Adapter{
		log:      log,
		sessions: make(map[string]*sessionRun),
	}
}

// Start runs the given execution as a persistent opencode session on a
// serve instance (the host serve for the in-process population, the
// workflow's runtime container serve for workflow runs) and streams
// telemetry back via the callbacks (docs/03 §4, docs/04 §6). The session
// runs until completion or context cancellation.
//
// Per AGENTS.md verification standards: this adapter calls the REAL
// `opencode` runtime. Simulation mode is an explicit opt-in via the
// ORCHICON_SIMULATE_ADAPTER=1 env var (offline dev only) — it is NOT
// a silent fallback. If `opencode` is absent from PATH and simulation
// is not explicitly enabled, Start returns an error so the failure is
// loud and visible (do not fall back to simulation and claim dispatch
// works). Verification workers/executions must pin a free model in
// model_ref (e.g. opencode/deepseek-v4-flash-free).
//
// The legacy one-shot `opencode run` subprocess path was REMOVED: when
// no serve is available (or the session transport is disabled via
// ORCHICON_OPCODE_SESSION_TRANSPORT=0), Start returns an error and the
// execution fails (failed_to_start → workflow recovery) instead of
// degrading to a second, inferior transport.
//
// Two recovery-relevant guardrails (docs/06 §2 triggers):
//   - Stall detection: a progress monitor detects stuck-looping (no
//     progress, no file changes, repeated tool calls) and raises
//     OnStall → triggers recovery. Catches the loop a hard timeout
//     can't (a worker making "progress" but spinning).
//   - Wall-clock timeout: the worker's budget_overrides.wall_clock_seconds
//     (default 3600) is enforced as a per-execution context deadline.
//     When it hits, the session turn is aborted →
//     OnResult(false) → recovery triggered with reason
//     "wall_clock_timeout". This is the runaway-spend backstop.
func (a *Adapter) Start(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	// Simulation mode is opt-in ONLY (offline dev). Never a silent
	// fallback (AGENTS.md verification standards).
	if os.Getenv("ORCHICON_SIMULATE_ADAPTER") == "1" {
		a.log.Warn("opencode simulation mode ENABLED via ORCHICON_SIMULATE_ADAPTER=1 (offline dev only — not for verification)", "execution", execRow.ID)
		return a.runSimulation(ctx, execRow, manifest, callbacks)
	}

	// Resolve the adapter CLI. Orchicon never ships opencode — in
	// containerized deployments the operator's host install is mounted at
	// $HOME/.opencode (see scripts/container.sh and the runtime daemon's
	// standard mounts), so besides PATH we also probe that location
	// directly. The error is loud either way: the caller (TaskReconciler)
	// marks the execution failed_to_start and the operator sees it
	// (AGENTS.md).
	binary, err := exec.LookPath("opencode")
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, ".opencode", "bin", "opencode")
			if st, serr := os.Stat(cand); serr == nil && !st.IsDir() {
				binary = cand
			}
		}
	}
	if binary == "" {
		return fmt.Errorf("opencode binary not found on PATH or ~/.opencode/bin (install it on the host: curl -fsSL https://opencode.ai/install | bash; set ORCHICON_SIMULATE_ADAPTER=1 only for offline dev): %w", err)
	}

	// Wall-clock timeout backstop (docs/06 §2 budget overrun trigger).
	// The worker's budget_overrides.wall_clock_seconds bounds the
	// subprocess; when the deadline hits the process is killed →
	// OnResult(false) → recovery with reason "wall_clock_timeout".
	//
	// The deadline is applied ONLY to the subprocess context (procCtx),
	// never to the ctx handed to callbacks: when the deadline fires the
	// process is killed, but the terminal OnResult/OnStall writebacks must
	// still complete against the DB. Passing the exhausted ctx into
	// BeginTenantTx fails with "context deadline exceeded" and the
	// execution stays running forever (observed live on a wall-clock E2E).
	procCtx := ctx
	if deadline, ok := wallClockDeadline(ctx, manifest.Budgets); ok {
		var cancel context.CancelFunc
		procCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	// Resolve the model reference. This is the ONLY dispatch gate here —
	// the session transport sends it per message (docs/04 §6.0).
	modelRef := manifest.ModelRef
	if modelRef == "" {
		modelRef = manifest.DefaultModelRef
		if modelRef == "" {
			return fmt.Errorf("no model_ref specified on worker and no default model set in tenant settings — dispatch rejected")
		}
		a.log.Info("no model_ref on worker, using tenant default", "model", modelRef, "execution", execRow.ID)
	}

	// Best-effort: drop the safety lint script into .orchicon/ so review
	// and QA workers can run it (their bash tool is scoped to the project
	// directory). See internal/opencode/lint.go.
	writeSafetyLint(executionDir(manifest))

	// Session transport is the ONLY execution transport. Drive the
	// execution through a persistent opencode session on a serve instance
	// (the always-on host serve for the in-process population, or the
	// workflow's runtime container serve). It enables the liveness nudge,
	// mid-run human messages, and SSE-streamed progress.
	//
	// The legacy one-shot `opencode run` subprocess path is REMOVED: when
	// no serve is available the execution FAILS (failed_to_start →
	// workflow recovery) instead of silently degrading to a second,
	// inferior transport that Orchicon deliberately moved away from.
	if !a.sessionsEnabled(manifest) {
		return fmt.Errorf("opencode session transport disabled (ORCHICON_OPCODE_SESSION_TRANSPORT=0) — no execution transport available for execution %s", execRow.ID)
	}

	// Session-transport setup with self-heal. The serve converges within a
	// minute of its container starting (cold start: providers/MCP + the
	// docker-proxy settling), so session setup retries with backoff — but
	// never falls back to a one-shot subprocess. On top of the plain backoff
	// retries, an INFRA failure (see isInfraSessionError: serve unreachable —
	// connection refused — or POST /session 5xx on a poisoned session store)
	// triggers a bounded RUNTIME-CONTAINER REPAIR: the run's container is
	// recycled so the next dispatch builds a fresh serve AND a fresh store.
	// A poisoned store cannot be fixed by restarting the serve process (the
	// daemon watchdog reuses the same XDG data dir on disk — the observed
	// field class), so recycling the container is the only repair that
	// unblocks the run; the step's on-disk worktree state survives, so the
	// re-dispatch continues the work rather than starting cold.
	var lastErr error
	repairs := 0
	maxRepairs := sessionRepairBudget()
	consecutiveInfra := 0
	infraThreshold := infraRepairThreshold()
	for attempt := 0; attempt < 4+maxRepairs; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		// Re-resolve the client every attempt so a repair (container killed)
		// is picked up: sessionClientFor re-runs Create, which rebuilds the
		// container and returns the freshly-published serve once it answers
		// health — the repair's health-gate before re-dispatch.
		client := a.sessionClientFor(ctx, manifest)
		if client == nil {
			// No serve to talk to at all. For a runtime-container run this
			// is itself an infra condition: recycle and retry (bounded).
			lastErr = fmt.Errorf("no opencode serve available for execution %s (host serve down or runtime container serve unavailable) — execution failed to start", execRow.ID)
			consecutiveInfra++
		} else if err := a.startViaSession(ctx, procCtx, execRow, manifest, callbacks, client, modelRef); err == nil {
			return nil
		} else {
			lastErr = err
			a.log.Info("session transport setup attempt failed — retrying",
				"execution", execRow.ID, "attempt", attempt+1, "max", 4+maxRepairs, "error", err)
			if isInfraSessionError(err) {
				consecutiveInfra++
			} else {
				// A worker/model-side failure won't be fixed by recycling
				// the container — reset the counter so a later transient
				// infra blip starts fresh (and never nukes the container
				// on a single infra failure without a worker error in
				// between).
				consecutiveInfra = 0
			}
		}

		// Infra repair is NOT first-resort: a single infra failure is retried
		// on the SAME container (a fresh session create — the serve converges,
		// transient provider hiccups happen), so a HEALTHY parallel step on
		// the same run container is not torn down by one blip. Only a
		// PERSISTENT infra failure — consecutiveInfra at the threshold — is
		// treated as a genuinely broken backend (serve dead / store poisoned)
		// and repaired by recycling the container: the poisoned store lives
		// on disk in the container's stable XDG data dir, so only a fresh
		// container discards it; the step's on-disk worktree survives.
		// Bounded by sessionRepairBudget.
		if consecutiveInfra >= infraThreshold && repairs < maxRepairs && a.rt != nil && manifest.RuntimeWorkflowID != "" {
			repairs++
			consecutiveInfra = 0
			a.repairRuntimeContainer(ctx, manifest.RuntimeWorkflowID)
			continue
		}
		// Non-infra, or below the repair threshold, or past the repair
		// budget: keep the backoff retries; the step's own retry/recovery
		// loop is the bounded owner of persistent failures.
	}
	return fmt.Errorf("session transport setup failed: %w", lastErr)
}

// infraRepairThreshold is how many CONSECUTIVE infra failures (within one
// dispatch's session-setup) must occur before the adapter recycles the run's
// runtime container. Default 2: the first infra failure is retried on the
// same container so a transient service blip never tears down a healthy
// parallel step on the same run container; a second consecutive infra
// failure in the same dispatch indicates a broken backend worth repairing.
// Overridable via ORCHICON_SESSION_INFRA_THRESHOLD; < 2 means container
// repair can never be triggered by the consecutive counter (only the repair
// budget, reached via repeated infra failures across executions, recycles).
func infraRepairThreshold() int {
	if v := os.Getenv("ORCHICON_SESSION_INFRA_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 2
}

// repairRuntimeContainer is the session-backend infra repair: it kills the run's
// runtime container so the NEXT dispatch's Create rebuilds it with a fresh
// opencode serve and a FRESH session store. Restarting the serve process in
// place cannot heal a poisoned store (the daemon watchdog reuses the same XDG
// data dir on disk), which is why this is the kernel of "restart the runtime
// and continue": a Killed container has none of the poisoned on-disk state.
// The step's worktree (the on-disk work it already did) is untouched.
//
// The caller (the dispatch retry loop) re-resolves the SessionClient after
// this returns; the next sessionClientFor → rt.Create blocks until the fresh
// serve answers health, which is the repair's health-gate before re-dispatch.
// Best-effort: a failed kill is logged and the dispatch proceeds to its next
// attempt (backoff) as today.
func (a *Adapter) repairRuntimeContainer(ctx context.Context, workflowID string) {
	if a.rt == nil || workflowID == "" {
		return
	}
	a.log.Warn("session backend unhealthy — recycling runtime container (fresh serve + store)", "run", workflowID)
	sessionRecycleMetricsSingleton.recordInfraSessionCreate()
	killCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.rt.Kill(killCtx, workflowID); err != nil {
		a.log.Warn("session-backend repair: recycle runtime container failed", "run", workflowID, "error", err)
	}
}

// execStreamState tracks per-execution opencode stream structure so a
// run that ends before its final model step completes can be flagged as
// failed instead of reported as a clean, empty success. opencode emits a
// step_start before every model turn and a step_finish when the turn
// ends; a clean exit (exit 0) with an unpaired step_start means the
// final response was lost in transit (line-cap drop, stream truncation,
// runtime disconnect) — the worker's output and decision signal are
// missing, so the run must NOT count as succeeded.
type execStreamState struct {
	stepStarts   int
	stepFinishes int

	// toolUses counts evtToolUse events for the execution — the real
	// per-tool-call granularity (a single step/turn can contain several
	// tool calls before its step_finish). Drives the tool-call dimension of
	// the unified warn→escalate→abort budget ladder (maybeEnforceLadder).
	toolUses int

	// writtenFiles accumulates the paths opencode reported as file
	// modifications (file_diff events). Unlike diff markers parsed out of
	// the worker's text output, this is the telemetry the runtime itself
	// emits for every file the session writes or edits — including files
	// the model saves without echoing a diff (e.g. .orchicon/ review
	// notes written via the Write tool). Surfaced to the next worker so
	// it can read what the previous step actually produced.
	writtenFiles []string

	// truncatedFinish marks a FINAL step_finish that indicates the model
	// turn was interrupted rather than completed: reason "unknown" with
	// zero tokens (the signature of a truncated/aborted response — the
	// failing run's last part was exactly `step_finish {reason:"unknown",
	// tokens all 0}`). A run ending on such a step is not a clean success:
	// the worker's final text / ORCHICON WORKER SUMMARY is missing even
	// though the step counters are balanced (38/38 in the original bug).
	truncatedFinish bool
}

// unfinished reports whether the stream ended mid-step OR on a truncated
// final step — either way the final model response never completed.
func (s *execStreamState) unfinished() bool {
	if s == nil {
		return false
	}
	return s.stepStarts > s.stepFinishes || s.truncatedFinish
}

// allTokensZero reports whether a step_finish's tokens map is empty or all
// counts are zero. A real completion carries input/output/cache counts; an
// interrupted turn (reason "unknown") is emitted with no usage at all.
//
// opencode emits cache counts as a nested sub-object ({"cache":{"read":N,
// "write":M}}), so a cache-served turn has non-zero token counts but an
// empty input/output at the top level. The old top-level-only scan treated
// such a turn as "zero" and wrongly flagged a real cache-served completion
// as a truncated finish. The predicate must look inside the cache sub-object
// before deciding.
func allTokensZero(tokens map[string]any) bool {
	if len(tokens) == 0 {
		return true
	}
	for _, v := range tokens {
		if n, ok := v.(float64); ok && n > 0 {
			return false
		}
		if m, ok := v.(map[string]any); ok {
			for _, sub := range m {
				if n, ok := sub.(float64); ok && n > 0 {
					return false
				}
			}
		}
	}
	return true
}

// parseEvent dispatches a decoded opencode event into the telemetry
// pipeline. It is the single dispatch used by the session transport
// (legacyEventFromBus builds the {type, part} object from the server SSE
// bus; parseStdoutLine unmarshals a JSON line into the same shape). One
// dispatch guarantees consistent downstream behavior: progress monitor,
// usage recording, artifact capture, summary accumulation, and the
// streaming callbacks.
func (a *Adapter) parseEvent(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, evt map[string]any, callbacks scheduler.ExecutionCallbacks, monitor *progressMonitor, output *strings.Builder, lastStreamErr *string, textSeq *int, stats *execStreamState, budget *budgetAccumulator) {
	eventType, _ := evt["type"].(string)
	part, _ := evt["part"].(map[string]any)
	if monitor != nil {
		monitor.observe(eventType, part)
	}
	execID := execRow.ID
	switch eventType {
	case evtStepStart:
		if stats != nil {
			stats.stepStarts++
		}
		a.log.Info("opencode step started", "execution", execID)
	case evtText:
		// Text part: the model's response text. PR B: append to the
		// accumulator so the TaskReconciler can extract the
		// ORCHICON WORKER SUMMARY block on completion. Also fan the
		// text out as incremental chunks (textStreamingChunkSize +
		// textStreamingChunkDelay) so the runtime session pane shows
		// a typing-style live stream — opencode's --format json
		// delivers the full text in one event at step_finish, so
		// without chunking the user would see no streaming at all.
		text, _ := part["text"].(string)
		if output != nil && text != "" {
			output.WriteString(text)
		}
		if text != "" {
			a.emitTextChunked(ctx, callbacks, execID, text, textSeq)
		}
		a.log.Debug("opencode text", "execution", execID, "text_len", len(text))
	case evtToolUse:
		if stats != nil {
			stats.toolUses++
		}
		// opencode v1.x: input + output both arrive in a single
		// `tool_use` event. The previous dispatch only matched the
		// legacy `tool_call` / `tool_result` pair — which v1.x never
		// emits — so tool calls were silently dropped and the
		// runtime session pane never saw any tool cards. The model's
		// actual work (file writes, bash commands, web fetches) was
		// invisible to the operator. Handle the v1.x shape:
		//   {
		//     "type": "tool_use",
		//     "part": {
		//       "tool": "bash",
		//       "callID": "...",
		//       "state": {
		//         "status": "completed",
		//         "input":  {...},
		//         "output": "...",
		//         "title":  "...",
		//         "time":   {...}
		//       }
		//     }
		//   }
		toolName, _ := part["tool"].(string)
		state, _ := part["state"].(map[string]any)
		inRaw, _ := state["input"]
		outStr, _ := state["output"].(string)

		// Cap tool OUTPUT before it is streamed to the UI / persisted into
		// the durable transcript (which a follow-up or a recovery-resumed
		// session re-seeds as context). A single giant output (a `make ci`
		// build log, a full `git diff`, a huge glob/find listing) otherwise
		// inflates everything downstream of this execution. Truncate with a
		// clear marker so the worker/operator knows the tail is available on
		// the host or project disk without ballooning the transcript.
		outStr = capToolOutput(outStr)

		// Detect `write` tool calls (opencode built-in file writer)
		// and route them as artifacts instead of raw tool calls. The
		// model uses `write` to save output files (essays, configs,
		// code); capturing the content as an artifact event lets the
		// frontend render it inline as a rich document card instead
		// of a truncated tool input (docs/10 §11).
		if toolName == "write" {
			if inputMap, ok := inRaw.(map[string]any); ok {
				if content, ok := inputMap["content"].(string); ok && content != "" {
					path, _ := inputMap["path"].(string)
					a.log.Info("opencode write artifact",
						"execution", execID, "path", path, "content_len", len(content))
					// Stream the content as text FIRST so the user sees
					// the story appear incrementally in the runtime
					// session pane (chunked into 40-char pieces with
					// 60ms delay for a typing-style effect). Then emit
					// the artifact card with the full content so the
					// operator can inspect/download/copy the file.
					// Without this, the entire content arrives as one
					// artifact event at the END of the model's
					// processing, and the user sees "Waiting for model
					// output…" → nothing for 30s → artifact burst.
					if output != nil {
						output.WriteString(content)
					}
					a.emitTextChunked(ctx, callbacks, execID, content, textSeq)
					callbacks.OnArtifact(ctx, execID, path, artifactTypeFromPath(path), content)
					break // skip normal tool call — artifact event is sufficient
				}
			}
		}
		if toolName == "write_artifact" {
			if inputMap, ok := inRaw.(map[string]any); ok {
				if content, ok := inputMap["content"].(string); ok && content != "" {
					name, _ := inputMap["name"].(string)
					typ, _ := inputMap["type"].(string)
					if typ == "" {
						typ = "text"
					}
					if name == "" {
						name, _ = inputMap["path"].(string)
					}
					a.log.Info("opencode write_artifact",
						"execution", execID, "name", name, "type", typ, "content_len", len(content))
					if output != nil {
						output.WriteString(content)
					}
					a.emitTextChunked(ctx, callbacks, execID, content, textSeq)
					callbacks.OnArtifact(ctx, execID, name, typ, content)
					break
				}
			}
		}

		// `inp` is the JSON-marshalled input object so the frontend
		// can render it as a structured "Input:" block. If
		// marshalling fails (rare — input is always a JSON object)
		// fall back to a string form so the operator still sees what
		// was attempted.
		inp, err := json.Marshal(inRaw)
		if err != nil {
			inp = []byte(fmt.Sprintf("%v", inRaw))
		}
		a.log.Info("opencode tool use",
			"execution", execID, "tool", toolName,
			"status", state["status"], "output_len", len(outStr))
		callbacks.OnToolCall(ctx, execID, toolName, inp, []byte(outStr))
	case evtReasoning:
		// v1.x reasoning content (only when --thinking is enabled
		// on the opencode CLI). Show as a separate reasoning block
		// so the operator can see what the model was "thinking"
		// before each assistant turn. Without this the live pane
		// just shows the final answer.
		reasonText, _ := part["text"].(string)
		if reasonText == "" {
			return
		}
		a.log.Debug("opencode reasoning", "execution", execID, "len", len(reasonText))
		// Reasoning is also streamed chunk-by-chunk so it appears to
		// unfold live. We tag it with a `kind: reasoning` prefix in
		// the JSON payload so the frontend can render it in a
		// distinct style without a new event-type enum.
		a.emitReasoningChunked(ctx, callbacks, execID, reasonText, textSeq)
	case evtStepFinish:
		// Step completion carries token usage + cost (docs/04 §6.1).
		// Record it via the AI Gateway dual-write: Postgres source of
		// truth + OTel metrics → VictoriaMetrics (docs/08 §5.2). Best-effort
		// — telemetry loss never blocks control flow (docs/08 §8).
		tokens, _ := part["tokens"].(map[string]any)
		cost, _ := part["cost"].(float64)
		if stats != nil {
			stats.stepFinishes++
			// A step_finish with reason "unknown" and zero tokens is the
			// signature of a truncated/interrupted model turn (the failing
			// run's last part was exactly that). Flag it so the final
			// result can be downgraded to a failure instead of a clean
			// success with no decision signal.
			reason, _ := part["reason"].(string)
			if (reason == "unknown" || reason == "") && allTokensZero(tokens) {
				stats.truncatedFinish = true
			}
		}
		a.log.Info("opencode step finished", "execution", execID, "cost", cost, "tokens", tokens)
		a.recordUsage(ctx, execRow, manifest, tokens, cost)
		// Feed the compact-budget spend accumulator (same token/cost
		// unpacking recordUsage just used — no second pricing formula).
		if budget != nil {
			budget.add(tokens, cost)
		}
	case evtFileDiff:
		// A file_diff event carries the path of a file the session wrote
		// or edited (part["path"]). Capture it so the worker's written
		// files — including ones written via the Write tool without a
		// diff echoed in the output text — can be passed to the next
		// worker as explicit context to read. Dedup: a file edited many
		// times in one run is listed once.
		if stats != nil {
			if p, ok := part["path"].(string); ok && p != "" {
				for _, existing := range stats.writtenFiles {
					if existing == p {
						ok = false
						break
					}
				}
				if ok {
					stats.writtenFiles = append(stats.writtenFiles, p)
					a.log.Debug("opencode file modified", "execution", execID, "path", p)
				}
			}
		}
	case evtHealth:
		if state, ok := evt["state"].(string); ok {
			callbacks.OnHealth(ctx, execID, state)
		}
	case evtError:
		// opencode's --format json emits an error event shaped like
		// {"type":"error","error":{"name":"...","data":{"message":"..."}}}
		// — the human-readable message lives at error.data.message, NOT
		// at the top level (which has no `message` field). The previous
		// implementation read evt["message"] and got "" every time,
		// silently dropping the actual reason. Read it correctly here
		// AND stash the message in lastStreamErr so the OnResult call
		// below can include it in error_message (docs/04 §6.1: errors
		// surfaced via telemetry must also be surfaced in the failure
		// reason).
		msg := extractErrorMessage(evt)
		a.log.Warn("opencode error", "execution", execID, "message", msg)
		if msg != "" {
			*lastStreamErr = msg
		}
		callbacks.OnHealth(ctx, execID, domain.HealthUnhealthy)
	default:
		a.log.Debug("opencode event", "execution", execID, "type", eventType)
	}
}

// emitTextChunked fans a single assistant-text payload out as a stream
// of smaller chunks, each separated by a short delay, so the frontend
// RuntimeSessionPane can render a typing-style live view. Without this,
// opencode's --format json mode delivers the entire response in one
// `text` event at step_finish and the user would see nothing until
// completion.
//
// Each chunk is delivered via callbacks.OnText with a per-execution
// sequence number so the frontend can order chunks even if NATS
// delivers them out of order. The accumulator (`output`) is NOT
// touched here — it is updated in parseEvent's text case so the
// ORCHICON WORKER SUMMARY block is still extractable at OnResult time.
func (a *Adapter) emitTextChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, execID, text string, seq *int) {
	if text == "" {
		return
	}
	// Honor cancellation: if the context is done (execution
	// cancelled/terminated), drop remaining chunks. The final
	// accumulated output is still in `output` and will be folded
	// into OnResult.
	for i := 0; i < len(text); i += textStreamingChunkSize {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := i + textStreamingChunkSize
		if end > len(text) {
			end = len(text)
		}
		chunk := text[i:end]
		*seq++
		callbacks.OnText(ctx, execID, chunk)
		// Pace the chunks so the frontend actually has time to
		// render them. We skip the delay after the very last
		// chunk — there's nothing to wait for.
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// emitReasoningChunked is the reasoning-content counterpart to
// emitTextChunked. Reasoning text arrives on a `reasoning` event
// (opencode --thinking mode) and is also delivered in one big chunk,
// so we fan it out the same way. Each chunk is tagged via a JSON
// wrapper in the payload so the frontend can distinguish reasoning
// from assistant text without needing a new event-type enum value.
func (a *Adapter) emitReasoningChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, execID, text string, seq *int) {
	if text == "" {
		return
	}
	for i := 0; i < len(text); i += textStreamingChunkSize {
		select {
		case <-ctx.Done():
			return
		default:
		}
		end := i + textStreamingChunkSize
		if end > len(text) {
			end = len(text)
		}
		chunk := text[i:end]
		*seq++
		wrapped := map[string]any{
			"kind": "reasoning",
			"text": chunk,
			"seq":  *seq,
		}
		payload, _ := json.Marshal(wrapped)
		// Use the existing OnText callback but encode the reasoning
		// marker in the payload so the frontend can render it
		// differently. OnText writes the payload verbatim into the
		// outbox row.
		callbacks.OnText(ctx, execID, string(payload))
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// recordUsage records a usage sample from a step_finish event via the
// AI Gateway dual-write (docs/08 §5.2). It extracts token counts from
// the opencode JSON shape and derives provider/model from the manifest's
// ModelRef (which the human defined — docs/05 §11). Best-effort: a nil
// recorder means usage is not recorded (docs/08 §8).
//
// Token field naming: opencode emits `tokens.input` / `tokens.output`
// (plus reasoning and a cache sub-object) at the top level of the
// tokens object. The previous version read `prompt_tokens` /
// `completion_tokens` — names that don't appear on the wire — so
// `recordUsage` always saw zeros and the early-return dropped every
// sample. Result: usage_records stayed empty even when the model was
// clearly running, and the AI Gateway's Postgres source-of-truth had
// no data to surface. Now reads the actual opencode fields.
func (a *Adapter) recordUsage(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, tokens map[string]any, cost float64) {
	if a.usageRecorder == nil {
		return
	}
	promptTokens := toInt64(tokens["input"])
	cacheReadTokens := toInt64(cacheToken(tokens, "read"))
	cacheWriteTokens := toInt64(cacheToken(tokens, "write"))
	completionTokens := toInt64(tokens["output"])
	reasoningTokens := toInt64(tokens["reasoning"])
	// A genuinely empty sample (no tokens, no cost) is dropped. A cache-only
	// turn that was fully served from cache (zero fresh input/output but
	// non-zero cache read/write) must NOT be dropped — cache spend is real.
	if promptTokens == 0 && cacheReadTokens == 0 && cacheWriteTokens == 0 &&
		completionTokens == 0 && reasoningTokens == 0 && cost == 0 {
		return
	}
	provider, model, ok := adapter.SplitForServe(manifest.ModelRef)
	if !ok {
		// A malformed/empty model ref on a usage sample: attribute to
		// "unknown" so the record is never dropped on the parse.
		provider, model = "unknown", "unknown"
	}
	in := UsageRecord{
		TenantID:         execRow.TenantID,
		ProjectID:        execRow.ProjectID,
		TaskID:           execRow.TaskID,
		ExecutionID:      execRow.ID,
		WorkerID:         manifest.WorkerID,
		Provider:         provider,
		Model:            model,
		PromptTokens:     promptTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: cacheWriteTokens,
		CompletionTokens: completionTokens,
		ReasoningTokens:  reasoningTokens,
		CostUSD:          cost,
		WorkflowRunID:    execRow.WorkflowRunID,
	}
	if err := a.usageRecorder(ctx, in); err != nil {
		a.log.Warn("usage record failed", "execution", execRow.ID, "error", err)
	}
}

// cacheToken reads a sub-count from the opencode tokens.cache sub-object
// (e.g. {"cache":{"read":N,"write":M}} → cacheToken(tokens,"read")). opencode
// emits cache counts as a nested object rather than flat fields, so a plain
// toInt64(tokens["cache"]) would always be 0. Returns nil when the cache
// sub-object is absent, which toInt64 turns into 0.
func cacheToken(tokens map[string]any, key string) any {
	if cache, ok := tokens["cache"].(map[string]any); ok {
		return cache[key]
	}
	return nil
}

// extractErrorMessage pulls the human-readable message out of an opencode
// error event. The shape is
//
//	{"type":"error","error":{"name":"...","data":{"message":"..."}}}
//
// The `data.message` field can be a JSON-stringified payload (e.g. when
// the upstream provider returned a structured error), so we try to
// unquote it for readability. Falls back to whatever it can find.
func extractErrorMessage(evt map[string]any) string {
	errObj, ok := evt["error"].(map[string]any)
	if !ok {
		// Older shapes may still surface a top-level message field.
		if m, ok := evt["message"].(string); ok {
			return m
		}
		return ""
	}
	if data, ok := errObj["data"].(map[string]any); ok {
		if m, ok := data["message"].(string); ok && m != "" {
			// If the message looks like a JSON object (common with
			// provider errors like OpenRouter 500s), unquote it so the
			// error_message column carries the readable form.
			if len(m) > 0 && m[0] == '{' {
				if unq, err := strconv.Unquote(m); err == nil {
					return unq
				}
			}
			return m
		}
	}
	if n, ok := errObj["name"].(string); ok {
		return n
	}
	return ""
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// maxToolOutputBytes caps a single tool's output before it is persisted to
// the session transcript / re-enters the model context. opencode passes the
// FULL tool output (a build log, a directory listing) to OnToolCall; an
// uncapped output is then re-sent on every later turn — the main amplifier
// behind the observed ~45k-72k context-per-call across workflow runs. Keep
// the head of the output (the summary/decision part) and mark the truncation
// so the worker can grep/read the tail from disk if it needs to. Overridable
// via ORCHICON_MAX_TOOL_OUTPUT_BYTES; < 1 disables the cap.
const maxToolOutputBytesDefault = 128 * 1024 // 128 KiB ≈ ~30k tokens

func maxToolOutputBytes() int {
	if v := os.Getenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return maxToolOutputBytesDefault
}

// capToolOutput truncates a tool output to maxToolOutputBytes (128k default),
// keeping the head + a truncation marker. Returns the string unchanged when
// under the cap or the cap is disabled.
func capToolOutput(s string) string {
	limit := maxToolOutputBytes()
	if limit < 1 || len(s) <= limit {
		return s
	}
	head := limit - len(toolOutputTruncatedMarker)
	if head < 1 {
		head = 1
	}
	return s[:head] + toolOutputTruncatedMarker
}

const toolOutputTruncatedMarker = "\n…[output truncated by Orchicon — use a targeted read/grep on the host or project disk for the full tail]\n"

// capPartOutput applies the tool-output cap to a legacy event's `part`
// map (the durable-transcript shape). It returns the part unchanged when
// the part has no tool output. For a tool_use part it returns a shallow
// copy with state.output replaced by the capped value, so the transcript
// (re-seeded by follow-ups / recovery-resumed sessions) stays bounded
// without mutating the event the UI path consumed.
func capPartOutput(part any) any {
	m, ok := part.(map[string]any)
	if !ok {
		return part
	}
	state, ok := m["state"].(map[string]any)
	if !ok {
		return part
	}
	out, _ := state["output"].(string)
	if out == "" {
		return part
	}
	capped := capToolOutput(out)
	if capped == out {
		return part
	}
	cp := make(map[string]any, len(m)+1)
	for k, v := range m {
		cp[k] = v
	}
	cpState := make(map[string]any, len(state)+1)
	for k, v := range state {
		cpState[k] = v
	}
	cpState["output"] = capped
	cp["state"] = cpState
	return cp
}

// runSimulation emits synthetic telemetry events so the dispatch flow
// can be verified end-to-end without the `opencode` binary
// (docs/04 §6.3: in-process adapter for tests/dev only).
func (a *Adapter) runSimulation(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	callbacks.OnStarted(ctx, execRow.ID)

	// Simulate a short execution with progress events.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	steps := 0
	maxSteps := 3
	for {
		select {
		case <-ctx.Done():
			callbacks.OnResult(ctx, execRow.ID, false, "", ctx.Err().Error())
			return ctx.Err()
		case <-ticker.C:
			steps++
			a.log.Info("opencode simulation: progress",
				"execution", execRow.ID, "step", steps, "max", maxSteps,
				"goal", manifest.Goal)
			if steps >= maxSteps {
				// Simulation emits no real worker output, so the
				// summary is empty; the workflow run sees an empty
				// _summary for this stage.
				callbacks.OnResult(ctx, execRow.ID, true, "", "")
				return nil
			}
		}
	}
}

// Compile-time assertion that Adapter satisfies the AdapterBridge
// interface.
var _ scheduler.AdapterBridge = (*Adapter)(nil)

// artifactTypeFromPath returns a type label for an artifact based on its
// file extension. Used by the `write` tool handler to tag artifact events
// so the frontend can display them appropriately (docs/10 §11).
func artifactTypeFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".md"), strings.HasSuffix(path, ".markdown"):
		return "markdown"
	case strings.HasSuffix(path, ".json"):
		return "json"
	case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
		return "yaml"
	case strings.HasSuffix(path, ".html"), strings.HasSuffix(path, ".htm"):
		return "html"
	case strings.HasSuffix(path, ".csv"):
		return "csv"
	case strings.HasSuffix(path, ".xml"):
		return "xml"
	case strings.HasSuffix(path, ".svg"):
		return "svg"
	default:
		return "text"
	}
}

// textStreamingChunkSize is the number of bytes per chunk when the
// adapter fans out a single opencode `text` event into the streaming
// pipeline (docs/04 §6.1). opencode's `--format json` mode delivers
// the full assistant text as one event at step_finish — there is no
// per-token streaming on the wire — so we chunk it on the adapter
// side to give the frontend a typing-style experience. ~40 chars ≈
// one short clause; small enough to feel incremental, large enough to
// keep outbox writes under ~150/sec for the typical 1000-word
// response.
const textStreamingChunkSize = 40

// textStreamingChunkDelay is the gap between emitted text chunks.
// Tuned so a 1000-word response (~6000 chars) takes ~10s to "type
// out" — fast enough to feel live, slow enough that each chunk is a
// visible update rather than a flash.
const textStreamingChunkDelay = 60 * time.Millisecond

// opencode v1.x's --format json stream emits these event types.
// Keep them as named constants so the dispatch table in parseEvent
// reads like the schema.
const (
	evtStepStart  = "step_start"
	evtStepFinish = "step_finish"
	evtText       = "text"
	evtToolUse    = "tool_use"  // v1.x: input + output in one event
	evtReasoning  = "reasoning" // v1.x: only when --thinking is enabled
	evtFileDiff   = "file_diff"
	evtError      = "error"
	evtHealth     = "health"
)

// sessionErrorRecycleThreshold is the number of CONSECUTIVE model-layer
// session errors (a serve whose /global/health answers but whose model
// turns fail instantly — invisible to the health watchdog) after which the
// adapter recycles the affected workflow's runtime container so the next
// dispatch builds a fresh serve. Overridable via
// ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD. A value < 1 disables the
// recycle.
func sessionErrorRecycleThreshold() int {
	if v := os.Getenv("ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 3
}

// sessionRepairBudget bounds the runtime-container repairs the adapter
// attempts within a SINGLE dispatch when the session backend is infra-broken
// (serve unreachable — connection refused — or alive-but-poisoned: POST
// /session returns 5xx because its session store is corrupt). Each repair
// kills the run's runtime container so the next dispatch rebuilds it with a
// fresh serve AND a fresh store — a restart-in-place of the serve process
// (the daemon watchdog) cannot fix a poisoned store because it reuses the
// same XDG data dir on disk. After the budget is spent the dispatch fails as
// today (failed_to_start → the step-level retry/recovery loop, which is
// bounded separately). Overridable via ORCHICON_SESSION_REPAIR_ATTEMPTS; a
// value < 1 disables the repair (legacy behavior: only backoff retries).
func sessionRepairBudget() int {
	if v := os.Getenv("ORCHICON_SESSION_REPAIR_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 3
}

// isInfraSessionError reports whether a session-transport setup failure is an
// INFRASTRUCTURE failure — the serve is unreachable or refuses to create
// sessions — as opposed to a worker/model failure. This is the discriminator
// for the dispatch repair loop: an infra failure means EVERY re-dispatch
// fails through the same hole (the observed field class: run 01a742NSHW... on
// 2026-08-22/23 died when the runtime serve either process-died – dial
// connection refused – or stayed up but its session store was poisoned with
// `Failed to execute statement`, so `POST /session` 5xx'd every retry). Such
// failures are REPAIRED (recycle the container → fresh serve + store → the
// step continues) instead of counting against the step's retry budget.
//
// The signatures match what the SessionClient produces on the session-create
// path (session.go: doJSON https-5xx + CreateSession dial errors) plus the
// no-serve case the adapter itself raises. A 4xx is a client error, not an
// infra failure.
func isInfraSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no opencode serve available") ||
		strings.Contains(msg, "opencode serve not healthy") ||
		strings.Contains(msg, "opencode serve POST /session: http 5")
}

// isInfraModelTurnError reports whether a MODEL-TURN failure is an
// INFRASTRUCTURE failure — the session's model call could not reach the
// model API at the socket/transport layer — as opposed to a per-request or
// server-decision rejection that must stay on the retry → recovery → human
// path. This is the discriminator that lets a model-turn socket connect
// failure recycle the run's runtime container IMMEDIATELY (the next dispatch
// builds a fresh serve + store) instead of waiting for the 3-consecutive
// `recycleOnWedgedServe` counter to burn a dead-API session three times.
//
// The input is the human-readable reason from a session.error bus event (the
// observed field: "Cannot connect to API: Unable to connect. Is the computer
// able to access the url?"), which is the same TCP-class as the session-create
// path's "connection refused" but surfaces on the model-turn path. It is a
// string heuristic, so it is guarded FIRST against the per-request /
// server-decision class: if any guard term is present the message is
// definitively NOT infra, even if it also carries a socket phrase. Only when
// no guard matches does the socket/transport class decide.
func isInfraModelTurnError(msg string) bool {
	if msg == "" {
		return false
	}
	m := strings.ToLower(msg)
	// Guard terms: a wrapped provider error that carries a status/auth/quota/
	// policy rejection must never be reclassified as infra. Checked first so a
	// message that happens to mention a socket phrase AND "http 400" stays on
	// the retry path.
	for _, term := range []string{
		"http 4", "http 5",
		"401", "403", "unauthorized", "forbidden",
		"rate limit", "429",
		"insufficient", "quota", "policy",
	} {
		if strings.Contains(m, term) {
			return false
		}
	}
	// Socket / transport-layer connect class: the serve tried to reach the
	// model API and the attempt failed below the HTTP layer.
	for _, term := range []string{
		"unable to connect",
		"connection refused",
		"dial tcp", "dial udp",
		"no such host",
		"i/o timeout",
		"connection reset",
		"network unreachable",
	} {
		if strings.Contains(m, term) {
			return true
		}
	}
	return false
}

// sessionRecycleMetrics holds the OTel counter for runtime-container recycles
// by reason. A single `orchicon_session_recycle{reason=…}` counter keeps the
// recycle family (session-create infra, wedged-consecutive, infra-model-turn)
// observable with one instrument and minimal wiring, mirroring the
// recoverySeedMetrics best-effort pattern (internal/scheduler/recovery_seed.go).
// Best-effort: a nil OTel pipeline is a no-op, never an error.
type sessionRecycleMetrics struct {
	ensureOnce sync.Once
	recycled   otelmetric.Int64Counter
}

func (m *sessionRecycleMetrics) ensure() {
	m.ensureOnce.Do(func() {
		c, err := telemetry.Meter().Int64Counter("orchicon_session_recycle",
			otelmetric.WithDescription("Runtime container recycles by reason"))
		if err == nil {
			m.recycled = c
		}
	})
}

func (m *sessionRecycleMetrics) record(reason string, exhausted bool) {
	m.ensure()
	if m.recycled != nil {
		attrs := []attribute.KeyValue{attribute.String("reason", reason)}
		if reason == "infra_model_turn" {
			attrs = append(attrs, attribute.Bool("exhausted", exhausted))
		}
		m.recycled.Add(context.Background(), 1, otelmetric.WithAttributes(attrs...))
	}
}

func (m *sessionRecycleMetrics) recordInfraSessionCreate()   { m.record("infra_session_create", false) }
func (m *sessionRecycleMetrics) recordWedgedConsecutive()    { m.record("wedged_consecutive", false) }
func (m *sessionRecycleMetrics) recordInfraModelTurn(v bool) { m.record("infra_model_turn", v) }

// sessionRecycleMetricsSingleton is the shared instance for the recycle
// counters (single instrument instance avoids duplicate-instrument churn on
// the OTel Meter).
var sessionRecycleMetricsSingleton = &sessionRecycleMetrics{}

// projectMount returns the project-dir mount spec for a runtime container
// (empty when no project dir — the daemon still adds the standard home
// mounts).
func projectMount(projectDir string) []runtime.MountSpec {
	if projectDir == "" {
		return nil
	}
	return []runtime.MountSpec{{Source: projectDir, Dest: projectDir}}
}
