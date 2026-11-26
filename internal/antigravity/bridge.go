package antigravity

// bridge.go — the Antigravity (agy) worker bridge, at parity with internal/claude/adapter.go.
//
// THE SHAPE IS CLAUDE'S, METHOD FOR METHOD, ON PURPOSE. The dispatcher, the recovery engine, the
// execution RPCs and the Ask surface all reach adapters through a handful of OPTIONAL capability
// interfaces (scheduler.AdapterBridge, ConfigurableBridge, MessageInjector, Aborter, ContextCompacter,
// SessionContinuer, LivenessReporter, DiffLedgerSink, SessionOwnerKind). An adapter that implements a
// different subset is dispatched differently — recovery will not resume it, `SendExecutionMessage` will
// return "not supported", and the operator sees an execution whose buttons do nothing. So the parity
// list is implemented literally, and the compile-time assertions at the bottom of this file are the
// guard: dropping one is a build failure rather than a runtime surprise.
//
// THE ONE THING THAT IS NOT PARITY: agy cannot accept a message into a RUNNING turn (no ACP method
// exists for it), so `SendExecutionMessage` QUEUES the text and delivers it as the next
// `session/prompt` the moment the in-flight turn reports its stopReason. That is agy's own
// `queuedMessages:"queue"` semantics, and it is why the capability is advertised as
// "mid_run_injection (queued)" rather than as preemption.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// ErrNoExecutionID reports a Start with no execution identity — it would have no way to attribute a
// callback, a usage row or a ledger entry.
var ErrNoExecutionID = errors.New("antigravity: Start needs an execution id (exec.ID or manifest.ExecutionID)")

// ErrAlreadyLive reports a second Start for an execution that already has a live session. It is the
// single-writer invariant at the EXECUTION level: two subprocesses driving one execution would both
// stream OnResult and both record usage.
func ErrAlreadyLive(execID string) error {
	return fmt.Errorf("antigravity: execution %s already has a live agy session — refusing a second writer", execID)
}

// ErrSessionLive reports a resume whose target agy session id is already owned by a live execution. Two
// writers to one agy conversation would interleave their prompts on one wire and corrupt both.
func ErrSessionLive(sid, owner string) error {
	return fmt.Errorf("antigravity: agy session %s already has a live writer (execution %s) — refusing a second writer", sid, owner)
}

// ErrNoLiveSession reports that an operation needs a live session and there is none for that execution.
func ErrNoLiveSession(execID string) error {
	return fmt.Errorf("antigravity: execution %s has no live agy session", execID)
}

// SettingsResolver resolves this tenant's Antigravity spawn settings (auth method + telemetry) and, for
// the gemini-api-key method, the decrypted GEMINI_API_KEY.
//
// IT IS A CLOSURE, NOT A DB READ IN THIS PACKAGE, for two reasons: the bridge must be testable without a
// pool, and the decryption key (the plane's secrets KEK) must never travel into an adapter. The server
// supplies the same resolution internal/runtime/antigravity_auth.go performs.
type SettingsResolver func(ctx context.Context, tenantID string) (SpawnSettings, string, error)

// MCPSecretResolver expands ${SECRET_NAME} references in an MCP spec's env/headers. Same signature as
// claude.MCPSecretResolver, so the server wires exactly one closure to both bridges.
type MCPSecretResolver = func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error)

// Bridge is the Antigravity (agy) adapter.
type Bridge struct {
	log           *slog.Logger
	usageRecorder scheduler.UsageRecorderFunc
	sessionStore  scheduler.SessionStoreFunc
	fileEdits     FileEditHookFunc
	// askFileEdits is the Ask-population ledger half. Child 6 installs it; it is held here so
	// `SetDiffLedger` (the interface the dispatcher calls) satisfies its contract without this file
	// pretending to implement an Ask surface it does not have.
	askFileEdits FileEditHookFunc

	rt *runtime.Client

	// factoryOverride replaces the process factory (tests only).
	factoryOverride acp.ProcFactory
	// binaryOverride replaces the agy binary resolution (tests only).
	binaryOverride func() (string, error)
	// settingsOverride replaces the tenant settings resolution (tests only).
	settingsOverride SettingsResolver

	mu        sync.Mutex
	live      map[string]*session // execID → live session
	bySession map[string]string   // agy session id → owning execID

	// mcpResolver resolves the owner-scoped MCP union, and mcpSecretResolver expands the refs inside it.
	mcpResolver       mcpclient.ScopeResolver
	mcpSecretResolver MCPSecretResolver
	// settings resolves the tenant's spawn settings at launch time.
	settings SettingsResolver
}

// Compile-time assertions: the bridge must satisfy the FULL parity set. An adapter that silently stopped
// satisfying one would compile fine and break routing, recovery or a UI button at runtime.
//
// NOTE ON ConfigurableBridge: it declares SetRuntimeClient(scheduler.RuntimeClient), while this bridge (like
// claude's and opencode's) takes the CONCRETE *runtime.Client so the daemon's full surface is available
// without a type assertion. The two setters that define the contract's actual behaviour — SetUsageRecorder
// and SetSessionStore — are asserted by their own interface below, and the server wires the concrete
// SetRuntimeClient exactly as it does for claude.
var (
	_ scheduler.AdapterBridge    = (*Bridge)(nil)
	_ usageAndSessionSink        = (*Bridge)(nil)
	_ scheduler.MessageInjector  = (*Bridge)(nil)
	_ scheduler.Aborter          = (*Bridge)(nil)
	_ scheduler.ContextCompacter = (*Bridge)(nil)
	_ scheduler.SessionContinuer = (*Bridge)(nil)
	_ scheduler.LivenessReporter = (*Bridge)(nil)
	_ scheduler.DiffLedgerSink   = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind = (*Bridge)(nil)
	_ acp.Handler                = (*PolicyHandler)(nil)
)

// New builds an Antigravity bridge.
func New(log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{
		log:       log,
		live:      make(map[string]*session),
		bySession: make(map[string]string),
	}
}

// SetUsageRecorder implements scheduler.ConfigurableBridge.
func (b *Bridge) SetUsageRecorder(fn scheduler.UsageRecorderFunc) { b.usageRecorder = fn }

// SetSessionStore implements scheduler.ConfigurableBridge.
func (b *Bridge) SetSessionStore(fn scheduler.SessionStoreFunc) { b.sessionStore = fn }

// SetRuntimeClient implements scheduler.ConfigurableBridge (a concrete setter, like claude's and
// opencode's): nil keeps the bridge in-process.
func (b *Bridge) SetRuntimeClient(rt *runtime.Client) { b.rt = rt }

// SetDiffLedger implements scheduler.DiffLedgerSink. The dispatcher calls it for every registered bridge
// as soon as the plane installs the ledger, so the diff pipeline needs no per-adapter wiring.
func (b *Bridge) SetDiffLedger(execution, ask scheduler.DiffLedgerFunc) {
	b.fileEdits = execution
	if ask != nil {
		b.askFileEdits = ask
	}
}

// SetFileEditHook injects just the execution ledger (kept for symmetry with claude's setter).
func (b *Bridge) SetFileEditHook(fn FileEditHookFunc) { b.fileEdits = fn }

// SetScopeResolver wires the owner-scoped MCP resolution: the SAME resolver the native and claude
// bridges receive, so the three adapters cannot disagree about which servers a scope gets.
func (b *Bridge) SetScopeResolver(src mcpclient.ScopeResolver) { b.mcpResolver = src }

// SetMCPSecretResolver wires the ${SECRET_NAME} → plaintext resolver used just before the ACP
// `mcpServers` are rendered.
func (b *Bridge) SetMCPSecretResolver(r MCPSecretResolver) { b.mcpSecretResolver = r }

// SetSettingsResolver wires the tenant's Antigravity spawn settings.
func (b *Bridge) SetSettingsResolver(r SettingsResolver) { b.settings = r }

// SetSpawnOverride replaces the process factory (tests only). A wired override forces the LOCAL
// transport, because a fake process cannot live inside a runtime container.
func (b *Bridge) SetSpawnOverride(fn acp.ProcFactory) { b.factoryOverride = fn }

// SetBinaryOverride replaces the agy binary resolution (tests only).
func (b *Bridge) SetBinaryOverride(fn func() (string, error)) { b.binaryOverride = fn }

// SetSettingsOverride replaces the tenant settings resolution (tests only).
func (b *Bridge) SetSettingsOverride(fn SettingsResolver) { b.settingsOverride = fn }

// Start implements scheduler.AdapterBridge.
func (b *Bridge) Start(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	execID := exec.ID
	if execID == "" {
		execID = manifest.ExecutionID
	}
	if execID == "" {
		return ErrNoExecutionID
	}
	// ONE LIVE SESSION PER EXECUTION.
	if b.isLive(execID) {
		return ErrAlreadyLive(execID)
	}
	// ONE LIVE WRITER PER AGY SESSION ID. A resume whose target conversation already has a live writer is
	// refused: agy multiplexes turns on one session id, so two clients would interleave their prompts.
	if manifest.SequenceContinue && manifest.ContinueFromSessionID != "" {
		if owner := b.sessionOwner(manifest.ContinueFromSessionID); owner != "" {
			return ErrSessionLive(manifest.ContinueFromSessionID, owner)
		}
	}

	tenantID := exec.TenantID
	s := newSession(b, execID, tenantID, manifest, callbacks)
	b.registerExec(execID, s)
	defer b.unregisterExec(execID, s)

	b.log.Info("antigravity: starting agy worker session",
		"execution", execID, "worker", exec.WorkerID, "tenant", tenantID,
		"model", manifest.ModelRef, "resume", manifest.ContinueFromSessionID,
		"worktree", manifest.WorktreePath)
	return s.run(ctx)
}

// SendExecutionMessage implements scheduler.MessageInjector.
//
// A SECOND `session/prompt` IS NOT DEFINED while one is in flight, so the message is QUEUED and delivered
// as the next turn the moment the current `stopReason` arrives. The caller is told it was accepted, not
// that the model has seen it: agy provides no way to preempt a running turn.
func (b *Bridge) SendExecutionMessage(ctx context.Context, execID, message string) error {
	s := b.liveSession(execID)
	if s == nil {
		return ErrNoLiveSession(execID)
	}
	if err := s.SendTurn(ctx, message); err != nil {
		return err
	}
	b.log.Info("antigravity: message queued for the live session", "execution", execID)
	return nil
}

// AbortExecution implements scheduler.Aborter: `session/cancel` immediately, then a hard process shutdown
// after the grace window if the turn has not ended. A safe no-op for an unknown or finished execution.
func (b *Bridge) AbortExecution(ctx context.Context, execID, reason string) error {
	s := b.liveSession(execID)
	if s == nil {
		b.log.Debug("antigravity abort: no live session", "execution", execID)
		return nil
	}
	b.log.Info("antigravity: aborting execution session", "execution", execID, "reason", reason)
	s.abort(reason)
	return nil
}

// CompactExecution implements scheduler.ContextCompacter: it prompts the literal `/compact` (agy's own
// slash command, docs §3.10) on the live session, queued behind the in-flight turn.
func (b *Bridge) CompactExecution(ctx context.Context, execID, provider, model, remainingScope string) error {
	s := b.liveSession(execID)
	if s == nil {
		return ErrNoLiveSession(execID)
	}
	return s.Compact(ctx, provider, model, remainingScope)
}

// ContinueSession implements scheduler.SessionContinuer: a follow-up runs against the SAME agy session
// identity, never a fresh conversation.
func (b *Bridge) ContinueSession(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error) {
	if s := b.liveSession(opts.ExecutionID); s != nil {
		if err := s.SendTurn(ctx, opts.Message); err != nil {
			return "", err
		}
		return s.sessionIdentity(), nil
	}
	sid := strings.TrimSpace(opts.SessionID)
	if sid == "" {
		return "", ErrNoLiveSession(opts.ExecutionID)
	}
	manifest := b.followUpManifest(opts)
	go func() {
		// WithoutCancel strips the request's cancellation/deadline while keeping its values: an RPC
		// returning (or a browser disconnect) must neither cancel the resumed session nor lose its reply.
		detached := context.WithoutCancel(ctx)
		cbs := &followUpCallbacks{log: b.log, execID: opts.ExecutionID}
		if err := b.Start(detached, db.ExecutionRow{ID: opts.ExecutionID, TenantID: opts.TenantID}, manifest, cbs); err != nil {
			b.log.Warn("antigravity follow-up session failed",
				"execution", opts.ExecutionID, "session", sid, "error", err)
		}
	}()
	return sid, nil
}

// followUpManifest reconstructs the manifest a resumed follow-up needs: the SAME session identity
// (ContinueFromSessionID → `session/resume`), the follow-up as the turn's goal.
//
// WORKTREE PATH IS INCLUDED, unlike claude's: agy's cwd is the session's workspace and a resume whose cwd
// differs silently points the agent at a different tree.
func (b *Bridge) followUpManifest(opts scheduler.ContinueSessionOpts) scheduler.ExecutionManifest {
	return scheduler.ExecutionManifest{
		ExecutionID:           opts.ExecutionID,
		Goal:                  opts.Message,
		SystemPrompt:          opts.SystemPrompt,
		ModelRef:              opts.ModelRef,
		ProjectDir:            opts.ProjectDir,
		WorktreePath:          opts.WorktreePath,
		ProjectID:             opts.ProjectID,
		TaskID:                opts.TaskID,
		WorkerID:              opts.WorkerID,
		RuntimeWorkflowID:     opts.RuntimeWorkflowID,
		ExecutionMode:         opts.ExecutionMode,
		SequenceContinue:      true,
		ContinueFromSessionID: opts.SessionID,
	}
}

// followUpCallbacks is the minimal ExecutionCallbacks sink for a resumed follow-up session: the follow-up
// has no execution row to reconcile, so its lifecycle is logged rather than reported.
type followUpCallbacks struct {
	log    *slog.Logger
	execID string
}

func (c *followUpCallbacks) OnStarted(context.Context, string)                {}
func (c *followUpCallbacks) OnText(context.Context, string, string)           {}
func (c *followUpCallbacks) OnWrittenFiles(context.Context, string, []string) {}
func (c *followUpCallbacks) OnHealth(context.Context, string, string)         {}
func (c *followUpCallbacks) OnStall(context.Context, string, string, bool)    {}
func (c *followUpCallbacks) OnRecovered(context.Context, string, string)      {}
func (c *followUpCallbacks) OnToolCall(context.Context, string, string, []byte, []byte) {
}
func (c *followUpCallbacks) OnArtifact(context.Context, string, string, string, string) {}

func (c *followUpCallbacks) OnResult(_ context.Context, execID string, ok bool, _ string, errMsg string) {
	c.log.Info("antigravity follow-up session finished", "execution", execID, "ok", ok, "error", errMsg)
}

// IsExecutionActive implements scheduler.LivenessReporter.
func (b *Bridge) IsExecutionActive(execID string) bool { return b.isLive(execID) }

// SessionOwnerKind implements scheduler.SessionOwnerKind.
func (b *Bridge) SessionOwnerKind() string { return adapter.KindAntigravity }

// ---------------------------------------------------------------------
// live registry
// ---------------------------------------------------------------------

func (b *Bridge) registerExec(execID string, s *session) {
	b.mu.Lock()
	b.live[execID] = s
	b.mu.Unlock()
}

func (b *Bridge) unregisterExec(execID string, s *session) {
	b.mu.Lock()
	if cur, ok := b.live[execID]; ok && cur == s {
		delete(b.live, execID)
	}
	for sid, owner := range b.bySession {
		if owner == execID {
			delete(b.bySession, sid)
		}
	}
	b.mu.Unlock()
}

func (b *Bridge) isLive(execID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.live[execID]
	return ok
}

func (b *Bridge) liveSession(execID string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live[execID]
}

// bindSessionID claims an agy session id for execID. It returns false when another LIVE execution already
// owns it — the single-writer invariant.
func (b *Bridge) bindSessionID(sid, execID string) bool {
	if sid == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner, ok := b.bySession[sid]; ok && owner != execID {
		return false
	}
	b.bySession[sid] = execID
	return true
}

// sessionOwner returns the execID of the live session owning sid, or "".
func (b *Bridge) sessionOwner(sid string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bySession[sid]
}

// resolveSettings reads the tenant's Antigravity settings, with a test override taking precedence and a
// sane default (oauth-personal, telemetry off) when the plane wired no resolver.
func (b *Bridge) resolveSettings(ctx context.Context, tenantID string) (SpawnSettings, string, error) {
	if b.settingsOverride != nil {
		return b.settingsOverride(ctx, tenantID)
	}
	if b.settings != nil {
		return b.settings(ctx, tenantID)
	}
	// NO RESOLVER: the CLI's own default method with telemetry disabled, which is the same shape a tenant
	// that never opened the setting gets. It is logged because a wired plane should always supply one.
	b.log.Warn("antigravity: no settings resolver wired — launching with the default auth method",
		"tenant", tenantID)
	return SpawnSettings{AuthMethod: "", ShareTelemetry: false}, "", nil
}

// resolveBinary resolves the agy executable, with a test override taking precedence.
func (b *Bridge) resolveBinary() (string, error) {
	if b.binaryOverride != nil {
		return b.binaryOverride()
	}
	return resolveAgyBinary()
}

// usageAndSessionSink is the behavioural half of scheduler.ConfigurableBridge: the two setters the
// dispatcher/server actually rely on to attribute usage and persist a transcript. It is asserted
// separately because the interface's SetRuntimeClient takes the scheduler.RuntimeClient INTERFACE while
// every adapter implements the concrete setter (see the note above the assertion block).
type usageAndSessionSink interface {
	SetUsageRecorder(fn scheduler.UsageRecorderFunc)
	SetSessionStore(fn scheduler.SessionStoreFunc)
}

// clientVersion is what this bridge reports in `initialize`. It is a constant rather than a build stamp: the
// ACP clientInfo is informational for agy and a version derived at runtime would make two identical runs
// compare unequal in a log diff.
const clientVersion = "1.0.0"
