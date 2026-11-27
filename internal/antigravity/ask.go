package antigravity

// ask.go — the ASK (interactive) transport: scheduler.ChatTurnClient on *Bridge.
//
// THE SHAPE IS CLAUDE'S Ask transport, method for method, and for the same reason bridge.go gives about
// the worker surface: the collector that drains a turn, the consent layer that answers a card and the
// three clients that render the stream were all built against ONE event vocabulary. An adapter that
// implemented a different subset would dispatch differently and render nothing, so this file implements
// the whole ChatTurnClient plus the optional capabilities askorchicon type-asserts.
//
// WHAT IS GENUINELY DIFFERENT ABOUT AGY, and why each difference is written the way it is:
//
//  1. THE SESSION ID IS AGY'S, NOT OURS. claude's Ask transport CHOOSES the session id (it passes
//     `--session-id`) because the CLI accepts one. agy allocates the id in its `session/new` response, so
//     the id is known only after the child has spawned, handshaked, authenticated and opened a
//     conversation. CreateConversationSession therefore SPAWNS EAGERLY and returns agy's own id: anything
//     else would persist a placeholder id, and the resume on the next plane restart would name a session
//     agy has never heard of. That decision is the reason this file has an `ensureLive` separate from
//     SendTurnMessage.
//
//  2. ONE PROCESS PER CONVERSATION, LONG-LIVED ACROSS TURNS. The child (and its ACP session) outlive the
//     turn, so a conversation's context accumulates in agy exactly as it does in a claude Ask session.
//     A turn is one `session/prompt`; the turn's lifetime is the Prompt call's, NOT the child's.
//
//  3. A TURN THAT ARRIVES FOR A SESSION WE ARE NOT HOLDING IS A RESUME, NOT A NEW CONVERSATION. After a
//     plane restart the bridge has no live child but askorchicon persists the agy session id, so
//     SendTurnMessage with an unknown-but-named id does `session/resume` (falling back to
//     `session/load`) instead of starting over. That is AC6.
//
//  4. THE MODEL IS SWITCHED IN PLACE. `session/set_model` on the live conversation when the turn's
//     model_ref changes, so a model switch mid-conversation keeps the context.
//
// THE ASK DIFF LEDGER IS SEPARATE FROM THE WORKER ONE. b.askFileEdits (set by SetDiffLedger's second
// argument) is what this transport feeds, so an Ask conversation's edits land under owner kind
// ask_conversation rather than on an execution that does not exist.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time proof that the antigravity bridge satisfies the Ask chat-session capability. This is the
// ONE thing that makes `antigravity` appear in Dispatcher.ChatKinds() → AIGatewayService.ListAdapterKinds'
// AskCapableKinds → askorchicon.SetChatKinds: a pure type assertion, with no registry to update.
var (
	_ scheduler.ChatTurnClient       = (*Bridge)(nil)
	_ scheduler.SessionQueuesPrompts = (*Bridge)(nil)
	_ scheduler.ChatToolRestrictor   = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind     = (*Bridge)(nil)
)

// AntigravityAskRootEnv overrides where Ask conversations without a project run.
const AntigravityAskRootEnv = "ORCHICON_ANTIGRAVITY_ASK_ROOT"

// DefaultAntigravityAskRoot is the stable per-plane directory Ask conversations run in when their
// conversation has no project. It is a SIBLING of claude's ask root, not the same directory: two adapters
// sharing one tree would let a conversation of one kind write into the other's cwd.
func DefaultAntigravityAskRoot() string {
	if v := strings.TrimSpace(os.Getenv(AntigravityAskRootEnv)); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "orchicon-agy-ask")
}

// ProjectDirResolver resolves a conversation's project id to its directory. It is a closure rather than a
// pool read here for the same reason SettingsResolver is: the bridge stays testable without a database,
// and the server owns the transaction.
type ProjectDirResolver func(ctx context.Context, tenantID, projectID string) (string, error)

// askConsentWait is the OPTIONAL bound on a consent card. Zero is the default and means NO BOUND: this is
// a human decision, and claude's Ask path takes the same default for the same reason
// (hookConsentWaitDefault). An operator who wants a shorter leash, and a test that must not hang, set
// ORCHICON_ANTIGRAVITY_CONSENT_WAIT.
func askConsentWait() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ORCHICON_ANTIGRAVITY_CONSENT_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// askSession is ONE live Ask conversation: its child, its ACP session, its bus and its parked cards.
type askSession struct {
	b      *Bridge
	convID string
	log    *slog.Logger

	// --- the child's lifetime (NOT the turn's) ---------------------------------------------------
	//
	// liveCtx/done own the child. A turn's context is a REQUEST's; the child must outlive it, so it is
	// cancelled only by a teardown, a mode/project respawn, or CloseAsk.
	liveCtx    context.Context
	liveCancel context.CancelFunc
	done       chan struct{}
	doneOnce   sync.Once

	// mu guards the mutable per-turn state below. It is separate from askMu (the card registry) because
	// the two are touched from different goroutines: mu from turn dispatch, askMu from the ACP reader's
	// permission requests.
	mu      sync.Mutex
	client  *acp.Client
	conv    *acp.Session
	sid     string // the agy session id (agy's own, once known)
	model   string // the bare agy model id this conversation is driving
	dir     string // the cwd this child was spawned in
	bus     *askBus
	mode    string // the turn's Ask mode, captured from the stamped context
	tenant  string
	project string
	// ready is true once the child has handshaked and the conversation is open. An ensureLive while it is
	// false must WAIT rather than spawn a second child.
	ready bool
	// spawnErr is the failure from the most recent spawn attempt, so a caller gets the real cause rather
	// than a generic one.
	spawnErr error
	// mcpFingerprint is the resolved MCP set (with the may-act flag) at spawn, so a change respawns.
	mcpFingerprint string
	// pendingNotice is a refusal the MODEL must be told about. ACP carries no reason on a permission
	// rejection, so the refusal text is prepended to the next prompt (`[platform] …`) — otherwise the model
	// sees an unexplained failure and retries.
	pendingNotice []string
	// pumpDone closes when the update pump for the CURRENT conv has drained.
	pumpDone chan struct{}
	// turnMu serialises turns: agy defines no behaviour for a second `session/prompt` in flight, and the
	// acp Session already serialises them — this is the conversation-level lock the turn queue needs so a
	// superseding turn does not interleave its setup with a running one.
	turnMu sync.Mutex
	// started records the first prompt delivered since spawn, which is when the system prompt preamble must
	// ride along (it is NOT re-sent on later turns: agy already has it in the conversation).
	prompted bool

	// guardCloser retires the OS-level execution guard's shim directory (nil when no guard was built).
	guardCloser func()
	// mapper is the update → bus fan-out for this conversation (ask_mapper.go). Rebuilt on every spawn.
	mapper *askMapper

	// --- the card registry (ask_permission.go) ---------------------------------------------------
	askMu  sync.Mutex
	waits  map[string]chan string
	askSeq int64
	// inflight counts permission handlers currently running. A teardown uses it to let a settled card BE
	// ANSWERED on the wire before the child goes: closing first would race the acp client's own
	// fail-closed cancel against our refusal, and the operator's answer had already been decided.
	inflight atomic.Int64
}

// newAskSession builds a conversation's (not yet spawned) session.
func newAskSession(b *Bridge, convID string) *askSession {
	return &askSession{
		b:      b,
		convID: convID,
		log:    b.log,
		done:   make(chan struct{}),
		waits:  make(map[string]chan string),
	}
}

// --- the Bridge surface -------------------------------------------------------------------------

// Subscribe implements scheduler.ChatTurnClient: a fresh bus for the conversation's next turn,
// registered on the session so the update pump feeds it.
func (b *Bridge) Subscribe(_ context.Context, conversationID string) (scheduler.SessionBus, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, errors.New("antigravity ask: Subscribe requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	s.mu.Lock()
	old := s.bus
	bus := newAskBus()
	s.bus = bus
	s.mu.Unlock()
	if old != nil {
		// A superseded turn's bus has no consumer; closing it releases anything parked on it.
		old.Close()
	}
	return bus, nil
}

// CreateConversationSession implements scheduler.ChatTurnClient.
//
// IT SPAWNS EAGERLY AND RETURNS AGY'S OWN SESSION ID (see the file comment, difference 1). askorchicon
// persists whatever this returns, so returning a placeholder would make the next restart's resume name a
// session agy has never heard of — AC6 would fail in the one way that looks like an agy bug.
func (b *Bridge) CreateConversationSession(ctx context.Context, conversationID, _ string) (string, error) {
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("antigravity ask: CreateConversationSession requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	// A recreate discards whatever was live: the caller is telling us the old session is gone.
	s.teardown()
	// The turn's mode/tenant/project come from the SAME stamped context the senders read, so a session
	// created here is scoped identically to one created lazily.
	s.captureScope(ctx)
	if err := s.ensureLive(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	sid := s.sid
	s.mu.Unlock()
	if sid == "" {
		return "", errors.New("antigravity ask: the session opened without an id")
	}
	b.bindAskSessionID(sid, s)
	return sid, nil
}

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers FALSE.
//
// agy DEFINES NO WAY to add a message to a running turn: `session/prompt` is one turn, and a second one
// waits for the first to report its stopReason (the acp Session's own promptMu). Claiming TRUE would tell
// askorchicon an interjection joins the running turn — it does not, and an operator would see their words
// answered one turn late with no explanation. The worker bridge advertises exactly the same limitation as
// "mid_run_injection (queued)".
func (b *Bridge) QueuesPrompts() bool { return false }

// RestrictChatTools implements scheduler.ChatToolRestrictor as a no-op that REPORTS success.
//
// THE BOUNDARY IS ALREADY ENFORCED, and that is the honest answer: askModeDenial runs in the permission
// handler BEFORE any card (ask_permission.go), and the mode's denied tools are ALSO refused by agy itself
// via `--settings` at spawn. There is no per-turn tool list to install, so returning an error here — which
// would make askorchicon log "the adapter could not apply the mode's tool policy" on every turn — would be
// a false alarm about a boundary that holds. claude's Ask transport returns nil for the same reason.
func (b *Bridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error { return nil }

// ReplyPermission implements scheduler.ChatTurnClient by approving the ask once.
func (b *Bridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: hand the operator's choice to the card that
// is parked on it.
//
// THERE IS NO GRANT TO RECORD HERE, and that is parity rather than an omission: the grants live in
// askorchicon's store (claude's Ask path documents the same division at ask.go:370 — "The grants live in
// the caller's store, never here"), and askorchicon is what decides whether a later call is re-asked.
func (b *Bridge) ReplyPermissionDecision(_ context.Context, sessionID, permissionID, decision string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return fmt.Errorf("antigravity ask: no live session for %q (the ask cannot be answered)", sessionID)
	}
	if strings.TrimSpace(permissionID) == "" {
		return errors.New("antigravity ask: an empty permission id cannot be correlated to a pending ask")
	}
	s.askMu.Lock()
	ch, ok := s.waits[permissionID]
	s.askMu.Unlock()
	if !ok {
		return fmt.Errorf("antigravity ask: no card is parked under %q (the turn may have ended)", permissionID)
	}
	// Buffered(1) at the wait, so this never blocks even if the waiter already gave up.
	select {
	case ch <- decision:
	default:
	}
	return nil
}

// AbortConversationSession implements scheduler.ChatTurnClient: `session/cancel` on the live turn, so the
// model stops generating NOW. The conversation survives — only the turn ends.
func (b *Bridge) AbortConversationSession(_ context.Context, sessionID string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		// Purging an unknown conversation is a successful no-op (the interface's contract).
		return nil
	}
	s.abort()
	return nil
}

// CloseAsk tears down every Ask conversation.
//
// THE SERVER CALLS THIS ON SHUTDOWN, beside claudeBridge.CloseAsk(). An agy Ask child is a HOST process,
// not something the plane's exit reaps, so without this a plane restart leaves every conversation's child
// running, holding its session and its stdin.
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
	if len(sessions) > 0 {
		b.log.Info("antigravity ask: closed every Ask conversation", "conversations", len(sessions))
	}
}

// SetAskRoot overrides the per-plane Ask directory for conversations with no project.
func (b *Bridge) SetAskRoot(dir string) { b.askRootOverride = strings.TrimSpace(dir) }

// SetProjectDirResolver wires the conversation-project → directory lookup.
func (b *Bridge) SetProjectDirResolver(fn ProjectDirResolver) { b.projectDir = fn }

// --- the Ask registry ---------------------------------------------------------------------------

// ensureAskSession returns the conversation's session, creating the (not yet spawned) shell on first use.
func (b *Bridge) ensureAskSession(convID string) *askSession {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	if b.askByConv == nil {
		b.askByConv = make(map[string]*askSession)
	}
	if s := b.askByConv[convID]; s != nil {
		return s
	}
	s := newAskSession(b, convID)
	b.askByConv[convID] = s
	return s
}

// bindAskSessionID makes an agy session id resolvable to its conversation, which is what
// ReplyPermissionDecision and AbortConversationSession receive from the collector.
func (b *Bridge) bindAskSessionID(sid string, s *askSession) {
	if strings.TrimSpace(sid) == "" || s == nil {
		return
	}
	b.askMu.Lock()
	if b.askBySID == nil {
		b.askBySID = make(map[string]*askSession)
	}
	b.askBySID[sid] = s
	b.askMu.Unlock()
}

// askSessionForID resolves a session id to its conversation.
func (b *Bridge) askSessionForID(sid string) *askSession {
	b.askMu.Lock()
	defer b.askMu.Unlock()
	return b.askBySID[sid]
}

// askRoot resolves the per-plane Ask directory.
func (b *Bridge) askRoot() string {
	if b.askRootOverride != "" {
		return b.askRootOverride
	}
	return DefaultAntigravityAskRoot()
}
