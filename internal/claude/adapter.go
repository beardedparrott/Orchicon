package claude

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// FileEditHookFunc is the diff-pipeline ledger callback (the same shape the
// opencode adapter's concrete SetFileEditHook takes). Declared as an ALIAS
// (not a distinct named type) so the server's single newFileEditHook value
// satisfies both adapters without a conversion. SetHostServe-style setters
// are deliberately concrete (scheduler.ConfigurableBridge does not carry
// them).
type FileEditHookFunc = func(ctx context.Context, execID, tenantID, execDir, toolName string, input map[string]any, output string)

// Bridge is the Claude Code adapter. It satisfies scheduler.AdapterBridge
// (Start) plus the optional capabilities MessageInjector, SessionContinuer,
// Aborter, LivenessReporter and ContextCompacter. It deliberately does NOT
// implement ChatTurnClient — Ask chat on claude is out of scope, so
// Dispatcher.ChatKinds() omits "claude" and the Ask guard surfaces an
// actionable error (never a panic).
//
// Context compaction is Claude-NATIVE: claude has no in-container serve
// (servePortFor("claude") == 0 and it is a MOUNT-only boot-profile entry),
// so there is no summarize endpoint — CompactExecution writes an in-session
// directive turn that makes the session summarize its own working context,
// exactly the soft/lossy compact the opencode adapter's summarize produces.
type Bridge struct {
	log           *slog.Logger
	usageRecorder scheduler.UsageRecorderFunc
	sessionStore  scheduler.SessionStoreFunc
	fileEdits     FileEditHookFunc
	rt            *runtime.Client

	// spawn is the process factory (local vs container). Overridable in
	// tests so the core acceptance runs against a fake ProcSession with no
	// real Anthropic spend.
	spawnOverride procFactory

	mu        sync.Mutex
	live      map[string]*session // execID → live session
	bySession map[string]string   // claude session id → owning execID
}

// New builds a Claude Code bridge.
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

// SetRuntimeClient injects the workflow runtime daemon client (concrete
// setter, mirroring opencode's). Nil keeps the bridge in-process.
func (b *Bridge) SetRuntimeClient(rt *runtime.Client) { b.rt = rt }

// SetFileEditHook injects the diff-pipeline ledger callback. Nil = no ledger.
func (b *Bridge) SetFileEditHook(fn FileEditHookFunc) { b.fileEdits = fn }

// SetSpawnOverride replaces the process factory (tests only).
func (b *Bridge) SetSpawnOverride(fn procFactory) { b.spawnOverride = fn }

// Start implements scheduler.AdapterBridge. It spawns ONE long-lived claude
// subprocess and drives it across the execution's turns; it returns when the
// execution reaches a terminal turn (or fails).
func (b *Bridge) Start(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest, callbacks scheduler.ExecutionCallbacks) error {
	execID := exec.ID
	if execID == "" {
		execID = manifest.ExecutionID
	}
	if execID == "" {
		return errNoExecutionID
	}
	if b.isLive(execID) {
		return errAlreadyLive(execID)
	}
	// One active subprocess per SESSION id: a resume whose target transcript
	// already has a live writer is refused (never two writers to one JSONL).
	if manifest.SequenceContinue && manifest.ContinueFromSessionID != "" {
		if owner := b.sessionOwner(manifest.ContinueFromSessionID); owner != "" {
			return errSessionLive(manifest.ContinueFromSessionID, owner)
		}
	}

	s := newSession(b, execID, exec.TenantID, manifest, callbacks)
	b.registerExec(execID, s)
	defer b.unregisterExec(execID, s)
	return s.run(ctx)
}

// SendExecutionMessage implements scheduler.MessageInjector: it writes a
// user turn onto the live session's stdin; the reply streams back on the
// same session.
func (b *Bridge) SendExecutionMessage(ctx context.Context, execID, message string) error {
	s := b.liveSession(execID)
	if s == nil {
		return errNoLiveSession(execID)
	}
	return s.SendTurn(ctx, message)
}

// AbortExecution implements scheduler.Aborter: SIGINT then (after a grace
// window) SIGTERM. A safe no-op for unknown/finished executions.
func (b *Bridge) AbortExecution(ctx context.Context, execID, reason string) error {
	s := b.liveSession(execID)
	if s == nil {
		b.log.Debug("claude abort: no live session", "execution", execID)
		return nil
	}
	b.log.Info("aborting claude execution session", "execution", execID, "reason", reason)
	s.abort()
	return nil
}

// CompactExecution implements scheduler.ContextCompacter: it compacts the
// live claude session's working context in place (the Claude-native compact
// directive turn — there is no serve to call, so no HTTP path exists here).
// An execution with no live session gets an actionable error naming the
// capability, never a panic (bridge contract rule).
func (b *Bridge) CompactExecution(ctx context.Context, execID, provider, model, remainingScope string) error {
	s := b.liveSession(execID)
	if s == nil {
		return errNoLiveSessionForCompact(execID)
	}
	return s.Compact(ctx, provider, model, remainingScope)
}

// ContinueSession implements scheduler.SessionContinuer: a follow-up runs
// against the SAME claude session identity, never a fresh conversation.
//
//   - A live session for the execution: the follow-up is written onto the
//     live stdin, so it keeps the session's full context (the same path a
//     mid-run injection takes).
//   - No live session (a control-plane restart, a lost runtime container, an
//     execution that already finished): the recorded session id is
//     RE-ATTACHED through `--resume <session-id>`, and the follow-up is
//     driven there. The reply lands in the durable transcript through the
//     same session machinery (the session's mapper owns that fan-out), so the
//     reply collection is fire-and-forget and detached from the RPC context.
//
// An execution with neither a live session nor a recorded session id gets an
// actionable error (never a panic).
func (b *Bridge) ContinueSession(ctx context.Context, opts scheduler.ContinueSessionOpts) (string, error) {
	if s := b.liveSession(opts.ExecutionID); s != nil {
		if err := s.SendTurn(ctx, opts.Message); err != nil {
			return "", err
		}
		return s.sessionIdentity(), nil
	}
	sid := strings.TrimSpace(opts.SessionID)
	if sid == "" {
		return "", errNoLiveSession(opts.ExecutionID)
	}
	manifest := b.followUpManifest(opts)
	go func() {
		// WithoutCancel strips the request's cancellation/deadline while
		// keeping its values: an RPC returning (or a browser disconnect) must
		// neither cancel the resumed session nor lose its reply.
		detached := context.WithoutCancel(ctx)
		cbs := &followUpCallbacks{log: b.log, execID: opts.ExecutionID}
		if err := b.Start(detached, db.ExecutionRow{ID: opts.ExecutionID, TenantID: opts.TenantID}, manifest, cbs); err != nil {
			b.log.Warn("claude follow-up session failed", "execution", opts.ExecutionID, "session", sid, "error", err)
		}
	}()
	return sid, nil
}

// followUpManifest reconstructs the execution manifest a resumed follow-up
// needs: the SAME session identity (ContinueFromSessionID → `--resume`), the
// follow-up as the turn's goal, and the caller's model/system prompt so the
// follow-up runs with the worker's own configuration.
func (b *Bridge) followUpManifest(opts scheduler.ContinueSessionOpts) scheduler.ExecutionManifest {
	return scheduler.ExecutionManifest{
		ExecutionID:           opts.ExecutionID,
		Goal:                  opts.Message,
		SystemPrompt:          opts.SystemPrompt,
		ModelRef:              opts.ModelRef,
		ProjectDir:            opts.ProjectDir,
		SequenceContinue:      true,
		ContinueFromSessionID: opts.SessionID,
	}
}

// followUpCallbacks is the minimal ExecutionCallbacks sink for a resumed
// follow-up session. The durable transcript is written by the session's own
// mapper, so this sink exists to observe the outcome (and to keep the bridge
// contract honest: Start always receives a callbacks value).
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
	c.log.Info("claude follow-up session finished", "execution", execID, "ok", ok, "error", errMsg)
}

// IsExecutionActive implements scheduler.LivenessReporter.
func (b *Bridge) IsExecutionActive(execID string) bool { return b.isLive(execID) }

// SessionOwnerKind implements scheduler.SessionOwnerKind.
func (b *Bridge) SessionOwnerKind() string { return adapter.KindClaude }

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

// bindSessionID claims a claude session id for execID. It returns false when
// another LIVE execution already owns it (the single-writer invariant).
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

// sessionOwner returns the execID of a live subprocess owning sid, or "".
func (b *Bridge) sessionOwner(sid string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bySession[sid]
}

// spawn selects the transport: a container child when the execution belongs
// to a workflow run with a daemon wired and is not in local mode; otherwise
// a host-local subprocess.
func (b *Bridge) spawn(ctx context.Context, spec procSpec, manifest scheduler.ExecutionManifest) (ProcSession, error) {
	if b.spawnOverride != nil {
		return b.spawnOverride(ctx, spec)
	}
	if b.rt != nil && manifest.RuntimeWorkflowID != "" && manifest.ExecutionMode != db.ExecutionModeLocal {
		return newContainerProc(ctx, b.rt, spec)
	}
	return newLocalProc(ctx, spec)
}

// isContainer reports whether this execution's child runs inside the run's
// runtime container rather than as a host subprocess. It MUST agree with
// spawn()'s transport selection: childEnv uses it to decide who owns the
// OS-level guard shim (the supervisor inside the container, the adapter on the
// host).
func (b *Bridge) isContainer(m scheduler.ExecutionManifest) bool {
	if b.spawnOverride != nil {
		return false
	}
	return b.rt != nil && m.RuntimeWorkflowID != "" && m.ExecutionMode != db.ExecutionModeLocal
}

// parseModelRefLoose extracts the model segment from a model_ref, tolerating
// structural-only parsing (nil registry) for refs whose provider is unknown
// at this layer. Returns "" on error.
func parseModelRefLoose(ref string) (string, error) {
	mr, err := adapter.ParseModelRef(ref, nil)
	if err != nil {
		return "", err
	}
	return mr.Model, nil
}
