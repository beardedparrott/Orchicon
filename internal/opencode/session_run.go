package opencode

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// sessionRun is the per-execution state for the session transport: one
// persistent opencode session, driven entirely through the serve's
// HTTP+SSE API instead of a one-shot `opencode run` subprocess. The goal
// is the first user message; liveness nudges and mid-run human messages
// (SendExecutionMessage) are later turns on the SAME session, serialized
// by the server's per-session prompt queue.
type sessionRun struct {
	a         *Adapter
	parentCtx context.Context // clean ctx for DB writebacks (never deadline-exhausted)
	procCtx   context.Context // wall-clock deadline applied; abort = both cancel
	execRow   db.ExecutionRow
	manifest  scheduler.ExecutionManifest
	callbacks scheduler.ExecutionCallbacks
	client    *SessionClient
	sessionID string
	modelRef  string
	system    string // worker system prompt (per-message; must ride every turn)

	mu           sync.Mutex
	pendingTurns int  // unanswered user messages (goal + nudges + human)
	finished     bool // finish() already called
	resultOk     bool
	resultErr    string
	done         chan struct{}

	output        strings.Builder
	lastStreamErr string
	textSeq       int
	stats         *execStreamState
	monitor       *progressMonitor

	subCtx    context.Context
	subCancel context.CancelFunc

	nudgesSent    int
	lastNudgeAt   time.Time
	probeDeadline time.Time
	probePending  bool

	// Session-scoped nudge tuning (manifest value first, env fallback).
	// Populated by initNudgeTuning before the monitor starts. These drive
	// both the advisory-stall nudge path and the completion-probe path.
	nudgeMaxVal         int
	nudgeReplyWindowVal time.Duration
	nudgeCooldownVal    time.Duration

	// Durable transcript (execution_session_parts): recorded as events
	// arrive, flushed in batches by a background goroutine and at finish.
	store        SessionStoreFunc
	muParts      sync.Mutex
	seq          int64
	pendingParts []db.SessionPart

	// Soft-first compact-on-budget-breach gate (see compact.go). budget is
	// the per-execution spend accumulator fed on each step_finish via
	// parseEvent; budgetSpec is the parsed merged budget (cost_usd primary,
	// tokens fallback). compactsPerformed + lastCompactStep implement the
	// "at most once per step, never before the minimum-turn floor,
	// re-arms only across normal forward progress" latch that prevents a
	// compact loop.
	budget            *budgetAccumulator
	budgetSpec        budgetSpec
	compactsPerformed int
	lastCompactStep   int
}

// Nudge tuning (env-overridable; default matches the advisory no-file
// window being a real probe opportunity rather than a silent notice).
const (
	defaultMaxNudges        = 2
	defaultNudgeReplyWindow = 300 * time.Second
	defaultNudgeCooldown    = 60 * time.Second
	defaultSettleAfterIdle  = 1 * time.Second
	defaultSSEReconnectMax  = 10 * time.Second
)

func nudgeMax() int {
	if v := os.Getenv("ORCHICON_STALL_NUDGE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxNudges
}

func nudgeReplyWindow() time.Duration {
	return envDuration("ORCHICON_STALL_NUDGE_REPLY_WINDOW", defaultNudgeReplyWindow)
}

func nudgeCooldown() time.Duration {
	return envDuration("ORCHICON_STALL_NUDGE_COOLDOWN", defaultNudgeCooldown)
}

// initNudgeTuning resolves the session's nudge knobs: manifest (tenant
// settings) value first, env-var fallback, then code default. Zero in the
// manifest means "use the env var or code default". Called once before the
// monitor starts so the session's nudge budget is stable for its lifetime.
func (r *sessionRun) initNudgeTuning() {
	r.nudgeMaxVal = nudgeMax()
	r.nudgeReplyWindowVal = nudgeReplyWindow()
	r.nudgeCooldownVal = nudgeCooldown()
	if r.manifest.StallNudgeMax > 0 && os.Getenv("ORCHICON_STALL_NUDGE_MAX") == "" {
		r.nudgeMaxVal = int(r.manifest.StallNudgeMax)
	}
	if r.manifest.StallNudgeReplyWindowSeconds > 0 && os.Getenv("ORCHICON_STALL_NUDGE_REPLY_WINDOW") == "" {
		r.nudgeReplyWindowVal = time.Duration(r.manifest.StallNudgeReplyWindowSeconds) * time.Second
	}
	if r.manifest.StallNudgeCooldownSeconds > 0 && os.Getenv("ORCHICON_STALL_NUDGE_COOLDOWN") == "" {
		r.nudgeCooldownVal = time.Duration(r.manifest.StallNudgeCooldownSeconds) * time.Second
	}
}

// nudgeMax returns the session's resolved nudge budget (manifest value
// first, env fallback). Falls back to the package default if the session
// was constructed without initNudgeTuning (e.g. tests).
func (r *sessionRun) nudgeMax() int {
	if r.nudgeMaxVal > 0 {
		return r.nudgeMaxVal
	}
	return nudgeMax()
}

// nudgeReplyWindow returns the session's resolved nudge reply window.
func (r *sessionRun) nudgeReplyWindow() time.Duration {
	if r.nudgeReplyWindowVal > 0 {
		return r.nudgeReplyWindowVal
	}
	return nudgeReplyWindow()
}

// nudgeCooldown returns the session's resolved nudge cooldown.
func (r *sessionRun) nudgeCooldown() time.Duration {
	if r.nudgeCooldownVal > 0 {
		return r.nudgeCooldownVal
	}
	return nudgeCooldown()
}

// livenessProbeText is the injected liveness check sent on an advisory
// no_file_progress stall. It is designed to be answered at the next turn
// boundary WITHOUT derailing the task: report status, then continue. It
// preserves the ORCHICON WORKER SUMMARY decision-signal contract so the
// probe reply cannot corrupt the routing signal.
const livenessProbeText = "Do NOT stop or restart your task. This is a liveness check from Orchicon, not new work. " +
	"In one short paragraph report: (1) what you have completed so far, (2) what you are doing right now, " +
	"(3) what you will do next. Then continue your task exactly as planned. " +
	"If your task is complete, end your output with: ORCHICON WORKER SUMMARY: success — <summary>. " +
	"If you are genuinely blocked and cannot proceed, end your output with: ORCHICON WORKER SUMMARY: failure — <reason>."

// decisionMarker is the single marker signal every worker execution ends
// with (docs: the first word after it is success|failure). The completion
// probe and the settle-time success guard check for its presence so a
// session that ended without delivering the signal is never recorded as a
// clean success.
const decisionMarker = "ORCHICON WORKER SUMMARY:"

// afterCompactReminderText is interjected into the session immediately
// after a mid-flight compaction (compact.go maybeCompact) so the model
// that just had its context summarized is reminded of the ORCHICON WORKER
// SUMMARY contract and the todowrite obligation before it continues. A
// summarized session otherwise can drift into "write a plan for the final
// summary" and echo the marker as a literal template — which extraction
// then parses as a fake `success` (see placeholderMarkerBody). The
// reminder keeps the deliverable, its final line, and the live todo list
// in view for the resumed turns.
const afterCompactReminderText = "Your conversation was just compacted to keep it within budget — your task and progress notes are preserved in the compacted summary. " +
	"Continue working on your task exactly as before. " +
	"Keep maintaining your `todowrite` list (full replacement array, `pending | in_progress | completed | cancelled`) so the operator sees live progress. " +
	"When you have actually finished — and only then — end your response with the literal line: " +
	"ORCHICON WORKER SUMMARY: success — <your summary of what you did>  (or  ORCHICON WORKER SUMMARY: failure — <the blocker>). " +
	"Do NOT emit that line as part of a plan; emit it only as your final sign-off when the deliverable is complete."

// placeholderMarkerBody reports whether the text following an
// ORCHICON WORKER SUMMARY marker is a doc/plan placeholder ("success — <summary>",
// "<reason>", an empty body) rather than a real worker-written summary. A
// worker that echoes the marker as an example inside its plan must not be
// treated as having delivered the signal — both the completion probe
// (completionProbeDecision) and settle-time success guard must ignore it.
func placeholderMarkerBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	if strings.Contains(rest, "<summary>") || strings.Contains(rest, "<reason>") ||
		strings.Contains(rest, "<your summary>") || strings.Contains(rest, "<your-summary>") {
		return true
	}
	// "success — <summary>", "success", "—", "failure" with nothing real.
	lower := strings.ToLower(rest)
	switch lower {
	case "", "success", "failure", "success —", "failure —", "success — <summary>", "failure — <reason>":
		return true
	}
	return false
}

// realDecisionMarkerIn reports the index of the LAST real ORCHICON WORKER
// SUMMARY marker in output — one whose body is actual content, not a
// placeholder/template echo. Returns -1 when no real marker exists.
func realDecisionMarkerIn(output string) int {
	idx := strings.LastIndex(output, decisionMarker)
	for idx >= 0 {
		if !placeholderMarkerBody(output[idx+len(decisionMarker):]) {
			return idx
		}
		// The last occurrence was a placeholder echo — look for an earlier
		// genuine one (a worker may plan with the marker then deliver it).
		idx = strings.LastIndex(output[:idx], decisionMarker)
	}
	return -1
}

// completionProbeText is sent on session.idle when the worker's turn ended
// WITHOUT the decision marker — e.g. the final model response was truncated
// mid-stream (a step_finish with reason "unknown"/0 tokens), so the worker
// never delivered its ORCHICON WORKER SUMMARY. Interjecting a prompt asks
// the (still-live) session to finish the signal instead of recording a
// hollow success; a session that still cannot produce the marker fails.
const completionProbeText = "Your response appears to have been cut off before your final ORCHICON WORKER SUMMARY was captured. " +
	"Please do not restart your work. " +
	"If you have finished your task, reply with your final summary exactly in this form: " +
	"ORCHICON WORKER SUMMARY: success — <summary>  (or  failure — <reason>). " +
	"If you are still working, report your current status and then continue, and be sure to end with your ORCHICON WORKER SUMMARY when done."

// run executes the whole session lifecycle. It returns nil once the
// execution has completed (OnResult fired). A non-nil error means the
// session transport could not be set up — with the one-shot path removed,
// the caller surfaces it as a failed execution.
func (r *sessionRun) run() error {
	client := r.client
	// The durable transcript writer must be set before any recordPart
	// (the session_info entry is the first one).
	r.store = r.a.sessionStore

	// The serve's published port can be refused for a beat after the
	// docker-proxy binds it; retry the session create briefly so a
	// converging serve doesn't fail the execution.
	var sid string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		sid, err = client.CreateSession(r.parentCtx, r.execRow.ID)
		if err == nil {
			break
		}
		if attempt == 2 {
			return fmt.Errorf("create session: %w", err)
		}
		select {
		case <-r.parentCtx.Done():
			return fmt.Errorf("create session: %w", r.parentCtx.Err())
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	r.sessionID = sid
	r.a.log.Info("opencode session created", "execution", r.execRow.ID, "session", sid, "serve", client.BaseURL())
	// Persist the opencode session identity so the UI can show which
	// serve/session a worker ran on (troubleshooting + follow-up seed).
	r.recordPart(db.SessionPartSessionInfo, map[string]any{
		"session_id": sid,
		"serve_url":  client.BaseURL(),
	})
	// Persist the full system prompt sent to the worker (the per-message
	// `system` field) so the session chat can show exactly what the worker
	// was told — the goal bubble alone (the task title) carries very little.
	if r.system != "" {
		r.recordPart(db.SessionPartSystemPrompt, map[string]any{"text": r.system})
	}

	// Register the live-session handle so SendExecutionMessage can route
	// mid-run human messages into this session.
	r.a.mu.Lock()
	if r.a.sessions == nil {
		r.a.sessions = map[string]*sessionRun{}
	}
	r.a.sessions[r.execRow.ID] = r
	r.a.mu.Unlock()
	defer func() {
		r.a.mu.Lock()
		delete(r.a.sessions, r.execRow.ID)
		r.a.mu.Unlock()
	}()

	// Progress monitor: same stall signals as the one-shot path (which
	// shares the guardrails). Advisory
	// (no_file_progress) trips a liveness probe; fatal signals abort the
	// session and fail the execution.
	r.initNudgeTuning()
	r.monitor = newProgressMonitor(r.execRow.ID, stallWindowsFromManifest(r.manifest))
	go r.monitor.run(r.parentCtx,
		func(_ string, reason string) { r.onStall(reason) },
		func(_, recovered string) { r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, recovered) },
	)
	defer r.monitor.close()

	// Durable transcript: flush every few seconds so a crash loses at most
	// the trailing batch, and once at finish.
	if r.store != nil {
		go r.flushLoop()
	}
	defer r.flushParts()

	// SSE event subscription (auto-reconnecting so a serve restart or
	// transport blip doesn't lose the stream).
	r.subCtx, r.subCancel = context.WithCancel(r.parentCtx)
	defer r.subCancel()
	go r.runSSE()

	// Termination watchers: wall-clock deadline and execution cancel/abort
	// both funnel into finish() (once).
	go func() {
		<-r.procCtx.Done()
		msg := "cancelled"
		if r.procCtx.Err() == context.DeadlineExceeded {
			msg = "wall_clock_timeout"
		}
		r.finish(false, msg)
	}()
	go func() {
		<-r.parentCtx.Done()
		r.finish(false, "cancelled")
	}()

	r.callbacks.OnStarted(r.parentCtx, r.execRow.ID)

	// The goal is the first user message of the session.
	if err := client.SendMessage(r.parentCtx, sid, r.system, r.modelRef, r.manifest.Goal); err != nil {
		_ = client.Abort(r.parentCtx, sid)
		return fmt.Errorf("send goal: %w", err)
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": r.manifest.Goal, "source": "goal"})
	r.a.log.Info("opencode session goal sent", "execution", r.execRow.ID, "session", sid)

	<-r.done

	// Finalize: fold the terminal reason, stream error, and the
	// step-balance check into OnResult.
	ok := r.resultOk
	var parts []string
	if r.resultErr != "" {
		parts = append(parts, r.resultErr)
	}
	if r.lastStreamErr != "" {
		parts = append(parts, r.lastStreamErr)
	}
	// A stream that ended mid-step or on a truncated final step is NOT a
	// success — unless the output still carries the decision signal (the
	// completion probe may have salvaged it, or the summary was delivered
	// before the trailing step event was lost). A missing signal means the
	// worker's final response never completed.
	if r.stats.unfinished() && realDecisionMarkerIn(r.output.String()) < 0 {
		ok = false
		parts = append(parts, "execution ended before the final model step completed (model response stream truncated or event dropped)")
	}
	if r.sessionID != "" {
		_ = client.Abort(r.parentCtx, r.sessionID)
	}
	if r.stats != nil && len(r.stats.writtenFiles) > 0 {
		r.callbacks.OnWrittenFiles(r.parentCtx, r.execRow.ID, r.stats.writtenFiles)
	}
	r.callbacks.OnResult(r.parentCtx, r.execRow.ID, ok, r.output.String(), strings.Join(parts, "; "))
	return nil
}

// handleEvent routes one SSE bus event for this session.
func (r *sessionRun) handleEvent(evt BusEvent) {
	if sid, _ := evt.Properties["sessionID"].(string); sid != "" && sid != r.sessionID {
		return
	}
	switch evt.Type {
	case "permission.asked":
		// Auto-approve (the server-side --auto equivalent) so tool calls
		// never block an unattended execution.
		if pid, _ := evt.Properties["id"].(string); pid != "" {
			go func() { _ = r.client.ReplyPermission(r.parentCtx, r.sessionID, pid) }()
		}
		return
	case "session.idle":
		// The server emits idle only when its per-session queue has fully
		// drained — EVERY message we sent (goal, nudges, human) has been
		// answered. This is the completion signal (a single user message
		// can span multiple steps/tool loops, so step-finish alone is not
		// a turn boundary).
		r.resolveProbe()
		if r.maybeProbeCompletion() {
			return
		}
		r.allTurnsDone()
		return
	case "session.status":
		// Lifecycle signal; completion is tracked via session.idle.
		return
	case "session.error":
		r.recordStreamError(evt)
		return
	}
	// Mid-generation token deltas (streamed text/reasoning) are LIVENESS
	// evidence: feed them to the progress monitor so a long, slow local-model
	// generation counts as progress and never false-trips stalled:no_progress.
	// This must happen BEFORE the LegacyEventFromBus dispatch so deltas never
	// reach parseEvent / the transcript / the UI (completed parts carry the
	// durable record — see TokenDeltaFromBus).
	if _, ok := TokenDeltaFromBus(evt); ok {
		r.resolveProbe()
		r.noteSessionProgress()
		if r.monitor != nil {
			// observe("text") advances lastStepFinish — the no_progress
			// signal — without touching lastMeaningfulAction, so the
			// text_loop guard (an infinite single-step reasoning loop) is
			// unchanged (D4). The part arg is nil: for "text" the monitor
			// ignores it, so we avoid a per-delta allocation (D3). The
			// delta text itself is liveness evidence only — the completed
			// part carries the durable record.
			r.monitor.observe("text", nil)
		}
		return
	}
	if legacy, ok := LegacyEventFromBus(evt); ok {
		// ANY telemetry activity (text/tool/step/reasoning) after a probe
		// is evidence the worker is alive — resolve the probe and revive
		// without waiting for a full turn (the false-positive case: an
		// analyst producing output but not touching files). It is also
		// evidence the serve's model path is healthy — reset the
		// consecutive-session-error counter so a single transient failure
		// never triggers a container recycle.
		r.resolveProbe()
		r.noteSessionProgress()
		r.a.parseEvent(r.parentCtx, r.execRow, r.manifest, legacy, r.callbacks,
			r.monitor, &r.output, &r.lastStreamErr, &r.textSeq, r.stats, r.budget)
		// Record the raw part for the durable transcript, with the tool
		// OUTPUT capped like the live forward (a follow-up or a
		// recovery-resumed session re-seeds this transcript as context, so
		// an uncapped giant build log would re-inflate it).
		if t, _ := legacy["type"].(string); t != "" {
			r.recordPart(t, map[string]any{"part": capPartOutput(legacy["part"]), "error": legacy["error"]})
		}
		// Two independent gates, evaluated on their own event boundary:
		// step_finish → soft-first compact gate (evaluate the budget/
		// turn-count breach; the subsequent turn resends `system`, so
		// goal/AC survive the lossy summary). tool_use → hard tool-call
		// ceiling (no "compact away a tool call" option, so this aborts).
		switch et, _ := legacy["type"].(string); et {
		case evtStepFinish:
			r.maybeCompact()
		case evtToolUse:
			r.checkToolCallLimit()
		}
	}
}

// recordPart appends one transcript entry to the pending batch.
func (r *sessionRun) recordPart(kind string, payload any) {
	if r.store == nil {
		return
	}
	r.muParts.Lock()
	defer r.muParts.Unlock()
	r.seq++
	r.pendingParts = append(r.pendingParts, db.SessionPart{
		ExecutionID: r.execRow.ID,
		TenantID:    r.execRow.TenantID,
		Seq:         r.seq,
		Kind:        kind,
		Payload:     db.MarshalPartPayload(payload),
	})
}

// recordHumanMessage records a mid-run human message (SendExecutionMessage).
func (r *sessionRun) recordHumanMessage(text string) {
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": text, "source": "human"})
}

// flushLoop periodically flushes the pending transcript batch.
func (r *sessionRun) flushLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.parentCtx.Done():
			return
		case <-r.done:
			return
		case <-t.C:
			r.flushParts()
		}
	}
}

// flushParts persists the pending transcript batch (best-effort — a write
// failure loses at most the trailing batch and never blocks control flow).
func (r *sessionRun) flushParts() {
	if r.store == nil {
		return
	}
	r.muParts.Lock()
	if len(r.pendingParts) == 0 {
		r.muParts.Unlock()
		return
	}
	batch := r.pendingParts
	r.pendingParts = nil
	r.muParts.Unlock()
	if err := r.store(r.parentCtx, r.execRow.ID, r.execRow.TenantID, batch); err != nil {
		r.a.log.Warn("session transcript write failed", "execution", r.execRow.ID, "error", err)
	}
}

// resolveProbe clears a pending liveness probe and revives the execution
// (the advisory stall notice clears and every stall window restarts). A
// probe is resolved by the worker producing ANY activity after it was
// sent, or by the whole queue draining (session.idle). This is the
// active-prompt-bump semantics: the worker does not need to answer a full
// turn to prove liveness — continued output is proof enough.
func (r *sessionRun) resolveProbe() {
	r.mu.Lock()
	if !r.probePending {
		r.mu.Unlock()
		return
	}
	r.probePending = false
	r.lastNudgeAt = time.Now()
	revived := r.monitor.revive()
	r.mu.Unlock()
	if revived {
		r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, "recovered:liveness_probe")
		r.a.log.Info("liveness probe resolved — revived", "execution", r.execRow.ID)
	}
}

// allTurnsDone forces the outstanding-turn counter to zero and settles —
// called from session.idle, which the server emits only once every queued
// prompt has been answered.
func (r *sessionRun) allTurnsDone() {
	r.mu.Lock()
	if r.pendingTurns > 0 {
		r.pendingTurns = 0
	}
	if !r.finished {
		r.settleFinish()
	}
	r.mu.Unlock()
}

// settleFinish finishes the execution after a short delay if no new
// message has arrived (a just-sent human message bumps pendingTurns and
// cancels the settle). Caller holds r.mu.
func (r *sessionRun) settleFinish() {
	go func() {
		time.Sleep(defaultSettleAfterIdle)
		r.mu.Lock()
		p, f := r.pendingTurns, r.finished
		r.mu.Unlock()
		if p == 0 && !f {
			r.finish(true, "")
		}
	}()
}

// sessionErrorMessage extracts the human-readable reason from a
// session.error bus event. Provider errors nest the readable message at
// error.data.message (e.g. OpenRouter 500s) — fall back to that when the
// top-level error.message is absent so the failure reason is diagnosable
// instead of a generic "opencode session error".
func sessionErrorMessage(evt BusEvent) string {
	if errObj, ok := evt.Properties["error"].(map[string]any); ok {
		if m, ok2 := errObj["message"].(string); ok2 && m != "" {
			return m
		}
		if data, ok2 := errObj["data"].(map[string]any); ok2 {
			if m, ok3 := data["message"].(string); ok3 {
				return m
			}
		}
	}
	return "opencode session error"
}

// recordStreamError handles a session.error bus event: the turn failed at
// the model/API level.
func (r *sessionRun) recordStreamError(evt BusEvent) {
	msg := sessionErrorMessage(evt)
	r.a.log.Warn("opencode session error", "execution", r.execRow.ID, "message", msg)
	// Abort-echo guard: when the session was already terminated (e.g. a
	// fatal stall recorded its reason and Abort'd the session), the
	// `session.error: Aborted` echo must not re-mark health, bump the
	// recycle counter, or overwrite the terminal reason. The true cause
	// (e.g. stalled:no_progress) is already recorded by finish().
	if r.isFinished() {
		return
	}
	r.mu.Lock()
	if r.lastStreamErr == "" {
		r.lastStreamErr = msg
	}
	r.mu.Unlock()
	r.callbacks.OnHealth(r.parentCtx, r.execRow.ID, "unhealthy")
	r.recycleOnWedgedServe(msg)
	r.finish(false, "opencode_session_error: "+msg)
}

// recycleOnWedgedServe recycles the workflow's runtime container after a
// run of CONSECUTIVE model-layer session errors. The observed wedge: a
// serve whose /global/health answers (so the supervisor's health watchdog
// never fires) but whose model turns fail instantly — every auto-retry
// then re-uses the same poisoned serve and fails in ~80ms. Killing the
// container makes the next dispatch's Create build a fresh serve, which is
// exactly what un-wedged the field incident (a manual RetryFailedWorkflowRun
// re-created the container and succeeded on the first try).
//
// The threshold is consecutive across executions — a wedge is serve-wide,
// not execution-specific, so counting within one session would never fire
// (the first failed execution fails fast and its retries are new sessions).
// Reset to zero on any non-error progress (a step, tool call, or message
// completing) so transient single failures never recycle.
func (r *sessionRun) recycleOnWedgedServe(msg string) {
	threshold := sessionErrorRecycleThreshold()
	if threshold < 1 || r.manifest.RuntimeWorkflowID == "" || r.a.rt == nil {
		return
	}
	if !r.a.countSessionError() {
		return
	}
	r.a.log.Warn("recycling wedged runtime container after consecutive session errors",
		"run", r.manifest.RuntimeWorkflowID, "execution", r.execRow.ID,
		"threshold", threshold, "error", msg)
	// Kill removes the container; the next dispatch's Create rebuilds it
	// with a fresh serve. Best-effort — a failed recycle is just a log.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.a.rt.Kill(ctx, r.manifest.RuntimeWorkflowID); err != nil {
		r.a.log.Warn("runtime container recycle failed", "run", r.manifest.RuntimeWorkflowID, "error", err)
	}
}

// countSessionError increments the consecutive session-error counter and
// returns true once the recycle threshold is reached (the counter is then
// reset so a still-wedged serve re-arms on later dispatches). Returns
// false before the threshold; a disabled threshold (<1) always returns
// false.
func (a *Adapter) countSessionError() bool {
	threshold := sessionErrorRecycleThreshold()
	if threshold < 1 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveSessionErrors++
	if a.consecutiveSessionErrors < threshold {
		return false
	}
	a.consecutiveSessionErrors = 0
	return true
}

// sessionErrorCount returns the current consecutive session-error count
// (test helper).
func (a *Adapter) sessionErrorCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.consecutiveSessionErrors
}

// resetSessionErrors zeroes the consecutive session-error counter (test
// helper).
func (a *Adapter) resetSessionErrors() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.consecutiveSessionErrors = 0
}

// noteSessionProgress resets the consecutive-session-error counter on any
// non-error session progress (a step, tool call, or message completing).
func (r *sessionRun) noteSessionProgress() {
	if sessionErrorRecycleThreshold() < 1 {
		return
	}
	r.a.mu.Lock()
	r.a.consecutiveSessionErrors = 0
	r.a.mu.Unlock()
}

// maybeCompact evaluates the soft-first compact gate at a quiet step
// boundary (after a step_finish feeds the spend accumulator). It fires on
// EITHER of two independent triggers — a cost/token budget breach
// (budgetBreached; the accumulator is cumulative, so once tripped it stays
// tripped), or the turn-count gate (effectiveCompactMaxTurns; steps since
// the last compact reaching the configured/default turn cap) — so a chatty
// session gets compacted periodically even when per-turn spend never crosses
// the cost gate. On either trigger it calls Compact once and CONTINUES —
// best-effort, never a hard abort, never a failure. The shared min-turn
// floor/re-arm latch (never at start, never immediately after a prior
// compact within minT turns) applies to both triggers, so a fresh
// post-compact summary is never immediately re-collapsed (the compact
// loop), and the per-execution cap (compactMax) is the coarse safety
// ceiling on top of that. The session's resolved provider/model is passed
// so the compaction runs under the model the session actually uses.
func (r *sessionRun) maybeCompact() {
	r.mu.Lock()
	if r.finished || r.budget == nil {
		r.mu.Unlock()
		return
	}
	maxC := compactMax()
	if maxC < 1 {
		r.mu.Unlock()
		return
	}
	steps := r.budget.steps
	minT := compactMinTurns()
	// Never at start / never immediately after a prior compact (the floor
	// is the re-arm win — the latch holds across a step so the fresh
	// summary gets to run before the gate can fire again).
	if steps < minT || (r.compactsPerformed > 0 && steps < r.lastCompactStep+minT) {
		r.mu.Unlock()
		return
	}
	if r.compactsPerformed >= maxC {
		r.mu.Unlock()
		return
	}
	turnsSinceLastCompact := steps
	if r.compactsPerformed > 0 {
		turnsSinceLastCompact = steps - r.lastCompactStep
	}
	maxTurns, turnGateOn := effectiveCompactMaxTurns(r.budgetSpec)
	turnTriggered := turnGateOn && turnsSinceLastCompact >= maxTurns
	breached := budgetBreached(r.budgetSpec, r.budget)
	if !breached && !turnTriggered {
		r.mu.Unlock()
		return
	}
	r.compactsPerformed++
	r.lastCompactStep = steps
	r.mu.Unlock()

	reason := "turn_count"
	if breached {
		reason = "budget_breach"
	}

	provider, model, ok := splitModelRef(r.modelRef)
	if !ok {
		r.a.log.Warn("compact skipped: malformed model ref",
			"execution", r.execRow.ID, "modelRef", r.modelRef)
		return
	}
	r.a.log.Info("compact triggering (soft-first)",
		"execution", r.execRow.ID, "step", steps, "reason", reason,
		"costUSD", r.budget.costUSD, "provider", provider, "model", model)
	if err := r.client.Compact(r.parentCtx, r.sessionID, provider, model); err != nil {
		// Best-effort: a failed compact never fails the execution. The
		// wall-clock and stall signals remain the only live killers.
		r.a.log.Warn("compact failed (best-effort)", "execution", r.execRow.ID, "error", err)
		return
	}
	r.recordPart(db.SessionPartCompacted, map[string]any{
		"step": steps, "reason": reason, "costUSD": r.budget.costUSD, "provider": provider, "model": model,
	})

	// After a mid-flight compact, the resumed context is a lossy summary —
	// the original system prompt's ORCHICON WORKER SUMMARY / todowrite
	// guidance may no longer be in the window, which is how a worker can
	// end a run by ECHOING the marker as a template in a plan instead of
	// delivering the actual summary. Interject a compact reminder that
	// re-states the summary contract + todowrite obligation so the resumed
	// turns finish correctly (best-effort; a failure never fails the run).
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, afterCompactReminderText); err != nil {
		r.a.log.Warn("post-compact reminder send failed (best-effort)",
			"execution", r.execRow.ID, "error", err)
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": afterCompactReminderText, "source": "compact_reminder"})
	r.a.log.Info("post-compact reminder sent", "execution", r.execRow.ID, "step", steps)
}

// checkToolCallLimit evaluates the tool_call_count HARD-abort gate on every
// evtToolUse event. Unlike maybeCompact's cost/token/turn-count gates (soft
// — compact and continue), a tool call already made cannot be "compacted
// away", so a breach here fails the execution immediately via the same
// abort path onStall's fatal branch uses (finish() first, THEN Abort, so
// the true reason survives the session.error echo the serve emits when the
// abort lands). Settings has always described tool_call_count as a live
// ceiling ("Max tool calls per execution. Empty = built-in default (100)")
// — this makes that description true instead of aspirational.
func (r *sessionRun) checkToolCallLimit() {
	r.mu.Lock()
	if r.finished || r.stats == nil {
		r.mu.Unlock()
		return
	}
	limit, ok := effectiveToolCallLimit(r.budgetSpec)
	count := r.stats.toolUses
	r.mu.Unlock()
	if !ok || count < limit {
		return
	}
	r.a.log.Warn("tool call limit exceeded — aborting session",
		"execution", r.execRow.ID, "toolCalls", count, "limit", limit)
	r.finish(false, "tool_call_limit_exceeded")
	_ = r.client.Abort(r.parentCtx, r.sessionID)
}

// onStall is the progress monitor's stall callback. Nudge-first routing:
//
//   - no_progress: FATAL — total silence, no responsive surface to nudge.
//     Abort the session and fail the execution immediately.
//   - text_loop / repetition / no_file_progress: ADVISORY-first. The
//     worker is generating text / issuing tool calls, so a nudge can reach
//     it. Send a liveness probe into the live session (context preserved);
//     only after the nudge budget is spent (and the worker still hasn't
//     broken the pattern) does the next trip escalate to a fatal kill +
//     recovery.
func (r *sessionRun) onStall(reason string) {
	fatal := isFatalStall(reason)
	r.callbacks.OnStall(r.parentCtx, r.execRow.ID, reason, fatal)
	if fatal {
		r.a.log.Warn("fatal stall — aborting session", "execution", r.execRow.ID, "reason", reason)
		// Record the terminal reason FIRST so the true cause survives the
		// `session.error: Aborted` echo that the serve emits when Abort
		// lands below — finish() is first-arrival-wins, and without this
		// ordering the SSE-triggered recordStreamError could win the race
		// and mask the real reason with "opencode_session_error: Aborted".
		r.finish(false, reason)
		_ = r.client.Abort(r.parentCtx, r.sessionID)
		return
	}
	r.maybeNudge(reason)
}

// maybeNudge injects the liveness probe when an advisory stall (text_loop /
// repetition / no_file_progress) trips and the nudge budget/cooldown allow
// it. When the budget is spent and the worker is STILL tripping the same
// advisory signal, it escalates to a fatal kill + recovery instead of
// silently dropping the signal (the nudge-first reframe: nudge the live
// session first, escalate only after persistent failure).
func (r *sessionRun) maybeNudge(reason string) {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	if r.nudgesSent >= r.nudgeMax() || now.Sub(r.lastNudgeAt) < r.nudgeCooldown() {
		// Nudge budget spent (or inside the cooldown) and the worker is
		// still tripping the advisory signal — escalate to fatal. The
		// worker has had its nudges and has not broken the pattern.
		r.mu.Unlock()
		r.a.log.Warn("advisory stall budget exhausted — escalating to fatal",
			"execution", r.execRow.ID, "reason", reason, "nudges", r.nudgesSent, "max", r.nudgeMax())
		r.finish(false, reason)
		_ = r.client.Abort(r.parentCtx, r.sessionID)
		return
	}
	r.nudgesSent++
	r.probePending = true
	r.probeDeadline = now.Add(r.nudgeReplyWindow())
	r.mu.Unlock()

	r.a.log.Info("advisory stall — sending liveness probe",
		"execution", r.execRow.ID, "reason", reason, "nudge", r.nudgesSent, "max", r.nudgeMax())
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, livenessProbeText); err != nil {
		r.mu.Lock()
		r.probePending = false
		r.mu.Unlock()
		r.a.log.Warn("liveness probe send failed — plain advisory", "execution", r.execRow.ID, "error", err)
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": livenessProbeText, "source": "nudge"})

	// Verdict: no reply within the window → the worker is not responding
	// → fail + recover (the true-hang case).
	go func() {
		select {
		case <-r.done:
			return
		case <-time.After(r.nudgeReplyWindow()):
		}
		r.mu.Lock()
		still := r.probePending && !r.finished
		r.mu.Unlock()
		if still {
			r.a.log.Warn("liveness probe timed out — failing", "execution", r.execRow.ID)
			r.finish(false, "stalled:no_file_progress:liveness_probe_no_response")
		}
	}()
}

// completionProbeDecision is the pure decision core of the completion-probe
// logic (testable without a live session):
//
//   - output has the decision marker  → (false, false): settle normally.
//   - marker missing, budget left     → (true,  false): send a probe.
//   - marker missing, budget spent    → (false, true): fail — the worker
//     never delivered its decision signal.
//
// nudgeMax and nudgeCooldown are the session's resolved tuning (manifest
// value first, env fallback) so the completion probe shares the same
// per-session budget as the advisory-stall nudge path.
func completionProbeDecision(output string, nudgesSent int, lastNudgeAt, now time.Time, nudgeMax int, nudgeCooldown time.Duration) (probe bool, fail bool) {
	if realDecisionMarkerIn(output) >= 0 {
		return false, false
	}
	if nudgesSent >= nudgeMax || now.Sub(lastNudgeAt) < nudgeCooldown {
		return false, true
	}
	return true, false
}

// maybeProbeCompletion decides whether a session that went idle WITHOUT the
// ORCHICON WORKER SUMMARY decision signal can still be completed. The most
// common cause is the final model turn being truncated mid-stream (a
// step_finish with reason "unknown"/0 tokens), so the worker never delivered
// its summary. The session is still live at idle, so a completion probe
// (a fresh prompt_async turn) asks it to finish the signal — the reply either
// carries the marker (the next idle settles normally) or counts as the
// liveness evidence that clears the probe. When the probe budget is
// exhausted and the marker is still absent, the run FAILS instead of being
// recorded as a hollow success (the workflow's loop decision / re-ask / fail
// path is the correct owner of a missing signal).
//
// Returns true when the caller must NOT settle: either a probe was sent and
// is being awaited, or the run was already failed for the missing signal.
func (r *sessionRun) maybeProbeCompletion() bool {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return false
	}
	probe, fail := completionProbeDecision(r.output.String(), r.nudgesSent, r.lastNudgeAt, time.Now(), r.nudgeMax(), r.nudgeCooldown())
	if fail {
		// Probe budget exhausted and still no signal — this is NOT a success.
		// The final turn was truncated / the summary was lost.
		r.mu.Unlock()
		r.a.log.Warn("session idle without decision signal — failing",
			"execution", r.execRow.ID, "nudges", r.nudgesSent)
		r.finish(false, "stalled:missing_decision_signal:completion_probe_no_response")
		return true
	}
	if !probe {
		r.mu.Unlock()
		return false
	}
	now := time.Now()
	r.nudgesSent++
	r.probePending = true
	r.probeDeadline = now.Add(r.nudgeReplyWindow())
	r.mu.Unlock()

	r.a.log.Info("session idle without decision signal — sending completion probe",
		"execution", r.execRow.ID, "nudge", r.nudgesSent, "max", r.nudgeMax())
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, completionProbeText); err != nil {
		r.mu.Lock()
		r.probePending = false
		r.mu.Unlock()
		r.a.log.Warn("completion probe send failed — failing", "execution", r.execRow.ID, "error", err)
		r.finish(false, "stalled:missing_decision_signal:completion_probe_no_response")
		return true
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": completionProbeText, "source": "nudge"})

	// Verdict: no reply within the window → fail. Any reply resolves the
	// probe (resolveProbe fires on telemetry activity) and the next
	// session.idle re-enters maybeProbeCompletion with the accumulated
	// output — so a successful summary probe settles normally.
	go func() {
		select {
		case <-r.done:
			return
		case <-time.After(r.nudgeReplyWindow()):
		}
		r.mu.Lock()
		still := r.probePending && !r.finished
		r.mu.Unlock()
		if still {
			r.a.log.Warn("completion probe timed out — failing", "execution", r.execRow.ID)
			r.finish(false, "stalled:missing_decision_signal:completion_probe_no_response")
		}
	}()
	return true
}

// runSSE maintains the /event subscription with reconnects, routing every
// event through handleEvent.
func (r *sessionRun) runSSE() {
	backoff := time.Second
	for {
		sub, err := r.client.Subscribe(r.subCtx)
		if err != nil {
			if r.isFinished() {
				return
			}
			select {
			case <-r.subCtx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < defaultSSEReconnectMax {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	readLoop:
		for {
			select {
			case <-r.subCtx.Done():
				sub.Close()
				return
			case evt, ok := <-sub.Events():
				if !ok {
					break readLoop
				}
				r.handleEvent(evt)
				if r.isFinished() {
					sub.Close()
					return
				}
			}
		}
		sub.Close()
		if r.isFinished() {
			return
		}
	}
}

// bumpPending increments the outstanding-turn counter.
func (r *sessionRun) bumpPending() {
	r.mu.Lock()
	r.pendingTurns++
	r.mu.Unlock()
}

// isFinished reports whether finish() has been called.
func (r *sessionRun) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// finish terminates the run exactly once, recording the terminal outcome.
func (r *sessionRun) finish(ok bool, errMsg string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.resultOk = ok
	r.resultErr = errMsg
	r.mu.Unlock()
	if r.subCancel != nil {
		r.subCancel()
	}
	close(r.done)
}
