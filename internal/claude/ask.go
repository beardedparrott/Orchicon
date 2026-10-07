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
//  2. `SendTurnMessageWithAttachments` IS implemented — the follow-up this note
//     used to defer. The CLI's stream-json input takes Messages-API content
//     blocks, so an attachment is an EXTRA BLOCK ON THE SAME FRAME rather than a
//     different transport: an `image` block for the media types the CLI's schema
//     accepts, a fenced text block for anything else that is valid UTF-8, and a
//     LOUD error for a binary document this transport cannot carry. Never a
//     silent drop to text-only — a turn that quietly loses an attachment and
//     reports success is the AC routing landmine.
//
//     The MIME routing is deliberately stricter than the native transport's,
//     which routes on the `image/` PREFIX: the CLI's image block accepts only
//     image/jpeg, image/png, image/gif and image/webp, so an SVG (or bmp, tiff,
//     avif) sent as an image block is a frame the CLI REFUSES — failing the whole
//     turn to deliver nothing. Those take the text path instead.
//
// CORRECTION: claim 1 was WRONG and is fixed below. Both other adapters DO shim
// the Ask path — opencode's host serve applies `guard.NewExecutionGuard("")`
// (internal/opencode/servehost.go, in startOnce, so BOTH profiles get it), and
// the native path builds the same shim in interactive mode
// (internal/askorchicon/ask_guard.go). Skipping it made claude the only adapter
// with no OS-level floor, which is LOOSER, not stricter — and it is the floor
// that carries the never-allow class, the protected roots and the operator
// policy against a subprocess ("a subprocess did it" is the hole the 2026-07-30
// /home wipe went through). The PreToolUse hook never sees those; the shim does.
//
// A second correction, to the fix itself: this file first built the shim SCOPED
// to the conversation's directory, reasoning that "one conversation, one
// directory" made that more precise. It does not. The shim permits absolute
// targets inside its project dir, so naming the ask dir WIDENED the sanctioned
// set rather than narrowing it — the opposite of parity. The scope is empty,
// as in both references. See ensureGuard()/childEnv().
//
// NEITHER LAYER CONTAINS THE SESSION, and nothing here should be read as
// claiming otherwise. The shim judges SUBPROCESSES; the hook judges claude's own
// tool calls. An absolute write is still an absolute write once the operator
// approves the card, and reads outside the ask dir are allowed. The ask
// directory is a cwd and a scope key, not a jail — see the profile table in
// hook.go for exactly what is allowed, asked and refused.

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

// Compile-time proof that the claude bridge satisfies the Ask chat-session
// capability. Ask Orchicon type-asserts this off the Dispatcher-resolved bridge,
// and Dispatcher.ChatKinds() reads the same assertion — so these two lines are
// what make claude appear in the Ask picker's capable set.
var (
	_ scheduler.ChatTurnClient     = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind   = (*Bridge)(nil)
	_ scheduler.ChatToolRestrictor = (*Bridge)(nil)
	// The attachment-aware sender is a SECOND, independent assertion: an Ask turn
	// carrying an image type-asserts this off the bridge, and without it the turn
	// fails at dispatch. A compile-time var is what keeps a signature drift from
	// silently dropping the capability.
	_ scheduler.SendTurnMessageWithAttachments = (*Bridge)(nil)
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

	mu    sync.Mutex
	sid   string // the claude session id (we choose it: see CreateConversationSession)
	model string
	proc  ProcSession
	bus   *askBus
	guard *guard.Guard // the OS-level execution shim on this session's PATH
	// modeFile is this conversation's Ask mode file (see modegate.go). The hook
	// reads it per tool call; this session rewrites it per TURN.
	modeFile string
	// tenantID scopes the built-in Orchicon MCP sidecar (see mcpconfig.go). It
	// arrives on the turn's context — the only place the adapter learns it. The
	// sidecar falls back to the dev tenant without it, so a session would read
	// the wrong tenant's work items.
	tenantID string
	// projectID is the PROJECT the conversation belongs to, captured from the turn's
	// stamped scope (askmode.ConversationScope) beside tenantID. It is one half of
	// the conversation's MCP scope: with s.convID it is what makes a conversation
	// receive its project's AND its own MCP servers instead of resolving with empty
	// ids (the defect this closes).
	projectID string
	// mcpFingerprint is the resolved MCP set at SPAWN, together with whether the
	// mode could act when it was built. The child's servers arrive as fixed
	// `--mcp-config` argv, so a set (or a mode's may-act) that CHANGES mid-conversation
	// is invisible to a live child; comparing this on the next turn is what triggers
	// the respawn that applies it (see SendTurnMessage).
	mcpFingerprint string
	// mode is the turn's Ask mode, captured from the SAME stamp the hook's mode file
	// is written from. It is kept on the session so argv() can decide whether an
	// OPAQUE MCP server may be OFFERED: a mode that may not act must not register the
	// operator's servers at all (the offered half of the mode rule), not merely deny
	// every call against them.
	mode    string
	pending map[string]struct{}    // open can_use_tool request ids
	tools   map[string]askToolCall // tool_use_id -> the call it resolved
	seeded  bool                   // whether the system prompt has been sent

	// liveCtx/liveCancel own the CHILD's lifetime, which is NOT the turn's.
	//
	// A conversation session is meant to outlive a turn — that is what makes it a
	// session rather than a request, and why its id can be resumed. Deriving the
	// child from the turn's context (as this first did) killed it the moment a
	// turn ended, so every turn paid a respawn and an aborted turn left the state
	// below describing a process that no longer existed. Only teardown() and
	// CloseAsk() cancel this.
	liveCtx    context.Context
	liveCancel context.CancelFunc

	// alive reports whether the read loop is still running for `proc`.
	//
	// IT IS NOT REDUNDANT WITH proc != nil, and conflating the two is the bug
	// this replaces: a child that EXITED left proc non-nil, so the next turn
	// "reused" a corpse — writing into a pipe nobody read, then waiting for a
	// reply that could never come. The operator's symptom was exactly that: a
	// permanent "thinking…", no child process, and nothing in the log.
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
	s, err := b.prepareAskTurn(ctx, conversationID, sessionID, modelRef)
	if err != nil {
		return err
	}
	return s.writeUserTurn(system, []map[string]any{{"type": "text", "text": text}})
}

// SendTurnMessageWithAttachments implements
// scheduler.SendTurnMessageWithAttachments, the OPTIONAL attachment-aware sender
// for Ask turns: the same turn, with the attachments appended as additional
// content blocks.
//
// It shares prepareAskTurn with SendTurnMessage rather than repeating it, so the
// two senders cannot drift on the mode boundary, the tenant/project scope or the
// argv-staleness respawn. A capability that ships while silently skipping one of
// those is the failure worth engineering against here: the CLI always accepted
// the image blocks, so the risk was never the wire — it was the second code path.
//
// The turn is validated BEFORE the session is touched, so an attachment this
// transport cannot carry is refused without spawning a child or rewriting the
// mode file.
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

// prepareAskTurn is everything the two senders do BEFORE writing the turn:
// resolve and bind the conversation's session, record the mode boundary, refresh
// the tenant/project scope, respawn a child whose argv-fixed scope has gone
// stale, and make sure a child is running. Both senders call it, so both get the
// same boundaries.
func (b *Bridge) prepareAskTurn(ctx context.Context, conversationID, sessionID, modelRef string) (*askSession, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, errors.New("claude ask: SendTurnMessage requires a conversation id")
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

	// The mode boundary is applied HERE, at the turn, because this is the one
	// place in the adapter that knows BOTH the conversation (conversationID) and
	// the turn's mode (stamped on ctx by askorchicon before dispatch). The mode is
	// not reachable from RestrictChatTools — its signature carries no conversation
	// — which is why claude's enforcement is here and that method is a
	// declaration, exactly as the native bridge's is.
	//
	// The write happens BEFORE the turn is written, so the first tool call of the
	// turn already sees the new policy. A failure is logged, not fatal: a missing
	// mode file reads as "no mode policy", so the turn degrades to the prompt-level
	// boundary rather than failing outright.
	if err := writeAskModeFile(s.modeFile, askmode.ModeFromContext(ctx)); err != nil {
		slog.Default().Warn("claude ask: could not record the turn's mode — the mode boundary is prompt-level for this turn",
			"error", err, "conversation", conversationID, "mode", askmode.ModeFromContext(ctx))
	}

	// The turn's MODE, recorded on the session for the same reason as the tenant and
	// the project: argv() is built at spawn, and it must know whether this mode may
	// OFFER an opaque MCP server. The mode file (written just above) carries the same
	// value to the hook for the ENFORCED half; this is the OFFERED half, and both read
	// the ONE table (internal/askmode). A mode switch that flips MayAct is picked up by
	// the respawn check below.
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
	// The conversation's PROJECT, from the SAME stamped scope the native adapter
	// reads (askmode.ConversationScope, stamped by askorchicon before dispatch). It
	// is the other half of the conversation MCP scope, and it arrives here for the
	// same reason the tenant does: the child's argv is fixed at spawn.
	if pid := askmode.ConversationScopeFromContext(ctx).ProjectID; strings.TrimSpace(pid) != "" {
		s.mu.Lock()
		changed := s.projectID != pid
		s.projectID = pid
		s.mu.Unlock()
		if changed {
			// THE CHILD'S MCP SET IS FIXED AT SPAWN. A project change means the
			// resolved servers may differ, so a live child is holding the wrong
			// `--mcp-config`. Retire it and let ensureRunning spawn fresh below —
			// the same recreate path a dead child takes.
			slog.Default().Info("claude ask: the conversation's project changed — respawning to apply its MCP set",
				"conversation", conversationID, "project", pid)
			s.teardown()
		}
	}
	// AND A CHANGE THAT DID NOT COME FROM THE PROJECT: a server added to the
	// conversation (or the project) since the live child was launched. The child's
	// servers ride fixed `--mcp-config` argv, so a differing resolution means it is
	// holding a stale set — respawn to apply the current one.
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

// RestrictChatTools implements scheduler.ChatToolRestrictor.
//
// A DECLARATION, like the native bridge's — and for a reason of the same shape.
// The native version is a no-op because the native provider IS the tool list and
// the tool executor, so askorchicon applies the policy at both points. claude's is
// a no-op because the method CANNOT do the work: its signature carries a context
// and a policy, no conversation, and this bridge serves many conversations
// concurrently — recording "the current mode" on the bridge would apply one
// conversation's mode to another's turns.
//
// The enforcement therefore lives at the one point that knows both the
// conversation and the mode: SendTurnMessage reads the mode off the ctx and
// records it for the HOOK to enforce per tool call (modegate.go). What this
// method buys is the honest answer to "is this turn enforced?" — without it the
// platform would report claude's mode boundary as PROSE ONLY while it is, in
// fact, enforced.
func (b *Bridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error {
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

	// A HOOK ASK IS NOT A CONTROL REQUEST. The CLI is not parked on a frame here — it is waiting on the
	// `PermissionRequest` hook, which is blocked on our socket (consent_hook.go). Writing a control_response
	// for an id the CLI never issued would answer nothing, and the hook would sit until its timeout with
	// the operator believing they had approved the call. So a parked hook ask is resolved FIRST, by handing
	// the decision back down the socket.
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

// SetScopeResolver wires the MCP scope resolver. Mirroring the native bridge's
// setter of the same name is deliberate: ONE resolution, one place the platform
// decides which servers an execution gets.
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
	// `PermissionRequest` hook connects to it the moment a gated tool needs an answer. A failure here is
	// logged and NOT fatal: without the socket the hook fails closed (it denies when it cannot reach us),
	// so the session still runs with the file/shell suite refusing rather than silently allowing.
	if err := s.serveConsentSocket(); err != nil {
		slog.Default().Warn("claude ask: the consent socket could not be served — a gated call will be refused rather than carded",
			"conversation", s.convID, "error", err)
	}

	// Build the OS-level guard BEFORE the environment is assembled: the shim dir
	// goes on the child's PATH, and a failure here must degrade to a warning
	// (guardless, like the worker supervisor does) rather than refuse the turn.
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

	// LOGGED, because the ABSENCE of any line here is what made the operator's
	// "thinking…" hang invisible: the adapter had no spawn record, no exit
	// record and no failure record, so nothing could distinguish "never started"
	// from "started and died" from "started and stayed silent".
	slog.Default().Info("claude ask: spawned the session child",
		"conversation", s.convID, "session", sid, "dir", s.askDir, "argv0", argv[0])

	go s.readLoop(liveCtx, proc)
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
		// NOT the bare name: see binary.go. A PATH that reaches a system
		// install runs a CLI that rejects this argv outright.
		ClaudeBinaryPath(), "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	// THE FLAG FOLLOWS THE DISK, not which turn this is (see transcript.go).
	//
	// `--session-id` CREATES and refuses an id that already exists ("Session ID
	// ... is already in use", exit 1, empty stdout); `--resume` CONTINUES and fails
	// when there is nothing to continue. Choosing by transcript existence is
	// therefore the only correct rule, and it also repairs the respawn case: a
	// child that died mid-conversation is not a new session, so it resumes.
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

	// THE ORCHICON TOOL SURFACE (mcpconfig.go). Without this the session has NO
	// orchicon_* tools: it cannot read its own conversation record, create a work
	// item, or drive a run — exactly what the operator's Opus 5 reported.
	//
	// The binary is this process's own executable, and the sidecar inherits our
	// environment (childEnv starts from os.Environ, so the plane's
	// ORCHICON_POSTGRES_DSN rides through), so its DB channel reaches the tenant
	// the conversation belongs to.
	// The Ask surface resolves the CONVERSATION scope (child 6 owns it): the
	// conversation's own owned definitions UNIONED with its project's, in ONE
	// owner-scoped read by the shared resolver. Resolving with empty ids (the old
	// `{Kind: ScopeProject}` with no ProjectID/ConversationID) meant a conversation's
	// project could never reach it — under the old model only the tenant default
	// applied, and there is no tenant tier any more, so the set was empty.
	s.mu.Lock()
	convID, projID := s.convID, s.projectID
	projRef := mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: projID, ConversationID: convID}
	s.mu.Unlock()

	// THE SIDECAR IS A CHILD PROCESS, so the conversation's scope has to travel in
	// its ENVIRONMENT. askmode.ConversationScope is a CONTEXT value, and a context
	// does not cross a stdio boundary — that is why an Orchicon tool in this session
	// used to answer "no conversation is stamped on this turn": get_current_conversation
	// is the one that fails LOUD on it, and it is the only way Quick Work has to read
	// the model_ref to offer before it pins one into a worker. The child is per
	// conversation and this argv is fixed at spawn, so the scope cannot go stale here.
	builtin := OrchiconMCPServer(HookBinaryPath(), s.tenantID, OrchiconMCPConversationEnv(convID, projID))
	res, rerr := s.b.resolveMCP(context.Background(), s.tenantID, projRef)
	var servers []MCPServer
	err = rerr
	if rerr == nil {
		servers, err = s.b.renderMCP(context.Background(), s.tenantID, res, builtin)
	}
	provenance := mcpclient.ProvenanceString(res.Servers)
	if err != nil {
		// A MISSING selection is fatal on purpose (an Ask session that silently
		// lost a project's MCP servers is a session that looks fine and cannot do
		// its job). argv() cannot return an error, so this is recorded and the
		// admin sees it; the built-in surface still registers.
		slog.Default().Warn("claude ask: MCP resolution failed — only the built-in Orchicon server will be registered", "error", err)
		servers = []MCPServer{builtin}
		provenance = ""
	}
	// THE OFFERED HALF OF THE MODE RULE. An operator's MCP server is OPAQUE to the
	// platform, so a mode that may not act must not be OFFERED its tools at all —
	// denying every call is the hook's job (the ENFORCED half), but advertising an
	// action the mode refuses would invite the model to try it. The platform's own
	// `orchicon` sidecar is CLASSIFIED by the table, so it stays registered in every
	// mode; only the operator's servers are withheld.
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
	// Record the set's FINGERPRINT (WITH the may-act flag) so a later turn whose set
	// or mode differs can respawn and apply it (a live child holds its servers as
	// fixed argv).
	s.recordMCPFingerprint(mcpFingerprint(mayAct, res.Servers))
	logMCPResolution("ask", servers, provenance)
	argv = append(argv, MCPArgs(servers)...)
	return argv
}

// ensureGuard builds the OS-level execution shim for this session.
//
// THE SCOPE IS EMPTY, exactly as both other adapters build it
// (guard.NewExecutionGuard("") in opencode's HostServe.startOnce and in
// askorchicon's ask_guard). An empty dir is not "no guard": it is the
// no-single-root mode, in which blocked_path refuses EVERY absolute target
// outside the Orchicon scratch dir and the operator's accept list.
//
// NAMING THE ASK DIR HERE WEAKENED IT, which is the bug this replaces. The shim
// allows an absolute target inside PROJECT_DIR (guard.go's blocked_path, allow
// arm 2), so baking the conversation's directory in made an absolute `rm`
// against that directory legitimate — where the other two adapters refuse it.
// The ask directory is a CWD and a scope key, NOT a containment boundary: it
// does not stop the session reaching the rest of the host, so treating it as a
// sanctioned absolute scope bought no safety and gave away the refusal. Cleanup
// is unaffected: RELATIVE targets are never blocked, so the session still
// manages its own working directory the ordinary way.
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
	// WHERE THE HOOK ASKS FOR A DECISION (consent_hook.go). The `PermissionRequest` hook is the only
	// surface that can answer a permission in this launch shape, and it is a SEPARATE PROCESS — so the
	// socket path travels on the environment exactly as the mode file does.
	env = setEnvVar(env, ConsentSockEnv, s.consentSockPath())

	// The OS-level guard, exactly as opencode's Ask serve applies it: the shim on
	// PATH, and NOTHING ELSE. No InteractiveEnviron, so the shim runs its default
	// (worker) path-scoped profile.
	//
	// THAT OMISSION IS THE PARITY, and it is also the safe choice:
	//
	//   - Nothing can WIDEN the sanctioned set. The interactive profile consults
	//     grants, once-targets, the operator policy and fullsend, all of which can
	//     allow a path the default profile refuses. This session wires none of
	//     them, so adding the profile would add only the ability to widen.
	//   - It avoids a fail-closed trap. In the interactive profile a policy file
	//     that is missing or unreadable makes the shim refuse EVERY path-scoped
	//     command ("fail-closed ... refusing rather than running it unguarded"),
	//     with no card and no way for the operator to see why. On a plane with no
	//     policy at permpolicy.DefaultPath() that would refuse every `rm`, `mv`,
	//     `cp` and `chmod` in every Ask conversation. The native path accepts that
	//     because its bash env is a per-call factory that always has a live policy
	//     to point at; a stdio child's environment is fixed at spawn.
	//
	// What remains in force is the part that matters and cannot be widened: the
	// never-allow class (refused on the binary name, before any argument is read)
	// and the protected roots (judged before every allow-set test).
	if s.guard != nil {
		env = s.guard.Apply(env)
	}
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
//
// The turn arrives as content BLOCKS rather than text, so an attachment is an
// extra block on the same frame (see askUserContent). The system prompt is folded
// into the turn's leading TEXT block when it has one, which keeps a text-only
// first turn byte-identical to what this transport wrote before attachments
// existed; a turn that opens with an attachment block gets the prompt as a
// prepended block instead, so the boundary is never lost and the attachment is
// never displaced or reordered.
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

// The attachment caps are the native Ask transport's numbers
// (internal/orchicon/chatturn.go) — deliberately the SAME ones. The caps are part
// of one adapter-neutral contract, and two different values would make "is this
// attachment accepted?" depend on which adapter a conversation happens to be
// pinned to. (The server also validates at dispatch; this is the adapter's own
// floor, and it exists so the adapter never silently truncates.)
const (
	claudeAskMaxAttachments           = 5
	claudeAskMaxAttachmentBytes       = 10 * 1024 * 1024
	claudeAskMaxAttachmentsTotalBytes = 20 * 1024 * 1024
)

// claudeAskImageMediaTypes is the set of media types the CLI's Messages-API image
// block accepts, taken from the CLI's OWN schema, which documents the parameter as
// "image/jpeg, image/png, image/gif or image/webp".
//
// This is NOT "any image/*", and the difference is load-bearing. The native Ask
// transport routes on the `image/` PREFIX; the CLI REFUSES an image block whose
// media type falls outside this set, so routing `image/svg+xml` (or bmp, tiff,
// avif) as an image block would hand the CLI a frame it rejects and fail the whole
// turn. Media types outside the set take the text path instead — which is also the
// more useful answer for the case that actually occurs, an SVG, whose markup the
// model can read directly.
var claudeAskImageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// askUserContent builds the content-block list for one Ask turn: the text (when
// non-empty), then one block per attachment — an `image` block for a media type
// the CLI accepts, a fenced text block for anything else that is valid UTF-8.
//
// A binary document this transport cannot carry is a LOUD error, never a silent
// drop: the caller's contract is that an attachment is either carried or refused,
// because a turn that quietly loses one and reports success is indistinguishable
// from one that was never given it.
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
	// AND THE SOCKET GOES WITH THE SESSION. Leaving it would let a later hook find a listener that
	// answers nothing (this session is retired), and the operator's card would never be raised for a
	// call they could still be asked about. Closing the listener also unblocks any wait parked on it
	// (the wait selects on liveCtx, which the lines above just cancelled).
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
	// THE DEFER IS THE FIX. Returning early on ctx.Done() used to leave the
	// session marked live with a `proc` that had already exited, so the next turn
	// reused a dead child and waited on it forever. However this loop ends — the
	// child exiting, or the session being torn down — the session must stop
	// claiming to be alive.
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
				// The child ended. Tell the consumer the turn is over, or the
				// collector waits forever on a session that no longer exists.
				//
				// THE STDERR TAIL IS THE POINT. A child that rejects our argv
				// (an older CLI: "unknown option '--permission-prompts'") writes
				// its complaint to stderr and NOTHING to stdout, so without this
				// the turn reports a bare "session ended" and the operator has no
				// way to learn why. The worker session already carried its tail;
				// the Ask path did not.
				tail := s.stderrTail(proc)
				slog.Default().Warn("claude ask: the session child exited", "conversation", s.convID, "stderr", tail)
				msg := "claude ask: the session ended"
				if tail != "" {
					msg += " — " + tail
				}
				s.busEmit(scheduler.SessionEvent{Kind: "error", Text: msg})
				return
			}
			s.handleLine(line)
		}
	}
}

// stderrTail drains whatever the child wrote to stderr and returns a bounded
// tail of it, so a failure the CHILD explained can be reported rather than
// guessed at. Draining is bounded by a short deadline: the channel may already
// be closed (then it returns immediately) or may never close, and an error path
// must not become the next hang.
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

	case "thinking_delta":
		// REASONING, on its own type. The drain loop and the consent/stream
		// layers classify a delta by `Type` and `IsReasoning` (opencode emits
		// exactly this shape — chatsession.go: Type "reasoning", IsReasoning
		// true), so claude matching it is what makes a reasoning bubble render
		// for an Ask conversation the same way it does for opencode.
		//
		// Sent as `delta` rather than folded into `text`: the askorchicon drain
		// opens a think segment on a reasoning delta (chat.go: "if
		// evt.IsReasoning && !segThink.inThink()"), and a reasoning chunk marked
		// as text would open a TEXT segment and leak the model's private thinking
		// into the visible answer.
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
