package claude

// ask.go — the Ask (interactive) transport: ChatTurnClient on *Bridge.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the claude bridge satisfies the Ask chat-session capability.
var (
	_ scheduler.ChatTurnClient     = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind   = (*Bridge)(nil)
	_ scheduler.ChatToolRestrictor = (*Bridge)(nil)
	// The attachment-aware sender is a SECOND, independent assertion: an Ask turn carrying an image
	// type-asserts this off the bridge, and without it the turn fails at dispatch.
	_ scheduler.SendTurnMessageWithAttachments = (*Bridge)(nil)
)

// askBusCapacity is the per-conversation event buffer.
const askBusCapacity = 256

// AskRootEnv overrides where Ask conversations run.
const AskRootEnv = "ORCHICON_CLAUDE_ASK_ROOT"

// DefaultAskRoot is the stable per-plane directory Ask conversations run in.
func DefaultAskRoot() string {
	if v := strings.TrimSpace(os.Getenv(AskRootEnv)); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "orchicon-ask")
}

// askBus is one conversation's SessionBus.
type askBus struct {
	events chan scheduler.SessionEvent
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
}

func newAskBus() *askBus {
	return &askBus{events: make(chan scheduler.SessionEvent, askBusCapacity), done: make(chan struct{})}
}

func (b *askBus) Events() <-chan scheduler.SessionEvent { return b.events }
func (b *askBus) Done() <-chan struct{}                 { return b.done }

func (b *askBus) Close() {
	b.once.Do(func() {
		close(b.done)
		b.mu.Lock()
		close(b.events)
		b.mu.Unlock()
	})
}

// emit delivers one event.
func (b *askBus) emit(evt scheduler.SessionEvent) {
	terminal := evt.Kind == "idle" || evt.Kind == "error"

	b.mu.RLock()
	defer b.mu.RUnlock()

	select {
	case <-b.done:
		return // closed: this turn was superseded and has no consumer
	default:
	}

	if terminal {
		select {
		case b.events <- evt:
		case <-b.done:
		}
		return
	}
	select {
	case b.events <- evt:
	default:
	}
}

// askToolCall remembers what a tool_use asked for, so its later tool_result can name the tool and
// carry the ORIGINAL arguments.
type askToolCall struct {
	name  string
	input map[string]any
}

// askSession is one live Ask conversation.
type askSession struct {
	b      *Bridge
	convID string
	askDir string

	mu    sync.Mutex
	sid   string // the claude session id (we choose it: see CreateConversationSession)
	model string
	proc  ProcSession
	bus   *askBus
	guard *guard.Guard // the OS-level execution shim on this session's PATH
	// modeFile is this conversation's Ask mode file (see modegate.go). The hook
	// reads it per tool call; this session rewrites it per TURN.
	modeFile string
	// tenantID scopes the built-in Orchicon MCP sidecar (see mcpconfig.go).
	tenantID string
	// projectID is the PROJECT the conversation belongs to, captured from the turn's stamped scope
	// (askmode.ConversationScope) beside tenantID.
	projectID string
	// mcpFingerprint is the resolved MCP set at SPAWN, together with whether the mode could act when it
	// was built.
	mcpFingerprint string
	// mode is the turn's Ask mode, captured from the SAME stamp the hook's mode file is written from.
	mode    string
	pending map[string]struct{}    // open can_use_tool request ids
	tools   map[string]askToolCall // tool_use_id -> the call it resolved
	seeded  bool                   // whether the system prompt has been sent

	// liveCtx/liveCancel own the CHILD's lifetime, which is NOT the turn's.
	liveCtx    context.Context
	liveCancel context.CancelFunc

	// alive reports whether the read loop is still running for `proc`.
	alive bool

	// --- the consent socket (consent_hook.go) -------------------------------------------------
	//
	// hookLn is this session's PermissionRequest-hook listener: the channel a BLOCKING hook uses to raise a
	// card and wait for the operator's answer. hookWaits holds the asks parked on it, by ask id, so
	// ReplyPermissionDecision can hand a decision to a HOOK instead of writing a can_use_tool frame for a
	// request id the CLI never issued. A SEPARATE mutex, because both are touched from the socket's own
	// goroutines while a turn is running.
	hookMu     sync.Mutex
	hookLn     net.Listener
	hookWaits  map[string]*hookAsk
	hookAskSeq int64
}

func newAskSession(b *Bridge, convID, askDir string) *askSession {
	return &askSession{
		b:        b,
		convID:   convID,
		askDir:   askDir,
		modeFile: askModeFilePath(filepath.Dir(askDir), convID),
		pending:  make(map[string]struct{}),
		tools:    make(map[string]askToolCall),
	}
}

// --- the Bridge surface -----------------------------------------------------

// Subscribe implements scheduler.ChatTurnClient: a fresh bus for the conversation's next turn,
// registered on the session so the read loop feeds it.
func (b *Bridge) Subscribe(_ context.Context, conversationID string) (scheduler.SessionBus, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, errors.New("claude ask: Subscribe requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	s.mu.Lock()
	old := s.bus
	bus := newAskBus()
	s.bus = bus
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return bus, nil
}

// CreateConversationSession implements scheduler.ChatTurnClient.
func (b *Bridge) CreateConversationSession(_ context.Context, conversationID, _ string) (string, error) {
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("claude ask: CreateConversationSession requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	s.teardown()

	sid := uuid.NewString()
	s.mu.Lock()
	s.sid = sid
	s.seeded = false
	s.mu.Unlock()

	b.bindAskSessionID(sid, s)
	return sid, nil
}

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers TRUE: a turn written to the live child
// while it is generating is QUEUED by the CLI and answered at that turn's boundary.
//
// THE EVIDENCE IS ALREADY IN THIS PACKAGE, on the execution path, and it is why this adapter is not treated
// like the native bridge. session.SendTurn writes onto the live child's stdin mid-run and increments a `queued`
// counter expressly so the run loop does NOT terminate at the next `result` boundary:
//
//	// SendTurn writes a follow-up user turn onto the live stdin and queues it so
//	// the run loop does not terminate at the next result boundary.
//	...
//	// onTerminal applies the turn boundary: a mid-run injected turn is still
//	// outstanding, so the session stays alive; otherwise the execution finishes.
//
// Same binary, same stream-json protocol on the Ask path (askSession.writeUserTurn), so an interjection written
// mid-turn is held by the CLI rather than starting a competing turn — the property this declaration exists to
// advertise.
//
// IT WAS UNDECLARED UNTIL IT WAS ASKED ABOUT ("did you cross check against the claude code adapter as well?"),
// and falling to the default meant interjections on a claude conversation were held until the WHOLE turn ended
// instead of folded in at the next boundary — the exact behaviour the operator asked for, silently downgraded.
//
// WHAT "THE BOUNDARY" MEANS HERE, AND WHY IT IS NOT THE OPENCODE CASE. TRUE means "the message joins the
// running turn rather than starting a competing one", and nothing more. On opencode the equivalent is
// `delivery: "steer"`, which runs the input at the NEXT STEP BOUNDARY of the running turn (see
// opencode.apiShape.promptStyle) — the model reads a mid-turn note one tool round later. Here the queue is
// drained by onTerminal, which fires on the stream's `result` event: the END OF THE ASSISTANT'S TURN. The note
// then runs as a NEW turn in the same session. So the operator's words are answered without re-sending, but
// AFTER the current turn completes rather than during it.
//
// THAT IS A STRUCTURAL LIMIT, NOT AN OVERSIGHT, and it is worth stating because it is the one adapter where
// the answer is "cannot" rather than "not yet". The CLI owns its generate loop; we write a user turn to its
// stdin and IT decides when to read it. There is no route into its context mid-step, because:
//
//   - the control protocol carries permission traffic only. Its subtypes, read out of the installed binary,
//     are `initialize`, `can_use_tool`, `set_permission_mode` and `mcp_message` — there is NO user-text
//     injection subtype. The mid-turn channel we already speak (control.go) exists to answer tool calls, not
//     to carry content.
//   - the binary does have `send_now` / `cancel_queued`, and both are paired with an interrupt. That is the
//     TUI's "interrupt and send now": it reaches the model sooner by ABORTING the current turn — the exact
//     behaviour removed in 2e4043ac7, because an aborted turn leaves its tool results and file changes behind
//     without the assistant message that produced them, and the model then inherits finished work it has no
//     record of doing ("seems to think the current work may have been done by someone else").
//
// So this adapter has two options and no third: NO ABORT (a turn-boundary queue, what we do) or SEEN SOONER
// (an abort we deliberately do not do). The native bridge can do better only because WE own its loop and
// drain our own queue before each provider call; claude's loop is not ours to drain.
//
// The consequence for an operator: on a claude Ask conversation, an interjection is saved and answered, but
// you should not expect the model to react to it mid-turn. If that matters for a given conversation, the
// opencode or native adapters are the ones that steer.
func (b *Bridge) QueuesPrompts() bool { return true }

// SendTurnMessage implements scheduler.ChatTurnClient: append a user turn to the
// conversation's live child, spawning it on the first message.
func (b *Bridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	s, err := b.prepareAskTurn(ctx, conversationID, sessionID, modelRef)
	if err != nil {
		return err
	}
	return s.writeUserTurn(system, []map[string]any{{"type": "text", "text": text}})
}

// SendTurnMessageWithAttachments implements scheduler.SendTurnMessageWithAttachments, the OPTIONAL
// attachment-aware sender for Ask turns: the same turn.
func (b *Bridge) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	content, err := askUserContent(text, attachments)
	if err != nil {
		return err
	}
	s, err := b.prepareAskTurn(ctx, conversationID, sessionID, modelRef)
	if err != nil {
		return err
	}
	return s.writeUserTurn(system, content)
}

// prepareAskTurn is everything the two senders do BEFORE writing the turn: resolve and bind the
// conversation's session, record the mode boundary, refresh the tenant/project scope.
func (b *Bridge) prepareAskTurn(ctx context.Context, conversationID, sessionID, modelRef string) (*askSession, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, errors.New("claude ask: SendTurnMessage requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)

	s.mu.Lock()
	if strings.TrimSpace(sessionID) != "" && sessionID != s.sid {
		// The collector persisted a session id we did not create (a recreate after ErrSessionNotFound, or
		// an adapter switch).
		s.sid = sessionID
	}
	if strings.TrimSpace(modelRef) != "" {
		s.model = modelForRef(modelRef)
	}
	s.mu.Unlock()
	b.bindAskSessionID(sessionID, s)

	// The mode boundary is applied HERE, at the turn, because this is the one place in the adapter that
	// knows BOTH the conversation (conversationID) and the turn's mode (stamped on ctx by askorchicon.
	if err := writeAskModeFile(s.modeFile, askmode.ModeFromContext(ctx)); err != nil {
		slog.Default().Warn("claude ask: could not record the turn's mode — the mode boundary is prompt-level for this turn",
			"error", err, "conversation", conversationID, "mode", askmode.ModeFromContext(ctx))
	}

	// The turn's MODE, recorded on the session for the same reason as the tenant and the project:
	// argv() is built at spawn, and it must know whether this mode may OFFER an opaque MCP server.
	mode := askmode.ModeFromContext(ctx)
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()

	// The tenant the Orchicon MCP sidecar is scoped to. Captured here because the
	// turn's context is where it lives, and the child's argv is fixed at spawn.
	if tid := tenant.FromContext(ctx); strings.TrimSpace(tid) != "" {
		s.mu.Lock()
		s.tenantID = tid
		s.mu.Unlock()
	}
	// The conversation's PROJECT, from the SAME stamped scope the native adapter reads
	// (askmode.ConversationScope, stamped by askorchicon before dispatch).
	if pid := askmode.ConversationScopeFromContext(ctx).ProjectID; strings.TrimSpace(pid) != "" {
		s.mu.Lock()
		changed := s.projectID != pid
		s.projectID = pid
		s.mu.Unlock()
		if changed {
			// THE CHILD'S MCP SET IS FIXED AT SPAWN.
			slog.Default().Info("claude ask: the conversation's project changed — respawning to apply its MCP set",
				"conversation", conversationID, "project", pid)
			s.teardown()
		}
	}
	// AND A CHANGE THAT DID NOT COME FROM THE PROJECT: a server added to the conversation (or the
	// project) since the live child was launched.
	if s.sessionMCPStale() {
		slog.Default().Info("claude ask: the conversation's MCP set changed — respawning the child to apply it",
			"conversation", conversationID)
		s.teardown()
	}

	if err := s.ensureRunning(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// AbortConversationSession implements scheduler.ChatTurnClient.
func (b *Bridge) AbortConversationSession(_ context.Context, sessionID string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return nil
	}
	s.abort()
	return nil
}

// What this method buys is the honest answer to "is this turn enforced?" — without it the platform
// would report claude's mode boundary as PROSE ONLY while it is, in fact, enforced.
func (b *Bridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error {
	return nil
}

// ReplyPermission implements scheduler.ChatTurnClient by approving the ask. The
// grants live in the caller's store, never here.
func (b *Bridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: turn the operator's choice into the
// control_response frame the CLI is parked on.
func (b *Bridge) ReplyPermissionDecision(_ context.Context, sessionID, permissionID, decision string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return fmt.Errorf("claude ask: no live session for %q (the ask cannot be answered)", sessionID)
	}
	if strings.TrimSpace(permissionID) == "" {
		return errors.New("claude ask: an empty permission id cannot be correlated to a pending ask")
	}

	// A HOOK ASK IS NOT A CONTROL REQUEST.
	if w := s.takeHookWait(permissionID); w != nil {
		// Buffered(1) at the wait, so this never blocks even if the waiter has already given up.
		w.decision <- decision
		return nil
	}

	var frame []byte
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "reject", "deny", "denied":
		frame = DenyResponse(permissionID, "refused by the operator", DecisionUserReject)
	case "always", "permanent", "user_permanent":
		frame = AllowResponse(permissionID, DecisionUserPermanent)
	default:
		// "once" / "approve" / "allow" — the one-shot approval.
		frame = AllowResponse(permissionID, DecisionUserTemporary)
	}
	if err := s.writeControl(frame); err != nil {
		return err
	}
	s.forgetAsk(permissionID)
	return nil
}

// --- session registry -------------------------------------------------------

func (b *Bridge) ensureAskSession(convID string) *askSession {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	if b.askByConv == nil {
		b.askByConv = make(map[string]*askSession)
	}
	s := b.askByConv[convID]
	if s == nil {
		s = newAskSession(b, convID, filepath.Join(b.askRoot(), convID))
		b.askByConv[convID] = s
	}
	return s
}

func (b *Bridge) bindAskSessionID(sid string, s *askSession) {
	if strings.TrimSpace(sid) == "" {
		return
	}
	b.askMu.Lock()
	if b.askBySID == nil {
		b.askBySID = make(map[string]*askSession)
	}
	b.askBySID[sid] = s
	b.askMu.Unlock()
}

func (b *Bridge) askSessionForID(sid string) *askSession {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	return b.askBySID[sid]
}

// askRoot resolves the per-plane Ask directory (see DefaultAskRoot).
func (b *Bridge) askRoot() string {
	if b.askRootOverride != "" {
		return b.askRootOverride
	}
	return DefaultAskRoot()
}

// SetAskRoot overrides the per-plane Ask directory (the server injects the
// plane's own root; tests use a temp dir).
func (b *Bridge) SetAskRoot(dir string) { b.askRootOverride = strings.TrimSpace(dir) }

// SetScopeResolver wires the MCP scope resolver.
func (b *Bridge) SetScopeResolver(src mcpclient.ScopeResolver) { b.mcpResolver = src }

// SetMCPSecretResolver wires the ${SECRET_NAME} → plaintext resolver used just
// before the MCP config is rendered.
func (b *Bridge) SetMCPSecretResolver(r MCPSecretResolver) { b.mcpSecretResolver = r }

// --- lifecycle --------------------------------------------------------------

// ensureRunning spawns the conversation's child if it is not already live.
func (s *askSession) ensureRunning(_ context.Context) error {
	s.mu.Lock()
	if s.alive && s.proc != nil {
		s.mu.Unlock()
		return nil // a live child serves this turn
	}
	// NOT ALIVE: reap whatever is left and spawn fresh. See the `alive` field for
	// why proc != nil alone is not a reuse signal.
	stale := s.proc
	s.proc = nil
	if s.liveCtx == nil {
		// Session-scoped, NOT the turn's: the child outlives the turn.
		s.liveCtx, s.liveCancel = context.WithCancel(context.Background())
	}
	liveCtx := s.liveCtx
	// A fresh id if CreateConversationSession was never called (a direct
	// SendTurnMessage): the session identity must exist before the argv is built.
	if strings.TrimSpace(s.sid) == "" {
		s.sid = uuid.NewString()
	}
	sid := s.sid
	s.mu.Unlock()
	if stale != nil {
		slog.Default().Info("claude ask: retiring a child that is no longer serving this session", "conversation", s.convID, "session", sid)
		_ = stale.Close()
	}
	s.b.bindAskSessionID(sid, s)

	if err := os.MkdirAll(s.askDir, 0o755); err != nil {
		return fmt.Errorf("claude ask: create the ask directory: %w", err)
	}

	// THE CONSENT SOCKET MUST BE LISTENING BEFORE THE CHILD STARTS, because the child's
	// `PermissionRequest` hook connects to it the moment a gated tool needs an answer.
	if err := s.serveConsentSocket(); err != nil {
		slog.Default().Warn("claude ask: the consent socket could not be served — a gated call will be refused rather than carded",
			"conversation", s.convID, "error", err)
	}

	// Build the OS-level guard BEFORE the environment is assembled: the shim dir goes on the child's
	// PATH, and a failure here must degrade to a warning (guardless.
	s.ensureGuard()

	argv := s.argv()
	env := s.childEnv()
	spec := procSpec{
		ExecID:     "ask:" + s.convID,
		Argv:       argv,
		Cwd:        s.askDir,
		ProjectDir: s.askDir,
		Env:        env,
	}

	proc, err := s.b.spawnAsk(liveCtx, spec)
	if err != nil {
		return fmt.Errorf("claude ask: spawn: %w", err)
	}

	s.mu.Lock()
	s.proc = proc
	s.alive = true
	s.mu.Unlock()

	// LOGGED, because the ABSENCE of any line here is what made the operator's "thinking…" hang
	// invisible: the adapter had no spawn record, no exit record and no failure record.
	slog.Default().Info("claude ask: spawned the session child",
		"conversation", s.convID, "session", sid, "dir", s.askDir, "argv0", argv[0])

	go s.readLoop(liveCtx, proc)
	return nil
}

// argv is the launch shape.
func (s *askSession) argv() []string {
	s.mu.Lock()
	sid, model := s.sid, s.model
	s.mu.Unlock()

	argv := []string{
		// NOT the bare name: see binary.go. A PATH that reaches a system
		// install runs a CLI that rejects this argv outright.
		ClaudeBinaryPath(), "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	// THE FLAG FOLLOWS THE DISK, not which turn this is (see transcript.go).
	if sid != "" {
		if claudeSessionHasTranscript(sid) {
			argv = append(argv, "--resume", sid)
		} else {
			argv = append(argv, "--session-id", sid)
		}
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	args, err := PermissionArgs(PermissionOptions{
		// ProfileAskEnvValue is what selects the hook's interactive rule set and
		// the explicit `--permission-prompts host`.
		Profile:        ProfileAskEnvValue,
		HookBinary:     HookBinaryPath(),
		PolicyPath:     permpolicy.DefaultPath(),
		AdditionalDirs: []string{s.askDir},
	})
	if err != nil {
		slog.Default().Warn("claude ask: the operator permission policy could not be loaded — launching with the never-allow class and the interactive hook, but without its deny entries",
			"error", err)
	}
	argv = append(argv, args...)

	// THE ORCHICON TOOL SURFACE (mcpconfig.go).
	s.mu.Lock()
	convID, projID := s.convID, s.projectID
	projRef := mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID}
	s.mu.Unlock()

	// THE SIDECAR IS A CHILD PROCESS, so the conversation's scope has to travel in its ENVIRONMENT.
	// ONE construction of it (askMCPBuiltin), shared with the staleness comparison.
	builtin, berr := s.askMCPBuiltin(convID, projID)
	if berr != nil {
		slog.Default().Warn("claude ask: the built-in Orchicon MCP server could not be built", "error", berr)
	}
	res, rerr := s.b.resolveMCP(context.Background(), s.tenantID, projRef)
	var servers []MCPServer
	err = rerr
	if rerr == nil {
		servers, err = s.b.renderMCP(context.Background(), s.tenantID, res, builtin)
	}
	provenance := mcpclient.ProvenanceString(res.Servers)
	if err != nil {
		// A MISSING selection is fatal on purpose (an Ask session that silently lost a project's MCP
		// servers is a session that looks fine and cannot do its job).
		slog.Default().Warn("claude ask: MCP resolution failed — only the built-in Orchicon server will be registered", "error", err)
		servers = []MCPServer{builtin}
		provenance = ""
	}
	// THE OFFERED HALF OF THE MODE RULE.
	s.mu.Lock()
	mayAct := askmode.MayAct(s.mode)
	s.mu.Unlock()
	if !mayAct {
		servers = withholdOpaqueMCP(servers)
		if len(servers) < 2 && provenance != "" {
			// The operator's servers were withheld by the mode: the log must say so,
			// or the operator sees an MCP surface that quietly vanished.
			slog.Default().Info("claude ask: the mode may not act — the operator's MCP servers are not offered this turn",
				"conversation", s.convID, "mode", s.mode, "withheld", provenance)
		}
	}
	// Record the set's FINGERPRINT (WITH the may-act flag) so a later turn whose set or mode differs
	// can respawn and apply it (a live child holds its servers as fixed argv).
	//
	// OVER THE RENDERED SERVERS — the ones going into argv below — and not over res.Servers, which still
	// holds ${SECRET_NAME} references and therefore could not see a ROTATED SECRET. With servers withheld
	// by the mode, argv carries only the sidecar, so that is what is fingerprinted.
	fpServers := servers
	s.recordMCPFingerprint(mcpFingerprint(mayAct, fpServers))
	logMCPResolution("ask", servers, provenance)
	argv = append(argv, MCPArgs(servers)...)
	return argv
}

// ensureGuard builds the OS-level execution shim for this session.
func (s *askSession) ensureGuard() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.guard != nil {
		return
	}
	g, err := guard.NewExecutionGuard("")
	if err != nil {
		// Degrade to unguarded, matching the worker supervisor's warn-and-continue
		// contract: a temp-dir failure is transient and must not refuse the turn.
		slog.Default().Warn("claude ask: the OS-level execution guard could not be built — this Ask session runs without the PATH shim", "error", err)
		return
	}
	s.guard = g
}

// childEnv is the Ask child's environment. It carries the hook's profile and the
// ask directory, and the OS-level execution guard shim.
func (s *askSession) childEnv() []string {
	env := os.Environ()
	env = setEnvVar(env, HookProfileEnv, ProfileAskEnvValue)
	env = setEnvVar(env, AskDirEnv, s.askDir)
	// The hook falls back to this when the ask-dir var is absent.
	env = setEnvVar(env, ProjectDirEnv, s.askDir)
	// Same opt-in the worker session uses: without it the todo/task family is
	// absent from the tool registry on current models.
	env = setEnvVar(env, TodoToolsEnv, "1")
	// Ask is a HOST session, so the host binary path is correct here (unlike the
	// worker's container transport, which must use the daemon's bind mount).
	env = setEnvVar(env, HookBinEnv, HookBinaryPath())
	// The mode boundary (modegate.go): the hook reads this file per tool call, so
	// a mid-conversation mode switch is enforced on the very next call.
	env = setEnvVar(env, AskModeFileEnv, s.modeFile)
	// WHERE THE HOOK ASKS FOR A DECISION (consent_hook.go).
	env = setEnvVar(env, ConsentSockEnv, s.consentSockPath())

	// The OS-level guard, exactly as opencode's Ask serve applies it: the shim on PATH, and NOTHING
	// ELSE.
	if s.guard != nil {
		env = s.guard.Apply(env)
	}
	return env
}

// spawnAsk spawns the Ask child.
func (b *Bridge) spawnAsk(ctx context.Context, spec procSpec) (ProcSession, error) {
	if b.spawnOverride != nil {
		return b.spawnOverride(ctx, spec)
	}
	return newLocalProc(ctx, spec)
}

// writeUserTurn appends one user turn.
func (s *askSession) writeUserTurn(system string, content []map[string]any) error {
	s.mu.Lock()
	seeded := s.seeded
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return errors.New("claude ask: no live child to write to")
	}

	blocks := content
	if !seeded && strings.TrimSpace(system) != "" {
		blocks = prependSystemPrompt(content, "=== SYSTEM ===\n"+strings.TrimSpace(system)+"\n\n")
	}
	if err := proc.WriteTurn(userTurnPayloadContent(blocks)); err != nil {
		slog.Default().Warn("claude ask: could not write the turn to the child", "conversation", s.convID, "error", err)
		return fmt.Errorf("claude ask: write turn: %w", err)
	}
	slog.Default().Info("claude ask: turn written to the child", "conversation", s.convID, "seeded", !seeded, "blocks", len(blocks))
	s.mu.Lock()
	s.seeded = true
	s.mu.Unlock()
	return nil
}

// prependSystemPrompt folds prefix into the turn's leading text block, or inserts
// a text block ahead of the turn when it does not open with one.
func prependSystemPrompt(content []map[string]any, prefix string) []map[string]any {
	if len(content) > 0 && content[0]["type"] == "text" {
		if lead, ok := content[0]["text"].(string); ok {
			merged := make([]map[string]any, len(content))
			copy(merged, content)
			merged[0] = map[string]any{"type": "text", "text": prefix + lead}
			return merged
		}
	}
	out := make([]map[string]any, 0, len(content)+1)
	out = append(out, map[string]any{"type": "text", "text": prefix})
	return append(out, content...)
}

// The attachment caps are the native Ask transport's numbers (internal/orchicon/chatturn.go) —
// deliberately the SAME ones.
const (
	claudeAskMaxAttachments           = 5
	claudeAskMaxAttachmentBytes       = 10 * 1024 * 1024
	claudeAskMaxAttachmentsTotalBytes = 20 * 1024 * 1024
)

// claudeAskImageMediaTypes is the set of media types the CLI's Messages-API image block accepts,
// taken from the CLI's OWN schema, which documents the parameter as "image/jpeg, image/png.
var claudeAskImageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// askUserContent builds the content-block list for one Ask turn: the text (when non-empty), then
// one block per attachment — an `image` block for a media type the CLI accepts.
func askUserContent(text string, attachments []scheduler.ChatAttachment) ([]map[string]any, error) {
	var content []map[string]any
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if len(attachments) > claudeAskMaxAttachments {
		return nil, fmt.Errorf("claude ask: too many attachments (%d, max %d)", len(attachments), claudeAskMaxAttachments)
	}
	total := 0
	for _, a := range attachments {
		if len(a.Data) == 0 {
			continue
		}
		if len(a.Data) > claudeAskMaxAttachmentBytes {
			return nil, fmt.Errorf("claude ask: attachment %q too large (max 10MB)", a.Name)
		}
		total += len(a.Data)
		if total > claudeAskMaxAttachmentsTotalBytes {
			return nil, fmt.Errorf("claude ask: attachments too large (max 20MB total)")
		}
		mime := normalizeMediaType(a.MimeType)
		if mime == "" {
			mime = "application/octet-stream"
		}
		if claudeAskImageMediaTypes[mime] {
			// The CLI separates the media type from the bytes, so `data` is RAW
			// base64 — a `data:` URL here would be a malformed block.
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mime,
					"data":       base64.StdEncoding.EncodeToString(a.Data),
				},
			})
			continue
		}
		if utf8.Valid(a.Data) {
			name := a.Name
			if name == "" {
				name = "attachment"
			}
			fenced := "--- attachment: " + name + " (" + mime + ") ---\n" + string(a.Data)
			content = append(content, map[string]any{"type": "text", "text": fenced})
			continue
		}
		return nil, fmt.Errorf("claude ask: attachment %q (%s) is a binary document the claude Ask transport cannot carry — send it as text, or use an opencode-adapter model", a.Name, mime)
	}
	if len(content) == 0 {
		// A frame with an empty content array is not a well-formed turn.
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	return content, nil
}

// normalizeMediaType lowercases a media type and drops any parameters, so
// "IMAGE/PNG; charset=binary" still matches the accepted set.
func normalizeMediaType(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return strings.ToLower(strings.TrimSpace(mime))
}

// writeControl writes one control frame (a verdict for a parked ask).
func (s *askSession) writeControl(frame []byte) error {
	if len(frame) == 0 {
		return errors.New("claude ask: empty control frame")
	}
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return errors.New("claude ask: no live child to answer the ask on")
	}
	if err := proc.WriteTurn(frame); err != nil {
		return fmt.Errorf("claude ask: write control frame: %w", err)
	}
	return nil
}

// abort stops the live turn without destroying the conversation: SIGINT first (claude treats it as
// an interrupt and keeps the session resumable).
func (s *askSession) abort() {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return
	}
	_ = proc.Signal("INT")

	// Settle every parked ask: the CLI documents a cancel as the way an in-flight can_use_tool is
	// retired, and a card left on screen after the turn is gone is exactly what the operator reported.
	s.mu.Lock()
	open := make([]string, 0, len(s.pending))
	for id := range s.pending {
		open = append(open, id)
	}
	s.pending = make(map[string]struct{})
	s.mu.Unlock()
	for _, id := range open {
		_ = s.writeControl(CancelResponse(id))
	}
}

// teardown retires the conversation's child entirely (a recreate).
func (s *askSession) teardown() {
	s.mu.Lock()
	proc, cancel := s.proc, s.liveCancel
	s.proc, s.liveCancel, s.alive = nil, nil, false
	s.liveCtx = nil
	bus := s.bus
	s.bus = nil
	s.tools = make(map[string]askToolCall)
	s.pending = make(map[string]struct{})
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if proc != nil {
		_ = proc.Close()
	}
	if bus != nil {
		bus.Close()
	}
	if s.guard != nil {
		// The shim dir is a temp directory; leaving it behind on every recreate
		// would litter /tmp for the life of the plane.
		s.guard.Close()
		s.guard = nil
	}
	// AND THE SOCKET GOES WITH THE SESSION.
	s.stopConsentSocket()
}

// CloseAsk tears down every Ask conversation. It is the shutdown seam the server
// calls so a plane restart does not leak children.
func (b *Bridge) CloseAsk() {
	b.askMu.Lock()
	sessions := make([]*askSession, 0, len(b.askByConv))
	for _, s := range b.askByConv {
		sessions = append(sessions, s)
	}
	b.askByConv = nil
	b.askBySID = nil
	b.askMu.Unlock()

	for _, s := range sessions {
		s.teardown()
	}
}

// --- the read loop and mapper ----------------------------------------------

// readLoop decodes the child's stdout lines until it ends, mapping each onto the
// session bus.
func (s *askSession) readLoop(ctx context.Context, proc ProcSession) {
	// THE DEFER IS THE FIX.
	defer func() {
		s.mu.Lock()
		if s.proc == proc {
			s.proc = nil
			s.alive = false
		}
		s.mu.Unlock()
	}()

	lines := proc.Lines()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				// The child ended.
				tail := s.stderrTail(proc)
				slog.Default().Warn("claude ask: the session child exited", "conversation", s.convID, "stderr", tail)
				msg := "claude ask: the session ended"
				if tail != "" {
					msg += " — " + tail
				}
				s.busEmit(scheduler.SessionEvent{Kind: "error", Text: msg})
				return
			}
			s.handleLine(ctx, line)
		}
	}
}

// stderrTail drains whatever the child wrote to stderr and returns a bounded tail of it, so a
// failure the CHILD explained can be reported rather than guessed at.
func (s *askSession) stderrTail(proc ProcSession) string {
	if proc == nil {
		return ""
	}
	ch := proc.Stderr()
	if ch == nil {
		return ""
	}
	var b strings.Builder
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return strings.TrimSpace(b.String())
			}
			if b.Len() < 512 {
				b.Write(line)
				b.WriteByte(' ')
			}
		case <-deadline:
			return strings.TrimSpace(b.String())
		}
	}
}

// handleLine maps one stream-json line onto SessionEvents.
func (s *askSession) handleLine(ctx context.Context, line []byte) {
	ev, err := ParseLine(line)
	if err != nil {
		return // a malformed line is not a reason to end a live turn
	}

	// The control protocol first: an ask must reach the consent layer before any
	// other interpretation of the same line.
	switch {
	case ev.IsControlCancel:
		s.forgetAsk(ev.ControlRequestID)
		return
	case ev.IsToolPermissionAsk():
		s.emitAsk(ev)
		return
	}

	switch ev.Type {
	case "system":
		if ev.SessionID != "" {
			s.mu.Lock()
			s.sid = ev.SessionID
			s.mu.Unlock()
			s.b.bindAskSessionID(ev.SessionID, s)
		}

	case "text_delta":
		s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "text", Text: ev.Text})

	case "thinking_delta":
		// REASONING, on its own type.
		s.busEmit(scheduler.SessionEvent{Kind: "delta", Type: "reasoning", IsReasoning: true, Text: ev.Text})

	case "assistant":
		// Reasoning FIRST: a single assistant message can carry both, and
		// thinking precedes the answer it produced.
		if ev.Reasoning != "" {
			s.busEmit(scheduler.SessionEvent{Kind: "part", Type: "reasoning", IsReasoning: true, Text: ev.Reasoning})
		}
		if ev.Text != "" {
			s.busEmit(scheduler.SessionEvent{Kind: "part", Type: "text", Text: ev.Text})
		}
		for _, tu := range ev.ToolUses {
			s.rememberTool(tu)
			s.busEmit(scheduler.SessionEvent{
				Kind: "part",
				Type: "tool_use",
				// The part shape the shared consumers read (askorchicon's ledger
				// and stall monitor, the TUI's stream): tool + state.input.
				Part: map[string]any{
					"tool":   tu.Name,
					"callID": tu.ID,
					"state":  map[string]any{"input": tu.Input},
				},
			})
		}

	case "user":
		for _, tr := range ev.ToolResults {
			call := s.takeTool(tr.ToolUseID)
			// THE DIFF LEDGER, FIRED WHERE THE FILE IS ON DISK.
			if !tr.IsError && s.b.askFileEdits != nil && call.name != "" {
				s.b.askFileEdits(ctx, s.convID, s.tenantID, s.askDir, call.name, call.input, tr.Content)
			}
			s.busEmit(scheduler.SessionEvent{
				Kind:       "tool_result",
				Type:       "tool_result",
				ToolCallID: tr.ToolUseID,
				ToolName:   call.name,
				ArgsJSON:   jsonString(call.input),
				Output:     tr.Content,
				IsError:    tr.IsError,
			})
		}

	case "result":
		// One turn is over. `idle` ends the collector's drain; an error result
		// ends it as a failure so the turn is not reported as a clean answer.
		if ev.IsError {
			s.busEmit(scheduler.SessionEvent{Kind: "error", Text: ev.ResultText})
		} else {
			s.busEmit(scheduler.SessionEvent{Kind: "idle"})
		}
		s.mu.Lock()
		s.pending = make(map[string]struct{})
		s.mu.Unlock()
	}
}

// emitAsk raises a consent event for a can_use_tool request.
func (s *askSession) emitAsk(ev StreamEvent) {
	s.mu.Lock()
	s.pending[ev.ControlRequestID] = struct{}{}
	s.mu.Unlock()

	cmd := stringInput(ev.ControlInput, "command")
	targets := askTargets(ev)

	s.busEmit(scheduler.SessionEvent{
		Kind:         "permission",
		PermissionID: ev.ControlRequestID,
		Tool:         ev.ControlToolName,
		Command:      cmd,
		Targets:      targets,
		InputJSON:    jsonString(ev.ControlInput),
	})
}

// askTargets lists the paths an ask names, from claude's own `blocked_path` and the call's
// arguments.
func askTargets(ev StreamEvent) []string {
	var out []string
	if p := strings.TrimSpace(ev.ControlBlockedPath); p != "" {
		out = append(out, p)
	}
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if v := stringInput(ev.ControlInput, k); v != "" && !containsString(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// jsonString renders a map as its JSON text (the InputJSON the consent layer
// parses), returning "" for an empty map so an absent input is not "{}".
func jsonString(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	out, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(out)
}

func (s *askSession) rememberTool(tu ToolUse) {
	if tu.ID == "" {
		return
	}
	s.mu.Lock()
	s.tools[tu.ID] = askToolCall{name: tu.Name, input: tu.Input}
	s.mu.Unlock()
}

func (s *askSession) takeTool(id string) askToolCall {
	if id == "" {
		return askToolCall{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.tools[id]
	delete(s.tools, id)
	return c
}

func (s *askSession) forgetAsk(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// busEmit publishes to the conversation's current bus, if any.
func (s *askSession) busEmit(evt scheduler.SessionEvent) {
	s.mu.Lock()
	bus := s.bus
	s.mu.Unlock()
	if bus == nil {
		return
	}
	bus.emit(evt)
}
