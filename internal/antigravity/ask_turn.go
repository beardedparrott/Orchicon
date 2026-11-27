package antigravity

// ask_turn.go — the Ask conversation's SPAWN, RESUME, TURN and TEARDOWN.
//
// This is the half that owns a child process. Kept apart from ask.go (the Bridge's ChatTurnClient
// surface) so the interface reads as one thing and the process lifecycle reads as another; they share
// only the askSession fields.
//
// THE HANDSHAKE IS THE WORKER ONE, deliberately: initialize → authenticate → session/new|resume|load →
// set_model. It is copied in ORDER from sessionrun.go rather than shared, because the worker's version
// finishes an execution on failure (OnResult, a durable error part) while this one reports the failure to
// the caller and keeps the conversation available for a retry.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// SendTurnMessage implements scheduler.ChatTurnClient: deliver one user turn to the conversation's
// session, spawning or resuming the child first when it is not live.
func (b *Bridge) SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("antigravity ask: SendTurnMessage requires a conversation id")
	}
	s := b.ensureAskSession(conversationID)

	// THE KIND IS OUR OWN TURN-LOOP LOCK, NOT THE CHILD'S. A superseding turn must not interleave its
	// spawn/set-model with a running prompt.
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.captureScope(ctx)
	if strings.TrimSpace(modelRef) != "" {
		s.mu.Lock()
		s.model = askModelID(modelRef)
		s.mu.Unlock()
	}
	if strings.TrimSpace(sessionID) != "" {
		s.mu.Lock()
		if s.sid == "" || s.sid != sessionID {
			// The collector persisted an id we are not holding: a plane restart, or a session this bridge
			// re-created. Adopting it is what makes the NEXT spawn a RESUME rather than a new conversation.
			s.sid = sessionID
		}
		s.mu.Unlock()
		b.bindAskSessionID(sessionID, s)
	}
	if err := s.ensureLive(ctx); err != nil {
		return err
	}

	// THE MCP SET (and the may-act flag) IS FIXED AT SPAWN, so a change must respawn before the turn.
	if s.mcpStale(ctx) {
		s.log.Info("antigravity ask: the conversation's MCP set changed — respawning to apply it",
			"conversation", s.convID)
		s.teardown()
		if err := s.ensureLive(ctx); err != nil {
			return err
		}
	}

	if err := s.prepareTurnModel(ctx); err != nil {
		return err
	}
	return s.prompt(ctx, system, text)
}

// captureScope reads the turn's mode, tenant and conversation project from the SAME stamped context
// askorchicon builds (withAskMode / tenant.WithID / withAskConversationProject). Every one of them lives on
// the turn's context because the child's argv and cwd are fixed at spawn, and a later spawn must be scoped
// identically to the first.
func (s *askSession) captureScope(ctx context.Context) {
	mode := askmode.ModeFromContext(ctx)
	tid := tenant.FromContext(ctx)
	pid := askmode.ConversationScopeFromContext(ctx).ProjectID

	s.mu.Lock()
	projectChanged := s.project != "" && pid != "" && s.project != pid
	s.mode = mode
	if strings.TrimSpace(tid) != "" {
		s.tenant = tid
	}
	if strings.TrimSpace(pid) != "" {
		s.project = pid
	}
	s.mu.Unlock()

	// A PROJECT CHANGE MEANS A DIFFERENT CWD, and the cwd is fixed at spawn: the child must be replaced,
	// keeping the agy session id so the resume continues the same conversation (claude's prepareAskTurn
	// does exactly this).
	if projectChanged {
		s.log.Info("antigravity ask: the conversation's project changed — respawning in its new directory",
			"conversation", s.convID, "project", pid)
		s.teardown()
	}
}

// turnMode reports the mode captured for this turn.
func (s *askSession) turnMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// workDir reports the directory this conversation works in: its project's directory when it has one, else
// the per-conversation ask directory. It is what the mode/boundary rules are judged against.
func (s *askSession) workDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		return s.dir
	}
	return filepath.Join(s.b.askRoot(), s.convID)
}

// resolveWorkDir picks the cwd and its additional readable roots.
//
// THE PROJECT DIR IS THE POINT. claude's Ask path uses `Cwd = askDir` (ask.go:515), so an Ask conversation
// on claude cannot see the project it is about unless the operator pastes paths. An agy Ask conversation is
// assigned to a project, so it runs IN that project — that is what makes "approving a write writes the
// file into the project" true rather than aspirational (AC4).
func (s *askSession) resolveWorkDir(ctx context.Context) (string, []string, error) {
	s.mu.Lock()
	pid, tid := s.project, s.tenant
	s.mu.Unlock()

	if strings.TrimSpace(pid) != "" && s.b.projectDir != nil {
		dir, err := s.b.projectDir(ctx, tid, pid)
		if err != nil {
			return "", nil, fmt.Errorf("antigravity ask: resolve the conversation's project directory: %w", err)
		}
		if dir = strings.TrimSpace(dir); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", nil, fmt.Errorf("antigravity ask: the project directory is unusable: %w", err)
			}
			// The worker's own carve-outs (/tmp/orchicon and the .orchicon run metadata) are readable from
			// an Ask session for the same reason they are from a worker: a conversation that cannot read its
			// own run scratch cannot report anything about it.
			return dir, workerCarveOuts(dir), nil
		}
	}

	dir := filepath.Join(s.b.askRoot(), s.convID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("antigravity ask: create the ask directory: %w", err)
	}
	roots := []string{dir}
	if strings.TrimSpace(pid) != "" {
		// A project was named but its directory did not resolve: the ask dir is still readable, and the log
		// says so rather than letting the conversation silently lose its project.
		s.log.Warn("antigravity ask: the conversation's project resolved to no directory — running in the ask directory",
			"conversation", s.convID, "project", pid)
	}
	return dir, roots, nil
}

// ensureLive spawns the child and opens (or resumes) the conversation when it is not already serving.
func (s *askSession) ensureLive(ctx context.Context) error {
	s.mu.Lock()
	if s.ready && s.client != nil {
		s.mu.Unlock()
		return nil
	}
	if s.liveCtx == nil {
		// Session-scoped, NOT the turn's: the child outlives the request that created it.
		lctx, cancel := context.WithCancel(context.Background())
		s.liveCtx, s.liveCancel = lctx, cancel
	}
	liveCtx := s.liveCtx
	resumeID := strings.TrimSpace(s.sid)
	tenantID := s.tenant
	s.mu.Unlock()

	// --- 1. THE INVOCATION, from the one builder (procspec.go) --------------------------------------
	settings, apiKey, err := s.b.resolveSettings(ctx, tenantID)
	if err != nil {
		s.setSpawnErr(err)
		return fmt.Errorf("antigravity ask: resolve tenant settings: %w", err)
	}
	// THE MODE'S DENIALS RIDE ON `--settings` (session-only, never saved), which is the hardening half of
	// the boundary: a tool the mode refuses cannot even reach a permission request. The in-handler gate is
	// still the authority (see modegate.go), because the mode can change while argv is fixed.
	settings.SessionSettings = agyAskModeSettings(s.turnMode())

	bin, err := s.b.resolveBinary()
	if err != nil {
		s.setSpawnErr(err)
		return err
	}
	baseEnv := os.Environ()
	spec, err := BuildSpawnSpec(bin, settings, apiKey, baseEnv)
	if err != nil {
		s.setSpawnErr(err)
		return err
	}
	// THE ARGV GUARD RUNS BEFORE THE SPAWN, exactly as on the worker path: a bypass spelling would discard
	// the entire Ask consent surface and the bridge would look identical to a gated one in every log line.
	if err := assertNoBypass(spec.Argv); err != nil {
		s.setSpawnErr(err)
		return err
	}

	dir, roots, err := s.resolveWorkDir(ctx)
	if err != nil {
		s.setSpawnErr(err)
		return err
	}
	// THE OS-LEVEL GUARD SHIM, the same one claude's Ask child gets: destructive commands the permission
	// handler could not see are refused by the shim underneath it.
	if g, gerr := guard.NewExecutionGuard(dir); gerr == nil {
		baseEnv = g.Apply(baseEnv)
		// The guard's temp dir lives as long as the child; teardown closes it.
		s.mu.Lock()
		s.guardCloser = g.Close
		s.mu.Unlock()
	} else {
		s.log.Warn("antigravity ask: the OS-level execution guard could not be built — this child runs with the "+
			"ACP consent surface only", "conversation", s.convID, "error", gerr)
	}

	procSpec := spec.ProcSpec()
	procSpec.ExecID = "ask:" + s.convID
	procSpec.Env = baseEnv
	procSpec.Cwd = dir

	// --- 2. THE SPAWN ----------------------------------------------------------------------------
	client, err := acp.Start(liveCtx, acp.Config{
		Factory:       s.b.spawnFactory(),
		Launch:        acp.LaunchOptions{Binary: spec.Argv[0]},
		Spec:          procSpec,
		Handler:       &askPermissionHandler{s: s, log: s.log},
		AuthMethod:    spec.AuthMethod,
		ClientName:    "orchicon",
		ClientVersion: clientVersion,
		Logger:        s.log,
	})
	if err != nil {
		s.setSpawnErr(err)
		return fmt.Errorf("antigravity ask: spawn: %w", err)
	}
	s.mu.Lock()
	s.client = client
	s.dir = dir
	s.mu.Unlock()

	// A SPAWN THAT FAILS AFTER THIS POINT MUST NOT LEAK THE CHILD.
	fail := func(cause error) error {
		s.setSpawnErr(cause)
		closeCtx, cancel := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace)
		defer cancel()
		_ = client.Close(closeCtx)
		s.mu.Lock()
		s.client = nil
		s.mu.Unlock()
		return cause
	}

	// --- 3. THE HANDSHAKE ------------------------------------------------------------------------
	if _, err := client.Initialize(liveCtx); err != nil {
		return fail(fmt.Errorf("antigravity ask: handshake failed: %w", err))
	}
	// AUTH IS EXPLICIT. agy's initialize and session/new succeed WITHOUT a credential and defer the check
	// to the first model call, so "session opened" must never be read as "signed in".
	if err := client.Authenticate(liveCtx, spec.AuthMethod); err != nil {
		return fail(fmt.Errorf("antigravity ask: authenticate: %s", authMessage(err)))
	}

	// --- 4. THE CONVERSATION ---------------------------------------------------------------------
	servers, fingerprint := s.resolveMCP(ctx, tenantID)
	load := acp.LoadParams{
		Cwd:                   dir,
		MCPServers:            servers,
		AdditionalDirectories: roots,
		// THE SYSTEM PROMPT GOES HERE, on `session/new`/`resume` via `_meta.antigravity.systemPrompt.append`,
		// because it survives a resume where a first-turn text preamble would not.
		SystemPromptAppend: s.pendingSystemPrompt(),
	}

	var (
		conv  *acp.Session
		state *acp.SessionState
	)
	switch {
	case resumeID != "":
		// THE RESUME IS THE WHOLE POINT OF PERSISTING THE ID (AC6): `session/resume` replays nothing, and
		// `session/load` — the fallback for a build that does not offer resume — replays the history as
		// updates, which the ask mapper tolerates (it ignores the user chunks).
		conv, state, err = client.ResumeSession(liveCtx, resumeID, load)
		if err != nil {
			s.log.Warn("antigravity ask: session/resume failed — falling back to session/load (history will replay)",
				"conversation", s.convID, "session", resumeID, "error", authMessage(err))
			conv, state, err = client.LoadSession(liveCtx, resumeID, load)
		}
		if err != nil {
			return fail(fmt.Errorf("antigravity ask: resume agy session %s: %s", resumeID, authMessage(err)))
		}
		// THE AC6 EVIDENCE LINE: the id is the SAME one the previous turn persisted, and it is logged so a
		// restart's resume is provable from the plane log rather than inferred from the model's recall.
		s.log.Info("antigravity ask: resumed agy session", "conversation", s.convID, "session", conv.ID())
	default:
		conv, state, err = client.NewSession(liveCtx, acp.NewSessionParams{
			Cwd:                   dir,
			MCPServers:            servers,
			AdditionalDirectories: roots,
			SystemPromptAppend:    s.pendingSystemPrompt(),
		})
		if err != nil {
			return fail(fmt.Errorf("antigravity ask: open the conversation: %s", authMessage(err)))
		}
		s.log.Info("antigravity ask: opened agy session", "conversation", s.convID, "session", conv.ID(), "dir", dir)
	}
	_ = state

	// THE REAL ID REPLACES ANY PLACEHOLDER, and it is bound BEFORE the first prompt so a permission request
	// arriving mid-turn can find this session.
	s.mu.Lock()
	s.conv = conv
	s.sid = conv.ID()
	s.mcpFingerprint = fingerprint
	s.ready = true
	s.spawnErr = nil
	s.mapper = newAskMapper(s.b, s.convID)
	s.mu.Unlock()
	s.b.bindAskSessionID(conv.ID(), s)

	// --- 5. THE UPDATE PUMP ---------------------------------------------------------------------
	// ONE PUMP PER SPAWN, feeding the CURRENT bus (set by Subscribe). A pump that stopped would hold the
	// session's event channel and stall the wire once its buffer filled.
	pumpDone := make(chan struct{})
	s.mu.Lock()
	s.pumpDone = pumpDone
	s.mu.Unlock()
	go func() {
		defer close(pumpDone)
		for u := range conv.Events() {
			s.handleUpdate(liveCtx, u)
		}
	}()

	// THE MODEL IS SET AT SPAWN, exactly as the worker session does it (sessionrun.go). Without this a
	// conversation created eagerly (CreateConversationSession) would run agy's DEFAULT model until the first
	// turn's prepareTurnModel, so the model the operator picked would not be the model that answered the
	// first prompt.
	if err := s.prepareTurnModel(liveCtx); err != nil {
		return fail(err)
	}
	return nil
}

// prepareTurnModel settles the model: switch it IN PLACE when the turn's ref differs from what the live
// conversation is driving, so a mid-conversation model change keeps the context.
func (s *askSession) prepareTurnModel(ctx context.Context) error {
	s.mu.Lock()
	conv := s.conv
	want := s.model
	s.mu.Unlock()
	if conv == nil || strings.TrimSpace(want) == "" {
		// No model named: KEEP agy's default. That is different from an unknown model and must not be
		// treated as one.
		return nil
	}
	if err := conv.SetModel(ctx, want); err != nil {
		return fmt.Errorf("antigravity ask: the model %q was refused by agy: %s", want, authMessage(err))
	}
	s.log.Info("antigravity ask: model set", "conversation", s.convID, "model", want)
	return nil
}

// prompt delivers one `session/prompt` and closes the turn on its stopReason.
//
// THE TURN IS DRIVEN ON THE CALLER'S GOROUTINE, which is what the collector expects: SendTurnMessage
// returns when the turn has ENDED, and askorchicon dispatches it from its own goroutine while its drain
// loop reads the bus.
func (s *askSession) prompt(ctx context.Context, system, text string) error {
	s.mu.Lock()
	conv := s.conv
	prompted := s.prompted
	notices := s.pendingNotice
	s.pendingNotice = nil
	s.mu.Unlock()
	if conv == nil {
		return errors.New("antigravity ask: no live conversation to prompt")
	}

	blocks := s.composePromptBlocks(system, text, prompted, notices)
	// A TURN RUNS UNDER THE SESSION'S OWN CONTEXT, not the caller's: an HTTP/dispatch context ending must
	// not cancel a turn the operator is watching. A Stop arrives as AbortConversationSession → session/cancel.
	turnCtx := ctx
	if s.liveCtx != nil {
		turnCtx = s.liveCtx
	}
	res, err := conv.Prompt(turnCtx, blocks)
	// THE TURN IS ONLY OVER ONCE ITS STREAM IS: Prompt returns when agy answered, but the updates
	// travelled ahead of it through a queue, so recording the turn before they are mapped would report an
	// empty answer. This is the same barrier the worker session applies.
	s.settle(conv)
	s.finishTurn(turnCtx, res)

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// A cancelled turn is a STOP, not a failure: the operator pressed Stop, or the conversation was
			// retired. It ends with `idle` so the collector closes the turn cleanly.
			s.busEmit(scheduler.SessionEvent{Kind: "idle"})
			return nil
		}
		msg := authMessage(err)
		if tail := s.stderrTail(); tail != "" {
			msg += " — " + tail
		}
		s.busEmit(scheduler.SessionEvent{Kind: "error", Text: msg})
		return err
	}
	if res != nil {
		if msg := stopReasonMessage(res.StopReason); msg != "" && res.StopReason != acp.StopReasonCancelled {
			s.busEmit(scheduler.SessionEvent{Kind: "error", Text: msg})
			return nil
		}
	}
	s.busEmit(scheduler.SessionEvent{Kind: "idle"})
	return nil
}

// composePromptBlocks is the turn's content.
//
// THE SYSTEM PROMPT RIDES THE FIRST PROMPT ONLY. It is also passed at spawn via
// `_meta.antigravity.systemPrompt.append`; sending the whole thing again on every turn would double it in
// the model's context, and NOT sending it at all on the first turn would lose the platform's instructions
// on a build that ignores the meta block. So: prepend once, then never again.
func (s *askSession) composePromptBlocks(system, text string, prompted bool, notices []string) []acp.ContentBlock {
	var b strings.Builder
	for _, n := range notices {
		// A REFUSAL THE MODEL MUST SEE. ACP carries no reason on a permission rejection, so the refusal text
		// travels here; without it the model sees an unexplained tool failure and retries the same call.
		if t := strings.TrimSpace(n); t != "" {
			b.WriteString("[platform] ")
			b.WriteString(t)
			b.WriteString("\n\n")
		}
	}
	if !prompted {
		if sys := strings.TrimSpace(system); sys != "" {
			b.WriteString("=== SYSTEM ===\n")
			b.WriteString(sys)
			b.WriteString("\n\n")
		}
		b.WriteString(agyAskPromptPreamble())
	}
	b.WriteString(text)

	s.mu.Lock()
	s.prompted = true
	s.mu.Unlock()
	return []acp.ContentBlock{acp.TextBlock(b.String())}
}

// agyAskPromptPreamble is the ONE agy-specific instruction an Ask conversation needs.
//
// `ask_question` IS AGY'S NATIVE MULTIPLE-CHOICE TOOL AND IT CANNOT BE ANSWERED OVER ACP: it is a
// tool_call that agy itself renders as its own assistant text and then ends the turn, with no permission
// request and no answer channel (docs/adapters/antigravity-acp.md, child 1's recording). The platform's
// equivalent — `orchicon_ask_user`, served by the Orchicon MCP sidecar — DOES round-trip, so the model is
// told to use it. The native tool is also denied via `--settings` (modegate.go), so this is the
// instruction half of a two-part rule: the model is told what to do, and prevented from doing the thing
// that does not work.
func agyAskPromptPreamble() string {
	return "\n[platform] To ask the user a multiple-choice question, call the Orchicon MCP tool " +
		"`orchicon_ask_user` (agy spells it `mcp_orchicon_orchicon_ask_user`). Do NOT use agy's own " +
		"`ask_question` tool: its answer cannot be returned to you on this transport, so it is refused. " +
		"Ask, then stop and wait.\n\n"
}

// finishTurn closes one turn: the assistant text, the reasoning, the tool results and the usage.
func (s *askSession) finishTurn(ctx context.Context, res *acp.PromptResult) {
	s.mu.Lock()
	mapper := s.mapper
	s.mu.Unlock()
	if mapper == nil {
		return
	}
	mapper.RecordTurn(ctx, s, res)
}

// abort cancels the live turn. The conversation survives.
func (s *askSession) abort() {
	s.mu.Lock()
	conv := s.conv
	s.mu.Unlock()
	if conv == nil {
		return
	}
	s.log.Info("antigravity ask: cancelling the live turn", "conversation", s.convID)
	// `session/cancel` ends the turn with stopReason "cancelled"; it also answers any outstanding permission
	// request `cancelled`, so a card left on screen cannot outlive the turn it belonged to.
	if err := conv.Cancel(); err != nil {
		s.log.Debug("antigravity ask: session/cancel failed", "conversation", s.convID, "error", err)
	}
}

// teardown retires the conversation's child entirely. The agy session id is KEPT so the next spawn resumes
// the same conversation (that is what makes a mode or project change preserve the context).
func (s *askSession) teardown() {
	s.mu.Lock()
	client, conv := s.client, s.conv
	cancel := s.liveCancel
	pumpDone := s.pumpDone
	guardClose := s.guardCloser
	bus := s.bus
	s.client, s.conv, s.ready = nil, nil, false
	s.liveCancel, s.liveCtx = nil, nil
	s.pumpDone = nil
	s.guardCloser = nil
	s.bus = nil
	s.mcpFingerprint = ""
	s.mu.Unlock()

	// SETTLE THE PARKED CARDS FIRST, AND LET THEM BE ANSWERED, BEFORE THE CHILD GOES.
	//
	// The ORDER here is the whole difference between "the operator's card was refused" and "the call hung
	// until the child died": a settled wait makes the handler return a REJECT decision, which the acp client
	// then writes on a wire that is still open. Closing the client first would race its own fail-closed
	// `serveCancel(errTeardown)` against our refusal, and either answer would be arbitrary.
	s.askMu.Lock()
	for id, ch := range s.waits {
		select {
		case ch <- "":
		default:
		}
		delete(s.waits, id)
	}
	s.askMu.Unlock()
	// Wait for the handlers to finish (bounded: a wedged handler must not hold the shutdown).
	deadline := time.Now().Add(time.Second)
	for s.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// A brief yield for the acp conn to write the answer it just received.
	time.Sleep(2 * time.Millisecond)

	if conv != nil {
		closeCtx, c := context.WithTimeout(context.Background(), time.Second)
		_ = conv.Close(closeCtx)
		c()
	}
	if cancel != nil {
		cancel()
	}
	if client != nil {
		// BOUNDED ABOVE THE CLIENT'S OWN GRACE, and it must be: CloseAsk is the PLANE'S SHUTDOWN SEAM, so a
		// child that will not die must cost one conversation a bounded delay rather than the whole plane a
		// hang. acp.Client.Close is itself bounded; this is the outer guard for anything it cannot account
		// for (a forked helper holding a pipe, a wedged transport).
		done := make(chan struct{})
		go func() {
			closeCtx, c := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+time.Second)
			_ = client.Close(closeCtx)
			c()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(acp.DefaultShutdownGrace + 2*time.Second):
			s.log.Warn("antigravity ask: the child did not close inside the shutdown grace — releasing the "+
				"conversation anyway (a plane shutdown must not hang on one child)", "conversation", s.convID)
		}
	}
	if pumpDone != nil {
		select {
		case <-pumpDone:
		case <-time.After(time.Second):
			s.log.Warn("antigravity ask: the update pump did not drain after the session closed",
				"conversation", s.convID)
		}
	}
	if guardClose != nil {
		// The shim dir is a temp directory; leaving it behind on every respawn would litter /tmp for the
		// life of the plane.
		guardClose()
	}
	if bus != nil {
		bus.Close()
	}
}

// stderrTail renders the LAST stderr lines the child emitted, so a failure the child itself explained can be
// reported rather than guessed at.
func (s *askSession) stderrTail() string {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return ""
	}
	lines := client.Stderr()
	if len(lines) == 0 {
		return ""
	}
	// Only the tail: a child that logged a lot before failing buries the cause otherwise.
	const keep = 5
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return strings.Join(lines, " | ")
}

// setSpawnErr records a spawn failure for the caller and the log.
func (s *askSession) setSpawnErr(err error) {
	s.mu.Lock()
	s.spawnErr = err
	s.mu.Unlock()
}

// nextAskID mints a card id unique within this conversation.
func (s *askSession) nextAskID() string {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.askSeq++
	return fmt.Sprintf("agy-%s-%d", s.convID, s.askSeq)
}

// queueNotice records a refusal the model must be told about on the next prompt.
func (s *askSession) queueNotice(text string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return
	}
	s.mu.Lock()
	// ONE NOTICE PER TURN IS ENOUGH: a model that tried three refused calls gets one instruction, not three
	// copies of the same paragraph eating its context.
	if len(s.pendingNotice) == 0 {
		s.pendingNotice = []string{t}
	}
	s.mu.Unlock()
}

// pendingSystemPrompt is what goes on `session/new`/`resume`.
//
// IT IS EMPTY ON PURPOSE, and the reason is worth stating: the platform's system prompt arrives per TURN
// (SendTurnMessage's `system`), not at spawn, so at spawn time this bridge does not have it. It is
// prepended to the first turn's prompt instead (composePromptBlocks), which is the same content one
// message later — and the agy preamble rides with it.
func (s *askSession) pendingSystemPrompt() string { return "" }

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

// emitRefusal publishes a refused tool call as a `tool_result` with IsError, so the operator sees the
// refusal INLINE where the call was — the same rendering a refused call gets on the claude Ask path.
func (s *askSession) emitRefusal(call acp.ToolCallInfo, tool, refusal string) {
	input := agyInput(call.RawInput, call.Args)
	s.busEmit(scheduler.SessionEvent{
		Kind:       "tool_result",
		Type:       "tool_result",
		ToolCallID: call.ToolCallID,
		ToolName:   tool,
		ArgsJSON:   jsonString(input),
		Output:     refusal,
		IsError:    true,
	})
}

// resolveMCP renders the conversation's ACP `mcpServers` and its fingerprint.
//
// THE SCOPE IS THE CONVERSATION'S, not a worker's: the sidecar is built with the Ask scope so
// `orchicon_ask_user` is registered, and a mode that may not act has the operator's servers WITHHELD
// (askmode.MayAct), which is the offered half of the mode boundary.
func (s *askSession) resolveMCP(ctx context.Context, tenantID string) ([]acp.MCPServer, string) {
	s.mu.Lock()
	pid, mode := s.project, s.mode
	s.mu.Unlock()

	ref := mcpclient.ScopeRef{Kind: mcpclient.ScopeConversation, ProjectID: pid, ConversationID: s.convID}
	builtin := s.b.orchiconACPSServerForAsk(tenantID, s.convID)

	res, err := s.b.resolveMCP(tenant.WithID(ctx, tenantID), tenantID, ref)
	var servers []acp.MCPServer
	if err != nil {
		// A MISSING selection must not leave the conversation with no platform tools at all.
		s.log.Warn("antigravity ask: MCP resolution failed — only the built-in Orchicon server is registered",
			"conversation", s.convID, "error", err)
		servers = []acp.MCPServer{builtin}
	} else {
		servers, err = s.b.renderMCP(tenant.WithID(ctx, tenantID), tenantID, res, builtin)
		if err != nil {
			s.log.Warn("antigravity ask: MCP rendering failed — only the built-in Orchicon server is registered",
				"conversation", s.convID, "error", err)
			servers = []acp.MCPServer{builtin}
		}
	}
	mayAct := askmode.MayAct(mode)
	if !mayAct {
		servers = withholdOpaqueMCP(servers)
		s.log.Info("antigravity ask: the mode may not act — the operator's MCP servers are not offered this session",
			"conversation", s.convID, "mode", mode)
	}
	logMCPResolution("ask", servers, "")
	return servers, mcpFingerprintOf(mayAct, servers)
}

// mcpStale reports whether the rendered MCP set differs from the one the live child was spawned with.
func (s *askSession) mcpStale(ctx context.Context) bool {
	s.mu.Lock()
	conv := s.conv
	had := s.mcpFingerprint
	s.mu.Unlock()
	if conv == nil || had == "" {
		return false
	}
	_, now := s.resolveMCP(ctx, s.tenantFor())
	return now != had
}

// currentSID reports the agy session id this conversation is bound to ("" before a spawn).
func (s *askSession) currentSID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

// tenantFor reads the session's captured tenant.
func (s *askSession) tenantFor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenant
}

// askModelID extracts the BARE agy model id from a model_ref, tolerating a provider/ vendor prefix.
//
// "" means KEEP agy's default (no model was named), which is different from an unknown model and must not
// be treated as one.
func askModelID(ref string) string {
	r := strings.TrimSpace(ref)
	if r == "" {
		return ""
	}
	if id, err := parseModelRefLoose(r); err == nil {
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	// A ref the parser does not recognise: take the last path segment, which is where a model id lives in
	// every shape this platform writes (`antigravity/google/gemini-x`, `google/gemini-x`).
	if i := strings.LastIndex(r, "/"); i >= 0 {
		return strings.TrimSpace(r[i+1:])
	}
	return r
}

// withholdOpaqueMCP keeps only the platform's own server entries. A mode that may not act is not OFFERED a
// third-party action tool at all — the same rule claude's Ask argv applies with withholdOpaqueMCP.
func withholdOpaqueMCP(servers []acp.MCPServer) []acp.MCPServer {
	out := make([]acp.MCPServer, 0, len(servers))
	for _, s := range servers {
		if strings.EqualFold(strings.TrimSpace(s.Name), "orchicon") {
			out = append(out, s)
		}
	}
	return out
}

// mcpFingerprintOf is a stable string over the rendered servers plus the may-act flag, so a change
// (a server added, a secret rotated, the mode flipping) respawns the child.
func mcpFingerprintOf(mayAct bool, servers []acp.MCPServer) string {
	parts := make([]string, 0, len(servers)+1)
	if mayAct {
		parts = append(parts, "mayact=1")
	} else {
		parts = append(parts, "mayact=0")
	}
	for _, s := range servers {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s", s.Name, s.Command, strings.Join(s.Args, ","), string(s.Env)))
	}
	return strings.Join(parts, ";")
}

// orchiconACPSServerForAsk builds the platform's own MCP sidecar entry for a CONVERSATION scope.
//
// IT REUSES claude.OrchiconMCPServer for the same reason mcp.go does: the sidecar is the same binary with
// the same `mcp` subcommand and the same tenant environment, and a second construction is a second place
// for the tenant id to be forgotten. The Ask scope is what makes `orchicon_ask_user` registered.
func (b *Bridge) orchiconACPSServerForAsk(tenantID, convID string) acp.MCPServer {
	extra := map[string]string{}
	if cid := strings.TrimSpace(convID); cid != "" {
		extra[claude.MCPConversationEnv] = cid
	}
	return acpMCPServer(claude.OrchiconMCPServer(claude.HookBinaryFor(false), tenantID, extra))
}

// settle waits until every update agy sent BEFORE its prompt answer has been mapped — the same barrier the
// worker session uses, and for the same measured reason: without it the turn's coalesced text part flushes
// empty and the operator sees no answer.
func (s *askSession) settle(conv *acp.Session) {
	if conv == nil {
		return
	}
	s.mu.Lock()
	mapper := s.mapper
	s.mu.Unlock()
	if mapper == nil {
		return
	}
	deadline := time.Now().Add(settleUpdateMax)
	prev := mapper.handled()
	for time.Now().Before(deadline) {
		if len(conv.Events()) == 0 {
			time.Sleep(settleUpdateQuiesce)
			cur := mapper.handled()
			if cur == prev && len(conv.Events()) == 0 {
				return
			}
			prev = cur
			continue
		}
		prev = mapper.handled()
		time.Sleep(2 * time.Millisecond)
	}
	s.log.Warn("antigravity ask: the update stream did not settle before the turn closed",
		"conversation", s.convID, "handled", prev)
}
