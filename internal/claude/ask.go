package claude

// ask.go — the Ask (interactive) transport: ChatTurnClient on *Bridge.
//
// An Ask conversation is a DIFFERENT session shape from a worker execution, and
// the difference is forced, not stylistic — the same reason opencode needs a
// second serve process (internal/opencode/servehost.go: NewAskHostServe):
//
//   - the permission profile rides the launch (`--settings` + the hook's rule
//     set), so one child cannot be both the worker sandbox and the interactive
//     profile;
//   - a worker session is anchored to a worktree and driven by a manifest; an Ask
//     conversation has neither.
//
// So this is a separate, long-lived child per CONVERSATION, running in a stable
// per-plane Ask directory (no project boundary; consent gates the action), with
// the interactive profile installed.
//
// Two deliberate non-choices, both to keep the reference behaviour:
//
//  1. NO OS-level guard shim. The shim enforces a PROJECT BOUNDARY, and an
//     interactive session deliberately has none — applying it would refuse, at
//     the binary level and with no card, exactly the commands the Ask profile
//     exists to ask about. The authority for this transport is the PreToolUse
//     hook (allow/deny/ask) plus the settings document's `permissions.deny`,
//     which is also what opencode's interactive profile relies on.
//  2. NO `SendTurnMessageWithAttachments`. An Ask turn carrying attachments
//     fails loudly rather than silently degrading to text-only (the documented
//     contract for an adapter that lacks the capability). Claude's stream-json
//     input does support image blocks, so this is a follow-up, not a blocker.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time proof that the claude bridge satisfies the Ask chat-session
// capability. Ask Orchicon type-asserts this off the Dispatcher-resolved bridge,
// and Dispatcher.ChatKinds() reads the same assertion — so these two lines are
// what make claude appear in the Ask picker's capable set.
var (
	_ scheduler.ChatTurnClient   = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind = (*Bridge)(nil)
)

// askBusCapacity is the per-conversation event buffer. Sized with headroom: the
// consumer drains continuously, and an oversized buffer costs a few KB while a
// too-small one costs a dropped event.
const askBusCapacity = 256

// AskRootEnv overrides where Ask conversations run.
const AskRootEnv = "ORCHICON_CLAUDE_ASK_ROOT"

// DefaultAskRoot is the stable per-plane directory Ask conversations run in.
//
// Deliberately NOT the repository and NOT either protected root
// (~/.orchicon, ~/.local/share/orchicon — see internal/protectedpath): the ask
// directory becomes the session's cwd and the home of its transcript under
// ~/.claude/projects/<encoded-cwd>/, so pointing it at the checkout would make
// the operator's source tree the session's project, and pointing it at a
// protected root would make the session's OWN working directory something the
// guard must refuse to let it clean up.
func DefaultAskRoot() string {
	if v := strings.TrimSpace(os.Getenv(AskRootEnv)); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "orchicon-ask")
}

// askBus is one conversation's SessionBus.
//
// The Close ordering is COPIED FROM THE NATIVE BRIDGE on purpose
// (internal/orchicon/chatturn.go), which documents why: closing the events
// channel while a sender is inside a send is a PANIC that takes the process
// down, and it was observed in production when a superseded turn's deferred
// Close raced a live emit. Closing done FIRST stops new sends starting, the
// write lock then waits out in-flight ones, and only then is events closed. The
// blocking terminal send selects on done as well, so a consumer that has gone
// away cannot deadlock the lock.
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

// emit delivers one event. Progress events are best-effort (a dropped delta
// costs a repaint); TERMINAL events (idle/error) are blocking, because an `idle`
// dropped on a full buffer leaves the collector waiting forever with no error
// anywhere — a wedged turn with nothing to diagnose.
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

// askToolCall remembers what a tool_use asked for, so its later tool_result can
// name the tool and carry the ORIGINAL arguments. claude's tool_result block
// carries only the tool_use_id and the output, so the correlation has to be kept
// here.
type askToolCall struct {
	name  string
	input map[string]any
}

// askSession is one live Ask conversation.
type askSession struct {
	b      *Bridge
	convID string
	askDir string

	mu      sync.Mutex
	sid     string // the claude session id (we choose it: see CreateConversationSession)
	model   string
	proc    ProcSession
	bus     *askBus
	pending map[string]struct{}    // open can_use_tool request ids
	tools   map[string]askToolCall // tool_use_id -> the call it resolved
	seeded  bool                   // whether the system prompt has been sent
	cancel  context.CancelFunc
	running bool
}

func newAskSession(b *Bridge, convID, askDir string) *askSession {
	return &askSession{
		b:       b,
		convID:  convID,
		askDir:  askDir,
		pending: make(map[string]struct{}),
		tools:   make(map[string]askToolCall),
	}
}

// --- the Bridge surface -----------------------------------------------------

// Subscribe implements scheduler.ChatTurnClient: a fresh bus for the
// conversation's next turn, registered on the session so the read loop feeds it.
// A previous bus is closed, which is the supersede signal — a turn whose bus was
// replaced has no consumer and must not sit waiting for one.
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
//
// The id is OURS to choose: claude accepts `--session-id <uuid>`, so the adapter
// knows the session identity before the child says anything, instead of scraping
// it out of an init event and racing the first turn. The conversation's existing
// child (if any) is torn down: this is called on the first message and after a
// lost session, and both mean "start over".
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

// SendTurnMessage implements scheduler.ChatTurnClient: append a user turn to the
// conversation's live child, spawning it on the first message.
func (b *Bridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("claude ask: SendTurnMessage requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)

	s.mu.Lock()
	if strings.TrimSpace(sessionID) != "" && sessionID != s.sid {
		// The collector persisted a session id we did not create (a recreate
		// after ErrSessionNotFound, or an adapter switch). Adopt it rather than
		// dispatching a turn to a child that does not hold it.
		s.sid = sessionID
	}
	if strings.TrimSpace(modelRef) != "" {
		s.model = modelForRef(modelRef)
	}
	s.mu.Unlock()
	b.bindAskSessionID(sessionID, s)

	if err := s.ensureRunning(ctx); err != nil {
		return err
	}
	return s.writeUserTurn(system, text)
}

// AbortConversationSession implements scheduler.ChatTurnClient. It is a safe
// no-op for an unknown session: abort is idempotent and the durable record is
// already the caller's concern.
func (b *Bridge) AbortConversationSession(_ context.Context, sessionID string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return nil
	}
	s.abort()
	return nil
}

// ReplyPermission implements scheduler.ChatTurnClient by approving the ask. The
// grants live in the caller's store, never here.
func (b *Bridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: turn the
// operator's choice into the control_response frame the CLI is parked on.
//
// The decision vocabulary is the consent core's ("once" | "reject", plus the
// standing forms), NOT claude's — the mapping to claude's `behavior` and
// `decisionClassification` happens here, so no caller has to learn the protocol.
func (b *Bridge) ReplyPermissionDecision(_ context.Context, sessionID, permissionID, decision string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return fmt.Errorf("claude ask: no live session for %q (the ask cannot be answered)", sessionID)
	}
	if strings.TrimSpace(permissionID) == "" {
		return errors.New("claude ask: an empty permission id cannot be correlated to a pending ask")
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

// --- lifecycle --------------------------------------------------------------

// ensureRunning spawns the conversation's child if it is not already live.
func (s *askSession) ensureRunning(parent context.Context) error {
	s.mu.Lock()
	if s.running && s.proc != nil {
		s.mu.Unlock()
		return nil
	}
	// A fresh id if CreateConversationSession was never called (a direct
	// SendTurnMessage): the session identity must exist before the argv is built.
	if strings.TrimSpace(s.sid) == "" {
		s.sid = uuid.NewString()
	}
	sid := s.sid
	s.mu.Unlock()
	s.b.bindAskSessionID(sid, s)

	if err := os.MkdirAll(s.askDir, 0o755); err != nil {
		return fmt.Errorf("claude ask: create the ask directory: %w", err)
	}

	argv := s.argv()
	env := s.childEnv()
	spec := procSpec{
		ExecID:     "ask:" + s.convID,
		Argv:       argv,
		Cwd:        s.askDir,
		ProjectDir: s.askDir,
		Env:        env,
	}

	ctx, cancel := context.WithCancel(parent)
	proc, err := s.b.spawnAsk(ctx, spec)
	if err != nil {
		cancel()
		return fmt.Errorf("claude ask: spawn: %w", err)
	}

	s.mu.Lock()
	s.proc = proc
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()

	go s.readLoop(ctx, proc)
	return nil
}

// argv is the launch shape. `--session-id` pins the identity; the Ask profile
// adds `--permission-prompts host`, which is what makes the CLI raise the
// can_use_tool frame the consent cards answer.
func (s *askSession) argv() []string {
	s.mu.Lock()
	sid, model := s.sid, s.model
	s.mu.Unlock()

	argv := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if sid != "" {
		argv = append(argv, "--session-id", sid)
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
	return append(argv, args...)
}

// childEnv is the Ask child's environment. It carries the hook's profile and the
// ask directory, and deliberately NO guard shim (see the file header).
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
	return env
}

// spawnAsk spawns the Ask child. Ask is host-only: an Ask conversation has no
// workflow run, so there is no run container to spawn into and the manifest-based
// selection in Bridge.spawn does not apply.
func (b *Bridge) spawnAsk(ctx context.Context, spec procSpec) (ProcSession, error) {
	if b.spawnOverride != nil {
		return b.spawnOverride(ctx, spec)
	}
	return newLocalProc(ctx, spec)
}

// writeUserTurn appends one user turn. The system prompt rides the FIRST turn
// only: claude's stream-json input carries no separate system field, and a live
// session already holds the earlier turns (mirroring the worker session).
func (s *askSession) writeUserTurn(system, text string) error {
	s.mu.Lock()
	seeded := s.seeded
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return errors.New("claude ask: no live child to write to")
	}

	body := text
	if !seeded && strings.TrimSpace(system) != "" {
		body = "=== SYSTEM ===\n" + strings.TrimSpace(system) + "\n\n" + text
	}
	if err := proc.WriteTurn(userTurnPayload(body)); err != nil {
		return fmt.Errorf("claude ask: write turn: %w", err)
	}
	s.mu.Lock()
	s.seeded = true
	s.mu.Unlock()
	return nil
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

// abort stops the live turn without destroying the conversation: SIGINT first
// (claude treats it as an interrupt and keeps the session resumable), then the
// caller's teardown handles a child that will not go.
func (s *askSession) abort() {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil {
		return
	}
	_ = proc.Signal("INT")

	// Settle every parked ask: the CLI documents a cancel as the way an
	// in-flight can_use_tool is retired, and a card left on screen after the turn
	// is gone is exactly what the operator reported as "still there".
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
	proc, cancel := s.proc, s.cancel
	s.proc, s.cancel, s.running = nil, nil, false
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
	lines := proc.Lines()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				// The child ended. Tell the consumer the turn is over, or the
				// collector waits forever on a session that no longer exists.
				s.busEmit(scheduler.SessionEvent{Kind: "error", Text: "claude ask: the session ended"})
				s.mu.Lock()
				s.running = false
				s.mu.Unlock()
				return
			}
			s.handleLine(line)
		}
	}
}

// handleLine maps one stream-json line onto SessionEvents.
//
// The vocabulary is the shared one (internal/scheduler SessionEvent), NOT
// claude's: deltas and parts for text, a typed tool_result for a resolved call,
// "permission" for an ask, and "idle" to end the turn. That mapping is what keeps
// the Ask surfaces adapter-agnostic.
func (s *askSession) handleLine(line []byte) {
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

	case "assistant":
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
//
// The TYPED fields are used rather than opencode's property vocabulary: the
// contract was extended for exactly this (bridge.go: "An adapter that can name
// the action it is asking about supplies it HERE, rather than shaping it into
// the property vocabulary the opencode adapter happens to emit").
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

// askTargets lists the paths an ask names, from claude's own `blocked_path` and
// the call's arguments. It prefers the field the CLI supplies and falls back to
// the path keys a path-carrying tool uses, so the consent card has a target to
// show wherever the CLI put it.
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
