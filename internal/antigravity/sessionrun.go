package antigravity

// sessionrun.go — the session LIFECYCLE: spawn → handshake → open/resume → set model → turn loop → finish.
//
// Kept apart from session.go so the queue/abort/transcript logic reads as one thing and the launch
// sequence reads as another; the two have nothing to say to each other beyond the fields they share.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
)

// run spawns the child and drives the session to a terminal turn.
func (s *session) run(ctx context.Context) error {
	// THE INVOCATION IS BUILT BEFORE ANYTHING IS SPAWNED, in exactly one place, so the auth method, the
	// telemetry policy and the (key-free) argv cannot be assembled twice and disagree.
	settings, apiKey, err := s.b.resolveSettings(ctx, s.tenantID)
	if err != nil {
		msg := fmt.Sprintf("antigravity: resolve tenant settings: %v", err)
		s.finish(ctx, false, msg, err)
		return err
	}
	bin, binErr := s.b.resolveBinary()
	if binErr != nil {
		s.finish(ctx, false, binErr.Error(), binErr)
		return binErr
	}
	baseEnv, envCleanup := s.b.childEnv(s.manifest)
	defer envCleanup()

	spec, err := BuildSpawnSpec(bin, settings, apiKey, baseEnv)
	if err != nil {
		msg := authMessage(err)
		s.recordError(msg)
		s.finish(ctx, false, msg, err)
		return err
	}
	s.spec = spec
	// The auth method decides the usage rows' BILLING REGIME (plan quota vs metered), and it is only
	// known here — the mapper is built with the session, before tenant settings are resolved.
	s.mapper.SetAuthMethod(spec.AuthMethod)
	// THE ARGV GUARD RUNS BEFORE THE SPAWN, NOT AFTER. A bypass flag would defeat the whole policy
	// silently, so a spec carrying one fails the execution instead of launching an ungated agent.
	if err := assertNoBypass(spec.Argv); err != nil {
		s.recordError(err.Error())
		s.finish(ctx, false, err.Error(), err)
		return err
	}
	s.b.log.Info("antigravity: launching agy", "execution", s.execID, "spec", loggableSpec(spec),
		"cwd", executionDir(s.manifest), "container", s.b.isContainer(s.manifest))

	// THE MODEL IS RESOLVED BEFORE THE SPAWN so an unusable model_ref fails the execution without paying
	// for a process, a handshake and a session.
	modelID, err := modelIDFor(s.manifest)
	if err != nil {
		s.recordError(err.Error())
		s.finish(ctx, false, err.Error(), err)
		return err
	}
	s.model = modelID

	procSpec := spec.ProcSpec()
	procSpec.ExecID = s.execID
	procSpec.Cwd = executionDir(s.manifest)
	procSpec.ProjectDir = s.manifest.ProjectDir
	procSpec.WorkflowRunID = s.manifest.RuntimeWorkflowID

	client, err := acp.Start(ctx, acp.Config{
		Factory:       s.b.spawnFactory(),
		Launch:        acp.LaunchOptions{Binary: spec.Argv[0]},
		Spec:          procSpec,
		Handler:       s.policy,
		AuthMethod:    spec.AuthMethod,
		ClientName:    "orchicon",
		ClientVersion: clientVersion,
		Logger:        s.b.log,
	})
	if err != nil {
		msg := fmt.Sprintf("antigravity spawn failed: %v", authMessage(err))
		s.recordError(msg)
		s.finish(ctx, false, msg, err)
		return err
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), acp.DefaultShutdownGrace+5*time.Second)
		defer cancel()
		_ = client.Close(closeCtx)
	}()

	s.cbs.OnStarted(ctx, s.execID)

	// --- handshake -------------------------------------------------------------------------------
	if _, err := client.Initialize(ctx); err != nil {
		msg := fmt.Sprintf("antigravity handshake failed: %v%s", authMessage(err), s.stderrTail())
		s.recordError(msg)
		s.finish(ctx, false, msg, err)
		return err
	}
	// AUTH IS EXPLICIT, not implied. agy's `initialize` and `session/new` succeed WITHOUT a credential
	// and defer the check to the first model call (docs §3.12), so "session created" must never be read as
	// "signed in". Authenticating here turns a signed-out host into one actionable message up front.
	if err := client.Authenticate(ctx, spec.AuthMethod); err != nil {
		msg := authMessage(err)
		s.recordError(msg)
		s.finish(ctx, false, msg, err)
		return err
	}

	// --- MCP -------------------------------------------------------------------------------------
	servers, mcpErr := s.b.renderACPMCPServers(ctx, s.tenantID, s.manifest, s.b.isContainer(s.manifest))
	if mcpErr != nil {
		s.b.log.Warn("antigravity: MCP resolution failed — the worker runs without its configured MCP servers",
			"execution", s.execID, "error", mcpErr)
	}

	// --- open or resume the conversation ---------------------------------------------------------
	// THE SYSTEM PROMPT TRAVELS HERE, not in the first prompt: `_meta.antigravity.systemPrompt.append` is
	// agy's own mechanism for it (docs §3.7) and it survives a resume, where a first-turn text preamble
	// would not.
	cwd := executionDir(s.manifest)
	addDirs := additionalDirectories(s.manifest)

	var (
		conv  *acp.Session
		state *acp.SessionState
	)
	if s.resumed != "" {
		// THE SINGLE-WRITER CHECK COMES FIRST: binding before the request means a second writer is refused
		// before a prompt can be written onto the same conversation.
		if !s.b.bindSessionID(s.resumed, s.execID) {
			msg := fmt.Sprintf("antigravity: agy session %s is already owned by another live execution — refusing to resume it",
				s.resumed)
			s.recordError(msg)
			s.finish(ctx, false, msg, errors.New(msg))
			return errors.New(msg)
		}
		s.mu.Lock()
		s.sessionID = s.resumed
		s.mu.Unlock()
		s.mapper.BindSessionID(ctx, s.resumed)

		conv, state, err = client.ResumeSession(ctx, s.resumed, acp.LoadParams{
			Cwd:                   cwd,
			MCPServers:            servers,
			AdditionalDirectories: addDirs,
			SystemPromptAppend:    s.manifest.SystemPrompt,
		})
		if err != nil {
			// `session/resume` is the PREFERRED call because it replays nothing. Older/!resume builds only
			// offer `session/load`, which replays the whole history as update notifications — the mapper
			// swallows the user chunks, and the agent chunks land in `out`, which is why the fallback is
			// allowed but logged.
			s.b.log.Warn("antigravity: session/resume failed — falling back to session/load (history will replay)",
				"execution", s.execID, "session", s.resumed, "error", authMessage(err))
			conv, state, err = client.LoadSession(ctx, s.resumed, acp.LoadParams{
				Cwd:                   cwd,
				MCPServers:            servers,
				AdditionalDirectories: addDirs,
				SystemPromptAppend:    s.manifest.SystemPrompt,
			})
		}
	} else {
		conv, state, err = client.NewSession(ctx, acp.NewSessionParams{
			Cwd:                   cwd,
			MCPServers:            servers,
			AdditionalDirectories: addDirs,
			SystemPromptAppend:    s.manifest.SystemPrompt,
		})
	}
	if err != nil {
		msg := fmt.Sprintf("antigravity session open failed: %v%s", authMessage(err), s.stderrTail())
		s.recordError(msg)
		s.finish(ctx, false, msg, err)
		return err
	}
	s.mu.Lock()
	s.conv = conv
	s.mu.Unlock()
	if s.resumed == "" {
		s.mapper.BindSessionID(ctx, conv.ID())
	}

	// --- model -----------------------------------------------------------------------------------
	// AN UNKNOWN MODEL FAILS LOUD; IT NEVER FALLS BACK. The tenant chose it, and silently running a
	// different model is a claim about the run that is not true. agy itself refuses an unknown id with
	// -32602 (unknown_model.jsonl), so the message carries agy's own text plus the available ids.
	if id := strings.TrimSpace(s.model); id != "" {
		if err := s.checkModelAvailable(state, id); err != nil {
			msg := err.Error()
			s.recordError(msg)
			s.finish(ctx, false, msg, err)
			return err
		}
		if err := conv.SetModel(ctx, id); err != nil {
			msg := fmt.Sprintf("antigravity: the model %q was refused by agy: %v (available: %s)",
				id, authMessage(err), availableModels(state))
			s.recordError(msg)
			s.finish(ctx, false, msg, err)
			return err
		}
	}

	// --- the update pump -------------------------------------------------------------------------
	// A DEADLOCK GUARD, not an optimisation: the acp session queues updates unboundedly, but the mapper's
	// callbacks (a session store write, a ledger hook) are synchronous, so a pump that stops would hold the
	// events channel and eventually stall the wire once the channel's buffer fills.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for u := range conv.Events() {
			s.mapper.Handle(ctx, u)
			s.handled.Add(1)
		}
	}()

	// --- the turn loop ---------------------------------------------------------------------------
	// The goal is the FIRST turn. A resumed follow-up's goal is the follow-up message itself, so the same
	// composition works for both.
	first := queuedTurn{text: composeGoalPrompt(s.manifest), kind: "goal"}
	err = s.driveTurns(ctx, conv, first)

	// Closing the session ends the events channel, so the pump always terminates; wait for it so no update
	// is mapped after OnResult.
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conv.Close(closeCtx)
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
		s.b.log.Warn("antigravity: the update pump did not drain after the session closed", "execution", s.execID)
	}
	return err
}

// driveTurns runs the first turn, then every queued turn, until the queue is drained or the turn fails.
func (s *session) driveTurns(ctx context.Context, conv *acp.Session, first queuedTurn) error {
	turn := first
	for {
		// AN ABORTED SESSION DOES NOT RUN ANOTHER TURN: the queue's contents were queued for a run that a
		// human has since stopped.
		s.mu.Lock()
		aborted := s.aborted
		s.mu.Unlock()
		if aborted {
			s.finishCancelled(ctx)
			return nil
		}

		res, err := conv.Prompt(ctx, []acp.ContentBlock{acp.TextBlock(turn.text)})
		// THE TURN IS ONLY OVER ONCE THE STREAM IS. `session/prompt` returning means agy answered; the
		// updates for that turn travelled AHEAD of the answer on the wire, but the acp client delivers them
		// through a queue pump and this session maps them on its own goroutine. Recording the turn before
		// those are mapped would flush the coalesced parts with nothing in them and lose the assistant text
		// from OnResult — a real, observed failure, not a theoretical one.
		s.settle(conv)
		// The turn is closed either way: usage, coalesced parts and the health signal.
		s.mapper.RecordTurn(ctx, res)

		if err != nil {
			if ctx.Err() != nil {
				// The caller's context ended: a deadline and a cancel mean different things to whoever reads
				// the execution, and a deadline reported as "cancelled" hides a wall-clock gate.
				reason := endReason(ctx)
				s.finish(ctx, false, reason, ctx.Err())
				return ctx.Err()
			}
			if s.isAborted() {
				s.finishCancelled(ctx)
				return nil
			}
			msg := fmt.Sprintf("antigravity turn failed: %v%s", authMessage(err), s.stderrTail())
			s.recordError(msg)
			s.finish(ctx, false, msg, err)
			return err
		}

		// A STOP REASON THAT IS NOT end_turn IS A VERDICT ON THE RUN, not a note: "cancelled" from an abort
		// or a policy escape ends the execution as a failure, and any other non-end_turn reason (max_tokens,
		// refusal) ends it with that reason named.
		if res != nil && res.StopReason != "" && res.StopReason != acp.StopReasonEndTurn {
			if res.StopReason == acp.StopReasonCancelled {
				s.finishCancelled(ctx)
				return nil
			}
			msg := stopReasonMessage(res.StopReason)
			s.recordError(msg)
			s.finish(ctx, false, msg, errors.New(msg))
			return errors.New(msg)
		}

		// A POST-HOC POLICY ESCAPE ENDS THE RUN even though the turn itself completed cleanly: the tool the
		// policy would have refused has already run, and continuing would be acting as if it had not.
		if n, why := s.mapper.Violations(); n > 0 {
			msg := fmt.Sprintf("antigravity: the session made a tool call outside the worker policy — %s", why)
			s.recordError(msg)
			s.finish(ctx, false, msg, errors.New(msg))
			return errors.New(msg)
		}

		// THE TURN BOUNDARY IS WHERE AN INJECTED MESSAGE LANDS: agy defines no way to add one to a running
		// turn, so the queue is read exactly here. Nothing queued means the execution is done.
		next, ok := s.popTurn()
		if !ok {
			if ctx.Err() != nil {
				reason := endReason(ctx)
				s.finish(ctx, false, reason, ctx.Err())
				return ctx.Err()
			}
			if s.isAborted() {
				s.finishCancelled(ctx)
				return nil
			}
			if s.isFinished() {
				return nil
			}
			s.finish(ctx, true, "", nil)
			return nil
		}
		turn = next
		_ = ctx
	}
}

// isAborted reports whether an abort has been requested.
func (s *session) isAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// finishCancelled reports the terminal result for a cancelled run, naming the reason an abort carried.
func (s *session) finishCancelled(ctx context.Context) {
	reason := s.abortReason()
	if strings.TrimSpace(reason) == "" {
		reason = "execution cancelled"
	}
	s.recordError(reason)
	s.finish(ctx, false, reason, errors.New(reason))
}

// checkModelAvailable pre-validates the model against agy's advertised catalogue.
//
// IT IS A CHEAPER CHECK FIRST, NOT A REPLACEMENT FOR SetModel: the catalogue agy returns lists the
// thinking-level variants (`gemini-3.8-flash-low`), and a tenant's ref names one of those, so a match here
// means the SetModel below will succeed. When the catalogue is EMPTY (a build that does not advertise
// models) the check is skipped and SetModel is the authority.
func (s *session) checkModelAvailable(state *acp.SessionState, id string) error {
	if state == nil || len(state.Models) == 0 {
		return nil
	}
	for _, m := range state.Models {
		if strings.EqualFold(m.ModelID, id) {
			return nil
		}
	}
	// AN EXACT MATCH IS REQUIRED. A case-insensitive one above is the tolerance; a fuzzy or prefix match
	// would silently run a different thinking level than the tenant asked for.
	return fmt.Errorf("antigravity: the model %q is not offered by agy for this account (available: %s)",
		id, availableModels(state))
}

// availableModels renders agy's catalogue for a failure message, bounded so the message stays readable.
func availableModels(state *acp.SessionState) string {
	if state == nil || len(state.Models) == 0 {
		return "agy advertised no model list"
	}
	const max = 10
	ids := make([]string, 0, max)
	for i, m := range state.Models {
		if i >= max {
			ids = append(ids, fmt.Sprintf("… (%d more)", len(state.Models)-max))
			break
		}
		ids = append(ids, m.ModelID)
	}
	return strings.Join(ids, ", ")
}

// endReason names WHY the session context ended. A deadline and a cancellation both land on ctx.Done(),
// but they mean opposite things to whoever reads the execution: one is "the platform ran out of the time it
// allowed you", the other is "somebody stopped this".
func endReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "wall_clock_timeout"
	}
	return "execution cancelled"
}

// settleUpdateQuiesce is how long an idle stream must stay idle before the turn is treated as fully mapped.
// It is a comparison window, not a timeout: the loop below returns the moment two consecutive samples agree,
// so a normal turn costs one window and a slow disk write costs only as long as the write takes.
var settleUpdateQuiesce = 20 * time.Millisecond

// settleUpdateMax bounds the wait so a wedged pump cannot hang a turn forever.
var settleUpdateMax = 5 * time.Second

// settle waits until every update agy sent BEFORE its prompt answer has been mapped.
//
// WHY A BARRIER IS NEEDED AT ALL, since it is the kind of thing that looks unnecessary until it bites: the
// ACP client hands updates to a per-session queue drained by a pump, and this session maps them on its own
// goroutine (the alternative — mapping inside the prompt call — would block the wire behind a session-store
// write). So `Prompt` can return while an update agy sent earlier is still sitting in that queue. Without this
// wait the turn's coalesced text part flushes empty and `OnResult` reports no output, which is what the tests
// caught.
//
// The two conditions together are what make it correct rather than merely slow:
//
//   - the events channel is empty (nothing is waiting to be taken), AND
//   - the mapper's handled counter has stopped advancing (nothing is mid-Handle).
//
// Both are sampled, and a quiesce window separates "idle for an instant" from "idle". The bound keeps a broken
// client from hanging the run.
func (s *session) settle(conv *acp.Session) {
	if conv == nil {
		return
	}
	deadline := time.Now().Add(settleUpdateMax)
	prev := s.handled.Load()
	stable := 0
	for time.Now().Before(deadline) {
		if len(conv.Events()) == 0 {
			time.Sleep(settleUpdateQuiesce)
			cur := s.handled.Load()
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
		prev = s.handled.Load()
		stable = 0
		time.Sleep(2 * time.Millisecond)
	}
	s.b.log.Warn("antigravity: the update stream did not settle before the turn closed",
		"execution", s.execID, "handled", s.handled.Load())
}
