package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// timeAfterProcClose bounds how long Close waits after SIGTERM before it
// hard-kills the child.
var timeAfterProcClose = 5 * time.Second

// abortGrace is how long a cancelled session is given after SIGINT before the bridge escalates to
// SIGTERM.
var abortGrace = 3 * time.Second

// session is ONE long-lived claude session: a single subprocess spawned once and driven across N
// user turns on the same stdin.
type session struct {
	b        *Bridge
	execID   string
	tenantID string
	manifest scheduler.ExecutionManifest
	cbs      scheduler.ExecutionCallbacks

	model    string
	resumeID string

	// stderrRing holds the LAST stderr lines the CLI emitted.
	stderrRing *stderrRing
	proc     ProcSession

	// mapper owns the whole stream-event → ExecutionCallbacks fan-out plus
	// the durable transcript / ledger / todo side effects (parse.go).
	mapper *Mapper

	// Shared budget gate (ONE source of truth).
	ladder    *opencode.BudgetLadder
	spend     *opencode.BudgetSpend
	startedAt time.Time

	step            int
	lastCompactStep int
	compactions     int
	tierLatch       map[string]bool

	// abortWindow is this session's SIGINT→SIGTERM grace, resolved once at construction from the
	// package default so a live session never re-reads a package-level var.
	abortWindow time.Duration

	mu       sync.Mutex
	finished bool
	aborted  bool
	queued   int
	seq      int64
}

func newSession(b *Bridge, execID, tenantID string, manifest scheduler.ExecutionManifest, cbs scheduler.ExecutionCallbacks) *session {
	m := manifest.ModelRef
	if strings.TrimSpace(m) == "" {
		m = manifest.DefaultModelRef
	}
	s := &session{
		b:           b,
		execID:      execID,
		tenantID:    tenantID,
		manifest:    manifest,
		cbs:         cbs,
		model:       modelForRef(m),
		resumeID:    manifest.ContinueFromSessionID,
		ladder:      opencode.ParseBudgetLadder(manifest.Budgets),
		spend:       opencode.NewBudgetSpend(),
		startedAt:   time.Now(),
		tierLatch:   map[string]bool{},
		abortWindow: abortGrace,
	}
	s.mapper = NewMapper(execID, cbs, MapperDeps{
		TenantID:      tenantID,
		Model:         s.model,
		ExecDir:       executionDir(manifest),
		Manifest:      manifest,
		FileEdits:     b.fileEdits,
		UsageRecorder: b.usageRecorder,
		BindSession:   s.bindSession,
		Record:        s.recordPart,
		Log:           b.log,
	})
	return s
}

// run spawns the single subprocess and drives it to a terminal turn.
func (s *session) run(ctx context.Context) error {
	// THE HOOK MUST BE ABLE TO RUN BEFORE ANYTHING ELSE HAPPENS.
	if err := PreflightHookExecutable(s.inContainer()); err != nil {
		s.finish(ctx, false, err.Error(), err)
		return err
	}
	argv := s.argv()
	// The child's environment carries the project boundary the PreToolUse hook reads, and — on the
	// LOCAL transport — the OS-level execution guard shim on PATH.
	env, envCleanup := s.childEnv()
	defer envCleanup()
	spec := procSpec{
		ExecID: s.execID,
		Argv:   argv,
		Cwd:    executionDir(s.manifest),
		// The container is leased by RUN id, so the transport must name the run — see
		// procSpec.WorkflowRunID.
		ProjectDir:    s.manifest.ProjectDir,
		Env:           env,
		WorkflowRunID: s.manifest.RuntimeWorkflowID,
	}
	p, err := s.b.spawn(ctx, spec, s.manifest)
	if err != nil {
		s.finish(ctx, false, fmt.Sprintf("claude spawn failed: %v", err), err)
		return err
	}
	s.proc = p
	defer func() { _ = p.Close() }()
	// DRAIN STDERR FROM THE FIRST MOMENT.
	s.stderrRing = startStderrDrain(p.Stderr())
	defer s.stderrRing.stop()

	s.cbs.OnStarted(ctx, s.execID)
	if err := p.WriteTurn(s.initialTurnPayload()); err != nil {
		s.finish(ctx, false, fmt.Sprintf("claude initial turn write failed: %v", err), err)
		return err
	}

	lines := p.Lines()
	// The stall watchdog ticks independently of stdout: total silence past the no-progress window is
	// fatal (the child is hard-killed and the execution fails).
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish(ctx, false, "execution cancelled", ctx.Err())
			return ctx.Err()
		case <-tick.C:
			if reason := s.mapper.Tick(ctx, time.Now()); reason != "" {
				s.abort()
				msg := fmt.Sprintf("claude session stalled: %s", reason)
				s.finish(ctx, false, msg, fmt.Errorf("%s", msg))
				return fmt.Errorf("%s", msg)
			}
		case line, ok := <-lines:
			if !ok {
				// stdout closed → the child exited.
				if s.isFinished() {
					return nil
				}
				// Tear the transport down first so Wait cannot block on a stream that is already gone, then read
				// the exit code with a bounded wait.
				_ = p.Close()
				code := 0
				var waitErr error
				type waitResult struct {
					code int
					err  error
				}
				codeCh := make(chan waitResult, 1)
				go func() {
					c, e := p.Wait()
					codeCh <- waitResult{c, e}
				}()
				select {
				case r := <-codeCh:
					code, waitErr = r.code, r.err
				case <-time.After(2 * time.Second):
				}
				// THE CAUSE THE PROC HOLDS COMES FIRST.
				msg := fmt.Sprintf("claude session exited without a terminal result message (exit code %d)%s%s", code, exitCause(waitErr), s.stderrTail())
				err := fmt.Errorf("%s", msg)
				s.finish(ctx, false, msg, err)
				return err
			}
			if err := s.handleLine(ctx, line); err != nil {
				return err
			}
			if s.isFinished() {
				return nil
			}
		}
	}
}

// handleLine decodes one stream-json line and hands it to the mapper, then applies the
// turn-boundary logic (a queued injected turn keeps the session alive past the `result` message).
func (s *session) handleLine(ctx context.Context, line []byte) error {
	ev, err := ParseLine(line)
	if err != nil {
		return nil // tolerate a malformed line; the next one still parses
	}
	if ev.IsCompactBoundary {
		s.onCompactBoundary(ctx)
	}
	if s.mapper.Handle(ctx, ev) {
		// Turn boundary: charge the SHARED, cache-aware budget gate and evaluate the ladder BEFORE the
		// boundary is applied.
		if reason := s.observeTurnBudget(ctx); reason != "" {
			s.abort()
			err := fmt.Errorf("%s", reason)
			s.finish(ctx, false, reason, err)
			return err
		}
		s.maybeTurnCountCompact(ctx)
		s.onTerminal(ctx, ev)
	}
	return nil
}

// onCompactBoundary observes a compaction boundary reported by the child (Claude Code's own
// auto-compact, or the boundary that follows a compact directive turn).
func (s *session) onCompactBoundary(ctx context.Context) {
	s.mapper.stall.sawOutput(time.Now())
	s.recordPart(ctx, db.SessionPartCompacted, map[string]any{"source": "claude_auto_compact"})
}

// onTerminal applies the turn boundary: a mid-run injected turn is still
// outstanding, so the session stays alive; otherwise the execution finishes.
func (s *session) onTerminal(ctx context.Context, ev StreamEvent) {
	s.mu.Lock()
	queued := s.queued
	if queued > 0 {
		s.queued--
	}
	s.mu.Unlock()
	if queued > 0 {
		return // a mid-run injected turn is waiting; keep the session alive
	}
	ok := ev.TurnSucceeded()
	errMsg := ""
	if !ok {
		errMsg = fmt.Sprintf("claude turn ended with %s", ev.Subtype)
	}
	s.finish(ctx, ok, errMsg, nil)
}

// bindSession claims the claude session id for this execution, enforcing the
// one-active-subprocess-per-session invariant.
func (s *session) bindSession(_ context.Context, sid string) {
	if !s.b.bindSessionID(sid, s.execID) {
		s.b.log.Warn("claude: a live subprocess already owns this session id — refusing the second writer",
			"session", sid, "execution", s.execID)
	}
}

// sessionIdentity is the claude session id this session is bound to (the id the CLI reported, else
// the resumed id).
func (s *session) sessionIdentity() string {
	if sid := s.mapper.sessionIDFor(); sid != "" {
		return sid
	}
	return s.resumeID
}

// SendTurn writes a follow-up user turn onto the live stdin and queues it so the run loop does not
// terminate at the next result boundary.
func (s *session) SendTurn(ctx context.Context, message string) error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("claude execution %s has no live session (already finished)", s.execID)
	}
	p := s.proc
	s.queued++
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("claude execution %s has no live subprocess", s.execID)
	}
	if err := p.WriteTurn(userTurnPayload(message)); err != nil {
		s.mu.Lock()
		s.queued--
		s.mu.Unlock()
		return fmt.Errorf("claude session stdin write: %w", err)
	}
	s.recordPart(ctx, db.SessionPartUserMessage, map[string]any{"text": message})
	return nil
}

// abort SIGINTs the child immediately and escalates to SIGTERM after a grace
// window. An unfinished turn stays resumable via --resume.
func (s *session) abort() {
	s.mu.Lock()
	if s.aborted || s.finished {
		s.mu.Unlock()
		return
	}
	s.aborted = true
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return
	}
	_ = p.Signal("INT")
	grace := s.abortWindow
	go func() {
		select {
		case <-time.After(grace):
			_ = p.Signal("TERM")
		case <-time.After(grace * 4):
		}
	}()
}

// finish emits the terminal callbacks exactly once.
func (s *session) finish(ctx context.Context, ok bool, errMsg string, retErr error) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	out := s.mapper.Output()
	s.mu.Unlock()
	if retErr != nil && errMsg == "" {
		errMsg = retErr.Error()
	}
	// OnWrittenFiles was already emitted incrementally as file-writing tool
	// calls streamed in (addFiles); the terminal callbacks are emitted here.
	s.cbs.OnResult(ctx, s.execID, ok, out, errMsg)
}

func (s *session) isFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// stderrTail renders the LAST stderr lines the CLI emitted, for a failure message.
func (s *session) stderrTail() string {
	if s.stderrRing == nil {
		return ""
	}
	lines := s.stderrRing.last()
	if len(lines) == 0 {
		return "; the CLI emitted no stderr at all before exiting"
	}
	return "; stderr: " + strings.Join(lines, " ")
}

// argv builds the claude command line.
func (s *session) argv() []string {
	argv := []string{
		// NOT the bare name on the HOST: see binary.go.
		ClaudeBinaryFor(s.inContainer()), "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if strings.TrimSpace(s.model) != "" {
		argv = append(argv, "--model", s.model)
	}
	if rid := strings.TrimSpace(s.resumeID); rid != "" {
		// ALWAYS RESUME, WITHOUT A DISK CHECK — deliberately, and this is NOT the same rule as the Ask path
		// (ask.go), for two reasons:
		argv = append(argv, "--resume", rid)
	}
	// A policy LOAD failure is recorded, never silently dropped.
	args, err := PermissionArgs(s.permissionOptions())
	if err != nil {
		slog.Default().Warn("claude: the operator permission policy could not be loaded — launching with the never-allow class, the project boundary and the fail-closed hook, but without its deny entries", "error", err)
	}
	argv = append(argv, args...)
	return append(argv, s.mcpArgs()...)
}

// mcpArgs registers the Orchicon MCP servers a WORKER execution gets.
func (s *session) mcpArgs() []string {
	inContainer := s.inContainer()
	builtin := OrchiconMCPServer(
		HookBinaryFor(inContainer),
		s.tenantID,
		workerMCPExtraEnv(inContainer, s.manifest.RuntimeWorkflowID),
	)
	// The SAME resolution the native bridge runs, at the SAME scope: the WORKER scope — the project's
	// owned definitions ∪ THE EXECUTING VERSION's inline specs.
	ref := mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeWorker,
		ProjectID:      s.manifest.ProjectID,
		WorkerID:       s.manifest.WorkerID,
		Version:        s.manifest.WorkerVersion,
		OwnPermissions: s.manifest.Permissions,
	}
	res, rerr := s.b.resolveMCP(context.Background(), s.tenantID, ref)
	var servers []MCPServer
	err := rerr
	if rerr == nil {
		servers, err = s.b.renderMCP(context.Background(), s.tenantID, res, builtin)
	}
	provenance := mcpclient.ProvenanceString(res.Servers)
	if err != nil {
		// Fail the launch rather than run a worker without the MCP servers it was configured to have, which
		// is the native bridge's contract too.
		slog.Default().Warn("claude: MCP resolution failed — the worker will run without its configured MCP servers",
			"execution", s.execID, "error", mcpclient.DescribeFailedServer(err, res.Servers))
		servers = []MCPServer{builtin}
		// The registered set is the built-in alone; do not report provenance for
		// servers this session did NOT get.
		provenance = ""
	}
	logMCPResolution("worker", servers, provenance)
	return MCPArgs(servers)
}

// workerMCPExtraEnv is the transport-dependent part of a worker's MCP registration, split out so it
// is testable without a runtime client (which is what `inContainer` otherwise needs to establish).
func workerMCPExtraEnv(inContainer bool, workflowRunID string) map[string]string {
	extra := map[string]string{}
	if inContainer {
		// NOT inherited: the plane's ORCHICON_POSTGRES_DSN names the HOST's
		// Postgres, which inside the container is nothing.
		extra["ORCHICON_POSTGRES_DSN"] = runtime.SandboxPostgresDSN
	}
	if runID := strings.TrimSpace(workflowRunID); runID != "" {
		extra[MCPWorkflowRunEnv] = runID
	}
	return extra
}

// permissionOptions is the launch-time restriction input for this session.
func (s *session) permissionOptions() PermissionOptions {
	return PermissionOptions{
		ProjectDir:  s.manifest.ProjectDir,
		WorktreeDir: s.manifest.WorktreePath,
		HookBinary:  s.hookBinary(),
		PolicyPath:  permpolicy.DefaultPath(),
	}
}

// inContainer reports whether this session's child runs inside the run's runtime container — the
// same predicate Bridge.isContainer uses to pick the transport.
func (s *session) inContainer() bool {
	return s.b != nil && s.b.isContainer(s.manifest)
}

// hookBinary is the binary the PreToolUse hook command invokes for this session's transport: the
// in-container bind mount when the child runs inside the run's container.
func (s *session) hookBinary() string { return HookBinaryFor(s.inContainer()) }

// childEnv builds the environment the claude child (and every process IT spawns) inherits, and
// returns the cleanup that must run when the session ends.
func (s *session) childEnv() ([]string, func()) {
	env := os.Environ()
	env = setEnvVar(env, ProjectDirEnv, executionDir(s.manifest))
	// Opt the session into claude's todo/task-tracking tool family.
	env = setEnvVar(env, TodoToolsEnv, "1")
	// The hook binary follows the TRANSPORT, not this process: inside the run's container only the
	// daemon's bind mount exists.
	env = setEnvVar(env, HookBinEnv, s.hookBinary())
	cleanup := func() {}
	if s.inContainer() {
		return env, cleanup
	}
	g, err := guard.NewExecutionGuard(s.manifest.ProjectDir)
	if err != nil {
		slog.Default().Warn("claude: the OS-level execution guard could not be built for the local transport — the child runs without the PATH shim", "error", err)
		return env, cleanup
	}
	return g.Apply(env), g.Close
}

// setEnvVar replaces (or appends) one KEY=value entry, preserving the rest of
// the environment verbatim.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}

// initialTurnPayload is the first user turn: system prompt + goal + acceptance criteria in ONE text
// block (claude -p stream-json input carries no separate system field).
func (s *session) initialTurnPayload() []byte {
	return userTurnPayload(composeInitialPrompt(s.manifest))
}

func (s *session) recordPart(ctx context.Context, kind string, payload map[string]any) {
	if s.b.sessionStore == nil {
		return
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	body, _ := json.Marshal(payload)
	part := db.SessionPart{
		ExecutionID: s.execID,
		TenantID:    s.tenantID,
		Seq:         seq,
		Kind:        kind,
		Payload:     body,
		CreatedAt:   time.Now(),
	}
	if err := s.b.sessionStore(ctx, s.execID, s.tenantID, []db.SessionPart{part}); err != nil {
		s.b.log.Warn("claude: record session part failed", "execution", s.execID, "kind", kind, "error", err)
	}
}

// executionDir resolves the worker's working directory: the run's worktree
// when provisioned, else the project dir (identical to the opencode rule).
func executionDir(m scheduler.ExecutionManifest) string {
	if m.WorktreePath != "" {
		return m.WorktreePath
	}
	return m.ProjectDir
}

// modelForRef resolves the CLI --model value from a model_ref, preferring the
// parsed model segment and falling back to the raw ref.
func modelForRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if mr, err := parseModelRefLoose(ref); err == nil && mr != "" {
		return mr
	}
	return ref
}

// composeInitialPrompt folds the system prompt, goal and acceptance criteria
// into the single text block of the first user turn.
func composeInitialPrompt(m scheduler.ExecutionManifest) string {
	var b strings.Builder
	if s := strings.TrimSpace(m.SystemPrompt); s != "" {
		b.WriteString("=== SYSTEM ===\n")
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if g := strings.TrimSpace(m.Goal); g != "" {
		b.WriteString("=== GOAL ===\n")
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	if a := strings.TrimSpace(m.AcceptanceCriteria); a != "" {
		b.WriteString("=== ACCEPTANCE CRITERIA ===\n")
		b.WriteString(a)
		b.WriteString("\n")
	}
	return b.String()
}

// userTurnPayload builds one stdin user-turn frame carrying a single text block.
func userTurnPayload(text string) []byte {
	return userTurnPayloadContent([]map[string]any{{"type": "text", "text": text}})
}

// userTurnPayloadContent builds one stdin user-turn frame from an explicit content-block list.
func userTurnPayloadContent(content []map[string]any) []byte {
	body := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	out, _ := json.Marshal(body)
	return out
}

func marshalAny(v map[string]any) []byte {
	if v == nil {
		return nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}
