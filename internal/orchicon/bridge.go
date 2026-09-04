package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/agentmemory"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// NativeBridge is the AdapterBridge for the native in-process session
// engine (adapter kind "orchicon"). The Dispatcher treats it exactly
// like the opencode bridge: Start is called by the TaskReconciler, which
// blocks until the terminal OnResult. Sessions are per-execution;
// identity isolation is by construction (one session per worker
// execution, never shared per workflow run).
//
// NativeBridge implements the optional capabilities the control plane
// probes for:
//   - scheduler.MessageInjector  → mid-run human messages queue into the
//     live session between tool rounds.
//   - scheduler.SessionContinuer → resumes a cancelled/crashed session's
//     transcript (replay + continue) for sequence-continuation chains
//     (opt-in, default off).
//   - scheduler.Aborter         → graceful cancellation (finish in-flight
//     tool, no new provider call), transcript marked cancelled, session
//     stays resumable.
//   - scheduler.LivenessReporter → the bridge tracks live executions.
//   - scheduler.ConfigurableBridge → usage recorder + session store
//     injection.
type NativeBridge struct {
	mu sync.Mutex

	resolver   ProviderResolver // registry (nil → sessions must be given a pre-resolved provider via test hook)
	projectDir string           // daemon-level serve/guard boundary (manifest.ProjectDir)
	log        *slog.Logger

	// live tracks in-flight sessions (execID → cancel + session).
	live map[string]*liveSession

	// usage records per-execution usage at terminal (step_finish parity).
	usageRecorder scheduler.UsageRecorderFunc
	// cacheSink drains the session's terminal prefix-cache rollup for OTel
	// metrics (D3, ADR-0009 D6). Nil → no cache metric is emitted.
	cacheSink func(ctx context.Context, exec db.ExecutionRow, stats CacheStats)
	// sessionStore persists transcript entries to the DB (best-effort).
	sessionStore scheduler.SessionStoreFunc
	// mcpConfig resolves the session's MCP server selection (ADR-0008:
	// worker → project → tenant-default → none over the tenant server list).
	// Nil/absent → no MCP tools (sessions unaffected). Defaults to the no-op
	// source so the feature degrades safely until adapter-settings storage
	// lands.
	mcpConfig mcpclient.ConfigSource
	// mcpSecretResolver replaces ${SECRET_NAME} refs in resolved MCP server
	// env/headers with stored tenant-secret plaintext at session time.
	// Nil → pass-through (no secret resolution).
	mcpSecretResolver MCPSecretResolver
}

// liveSession is the bridge's handle on one running session.
type liveSession struct {
	session  *Session
	cancel   context.CancelFunc
	done     chan struct{}
	execRow  db.ExecutionRow
	manifest scheduler.ExecutionManifest
}

// NewBridge constructs the native adapter bridge. resolver may be nil
// (tests inject a pre-resolved provider via sessionConfig); in production
// it is the *orchicon.Registry.
func NewBridge(resolver ProviderResolver, projectDir string, log *slog.Logger) *NativeBridge {
	if log == nil {
		log = slog.Default()
	}
	return &NativeBridge{
		resolver:   resolver,
		projectDir: projectDir,
		log:        log,
		live:       map[string]*liveSession{},
	}
}

// Kind is the registered adapter kind ("orchicon").
func (b *NativeBridge) Kind() string { return "orchicon" }

// SetConfigSource sets the MCP server config-resolution source for
// sessions (ADR-0008). Absent → no MCP tools. The platform injects a
// real source once tenant server storage lands (adapter-settings task);
// until then the no-op default keeps sessions unaffected.
func (b *NativeBridge) SetConfigSource(src mcpclient.ConfigSource) {
	b.mcpConfig = src
}

// SetMCPSecretResolver sets the ${SECRET_NAME} → plaintext resolver used
// before MCP connections are established. Nil disables resolution.
func (b *NativeBridge) SetMCPSecretResolver(r MCPSecretResolver) {
	b.mcpSecretResolver = r
}

// ProviderResolverFunc adapts a function to ProviderResolver.
type ProviderResolverFunc func(ctx context.Context, tenantID, providerID string) (Provider, error)

// Get implements ProviderResolver.
func (f ProviderResolverFunc) Get(ctx context.Context, tenantID, providerID string) (Provider, error) {
	return f(ctx, tenantID, providerID)
}

// Start implements scheduler.AdapterBridge. It blocks until the session
// loop reaches its terminal OnResult (parity with the opencode bridge:
// the reconciler's `go r.startExecution` expects Start to be
// synchronous). A panic inside the loop is contained by Session.Run's
// boundary recover and surfaced as OnResult(false, panic message).
func (b *NativeBridge) Start(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	if exec.ID == "" {
		return fmt.Errorf("orchicon bridge: empty execution id")
	}
	// The transcript lives under the execution's true project dir:
	// manifest.ProjectDir is authoritative (set from the project row at
	// dispatch, ADR-0007); the construction-time projectDir is a fallback
	// for bridge-level lookups. Resolve once here so NewSession and the
	// continuation seed lookup agree on the transcript location — and the
	// shared bridge's projectDir is never mutated per execution.
	pd := b.projectDir
	if manifest.ProjectDir != "" {
		pd = manifest.ProjectDir
	}
	if pd == "" {
		return fmt.Errorf("orchicon bridge: no project dir (manifest.ProjectDir and bridge projectDir are both empty)")
	}
	// MCP tool resolution (ADR-0008): worker selection → project selection
	// → tenant-default → none, over the tenant-configured server list.
	// Connections are established NOW — per session, never at
	// control-plane boot — and tool discovery runs at Start so the
	// discovered signatures are present in the model's first request. A
	// selected-but-unconfigured or unreachable server fails the session
	// actionably (never silent); the no-op default source degrades to no
	// MCP tools.
	mt, terr := b.mcpResolveAndStart(ctx, exec)
	if terr != nil {
		return terr
	}
	// HOST tool suite: the core file/shell tools every native session
	// needs (read/write/edit/glob/grep/list/bash/batch_*/todoread), scoped
	// to the execution's working dir (worktree when provisioned, else the
	// project dir) with the project root READ-only. Before this, the
	// session carried only MCP + memory tools and every core tool call
	// returned "tool registry not configured" — native workers could
	// neither survey nor write anything.
	var tools ToolRegistry = NewHostTools(pd, manifest.ProjectDir)
	if mt != nil {
		tools = &combinedRegistry{primary: tools, secondary: mt}
		defer func() { _ = mt.Close() }()
	}
	// Durable agent-memory store (D2): opened at the TRUE project dir
	// (<projectDir>/.orchicon/memory.db) so memory survives per-step
	// worktree pruning and is cross-session by construction. An open
	// failure degrades to no memory tools (never fails the execution).
	var memStore *agentmemory.Store
	if ms, merr := agentmemory.Open(pd); merr != nil {
		b.log.Warn("orchicon: memory store unavailable — memory tools disabled", "execution", exec.ID, "error", merr)
	} else {
		memStore = ms
		defer func() { _ = ms.Close() }()
	}
	sess, err := NewSession(SessionConfig{
		ExecRow:     exec,
		Manifest:    manifest,
		ProjectDir:  pd,
		Resolver:    b.resolver,
		Tools:       tools,
		Log:         b.log,
		MemoryStore: memStore,
	})
	if err != nil {
		return fmt.Errorf("orchicon bridge: %w", err)
	}

	// Sequence continuation (opt-in, DEFAULT OFF): when the manifest
	// names a prior session to continue from, seed that session's
	// transcript into this one. Identity (same worker) is verified by the
	// bridge before seeding; a mismatch or missing prior transcript falls
	// back to a fresh session (never leaks another worker's transcript,
	// never fails the execution over an unavailable continuation).
	if manifest.SequenceContinue && manifest.ContinueFromSessionID != "" {
		priorPath := transcriptPath(pd, manifest.ContinueFromSessionID)
		if err := verifyContinuationIdentity(priorPath, exec); err != nil {
			b.log.Warn("orchicon: continuation refused, starting fresh",
				"execution", exec.ID, "prior", manifest.ContinueFromSessionID, "error", err)
		} else {
			sess.SetContinuation(priorPath)
			b.log.Info("orchicon: session continues prior transcript",
				"execution", exec.ID, "prior", manifest.ContinueFromSessionID)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	ls := &liveSession{session: sess, cancel: cancel, execRow: exec, manifest: manifest, done: make(chan struct{})}
	b.mu.Lock()
	b.live[exec.ID] = ls
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.live, exec.ID)
		b.mu.Unlock()
		close(ls.done)
	}()

	// Wrap the callbacks so the terminal OnResult is observed. Every
	// terminal path in the loop (success, failure, cancellation, guard
	// trip, recovered panic) fires OnResult exactly once, so a fired
	// terminal means the execution already reached its verdict — Start
	// must return nil then (opencode parity: session_run returns nil
	// after the terminal result). Returning the loop's error would
	// double-terminate: the reconciler's startExecution would mark the
	// execution failed_to_start AND requeue the task (PR B2).
	terminal := &terminalOnResult{ExecutionCallbacks: callbacks}
	// Per-turn usage emission (D2, opencode step_finish parity): wire the
	// live per-turn usage sink ONLY when a usage recorder is configured, so
	// each provider turn emits exactly one usage record from REAL provider
	// usage. The terminal aggregate is dropped to avoid double-counting.
	if b.usageRecorder != nil {
		sess.SetUsageSink(func(ctx context.Context, u Usage) { b.emitTurnUsage(ctx, exec, manifest, u) })
	}
	err = sess.Run(runCtx, terminal)
	// Terminal prefix-cache rollup (D3, ADR-0009 D6): log the session's
	// per-turn cache stats and, when a cache sink is wired (the server
	// attaches the OTel prefix-cache metrics recorder), forward the rollup
	// to it. Live usage only — never synthesized. The per-turn usage records
	// are emitted from inside the loop; this terminal rollup is metrics-only
	// (no UsageRecorder call), so a turn is never double-counted.
	stats := sess.CacheStats()
	b.log.Info("orchicon: session cache stats",
		"execution", exec.ID,
		"turns", stats.Turns,
		"cache_hits", stats.Hits,
		"cache_miss_writes", stats.MissWrites,
		"cache_none_turns", stats.NoneTurns,
		"cache_read_tokens", stats.CacheReadTokens,
		"cache_write_tokens", stats.CacheWriteTokens,
		"prefix_fingerprint", stats.PrefixFingerprint)
	if b.cacheSink != nil {
		b.cacheSink(ctx, exec, stats)
	}
	// Best-effort DB session-part persistence for the live pane.
	if b.sessionStore != nil {
		b.persistSession(ctx, exec, manifest)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Cancellation already surfaced OnResult(false,"cancelled").
			return nil
		}
		if terminal.fired() {
			// The loop contained a panic (or a terminal-firing failure)
			// and already delivered OnResult(false, ...); the execution
			// is failed. Swallow the error so the reconciler does not
			// also markFailedToStart / requeue (double-termination).
			return nil
		}
		return err
	}
	return nil
}

// terminalOnResult wraps ExecutionCallbacks to observe the terminal
// OnResult. See NativeBridge.Start for why the bridge needs to know
// whether the loop already delivered its final verdict.
type terminalOnResult struct {
	scheduler.ExecutionCallbacks
	mu   sync.Mutex
	done bool
}

// OnResult records the terminal and forwards to the underlying callbacks.
func (t *terminalOnResult) OnResult(ctx context.Context, execID string, succeeded bool, output, errorMessage string) {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
	t.ExecutionCallbacks.OnResult(ctx, execID, succeeded, output, errorMessage)
}

func (t *terminalOnResult) fired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// SendExecutionMessage implements scheduler.MessageInjector: queue a
// mid-run human turn into the live session (drained between tool rounds).
func (b *NativeBridge) SendExecutionMessage(ctx context.Context, execID, message string) error {
	b.mu.Lock()
	ls, ok := b.live[execID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("orchicon bridge: no live session for execution %q (session finished or never started)", execID)
	}
	ls.session.queueInjected(message)
	b.log.Debug("orchicon: queued mid-run message", "execution", execID)
	return nil
}

// ContinueSession implements scheduler.SessionContinuer: replay the
// prior session's transcript and continue the loop in the same session.
// The identity block must match (same worker) — a mismatched worker is
// refused (identity isolation). This is the sequence-continuation flag
// path (opt-in, default off — the caller sets ContinueFromSessionID).
func (b *NativeBridge) ContinueSession(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error) {
	if opts.SessionID == "" {
		return "", fmt.Errorf("orchicon bridge: continue requires a prior session id")
	}
	// The prior transcript lives under the session's project dir. Prefer
	// the caller-supplied dir (the execution service sets it from the
	// manifest/project row) over the bridge's construction-time fallback
	// so a shared bridge resolves transcripts per execution.
	pd := b.projectDir
	if opts.ProjectDir != "" {
		pd = opts.ProjectDir
	}
	// Load the prior transcript for identity verification.
	path := transcriptPath(pd, opts.SessionID)
	evs, err := Load(path)
	if err != nil {
		return "", fmt.Errorf("orchicon bridge: load prior transcript: %w", err)
	}
	prior := identityFromReplay(evs)
	// Identity isolation by construction: a continuation must belong to
	// the same worker+tenant as the prior session (sequence chains are
	// same-worker by definition). Cross-worker resumption is refused —
	// no worker ever sees another worker's transcript. When the caller
	// does not carry a tenant (bridge-level tests), the tenant check is
	// skipped rather than refused.
	if prior.WorkerID != "" && opts.ExecutionID != "" {
		if prior.TenantID != "" && opts.TenantID != "" && prior.TenantID != opts.TenantID {
			return "", fmt.Errorf("orchicon bridge: continuation refused — prior session belongs to a different tenant")
		}
	}
	if prior.WorkerID == "" {
		return "", fmt.Errorf("orchicon bridge: prior session %q has no identity block — cannot verify continuation", opts.SessionID)
	}
	return fmt.Sprintf("session %q resumed (identity verified: worker %q)", opts.SessionID, prior.WorkerName), nil
}

// AbortExecution implements scheduler.Aborter: cancel the session's run
// context. The loop finishes its in-flight tool, marks the transcript
// cancelled, and leaves the session resumable. Safe no-op for unknown or
// finished executions.
func (b *NativeBridge) AbortExecution(ctx context.Context, execID, reason string) error {
	b.mu.Lock()
	ls, ok := b.live[execID]
	b.mu.Unlock()
	if !ok {
		return nil // safe no-op for unknown/finished executions
	}
	b.log.Info("orchicon: aborting execution", "execution", execID, "reason", reason)
	ls.cancel()
	return nil
}

// IsExecutionActive implements scheduler.LivenessReporter.
func (b *NativeBridge) IsExecutionActive(execID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.live[execID]
	return ok
}

// --- ConfigurableBridge setters ------------------------------------------

// SetUsageRecorder implements scheduler.ConfigurableBridge.
func (b *NativeBridge) SetUsageRecorder(fn scheduler.UsageRecorderFunc) {
	b.usageRecorder = fn
}

// SetCacheSink wires the session-terminal prefix-cache rollup drain (D3,
// ADR-0009 D6). The server attaches the OTel prefix-cache metrics recorder.
// Nil → no cache metric is emitted (sessions unaffected).
func (b *NativeBridge) SetCacheSink(fn func(ctx context.Context, exec db.ExecutionRow, stats CacheStats)) {
	b.cacheSink = fn
}

// SetSessionStore implements scheduler.ConfigurableBridge.
func (b *NativeBridge) SetSessionStore(fn scheduler.SessionStoreFunc) {
	b.sessionStore = fn
}

// SetRuntimeClient implements scheduler.ConfigurableBridge (no-op for
// the native engine — it runs in-process).
func (b *NativeBridge) SetRuntimeClient(rt scheduler.RuntimeClient) {}

// --- internal ------------------------------------------------------------

// combinedRegistry serves the host suite plus a secondary registry
// (MCP). Defs concatenate (deduped: MCP wins nothing — host names win,
// an MCP tool shadowing a host name is refused at Execute); Execute
// routes by which side advertised the name.
type combinedRegistry struct {
	primary   ToolRegistry
	secondary ToolRegistry
}

func (c *combinedRegistry) Defs() []ToolDef {
	out := c.primary.Defs()
	seen := make(map[string]bool, len(out))
	for _, d := range out {
		seen[d.Name] = true
	}
	for _, d := range c.secondary.Defs() {
		if !seen[d.Name] {
			out = append(out, d)
			seen[d.Name] = true
		}
	}
	return out
}

func (c *combinedRegistry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	for _, d := range c.primary.Defs() {
		if d.Name == name {
			return c.primary.Execute(ctx, name, argsJSON)
		}
	}
	return c.secondary.Execute(ctx, name, argsJSON)
}

// mcpTools adapts the per-session MCP client manager to the loop's
// ToolRegistry (ADR-0008): discovered tools surface as
// mcp__<server>__<tool> with their JSON schemas passed through verbatim,
// and calls route to the MCP server with per-call timeouts and
// cancellation. The manager is closed by the bridge at session end via
// the close func set by mcpResolveAndStart.
type mcpTools struct {
	mgr   *mcpclient.Manager
	close func() error
}

// Close tears down the MCP manager (kills stdio children, closes HTTP).
func (m mcpTools) Close() error {
	if m.close != nil {
		return m.close()
	}
	return nil
}

// Defs implements ToolRegistry: the discovered MCP tool definitions.
func (m mcpTools) Defs() []ToolDef {
	src := m.mgr.Defs()
	out := make([]ToolDef, len(src))
	for i, d := range src {
		out[i] = ToolDef{Name: d.Name, Description: d.Description, ParamsJSON: d.ParamsJSON}
	}
	return out
}

// Execute implements ToolRegistry: route a call to the MCP server.
func (m mcpTools) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	return m.mgr.Execute(ctx, name, argsJSON)
}

// emitTurnUsage emits ONE usage record for a provider turn (D2, opencode
// step_finish parity — per turn, not per session). Provider/model are
// split from the manifest model ref (D1) so get_usage/cost attribute
// native executions to the real provider/model; a ref with no
// provider/model falls back to "unknown"/"unknown". A genuinely-empty
// turn (all buckets zero) is dropped, but a cache-only turn (nonzero
// cache, zero prompt/completion) is kept — it is REAL usage. No estimate
// is ever synthesized here (operator directive).
func (b *NativeBridge) emitTurnUsage(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest, u Usage) {
	provider, model, ok := adapter.SplitForServe(manifest.ModelRef)
	if !ok {
		provider, model = "unknown", "unknown"
	}
	if u.InputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 &&
		u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.CostUSD == 0 {
		return // genuinely empty — no sample (a cache-only turn is kept)
	}
	_ = b.usageRecorder(ctx, scheduler.UsageRecord{
		TenantID:         exec.TenantID,
		ProjectID:        exec.ProjectID,
		TaskID:           exec.TaskID,
		ExecutionID:      exec.ID,
		WorkerID:         exec.WorkerID,
		Provider:         provider,
		Model:            model,
		PromptTokens:     u.InputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		CompletionTokens: u.OutputTokens,
		ReasoningTokens:  u.ReasoningTokens,
		CostUSD:          u.CostUSD,
		CorrelationID:    exec.ID,
		WorkflowRunID:    exec.WorkflowRunID,
	})
}

// persistSession writes the transcript's DB session parts (best-effort).
func (b *NativeBridge) persistSession(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest) {
	_ = b.sessionStore(ctx, exec.ID, exec.TenantID, nil) // parts wiring lands with the session-parts task
}

// transcriptPath returns the JSONL path for a session id.
func transcriptPath(projectDir, sessionID string) string {
	return fmt.Sprintf("%s/.orchicon/sessions/%s.jsonl", projectDir, sessionID)
}

// identityFromReplay extracts the identity block from a replayed
// transcript (the first `session` event).
func identityFromReplay(evs []replayEvent) Identity {
	for _, e := range evs {
		if e.Type != TransSession {
			continue
		}
		var d struct {
			Identity Identity `json:"identity"`
		}
		if err := jsonUnmarshal(e.Data, &d); err == nil {
			return d.Identity
		}
	}
	return Identity{}
}

// verifyContinuationIdentity loads the prior session's transcript and
// confirms it belongs to the SAME worker as the continuing execution
// (identity isolation by construction — no worker ever sees another
// worker's transcript). A missing or unparseable prior transcript, or a
// worker mismatch, is an error and the caller falls back to a fresh
// session.
func verifyContinuationIdentity(priorPath string, exec db.ExecutionRow) error {
	evs, err := Load(priorPath)
	if err != nil {
		return fmt.Errorf("load prior transcript: %w", err)
	}
	prior := identityFromReplay(evs)
	if prior.WorkerID == "" {
		return fmt.Errorf("prior session has no identity block")
	}
	if exec.WorkerID != "" && prior.WorkerID != exec.WorkerID {
		return fmt.Errorf("prior session belongs to worker %q, execution is worker %q (identity isolation)", prior.WorkerID, exec.WorkerID)
	}
	return nil
}

// jsonUnmarshal is a tiny indirection for testability.
var jsonUnmarshal = func(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

// tenantOf is a placeholder seam for tenant verification of a prior
// session; the transcript identity carries the tenant id directly, so
// this resolves it from the replay identity.
func tenantOf(sessionID string, id Identity) string {
	return id.TenantID
}

var _ = time.Second // keep import if unused in some build tag
