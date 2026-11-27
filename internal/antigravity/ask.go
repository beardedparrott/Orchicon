package antigravity

// ask.go — THE ASK (conversational) TRANSPORT: scheduler.ChatTurnClient on *Bridge.
//
// THE POPULATION IS SEPARATE FROM the worker executions, deliberately and not just for tidiness. A worker
// session (session.go) has a goal, a worktree, an acceptance criterion and NO human: every permission request
// is answered by PolicyHandler from the platform's rule set. An Ask conversation has a cwd, a project, a
// conversation id, a MODE and an operator sitting in front of it: every permission request becomes a card the
// human answers. Sharing one struct for both would put a `mode` field on a worker and a `manifest` on a
// conversation, and the two permission policies would inevitably be entangled in one method.
//
// SO THE SHARED PARTS ARE THE PARTS THAT ARE GENUINELY SHARED:
//
//	spawnClient (spawn.go)            — settings → binary → argv → bypass guard → spawn
//	canonicalToolName / agyInput / …  — agy's tool vocabulary (toolnames.go)
//	toolOutput / editedPaths / diffNewText — the shapes agy's tool results take (mapper.go)
//	askmode + claude.AskModeRefusal   — the mode boundary (askmodegate.go)
//
// and what differs is here: the per-conversation session registry, the resume-by-id path, the bus, and the
// carded permission handler (askpermission.go).
//
// THE SESSION IS THE CONVERSATION, NOT THE TURN. One `agy acp` child and one agy session id per conversation,
// kept alive across turns, because that is what makes a conversation a conversation: the model's context
// lives in that session. `CreateConversationSession` returns the REAL agy session id so askorchicon can
// persist it, and a later turn with no live client (a plane restart) re-attaches it with `session/resume`,
// falling back to `session/load`.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// Compile-time proof that the antigravity bridge satisfies the Ask chat-session capability. THESE ASSERTIONS
// ARE THE REGISTRATION: `Dispatcher.ChatKinds` type-asserts `ChatTurnClient` on every registered bridge, and
// that slice is what reaches `AIGatewayService.ListAdapterKinds.AskCapableKinds` and
// `askorchicon.Service.SetChatKinds`. Dropping one of these is a build failure rather than an adapter that
// silently stops appearing in the Ask model picker.
var (
	_ scheduler.ChatTurnClient                 = (*Bridge)(nil)
	_ scheduler.ChatToolRestrictor             = (*Bridge)(nil)
	_ scheduler.SessionQueuesPrompts           = (*Bridge)(nil)
	_ scheduler.SendTurnMessageWithAttachments = (*Bridge)(nil)
)

// askBusCapacity is the per-conversation event buffer.
const askBusCapacity = 256

// askTurnCancelGrace bounds how long a superseding turn waits for the in-flight one to stop. agy answers
// `session/cancel` with a `cancelled` stop reason; a child that does not honour it inside this window has
// its prompt abandoned rather than the wire jammed.
const askTurnCancelGrace = 3 * time.Second

// askDirNameEnv overrides where Ask conversations run. It is CLAUDE'S variable, shared on purpose: an
// operator who moved Ask off /tmp has one setting to change, and two adapters writing to two different temp
// trees would double the disk an Ask-heavy day uses for no gain.
const askDirNameEnv = claude.AskRootEnv

// askBus is one conversation's SessionBus. Copied in shape from claude's askBus — including the emit rule,
// which is a behavioural contract the collector depends on: a TERMINAL event (idle/error) is never dropped,
// because dropping `idle` hangs the turn until its reply window; a non-terminal delta may be dropped when the
// buffer is full, because the next delta supersedes it.
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

// emit delivers one event. The done check runs FIRST so a superseded turn's bus cannot panic on a closed
// channel.
func (b *askBus) emit(evt scheduler.SessionEvent) {
	terminal := evt.Kind == "idle" || evt.Kind == "error"

	b.mu.RLock()
	defer b.mu.RUnlock()

	select {
	case <-b.done:
		return
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

// agyAsk is one carded permission request the handler is parked on.
type agyAsk struct {
	decision chan string
	session  string
	options  []acp.PermissionOption
	tool     string
}

// askToolCallInfo is what a tool_use asked for, remembered so its terminal update can name the tool and carry
// the ORIGINAL arguments (the terminal update's args are sometimes empty).
type askToolCallInfo struct {
	canonical string
	raw       string
	input     map[string]any
}

// askSession is ONE live Ask conversation.
type askSession struct {
	b      *Bridge
	convID string

	mu sync.Mutex
	// askDir is this conversation's working directory, and the cwd of its agy child.
	askDir string
	// sid is the agy session id. Chosen by agy on `session/new`, or re-attached from the persisted id.
	sid string
	// model is the bare agy model id the session drives ("" = agy's default).
	model string
	// modelRef is the ref it came from, for the "unknown model" message.
	modelRef string
	// mode is the CURRENT turn's mode, read per permission request (never captured at spawn).
	mode string
	// tenantID scopes the Orchicon MCP sidecar and the ledger.
	tenantID string
	// projectID is the conversation's project, from the stamped turn scope.
	projectID string
	// projectDir is the project's directory on disk, resolved per turn through the bridge's closure. It is
	// the additional root the child may read, and the base the diff ledger records paths against.
	projectDir string
	// system is the last system seed seen, re-sent as `_meta.antigravity.systemPrompt.append` on a re-attach
	// (a worker's manifest holds this for it; a conversation has only what the platform sent).
	system string
	// modelRefErr is why a model ref could not be parsed, surfaced on the turn rather than swallowed.
	modelRefErr error

	// envCleanup removes the OS-level guard shim directory the child's environment was built around. It is
	// held on the session because the child (and therefore the shim) outlives the turn that spawned it.
	envCleanup func()

	client *acp.Client
	conv   *acp.Session
	// state is the session state from new/load/resume (its model catalogue decides a switch's validity).
	state *acp.SessionState
	// liveCtx is the SESSION's context, not the turn's: the child outlives every turn.
	liveCtx    context.Context
	liveCancel context.CancelFunc
	alive      bool

	bus *askBus
	// mapper renders the update stream as SessionEvents, and owns the per-turn tool memory.
	mapper *askMapper

	asks   map[string]*agyAsk
	askSeq int64

	// mcpFingerprint is what the child was launched with (see askmodegate.go), compared on every turn.
	mcpFingerprint string
	// replaying is set around an attach that replays history (`session/load`), so replayed chunks do not land
	// in the next turn's bus as if the model had just said them.
	replaying bool

	// turnActive/turnDone describe the in-flight turn, for the supersede path.
	turnActive bool
	turnDone   chan struct{}
	// handled counts mapped updates, the settle barrier's other half.
	handled int64
}

// --- the Bridge surface -----------------------------------------------------

// Subscribe implements scheduler.ChatTurnClient: a fresh bus for the conversation's next turn.
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
		old.Close()
	}
	return bus, nil
}

// CreateConversationSession implements scheduler.ChatTurnClient.
//
// IT RETURNS THE REAL AGY SESSION ID, because that is the only value a later turn can resume: agy owns its
// session ids, and a synthetic one would be a claim the adapter could not honour after a restart. The cost is
// that this call SPAWNS the child (agy allocates the id in `session/new`), which is also why a scope the
// context does not yet carry is not fatal — the first `SendTurnMessage` re-attaches with it.
func (b *Bridge) CreateConversationSession(ctx context.Context, conversationID, _ string) (string, error) {
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("antigravity ask: CreateConversationSession requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	s.teardown()

	// DELIBERATELY NO SESSION ID YET: `ensureAttached` asks agy for a new one and records what it gets.
	s.mu.Lock()
	s.sid = ""
	s.mu.Unlock()

	if err := s.ensureAttached(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	sid := s.sid
	s.mu.Unlock()
	if strings.TrimSpace(sid) == "" {
		return "", errors.New("antigravity ask: agy returned no session id for the new conversation")
	}
	return sid, nil
}

// QueuesPrompts implements scheduler.SessionQueuesPrompts and answers FALSE.
//
// agy defines no way to add a message to a running turn (ACP's GAP TABLE: "not supported"), so a second
// prompt while one is in flight cannot be folded in — the honest answer is false, and `SendTurnMessage`
// therefore cancels the running turn before starting its own rather than pretending to queue.
func (b *Bridge) QueuesPrompts() bool { return false }

// SendTurnMessage implements scheduler.ChatTurnClient.
func (b *Bridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	return b.sendAskTurn(ctx, conversationID, sessionID, system, modelRef, []acp.ContentBlock{acp.TextBlock(text)})
}

// SendTurnMessageWithAttachments implements scheduler.SendTurnMessageWithAttachments.
//
// ATTACHMENTS ARE NOTED, NOT SENT — and that is a documented limitation rather than an oversight. agy
// advertises `promptCapabilities.image`, but the recording this adapter was built from never exercised an
// image block, so sending one would be a guess at a wire shape; the note keeps the turn going and tells the
// operator their file did not travel. It is a one-line change here if a future recording proves the shape.
func (b *Bridge) SendTurnMessageWithAttachments(ctx context.Context, conversationID, sessionID, system, modelRef, text string, attachments []scheduler.ChatAttachment) error {
	blocks := []acp.ContentBlock{acp.TextBlock(text)}
	if len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, a.Name)
		}
		blocks = append(blocks, acp.TextBlock("[attachments not transmitted on this transport: "+
			strings.Join(names, ", ")+"]"))
	}
	return b.sendAskTurn(ctx, conversationID, sessionID, system, modelRef, blocks)
}

// sendAskTurn is everything both senders do before the prompt goes out.
func (b *Bridge) sendAskTurn(ctx context.Context, conversationID, sessionID, system, modelRef string, blocks []acp.ContentBlock) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("antigravity ask: SendTurnMessage requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)
	if err := s.prepareAskTurn(ctx, sessionID, system, modelRef); err != nil {
		return err
	}
	go s.runTurn(blocks)
	return nil
}

// AbortConversationSession implements scheduler.ChatTurnClient.
func (b *Bridge) AbortConversationSession(_ context.Context, sessionID string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return nil
	}
	s.abortTurn()
	return nil
}

// RestrictChatTools implements scheduler.ChatToolRestrictor by returning nil (nothing to do).
//
// THE BOUNDARY IS ENFORCED, and that is why this is a return-nil and not an unimplemented capability: unlike
// claude (which needs a per-conversation mode FILE for its hook) and opencode (which sets a permission block),
// agy's boundary is enforced in the permission HANDLER (askmodegate.go), which runs before any card. The
// method exists so the platform can report "enforced" instead of "prose only" — its absence would be a
// downgrade in the reported capability, not in the behaviour.
func (b *Bridge) RestrictChatTools(_ context.Context, _ scheduler.ToolPolicy) error { return nil }

// ReplyPermission implements scheduler.ChatTurnClient by approving the ask. The grants live in the caller's
// store, never here: askorchicon records a session grant and this side only maps the answer onto agy's option
// ids.
func (b *Bridge) ReplyPermission(ctx context.Context, sessionID, permissionID string) error {
	return b.ReplyPermissionDecision(ctx, sessionID, permissionID, "once")
}

// ReplyPermissionDecision implements scheduler.ChatTurnClient: hand the operator's decision to the parked
// handler, which maps it onto the option id agy offered.
func (b *Bridge) ReplyPermissionDecision(_ context.Context, sessionID, permissionID, decision string) error {
	s := b.askSessionForID(sessionID)
	if s == nil {
		return fmt.Errorf("antigravity ask: no live session for %q (the ask cannot be answered)", sessionID)
	}
	if strings.TrimSpace(permissionID) == "" {
		return errors.New("antigravity ask: an empty permission id cannot be correlated to a pending ask")
	}
	ask := s.takeAsk(permissionID)
	if ask == nil {
		return fmt.Errorf("antigravity ask: no pending ask %q", permissionID)
	}
	// BUFFERED(1), so this never blocks even if the waiter has already been given up on.
	select {
	case ask.decision <- decision:
	default:
	}
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
		s = &askSession{
			b:      b,
			convID: convID,
			askDir: filepath.Join(b.askRoot(), convID),
			asks:   make(map[string]*agyAsk),
			bus:    newAskBus(),
		}
		// THE MAPPER IS PER SESSION, NOT PER TURN: it remembers what each in-flight tool call asked for, and
		// a tool call can outlive the turn that started it (an approved write lands after the card is
		// answered). A per-turn mapper would lose the tool's name and arguments at exactly that moment.
		s.mapper = newAskMapper(s)
		b.askByConv[convID] = s
	}
	return s
}

// bindAskSessionID records the sid → session mapping the reply RPC resolves through.
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

// askRoot resolves the per-plane Ask directory.
func (b *Bridge) askRoot() string {
	if b.askRootOverride != "" {
		return b.askRootOverride
	}
	return claude.DefaultAskRoot()
}

// SetAskRoot overrides the per-plane Ask directory (the server injects the plane's own root; tests use a
// temp dir).
func (b *Bridge) SetAskRoot(dir string) { b.askRootOverride = strings.TrimSpace(dir) }

// SetAskProjectDirResolver wires the closure that answers "which project directory does the turn's
// conversation belong to?".
//
// IT IS A CLOSURE AND NOT A DB READ for the reason the MCP resolver is one: the adapter must be testable
// without a pool, and the plane already knows how to resolve it (askorchicon.AskFileScopeFor, which is what
// the consent layer consults). An empty answer means "no project", which is a working conversation whose cwd
// is its ask directory.
func (b *Bridge) SetAskProjectDirResolver(fn func(ctx context.Context) string) { b.askProjectDir = fn }

// askProjectDirFor resolves the turn's project directory, tolerating an unwired or failing resolver: a
// conversation with no project still works, it just cannot reach the project's files.
func (b *Bridge) askProjectDirFor(ctx context.Context) string {
	if b.askProjectDir == nil {
		return ""
	}
	dir := strings.TrimSpace(b.askProjectDir(ctx))
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}

// CloseAsk tears down every Ask conversation. It is the shutdown seam the server calls so a plane restart
// does not leak agy children that hold a transcript and a stdin.
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

// --- one conversation's lifecycle -------------------------------------------

// prepareAskTurn records the turn's facts and makes sure a child is attached.
func (s *askSession) prepareAskTurn(ctx context.Context, sessionID, system, modelRef string) error {
	s.mu.Lock()
	if strings.TrimSpace(sessionID) != "" && sessionID != s.sid {
		// The collector persisted an id we did not create (a recreate after ErrSessionNotFound, or a change of
		// adapter): adopt it, because agy is the authority on its own ids.
		s.sid = sessionID
	}
	if strings.TrimSpace(modelRef) != "" {
		s.modelRef = modelRef
		if id, err := parseModelRefLoose(modelRef); err == nil {
			s.model = strings.TrimSpace(id)
		} else {
			s.modelRefErr = err
		}
	}
	if t := strings.TrimSpace(system); t != "" {
		s.system = t
	}
	// THE MODE IS THE TURN'S, recorded here and read PER PERMISSION REQUEST: an operator switching
	// Brainstorm→Iteration mid-conversation must change the boundary on the very next tool call.
	s.mode = askmode.ModeFromContext(ctx)
	if tid := tenant.FromContext(ctx); strings.TrimSpace(tid) != "" {
		s.tenantID = tid
	}
	pid := askmode.ConversationScopeFromContext(ctx).ProjectID
	s.projectID = pid
	s.mu.Unlock()
	s.b.bindAskSessionID(sessionID, s)

	return s.ensureAttached(ctx)
}

// currentMode reports the turn's mode.
func (s *askSession) currentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// ensureAttached makes sure this conversation has a live child attached to its agy session.
//
// THE DECISION TO RE-ATTACH RATHER THAN REUSE is made from three facts: there is no client, the child has
// exited, or the MCP set/mode drifted from what the child was launched with. The last one is not an
// optimisation — the child's `mcpServers` list is fixed at fork/exec, so a server added to the conversation
// (or a mode switch that changes whether the operator's servers are OFFERED at all) is only applied by
// starting a new child and re-attaching the SAME agy session id.
func (s *askSession) ensureAttached(ctx context.Context) error {
	s.mu.Lock()
	alive := s.alive && s.client != nil && !s.client.Exited()
	liveConv := s.conv
	askDir := s.askDir
	sid := s.sid
	fingerprint := s.mcpFingerprint
	tenantID := s.tenantID
	projectID := s.projectID
	mode := s.mode
	system := s.system
	s.mu.Unlock()

	if alive && liveConv != nil {
		// THE GAUGE HALF OF "STILL CORRECT": a mode switch or a new MCP server needs a NEW child, and the
		// fingerprint is the only record of what the live one has.
		if !s.mcpDrifted(fingerprint, tenantID, projectID, mode) {
			if err := s.applyModel(ctx); err != nil {
				return err
			}
			return nil
		}
		s.b.log.Info("antigravity ask: the conversation's MCP set or mode changed — re-attaching the agy session",
			"conversation", s.convID, "session", sid)
		s.teardown()
	}

	if strings.TrimSpace(askDir) == "" {
		askDir = filepath.Join(s.b.askRoot(), s.convID)
	}
	if err := os.MkdirAll(askDir, 0o755); err != nil {
		return fmt.Errorf("antigravity ask: create the ask directory: %w", err)
	}
	s.mu.Lock()
	s.askDir = askDir
	projectDir := s.b.askProjectDirFor(ctx)
	if projectDir == "" {
		projectDir = askDir
	}
	s.projectDir = projectDir
	// THE SESSION'S context, not the turn's: the child outlives the turn that started it.
	if s.liveCtx == nil {
		s.liveCtx, s.liveCancel = context.WithCancel(context.Background())
	}
	liveCtx := s.liveCtx
	s.mu.Unlock()

	servers, fingerprint, err := s.askMCPServers(ctx, tenantID, projectID, mode)
	if err != nil {
		// A RESOLUTION FAILURE IS NOT FATAL: the conversation still runs with the platform's own sidecar,
		// which is what makes orchicon_ask_user work at all.
		s.b.log.Warn("antigravity ask: MCP resolution failed — the conversation runs with the platform's own server only",
			"conversation", s.convID, "error", err)
	}

	handler := &askPermissionHandler{s: s}
	client, spec, envCleanup, err := s.b.spawnClient(liveCtx, tenantID, "ask:"+s.convID, askDir, projectDir, "", false, handler)
	if envCleanup != nil {
		s.envCleanup = envCleanup
	}
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.client = client
	s.mu.Unlock()

	// ONE PLACE THAT OWNS THE CHILD'S LIFETIME: whatever happens below, a failure closes it rather than
	// leaving a half-attached process behind for the next turn to trip over.
	ok := false
	defer func() {
		if !ok {
			closeCtx, cancel := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+time.Second)
			defer cancel()
			_ = client.Close(closeCtx)
			s.mu.Lock()
			if s.client == client {
				s.client = nil
				s.alive = false
			}
			s.mu.Unlock()
		}
	}()

	if _, err := client.Initialize(liveCtx); err != nil {
		return fmt.Errorf("antigravity ask: handshake failed: %s%s", authMessage(err), s.stderrTail(client))
	}
	// AUTH IS EXPLICIT: `session/new` succeeds without a credential and agy defers the check to the first
	// model call, so "session opened" must never be read as "signed in".
	if err := client.Authenticate(liveCtx, spec.AuthMethod); err != nil {
		return errors.New(authMessage(err))
	}

	cwd := askDir
	addDirs := s.askAdditionalDirs(projectDir, askDir)
	sysAppend := askSystemAppend(system)

	var (
		conv  *acp.Session
		state *acp.SessionState
	)
	s.mu.Lock()
	s.replaying = true
	s.mu.Unlock()
	if strings.TrimSpace(sid) != "" {
		// THE SINGLE-WRITER CHECK COMES FIRST, across BOTH populations: an Ask must not `load` a session id a
		// live WORKER execution owns (or vice versa). Two writers on one agy conversation would interleave
		// their prompts on one wire and corrupt both transcripts.
		if owner := s.b.sessionOwner(sid); owner != "" {
			return fmt.Errorf("antigravity ask: agy session %s already has a live worker execution (%s) — refusing a second writer", sid, owner)
		}
		conv, state, err = client.ResumeSession(liveCtx, sid, acp.LoadParams{
			Cwd:                   cwd,
			MCPServers:            servers,
			AdditionalDirectories: addDirs,
			SystemPromptAppend:    sysAppend,
		})
		if err != nil {
			// `session/resume` replays nothing, which is why it is preferred; older builds only offer
			// `session/load`, which replays the whole history as updates. That replay is why `replaying` is set
			// — the pump DROPS the replayed chunks instead of letting them land in the next turn's bus as if the
			// model had just said them.
			if isSessionNotFound(err) {
				s.b.log.Warn("antigravity ask: agy no longer knows this session — a fresh one is needed",
					"conversation", s.convID, "session", sid, "error", authMessage(err))
				s.mu.Lock()
				s.replaying = false
				s.mu.Unlock()
				return fmt.Errorf("%w: %s", scheduler.ErrSessionNotFound, authMessage(err))
			}
			s.b.log.Warn("antigravity ask: session/resume failed — falling back to session/load (history will replay)",
				"conversation", s.convID, "session", sid, "error", authMessage(err))
			conv, state, err = client.LoadSession(liveCtx, sid, acp.LoadParams{
				Cwd:                   cwd,
				MCPServers:            servers,
				AdditionalDirectories: addDirs,
				SystemPromptAppend:    sysAppend,
			})
		}
		if err == nil {
			s.b.log.Info("antigravity ask: resumed agy session",
				"conversation", s.convID, "session", sid, "model", s.modelSnapshot())
		}
	} else {
		conv, state, err = client.NewSession(liveCtx, acp.NewSessionParams{
			Cwd:                   cwd,
			MCPServers:            servers,
			AdditionalDirectories: addDirs,
			SystemPromptAppend:    sysAppend,
		})
	}
	if err != nil {
		s.b.log.Warn("antigravity ask: the agy session could not be opened",
			"conversation", s.convID, "session", sid, "error", authMessage(err))
		return fmt.Errorf("antigravity ask: session open failed: %s%s", authMessage(err), s.stderrTail(client))
	}

	s.mu.Lock()
	s.conv = conv
	s.state = state
	s.sid = conv.ID()
	s.alive = true
	s.replaying = false
	s.mcpFingerprint = fingerprint
	s.mu.Unlock()

	// THE PUMP STARTS BEFORE THE MODEL IS SET, so a model error still has a consumer for the child's stderr and
	// no update can queue behind a session nobody drains.
	go s.pump(liveCtx, conv)

	if err := s.applyModel(ctx); err != nil {
		return err
	}

	s.b.bindAskSessionID(conv.ID(), s)
	s.b.log.Info("antigravity ask: attached the conversation",
		"conversation", s.convID, "session", conv.ID(), "dir", askDir, "project", projectDir,
		"mode", mode, "model", s.modelSnapshot())
	ok = true
	return nil
}

// pump maps the child's update stream onto the bus until the session ends.
func (s *askSession) pump(ctx context.Context, conv *acp.Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case u, open := <-conv.Events():
			if !open {
				return
			}
			s.mu.Lock()
			replaying := s.replaying
			s.mu.Unlock()
			// A REPLAYED UPDATE IS HISTORY, NOT THIS TURN. Dropping it here is what stops `session/load`'s
			// replay of the conversation appearing as freshly generated text.
			if replaying {
				continue
			}
			s.mapper.Handle(ctx, u)
			s.mu.Lock()
			s.handled++
			s.mu.Unlock()
		}
	}
}

// applyModel switches agy's model when the turn's ref differs from the session's current one.
//
// AN UNKNOWN MODEL FAILS LOUD; IT NEVER FALLS BACK. The tenant chose it, and silently running a different
// model is a claim about the run that is not true.
func (s *askSession) applyModel(ctx context.Context) error {
	s.mu.Lock()
	conv := s.conv
	state := s.state
	want := s.model
	ref := s.modelRef
	refErr := s.modelRefErr
	s.mu.Unlock()
	if refErr != nil {
		return fmt.Errorf("antigravity ask: model ref %q: %w", ref, refErr)
	}
	if conv == nil || strings.TrimSpace(want) == "" {
		return nil
	}
	if state != nil && strings.EqualFold(state.CurrentModelID, want) {
		return nil
	}
	if err := checkModelInCatalogue(state, want); err != nil {
		return err
	}
	if err := conv.SetModel(ctx, want); err != nil {
		return fmt.Errorf("antigravity ask: the model %q was refused by agy: %s (available: %s)",
			want, authMessage(err), availableModels(state))
	}
	s.b.log.Info("antigravity ask: model set", "conversation", s.convID, "model", want)
	return nil
}

// modelSnapshot reads the model under the lock.
func (s *askSession) modelSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

// stderrTail renders the last stderr lines the child emitted, for a failure message.
func (s *askSession) stderrTail(client *acp.Client) string {
	if client == nil {
		return ""
	}
	lines := client.Stderr()
	if len(lines) == 0 {
		return ""
	}
	const keep = 5
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return "; stderr: " + strings.Join(lines, " | ")
}

// runTurn sends one prompt and closes the turn on the bus.
func (s *askSession) runTurn(blocks []acp.ContentBlock) {
	s.mu.Lock()
	conv := s.conv
	liveCtx := s.liveCtx
	done := make(chan struct{})
	s.turnDone = done
	s.turnActive = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.turnActive = false
		close(done)
		s.mu.Unlock()
	}()

	if conv == nil || liveCtx == nil {
		s.busEmit(scheduler.SessionEvent{Kind: "error", Text: "antigravity ask: the conversation has no live session"})
		return
	}

	res, err := conv.Prompt(liveCtx, blocks)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A CANCELLED TURN IS AN OPERATOR DECISION, not a failure: `idle` closes it without an error, the
			// same way the claude path ends an aborted turn.
			s.busEmit(scheduler.SessionEvent{Kind: "idle"})
			return
		}
		msg := fmt.Sprintf("antigravity ask turn failed: %s%s", authMessage(err), s.stderrTail(s.clientSnapshot()))
		s.b.log.Warn("antigravity ask: turn failed", "conversation", s.convID, "error", msg)
		s.busEmit(scheduler.SessionEvent{Kind: "error", Text: msg})
		return
	}

	// THE TURN IS ONLY OVER ONCE THE STREAM IS: the prompt answer can arrive while an update agy sent earlier
	// is still queued, and flushing usage before those are mapped would put a step_finish ahead of the text it
	// accounts for.
	s.settle(conv)
	if m := s.mapperSnapshot(); m != nil {
		m.turnEnd(res)
	}

	if res != nil && res.StopReason != "" && res.StopReason != acp.StopReasonEndTurn {
		if res.StopReason == acp.StopReasonCancelled {
			s.busEmit(scheduler.SessionEvent{Kind: "idle"})
			return
		}
		s.busEmit(scheduler.SessionEvent{Kind: "error", Text: stopReasonMessage(res.StopReason)})
		return
	}
	s.busEmit(scheduler.SessionEvent{Kind: "idle"})
}

// clientSnapshot reads the client under the lock.
func (s *askSession) clientSnapshot() *acp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// mapperSnapshot reads the mapper under the lock.
func (s *askSession) mapperSnapshot() *askMapper {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mapper
}

// settle waits until every update agy sent BEFORE its prompt answer has been mapped.
//
// A BARRIER IS NEEDED because the client delivers updates through a queue pump and this session maps them on
// its own goroutine, so `Prompt` can return while an earlier update is still queued. The two conditions are
// both required: the events channel is empty (nothing waiting to be TAKEN) and the handled counter stopped
// advancing (nothing mid-Handle). A bounded wait keeps a broken child from hanging the turn.
func (s *askSession) settle(conv *acp.Session) {
	if conv == nil {
		return
	}
	deadline := time.Now().Add(settleUpdateMax)
	prev := s.handledNow()
	stable := 0
	for time.Now().Before(deadline) {
		if len(conv.Events()) == 0 {
			time.Sleep(settleUpdateQuiesce)
			cur := s.handledNow()
			if cur == prev && len(conv.Events()) == 0 {
				stable++
				if stable >= 1 {
					return
				}
			}
			prev = cur
			stable = 0
			continue
		}
		prev = s.handledNow()
		stable = 0
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *askSession) handledNow() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handled
}

// abortTurn cancels the in-flight prompt.
func (s *askSession) abortTurn() {
	s.mu.Lock()
	conv := s.conv
	s.mu.Unlock()
	if conv == nil {
		return
	}
	if err := conv.Cancel(); err != nil {
		s.b.log.Debug("antigravity ask: session/cancel failed", "conversation", s.convID, "error", err)
	}
}

// supersedeTurn stops an in-flight turn before a new one starts. agy cannot fold a message into a running
// turn, so the only honest options are "wait" and "cancel"; the operator asked for a new message, so it is
// cancelled.
func (s *askSession) supersedeTurn() {
	s.mu.Lock()
	active := s.turnActive
	conv := s.conv
	done := s.turnDone
	s.mu.Unlock()
	if !active || conv == nil {
		return
	}
	s.b.log.Info("antigravity ask: superseding the in-flight turn", "conversation", s.convID)
	_ = conv.Cancel()
	select {
	case <-done:
	case <-time.After(askTurnCancelGrace):
		s.b.log.Warn("antigravity ask: the superseded turn did not stop in time — sending the new prompt anyway",
			"conversation", s.convID)
	}
}

// teardown closes this conversation's child. It CANCELS liveCtx FIRST so a parked permission handler is
// released and answers REJECT while there is still a wire to write it on (AC: teardown → reject), and only
// then closes the process.
func (s *askSession) teardown() {
	s.mu.Lock()
	cancel := s.liveCancel
	client := s.client
	conv := s.conv
	bus := s.bus
	cleanup := s.envCleanup
	s.client = nil
	s.conv = nil
	s.state = nil
	s.alive = false
	s.liveCtx = nil
	s.liveCancel = nil
	s.mcpFingerprint = ""
	s.turnActive = false
	s.replaying = false
	s.asks = make(map[string]*agyAsk)
	s.envCleanup = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conv != nil {
		closeCtx, c1 := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+time.Second)
		_ = conv.Close(closeCtx)
		c1()
	}
	if client != nil {
		closeCtx, c2 := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+5*time.Second)
		_ = client.Close(closeCtx)
		c2()
	}
	if cleanup != nil {
		cleanup()
	}
	if bus != nil {
		bus.Close()
	}
}

// --- helpers ----------------------------------------------------------------

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

// nextAskID mints an ask id unique within this session.
func (s *askSession) nextAskID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askSeq++
	return fmt.Sprintf("agy-%s-%d", s.convID, s.askSeq)
}

// registerAsk parks a carded ask.
func (s *askSession) registerAsk(ask *agyAsk, id string) {
	s.mu.Lock()
	s.asks[id] = ask
	s.mu.Unlock()
}

// takeAsk removes a parked ask and returns it.
func (s *askSession) takeAsk(id string) *agyAsk {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.asks[id]
	if !ok {
		return nil
	}
	delete(s.asks, id)
	return a
}

// pendingAsks reports how many cards are parked (the goroutine-leak assertion in T4).
func (s *askSession) pendingAsks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asks)
}

// askModeRefusalEvent builds the tool_result carrying a refusal, because agy gets no reason over ACP: the only
// way the model — and the transcript its operator reads afterwards — learns WHY is this event.
func askRefusalEvent(toolCallID, tool, refusal string) scheduler.SessionEvent {
	return scheduler.SessionEvent{
		Kind:       "tool_result",
		Type:       "tool_result",
		ToolCallID: toolCallID,
		ToolName:   tool,
		Output:     refusal,
		IsError:    true,
	}
}

// askLog returns a usable logger.
func (b *Bridge) askLog() *slog.Logger {
	if b.log == nil {
		return slog.Default()
	}
	return b.log
}
