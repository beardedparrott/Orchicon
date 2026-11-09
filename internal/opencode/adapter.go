// The adapter MUST NOT advertise capabilities the CLI surface cannot honestly deliver (docs/04
// §6.2).
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/worktree"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

// Adapter is the OpenCode adapter bridge.
type Adapter struct {
	log *slog.Logger
	mu  sync.Mutex

	// sessions tracks live session-transport executions (execution_id → runner).
	sessions map[string]*sessionRun

	// host is the always-on host opencode serve for the in-process
	// (local) execution population. Session-transport only.
	host *HostServe

	// askHost is the Ask Orchicon serve, built with the INTERACTIVE permission profile (see
	// NewAskHostServe).
	askHost *HostServe

	// askPool is the pool the ASK serve is drawn from, and it is what lets a conversation's own MCP
	// servers reach an Ask turn on this adapter.
	askPool *HostServePool

	// hostPool is the DEMAND-KEYED pool of host serves for the in-process population, keyed by the
	// RESOLVED MCP set (see HostServePool).
	hostPool *HostServePool

	// runServeCfg is the RUN-LEVEL serve-config provider (see RunServeConfigProvider).
	runServeCfg RunServeConfigProvider

	// hostScope resolves the in-process session's MCP set for the host pool.
	hostScope mcpclient.ScopeResolver

	// rt is the workflow runtime daemon client.
	rt *runtime.Client

	// usageRecorder records LLM usage (Postgres dual-write + OTel metrics) on each step_finish event
	// carrying tokens + cost (docs/04 §6.1, docs/08 §5.2).
	usageRecorder UsageRecorderFunc

	// sessionStore persists the durable per-execution session transcript (execution_session_parts).
	sessionStore SessionStoreFunc

	// fileEdits is the diff-pipeline hook: on every mutating tool_use event it parses the tool's
	// structured file_edits output (worktree engine tools) or takes a server-side after-snapshot
	// (opencode.
	fileEdits FileEditHookFunc
	// askFileEdits is the Ask-population ledger hook (owner kind ask_conversation).
	askFileEdits FileEditHookFunc
	// askDirResolver resolves an Ask conversation's working directory.
	askDirResolver func(ctx context.Context, conversationID string) string

	// resolveBinary is the adapter-CLI resolution seam.
	resolveBinary func() (string, error)

	// consecutiveSessionErrors counts back-to-back model-layer session failures across executions
	// (guarded by mu).
	consecutiveSessionErrors int

	// infraModelTurnRecycles counts runtime-container recycles performed for the infra-model-turn class
	// (a model call that couldn't reach the model API at the socket/transport layer) within a SINGLE.
	infraModelTurnRecycles int
}

// SessionStoreFunc persists transcript entries for one execution.
type SessionStoreFunc = scheduler.SessionStoreFunc

// SetRuntimeClient injects the workflow runtime daemon client.
func (a *Adapter) SetRuntimeClient(rt *runtime.Client) { a.rt = rt }

// SetHostServe injects the always-on host opencode serve manager.
func (a *Adapter) SetHostServe(hs *HostServe) { a.host = hs }

// SetAskHostServe injects the dedicated Ask Orchicon serve (the interactive permission profile).
func (a *Adapter) SetAskHostServe(hs *HostServe) { a.askHost = hs }

// SetAskHostServePool injects the resolved-set-keyed pool the ASK serve is drawn from. Nil keeps
// Ask on the single askHost, i.e. no conversation-scoped MCP servers.
func (a *Adapter) SetAskHostServePool(p *HostServePool) { a.askPool = p }

// SetHostServePool injects the resolved-set-keyed pool of host serves for the
// in-process population. Nil keeps today's single host serve (a.host).
func (a *Adapter) SetHostServePool(p *HostServePool) { a.hostPool = p }

// SetHostScopeResolver injects the resolver used to compute an in-process session's MCP set (the
// pool key).
func (a *Adapter) SetHostScopeResolver(r mcpclient.ScopeResolver) { a.hostScope = r }

// It takes ONLY a run id — never a per-execution input — because the daemon applies the config once
// at container creation.
type RunServeConfigProvider interface {
	// RunServeConfig returns the OPENCODE_CONFIG_CONTENT the run's container must carry, derived from
	// the RUN.
	RunServeConfig(ctx context.Context, runID string) (string, bool)
}

// SetRunServeConfigProvider injects the run-level serve-config provider.
func (a *Adapter) SetRunServeConfigProvider(p RunServeConfigProvider) { a.runServeCfg = p }

// SendExecutionMessage routes a mid-run human message into a live session execution.
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

// AbortExecution tears down a running execution's live opencode session so the model stops
// generating immediately.
func (a *Adapter) AbortExecution(ctx context.Context, execID, reason string) error {
	a.mu.Lock()
	r := a.sessions[execID]
	a.mu.Unlock()
	if r == nil {
		a.log.Debug("abort execution: no live session", "execution", execID)
		return nil
	}
	a.log.Info("aborting execution session", "execution", execID, "session", r.sessionID, "reason", reason)
	// Cancel the run loop's subscription context first so the runner unwinds (its defer cleans up the
	// sessions registry), then abort the opencode session so the model's current turn stops streaming.
	r.subCancel()
	if err := r.client.Abort(ctx, r.sessionID); err != nil {
		a.log.Warn("abort session failed", "execution", execID, "error", err)
	}
	return nil
}

// sessionsEnabled reports whether the session transport is enabled for an execution.
func (a *Adapter) sessionsEnabled(manifest scheduler.ExecutionManifest) bool {
	return os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0"
}

// executionDir resolves the worker's working directory: the run's provisioned worktree path when
// set, else the project dir.
func engineEditedFromOutput(out string) []string {
	entries, err := fileedit.ParseEngineOutput(out)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func executionDir(m scheduler.ExecutionManifest) string {
	if m.WorktreePath != "" {
		return m.WorktreePath
	}
	return m.ProjectDir
}

// sessionClientFor resolves the SessionClient for an execution: the per-container serve for
// workflow-run executions (ensuring the container exists + is serving).
func (a *Adapter) sessionClientFor(ctx context.Context, manifest scheduler.ExecutionManifest, tenantID string) *SessionClient {
	// Local execution mode: run in-process via the host serve, never create/exec a container.
	if runtimeContainerRouteEnabled(a.rt != nil, manifest) {
		// The serve config is the RUN-LEVEL one, resolved from the RUN by the plane's provider — NEVER
		// rebuilt here from the dispatching execution.
		serveCfg := ""
		if a.runServeCfg != nil {
			if cfg, ok := a.runServeCfg.RunServeConfig(ctx, manifest.RuntimeWorkflowID); ok {
				serveCfg = cfg
			}
		}
		resp, err := a.rt.Create(ctx, runtime.CreateRequest{
			WorkflowID:  manifest.RuntimeWorkflowID,
			Image:       manifest.RuntimeImage,
			Mounts:      projectMount(manifest.ProjectDir),
			ServeConfig: serveCfg,
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
		// AC 4: record the set the session ACTUALLY received, with the RUN it came from.
		a.log.Info("session MCP provenance", "execution", manifest.ExecutionID,
			"run", manifest.RuntimeWorkflowID, "servers", provenanceOfServeConfig(serveCfg))
		// The daemon returns the plane-reachable base URL (the docker bridge gateway, reachable from a
		// containerized plane).
		baseURL := resp.ServeURL
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://127.0.0.1:%d", resp.ServePort)
		}
		client := NewSessionClient(baseURL, resp.ServePassword, executionDir(manifest))
		// The container serve runs the SAME binary this adapter resolves, so it speaks the same API.
		client.setAPIShape(a.apiShape())
		// This serve was built for THIS run (see RunServeConfig), so it carries the run's step-worker
		// agents and the session can be switched to its own.
		client.SetAgentsAvailable(true)
		return client
	}
	if a.host != nil {
		// Lazy host serve (AC 2): the in-process serve starts on FIRST opencode demand.
		h := a.host
		if a.hostPool != nil {
			set := a.hostResolvedSet(ctx, manifest, tenantID)
			pooled, perr := a.hostPool.ServeFor(ctx, set)
			if perr != nil {
				a.log.Warn("session transport: host serve pool has no serve for this MCP set — failing execution",
					"execution", manifest.ExecutionID, "servers", mcpclient.ProvenanceString(set.Servers), "error", perr)
				return nil
			}
			h = pooled
		}
		if err := h.EnsureStarted(ctx); err != nil {
			a.log.Warn("session transport: host opencode serve unavailable — failing execution",
				"execution", manifest.ExecutionID, "error", err)
			return nil
		}
		return h.Client()
	}
	return nil
}

// hostResolvedSet resolves the MCP set an in-process (host-serve) session is entitled to, so the
// pool can hand out a serve whose config matches exactly that set.
func (a *Adapter) hostResolvedSet(ctx context.Context, manifest scheduler.ExecutionManifest, tenantID string) mcpclient.Resolution {
	if a.hostScope == nil || manifest.ProjectID == "" {
		return mcpclient.Resolution{}
	}
	res, err := a.hostScope.ResolveScope(tenant.WithID(ctx, tenantID), mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeWorker,
		ProjectID:      manifest.ProjectID,
		WorkerID:       manifest.WorkerID,
		Version:        manifest.WorkerVersion,
		OwnPermissions: manifest.Permissions,
	})
	if err != nil {
		// A failed resolution is NOT silently an empty set: an execution that silently lost its project's
		// servers is one that looks fine and cannot do its job.
		a.log.Warn("host serve: MCP resolution failed — using the default serve",
			"execution", manifest.ExecutionID, "project", manifest.ProjectID, "error", err)
		return mcpclient.Resolution{}
	}
	return res
}

// It reports names ONLY — the config's `mcp` entries carry resolved env/headers (credentials), and
// a resolved credential must never reach a log (the same rule mcpclient.ProvenanceString states).
func provenanceOfServeConfig(cfgContent string) string {
	if cfgContent == "" {
		return ""
	}
	var parsed struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(cfgContent), &parsed); err != nil {
		return ""
	}
	if len(parsed.MCP) == 0 {
		return ""
	}
	names := make([]string, 0, len(parsed.MCP))
	for k := range parsed.MCP {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// runtimeContainerRouteEnabled is the always-container routing gate: an opencode session dispatches
// into the run's container only when a daemon client is wired.
func runtimeContainerRouteEnabled(hasClient bool, m scheduler.ExecutionManifest) bool {
	return hasClient && m.RuntimeWorkflowID != "" && m.ExecutionMode != db.ExecutionModeLocal
}

// RuntimeServeConfig builds the OPENCODE_CONFIG_CONTENT for a runtime container's serve given its
// runtime image tag: the permission rules only, with the operator's MCP servers omitted.
func RuntimeServeConfig(imageTag, projectDir, workflowRunID string, planeEnv map[string]string, union mcpclient.Resolution, providers []runtime.ProviderConfig, workers []adapter.WorkerAgent) string {
	opts := ConfigOptions{
		AgentName:    workerAgent,
		AgentPrompt:  sessionToolShell,
		DefaultAgent: workerAgent,
		ModelRef:     "",
		SkipUserMCP:  true,
		RunMCP:       union.Servers,
		RunSkills:    union.Skills,
		// Workers are the RUN's step workers as agents, each carrying the run-level part of that worker's
		// prompt.
		Workers: workers,
		// WorkflowRunID reaches the worktree MCP sidecar, which needs it to derive the run-state directory
		// its todo snapshot is written into (see worktreeMCPServer).
		WorkflowRunID: workflowRunID,
		// Shape is deliberately NOT set: BuildConfigContent resolves it from the host's version probe, and
		// the container's serve runs the SAME opencode install bind-mounted from this host.
		Providers: providers,
	}
	if runtime.IsDevImageTag(imageTag) {
		opts.TenantID = serveTenantID()
		opts.OrchiconMCP = true
		opts.MCPEnv = map[string]string{"ORCHICON_POSTGRES_DSN": runtime.SandboxPostgresDSN}
		// The sandbox worker's Orchicon MCP sidecar is told its workflow run so it can inject the
		// run_context into create calls and stamp recurring- fire provenance (feature 4.1, AC2).
		if workflowRunID != "" {
			opts.MCPEnv["ORCHICON_MCP_WORKFLOW_RUN_ID"] = workflowRunID
		}
	}
	// Plane channel: when the runtime lifecycle minted a role-scoped worker credential for this run,
	// register the `orchicon-plane` MCP server so the worker gets `orchicon_plane_*` tools against.
	if len(planeEnv) > 0 {
		opts.PlaneMCP = true
		opts.PlaneMCPEnv = planeEnv
	}
	// The orchicon binary is bind-mounted read-only at /usr/local/bin/orchicon in EVERY runtime
	// container (the runtime daemon's own executable, daemon.go:450) — never baked into the image.
	opts.MCPBinaryPath = runtimeContainerBinaryPath

	// Composite worktree tools (batch_read / batch_grep / batch_write) are LIVE for ALL runtime images,
	// not just dev.
	projectRoot := projectDir
	if filepath.Base(filepath.Dir(projectDir)) == ".orchicon-worktrees" {
		projectRoot = filepath.Dir(filepath.Dir(projectDir))
	}
	opts.ProjectDir = projectRoot
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

// startViaSession runs an execution through a persistent opencode session on the given serve.
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
		system:    executionSystemPrompt(manifest, client.shape),
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

// executionSystemPrompt returns the per-session system prompt for an execution.
func executionSystemPrompt(manifest scheduler.ExecutionManifest, shape apiShape) string {
	sp := manifest.SystemPrompt
	if manifest.WorktreePath != "" {
		sp += worktreePathDiscipline
	}
	if manifest.RuntimeWorkflowID != "" && manifest.ProjectDir != "" {
		// WHICH DISCIPLINE DEPENDS ON WHETHER THE TOOLS CAN BE CALLED AT ALL, and the two are not
		// interchangeable.
		if shape.codeMode {
			sp += codeModeDiscipline
		} else {
			sp += batchToolsDiscipline
		}
	}
	return sp
}

// codeModeDiscipline steers a composite-tools worker on an opencode 2.x serve, where MCP tools
// CANNOT be called directly (see apiShape.codeMode).
const codeModeDiscipline = "\n\n# Tool discipline (Code Mode)\n" +
	"THIS SERVES OPENCODE 2.x, WHOSE CODE MODE FORBIDS CALLING MCP TOOLS DIRECTLY. " +
	"You have an `execute` tool that runs JavaScript, and our file/todo tools are MCP tools — " +
	"they are reachable ONLY from inside `execute`, never as a top-level tool call:\n" +
	"- Read: `return await tools[\"orchicon-worktree\"].batch_read({ paths: [\"a.go\", \"b.go\"] })`\n" +
	"- Search: `return await tools[\"orchicon-worktree\"].batch_grep({ patterns: [\"foo\", \"bar\"] })`\n" +
	"- Write: `return await tools[\"orchicon-worktree\"].batch_write({ writes: [...] })`\n" +
	"- Todos: `return await tools[\"orchicon-worktree\"].todowrite({ todos: [{content,status,priority}] })`\n" +
	"If unsure of a name, find it first: `return search({ query: \"batch\" })`. Do NOT guess tool names.\n" +
	"`shell` / `edit` / `write` / `read` / `grep` ARE directly callable and fine for a single quick " +
	"operation. But BATCH your file access through `execute`: one `batch_read` carrying many paths is " +
	"one turn, where several `shell` calls are several.\n" +
	"Never re-read a file whose content is already in context — every extra call re-sends the whole " +
	"conversation.\n"

// batchToolsDiscipline steers the worker to the composite context-efficient file tools.
const worktreePathDiscipline = "\n\n## File scoping\n" +
	"- Reads may resolve the project root too: run-state files under `.orchicon/<run>/` and `architecture-notes/` live at the project root, a sibling of this worktree — `batch_read`/`read`/`grep`/`list` can read them (by project-root or `../../.orchicon/<run>/...` path).\n" +
	"- Writes land ONLY in this worktree and the sanctioned scratch dir `/tmp/orchicon` — never the main checkout at the project root.\n" +
	"- Deletes are scoped to this worktree and `/tmp/orchicon`; anything escaping the worktree/project root stays blocked.\n"

const batchToolsDiscipline = "\n\n# Tool discipline (composite worktree tools)\n" +
	"Use the composite file tools for ALL file access:\n" +
	"- `batch_read` reads several files or a whole directory in ONE call.\n" +
	"- `batch_grep` searches several patterns across the tree in ONE call.\n" +
	"- `batch_write` applies several create/overwrite/edit/append writes in ONE atomic call.\n" +
	"- The single-op variants (`read`, `grep`, `write`, `edit`) are thin wrappers over the same batch engine — use them only for a genuinely one-file need; prefer the batch tools for independent operations (one call carrying several operations = one turn instead of N).\n" +
	"- `list` enumerates a directory's entries cheaply (the `ls` equivalent of glob); `todoread` re-syncs your live todo list.\n" +
	"- Do NOT use `glob` to read files, and never re-read a file whose content is already in context — every extra tool call re-sends the whole conversation.\n" +
	"- Bundle independent reads/searches/writes into ONE batch call; never split related work across many micro calls.\n" +
	"- Oversized tool outputs are truncated head+tail at capture time with the full payload offloaded to disk (<project>/.orchicon/outputs/); the transcript carries the reference — grep/read the offloaded file only if you actually need the middle.\n"

// IsExecutionActive reports whether an in-process execution subprocess is still tracked as running.
func (a *Adapter) IsExecutionActive(execID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[execID]
	return ok
}

// UsageRecord is the usage sample the adapter emits on step_finish (docs/04 §6.1 step_finish
// carries tokens + cost).
type UsageRecord = scheduler.UsageRecord

// UsageRecorderFunc records a usage sample.
type UsageRecorderFunc = scheduler.UsageRecorderFunc

// SetUsageRecorder injects the usage recording callback.
func (a *Adapter) SetUsageRecorder(fn UsageRecorderFunc) { a.usageRecorder = fn }

// SetSessionStore injects the durable transcript writer.
func (a *Adapter) SetSessionStore(fn SessionStoreFunc) { a.sessionStore = fn }

// FileEditHookFunc is the diff-pipeline ledger callback.
type FileEditHookFunc func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string)

// SetFileEditHook injects the file-edit ledger hook. Nil = no ledger.
// It is the opencode implementation of the diff-pipeline wiring point.
func (a *Adapter) SetFileEditHook(fn FileEditHookFunc) { a.fileEdits = fn }

// SetDiffLedger implements scheduler.DiffLedgerSink.
func (a *Adapter) SetDiffLedger(execution, ask FileEditHookFunc) {
	a.fileEdits = execution
	if ask != nil {
		a.askFileEdits = ask
	}
}

// workerAgent is the opencode agent name the adapter injects the worker's
// composed system prompt under (selected with --agent).
const workerAgent = "orchicon-worker"

// It deliberately carries ONLY a tool inventory — which tool fits which job — because its purpose
// is to REPLACE opencode's large built-in `build` agent prompt (a big per-turn token win).
const sessionToolShell = "File access tools:\n" +
	"- `batch_read` — read several files or a whole directory in ONE call\n" +
	"- `batch_grep` — search several patterns across the tree in ONE call\n" +
	"- `batch_write` — apply several create/overwrite/edit/append writes in ONE atomic call\n" +
	"- `read` / `grep` / `write` / `edit` — single-file wrappers over the same batch engine (one-op convenience)\n" +
	"- `list` — enumerate a directory's entries (the `ls` equivalent of glob)\n\n" +
	"Other tools:\n" +
	"- `glob` — find files by pattern (use it to find paths, never to read)\n" +
	"- `bash` — run a shell command in the project\n" +
	"- `todowrite` / `todoread` — maintain and re-read a task-progress list\n" +
	"- `webfetch` — fetch web content from a URL\n" +
	"- `websearch` — search the web (use only if needed)\n" +
	"- `skill` — load a skill's instructions\n" +
	"- `orchicon_*` — Orchicon platform tools: projects, work items, workers, workflows, executions, policies, runtime images, usage, settings, and the project-directory list/read tools."

// runtimeContainerBinaryPath is where the runtime daemon bind-mounts its own executable in every
// runtime container (internal/runtime/daemon.go).
const runtimeContainerBinaryPath = "/usr/local/bin/orchicon"

// apiShape resolves the API surface of this adapter's opencode binary, once.
func (a *Adapter) apiShape() apiShape {
	// THE RESOLVER IS AN OPTIONAL SEAM, so a nil one is not an error to report — it is an adapter that
	// has not been wired for spawning, and it will fail loudly on its own when it tries.
	if a.resolveBinary == nil {
		return shapeV1
	}
	binary, err := a.resolveBinary()
	if err != nil || strings.TrimSpace(binary) == "" {
		return shapeV1
	}
	return shapeForBinary(context.Background(), a.log, binary)
}

// New creates an OpenCode adapter bridge.
func (a *Adapter) SetBinaryResolver(f func() (string, error)) {
	if f == nil {
		f = resolveOpenCodeBinary
	}
	a.resolveBinary = f
}

// resolveOpenCodeBinary is the production resolver.

func New(log *slog.Logger) *Adapter {
	return &Adapter{
		log:           log,
		resolveBinary: resolveOpenCodeBinary,
		sessions:      make(map[string]*sessionRun),
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

	// Resolve the adapter CLI.
	binary, err := a.resolveBinary()
	if binary == "" || err != nil {
		return fmt.Errorf("opencode binary not found on PATH or ~/.opencode/bin (install it on the host: curl -fsSL https://opencode.ai/install | bash; set ORCHICON_SIMULATE_ADAPTER=1 only for offline dev): %w", err)
	}

	// The deadline is applied ONLY to the subprocess context (procCtx), never to the ctx handed to
	// callbacks: when the deadline fires the process is killed.
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

	// Best-effort: drop the safety lint script into .orchicon/ so review and QA workers can run it
	// (their bash tool is scoped to the project directory).
	writeSafetyLint(executionDir(manifest))

	// Session transport is the ONLY execution transport.
	if !a.sessionsEnabled(manifest) {
		return fmt.Errorf("opencode session transport disabled (ORCHICON_OPCODE_SESSION_TRANSPORT=0) — no execution transport available for execution %s", execRow.ID)
	}

	// Session-transport setup with self-heal.
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
		// Re-resolve the client every attempt so a repair (container killed) is picked up: sessionClientFor
		// re-runs Create, which rebuilds the container and returns the freshly-published serve once it.
		client := a.sessionClientFor(ctx, manifest, execRow.TenantID)
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
				// A worker/model-side failure won't be fixed by recycling the container — reset the counter so a
				// later transient infra blip starts fresh (and never nukes the container on a single infra failure.
				consecutiveInfra = 0
			}
		}

		// Infra repair is NOT first-resort: a single infra failure is retried on the SAME container (a
		// fresh session create — the serve converges, transient provider hiccups happen).
		if consecutiveInfra >= infraThreshold && repairs < maxRepairs && a.rt != nil && manifest.RuntimeWorkflowID != "" {
			repairs++
			consecutiveInfra = 0
			a.repairRuntimeContainer(ctx, manifest.RuntimeWorkflowID)
			continue
		}
		// Non-infra, or below the repair threshold, or past the repair budget: keep the backoff retries;
		// the step's own retry/recovery loop is the bounded owner of persistent failures.
	}
	return fmt.Errorf("session transport setup failed: %w", lastErr)
}

// infraRepairThreshold is how many CONSECUTIVE infra failures (within one dispatch's session-setup)
// must occur before the adapter recycles the run's runtime container.
func infraRepairThreshold() int {
	if v := os.Getenv("ORCHICON_SESSION_INFRA_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 2
}

// repairRuntimeContainer is the session-backend infra repair: it kills the run's runtime container
// so the NEXT dispatch's Create rebuilds it with a fresh opencode serve and a FRESH session store.
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

// execStreamState tracks per-execution opencode stream structure so a run that ends before its
// final model step completes can be flagged as failed instead of reported as a clean, empty
// success.
type execStreamState struct {
	stepStarts   int
	stepFinishes int

	// toolUses counts evtToolUse events for the execution — the real per-tool-call granularity (a
	// single step/turn can contain several tool calls before its step_finish).
	toolUses int

	// writtenFiles accumulates the paths opencode reported as file modifications (file_diff events).
	writtenFiles []string

	// engineEditedPaths holds the paths already ledgered by the worktree engine's structured file_edits
	// output during this run (batch_write family).
	engineEditedPaths map[string]bool

	// truncatedFinish marks a FINAL step_finish that indicates the model turn was interrupted rather
	// than completed: reason "unknown" with zero tokens (the signature of a truncated/aborted response.
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

// allTokensZero reports whether a step_finish's tokens map is empty or all counts are zero.
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

// parseTodoItems extracts the worker's todo list from a `todowrite` tool input payload.
func parseTodoItems(inRaw any) []worktree.TodoItem {
	inputMap, ok := inRaw.(map[string]any)
	if !ok {
		return nil
	}
	itemsRaw, ok := inputMap["todos"].([]any)
	if !ok {
		return nil
	}
	items := make([]worktree.TodoItem, 0, len(itemsRaw))
	for _, ir := range itemsRaw {
		im, ok := ir.(map[string]any)
		if !ok {
			continue
		}
		content, _ := im["content"].(string)
		if content == "" {
			continue
		}
		status, _ := im["status"].(string)
		priority, _ := im["priority"].(string)
		if status == "" {
			status = "pending"
		}
		items = append(items, worktree.TodoItem{Content: content, Status: status, Priority: priority})
	}
	return items
}

// parseEvent dispatches a decoded opencode event into the telemetry pipeline.
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
		// Text part: the model's response text.
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
		callID, _ := part["callID"].(string)
		state, _ := part["state"].(map[string]any)
		inRaw, _ := state["input"]
		outStr, _ := state["output"].(string)

		// The ledger hook below must see the PRE-capped output: capToolOutput splices the tail out of large
		// outputs, which would hide the file_edits payload of a huge batch_write (the JSON rides.
		rawOut := outStr

		// Cap tool OUTPUT before it is streamed to the UI / persisted into the durable transcript (which a
		// follow-up or a recovery-resumed session re-seeds as context).
		outStr = capToolOutput(outStr, executionDir(manifest), execID, callID)

		// Diff pipeline: record the file edits this tool made BEFORE any early-break branch below — notably
		// the `write` artifact path.
		toolStatus, _ := state["status"].(string)
		if a.fileEdits != nil && (toolStatus == "" || toolStatus == "completed") {
			inputMap, _ := inRaw.(map[string]any)
			if inputMap == nil {
				inputMap = map[string]any{}
			}
			// Remember engine-covered paths so the file_diff fallback never
			// double-records one edit (the engine entry is the exact one).
			if toolName == "batch_write" || toolName == "write" || toolName == "edit" {
				if stats != nil {
					if stats.engineEditedPaths == nil {
						stats.engineEditedPaths = map[string]bool{}
					}
					for _, e := range engineEditedFromOutput(rawOut) {
						stats.engineEditedPaths[e] = true
					}
				}
			}
			a.fileEdits(ctx, execID, execRow.TenantID, executionDir(manifest), toolName, inputMap, rawOut)
		}

		// Detect `todowrite` (native opencode todo tool) and mirror its payload into the DB-less sidecar
		// snapshot so `todoread` — and the native todo surface.
		if toolName == "todowrite" {
			items := parseTodoItems(inRaw)
			worktree.SaveTodoSnapshot(worktree.BaseFor(executionDir(manifest)), items)
			// AND THE RUN-STATE COPY, which is the one the todo PANEL reads.
			if rs := worktree.RunStateDir(manifest.ProjectDir, manifest.RuntimeWorkflowID); rs != "" {
				if err := worktree.SaveRunTodoSnapshot(rs, items); err != nil {
					a.log.Warn("todowrite run-state snapshot not written — the todo panel will not update",
						"execution", execID, "run", manifest.RuntimeWorkflowID, "error", err)
				}
			}
		}

		// Detect `write` tool calls (opencode built-in file writer) and route them as artifacts instead of
		// raw tool calls.
		if toolName == "write" {
			if inputMap, ok := inRaw.(map[string]any); ok {
				if content, ok := inputMap["content"].(string); ok && content != "" {
					path, _ := inputMap["path"].(string)
					a.log.Info("opencode write artifact",
						"execution", execID, "path", path, "content_len", len(content))
					// Stream the content as text FIRST so the user sees the story appear incrementally in the runtime
					// session pane (chunked into 40-char pieces with 60ms delay for a typing-style effect).
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

		// `inp` is the JSON-marshalled input object so the frontend can render it as a structured "Input:"
		// block.
		inp, err := json.Marshal(inRaw)
		if err != nil {
			inp = []byte(fmt.Sprintf("%v", inRaw))
		}
		a.log.Info("opencode tool use",
			"execution", execID, "tool", toolName,
			"status", state["status"], "output_len", len(outStr))
		callbacks.OnToolCall(ctx, execID, toolName, inp, []byte(outStr))
	case evtReasoning:
		// v1.x reasoning content (only when --thinking is enabled on the opencode CLI).
		reasonText, _ := part["text"].(string)
		if reasonText == "" {
			return
		}
		a.log.Debug("opencode reasoning", "execution", execID, "len", len(reasonText))
		// Reasoning is also streamed chunk-by-chunk so it appears to unfold live.
		a.emitReasoningChunked(ctx, callbacks, execID, reasonText, textSeq)
	case evtStepFinish:
		// Step completion carries token usage + cost (docs/04 §6.1).
		tokens, _ := part["tokens"].(map[string]any)
		cost, _ := part["cost"].(float64)
		if stats != nil {
			stats.stepFinishes++
			// A step_finish with reason "unknown" and zero tokens is the signature of a truncated/interrupted
			// model turn (the failing run's last part was exactly that).
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
		// PER-STEP FILE ATTRIBUTION, and on 2.x the only LIVE capture there is.
		if a.fileEdits != nil {
			files, _ := part["files"].([]any)
			for _, f := range files {
				fp, _ := f.(string)
				if fp == "" {
					continue
				}
				if stats != nil && stats.engineEditedPaths[fp] {
					continue
				}
				a.fileEdits(ctx, execID, execRow.TenantID, executionDir(manifest), "file_diff", map[string]any{"path": fp}, "")
			}
		}
	case evtFileDiff:
		// A file_diff event carries the path of a file the session wrote or edited (part["path"]).
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
				// Diff-pipeline fallback (plan step 7): a file_diff for a path the mutating-tool hook did not cover
				// (an unusual tool, or a tool whose input shape we don't special-case) still gets a ledger entry.
				if a.fileEdits != nil && !stats.engineEditedPaths[p] {
					input := map[string]any{"path": p}
					a.fileEdits(ctx, execID, execRow.TenantID, executionDir(manifest), "file_diff", input, "")
				}
			}
		}
	case evtHealth:
		if state, ok := evt["state"].(string); ok {
			callbacks.OnHealth(ctx, execID, state)
		}
	case evtError:
		// opencode's --format json emits an error event shaped like
		// {"type":"error","error":{"name":"...","data":{"message":"..."}}} — the human-readable message
		// lives at error.data.message.
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

// emitTextChunked fans a single assistant-text payload out as a stream of smaller chunks, each
// separated by a short delay, so the frontend RuntimeSessionPane can render a typing-style live
// view.
func (a *Adapter) emitTextChunked(ctx context.Context, callbacks scheduler.ExecutionCallbacks, execID, text string, seq *int) {
	if text == "" {
		return
	}
	// Honor cancellation: if the context is done (execution cancelled/terminated), drop remaining
	// chunks.
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
		// Pace the chunks so the frontend actually has time to render them.
		if end < len(text) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(textStreamingChunkDelay):
			}
		}
	}
}

// emitReasoningChunked is the reasoning-content counterpart to emitTextChunked.
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
		// Use the existing OnText callback but encode the reasoning marker in the payload so the frontend
		// can render it differently.
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

// recordUsage records a usage sample from a step_finish event via the AI Gateway dual-write
// (docs/08 §5.2).
func (a *Adapter) recordUsage(ctx context.Context, execRow db.ExecutionRow, manifest scheduler.ExecutionManifest, tokens map[string]any, cost float64) {
	if a.usageRecorder == nil {
		return
	}
	promptTokens := toInt64(tokens["input"])
	cacheReadTokens := toInt64(cacheToken(tokens, "read"))
	cacheWriteTokens := toInt64(cacheToken(tokens, "write"))
	completionTokens := toInt64(tokens["output"])
	reasoningTokens := toInt64(tokens["reasoning"])
	// A genuinely empty sample (no tokens, no cost) is dropped.
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

// cacheToken reads a sub-count from the opencode tokens.cache sub-object (e.g.
// {"cache":{"read":N,"write":M}} → cacheToken(tokens,"read")).
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
			// If the message looks like a JSON object (common with provider errors like OpenRouter 500s),
			// unquote it so the error_message column carries the readable form.
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

// maxToolOutputBytes caps a single tool's output before it is persisted to the session transcript /
// re-enters the model context.
const maxToolOutputBytesDefault = 128 * 1024 // 128 KiB ≈ ~30k tokens

// toolOutputOffloadRatio is the head+tail split of the cap: each side keeps
// (cap - marker) * ratio / (ratio+1) bytes.
const toolOutputOffloadRatio = 2 // head:tail = 2:1 — the decision part outweighs the error tail

func maxToolOutputBytes() int {
	if v := os.Getenv("ORCHICON_MAX_TOOL_OUTPUT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return maxToolOutputBytesDefault
}

// toolOutputOffloadDir returns the directory where oversized tool outputs are offloaded:
// <project_dir>/.orchicon/outputs/<execution_id>/.
func toolOutputOffloadDir(projectDir, execID string) string {
	if projectDir == "" || execID == "" {
		return ""
	}
	p := filepath.Join(projectDir, ".orchicon", "outputs", execID)
	if err := os.MkdirAll(p, 0o755); err != nil {
		return ""
	}
	return p
}

// offloadToolOutput writes the full oversized tool output to the offload dir and returns a short
// reference line the transcript carries instead of the payload.
func offloadToolOutput(projectDir, execID, callID string, full string) string {
	dir := toolOutputOffloadDir(projectDir, execID)
	if dir == "" {
		return ""
	}
	name := callID
	if name == "" {
		name = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	p := filepath.Join(dir, name+".out")
	if err := os.WriteFile(p, []byte(full), 0o644); err != nil {
		return ""
	}
	rel, err := filepath.Rel(projectDir, p)
	if err != nil {
		rel = p
	}
	return fmt.Sprintf(" (full output offloaded to %s)", rel)
}

// capToolOutput truncates a tool output to maxToolOutputBytes (128k default), keeping the HEAD (the
// decision/summary part) + a truncation marker + the TAIL (the error/stack part.
func capToolOutput(s, projectDir, execID, callID string) string {
	limit := maxToolOutputBytes()
	if limit < 1 || len(s) <= limit {
		return s
	}
	offloadRef := offloadToolOutput(projectDir, execID, callID, s)
	avail := limit - len(toolOutputTruncatedMarker)
	if avail < 4 {
		avail = 4
	}
	head := avail * toolOutputOffloadRatio / (toolOutputOffloadRatio + 1)
	tail := avail - head
	if head < 1 {
		head = 1
	}
	if tail < 1 {
		tail = 1
	}
	// The offload ref is part of the returned string; shrink the preview if
	// the ref alone pushes past the cap (extremely long callIDs).
	for len(toolOutputTruncatedMarker)+len(offloadRef)+head+tail > limit {
		if head > 1 {
			head--
		} else if tail > 1 {
			tail--
		} else {
			break
		}
	}
	return s[:head] + toolOutputTruncatedMarker + offloadRef + s[len(s)-tail:]
}

const toolOutputTruncatedMarker = "\n…[output truncated by Orchicon — head+tail shown, full payload on disk]\n"

// capPartOutput applies the tool-output cap to a legacy event's `part` map (the durable-transcript
// shape).
func (a *Adapter) capPartOutput(part any, execID, projectDir string) any {
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
	callID, _ := m["callID"].(string)
	capped := capToolOutput(out, projectDir, execID, callID)
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

// runSimulation emits synthetic telemetry events so the dispatch flow can be verified end-to-end
// without the `opencode` binary (docs/04 §6.3: in-process adapter for tests/dev only).
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
				// Simulation emits no real worker output, so the summary is empty; the workflow run sees an empty
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

// Compile-time assertions that Adapter satisfies the Ask chat-session capability interfaces it
// implements as an OPTIONAL surface (the same capability pattern as MessageInjector/Aborter).
var _ scheduler.ChatTurnClient = (*Adapter)(nil)
var _ scheduler.SendTurnMessageWithAttachments = (*Adapter)(nil)

// artifactTypeFromPath returns a type label for an artifact based on its file extension.
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

// textStreamingChunkSize is the number of bytes per chunk when the adapter fans out a single
// opencode `text` event into the streaming pipeline (docs/04 §6.1).
const textStreamingChunkSize = 40

// textStreamingChunkDelay is the gap between emitted text chunks.
const textStreamingChunkDelay = 60 * time.Millisecond

// opencode v1.x's --format json stream emits these event types.
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

// sessionErrorRecycleThreshold is the number of CONSECUTIVE model-layer session errors (a serve
// whose /global/health answers but whose model turns fail instantly.
func sessionErrorRecycleThreshold() int {
	if v := os.Getenv("ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 3
}

// sessionRepairBudget bounds the runtime-container repairs the adapter attempts within a SINGLE
// dispatch when the session backend is infra-broken (serve unreachable — connection refused.
func sessionRepairBudget() int {
	if v := os.Getenv("ORCHICON_SESSION_REPAIR_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 3
}

// isInfraSessionError reports whether a session-transport setup failure is an INFRASTRUCTURE
// failure — the serve is unreachable or refuses to create sessions.
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

// isInfraModelTurnError reports whether a MODEL-TURN failure is an INFRASTRUCTURE failure — the
// session's model call could not reach the model API at the socket/transport layer.
func isInfraModelTurnError(msg string) bool {
	if msg == "" {
		return false
	}
	m := strings.ToLower(msg)
	// Guard terms: a wrapped provider error that carries a status/auth/quota/ policy rejection must
	// never be reclassified as infra.
	for _, term := range []string{
		"http 4", "http 5",
		"401", "403", "unauthorized", "forbidden",
		// The AUTH/ACCESS class BY NAME, not only by status code — and this is load-bearing since v2
		// failures gained a path into here.
		"authentication", "unauthenticated", "free tier",
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

// sessionRecycleMetrics holds the OTel counter for runtime-container recycles by reason.
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

// sessionRecycleMetricsSingleton is the shared instance for the recycle counters (single instrument
// instance avoids duplicate-instrument churn on the OTel Meter).
var sessionRecycleMetricsSingleton = &sessionRecycleMetrics{}

// projectMount returns the project-dir mount spec for a runtime container (empty when no project
// dir — the daemon still adds the standard home mounts).
func projectMount(projectDir string) []runtime.MountSpec {
	if projectDir == "" {
		return nil
	}
	return []runtime.MountSpec{{Source: projectDir, Dest: projectDir}}
}
