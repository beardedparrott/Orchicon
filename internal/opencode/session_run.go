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

	// Durable transcript (execution_session_parts): recorded as events
	// arrive, flushed in batches by a background goroutine and at finish.
	store        SessionStoreFunc
	muParts      sync.Mutex
	seq          int64
	pendingParts []db.SessionPart
}

// Nudge tuning (env-overridable; default matches the advisory no-file
// window being a real probe opportunity rather than a silent notice).
const (
	defaultMaxNudges          = 2
	defaultNudgeReplyWindow   = 300 * time.Second
	defaultNudgeCooldown      = 60 * time.Second
	defaultSettleAfterIdle    = 1 * time.Second
	defaultSSEReconnectMax    = 10 * time.Second
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

// run executes the whole session lifecycle. It returns nil once the
// execution has completed (OnResult fired). A non-nil error means the
// session transport could not be set up at all — the caller falls back to
// the legacy one-shot subprocess path.
func (r *sessionRun) run() error {
	client := r.client
	sid, err := client.CreateSession(r.parentCtx, r.execRow.ID)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	r.sessionID = sid
	r.a.log.Info("opencode session created", "execution", r.execRow.ID, "session", sid, "serve", client.BaseURL())
	// Persist the opencode session identity so the UI can show which
	// serve/session a worker ran on (troubleshooting + follow-up seed).
	r.recordPart(db.SessionPartSessionInfo, map[string]any{
		"session_id": sid,
		"serve_url":  client.BaseURL(),
	})

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

	// Progress monitor: same stall signals as the one-shot path. Advisory
	// (no_file_progress) trips a liveness probe; fatal signals abort the
	// session and fail the execution.
	r.monitor = newProgressMonitor(r.execRow.ID, stallWindowsFromManifest(r.manifest))
	go r.monitor.run(r.parentCtx,
		func(_ string, reason string) { r.onStall(reason) },
		func(_, recovered string) { r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, recovered) },
	)
	defer r.monitor.close()

	// Durable transcript: flush every few seconds so a crash loses at most
	// the trailing batch, and once at finish.
	r.store = r.a.sessionStore
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
	// step-balance check into OnResult (mirrors the one-shot path).
	ok := r.resultOk
	var parts []string
	if r.resultErr != "" {
		parts = append(parts, r.resultErr)
	}
	if r.lastStreamErr != "" {
		parts = append(parts, r.lastStreamErr)
	}
	if r.stats.stepStarts > r.stats.stepFinishes {
		ok = false
		parts = append(parts, "execution ended before the final model step completed (model response stream truncated or event dropped)")
	}
	if r.sessionID != "" {
		_ = client.Abort(r.parentCtx, r.sessionID)
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
		// Fires when ALL queued prompts are answered (the server drains
		// its per-session queue, then goes idle). Belt-and-suspenders to
		// the per-turn step-finish accounting: force the outstanding
		// counter to zero and settle so a missed step-finish can't strand
		// an execution.
		r.allTurnsDone()
		return
	case "session.status":
		// Lifecycle signal; completion is tracked via step-finish + idle.
		return
	case "session.error":
		r.recordStreamError(evt)
		return
	}
	if legacy, ok := legacyEventFromBus(evt); ok {
		r.a.parseEvent(r.parentCtx, r.execRow, r.manifest, legacy, r.callbacks,
			r.monitor, &r.output, &r.lastStreamErr, &r.textSeq, r.stats)
		if t, _ := legacy["type"].(string); t == evtStepFinish {
			r.turnCompleted()
		}
		// Record the raw part for the durable transcript.
		if t, _ := legacy["type"].(string); t != "" {
			r.recordPart(t, map[string]any{"part": legacy["part"], "error": legacy["error"]})
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

// turnCompleted runs when a turn finishes (a step-finish event). It
// decrements the outstanding-turn counter, resolves a pending liveness
// probe, and — when every sent message has been answered — finishes the
// execution after a short settle window (so a just-arrived human message
// can still register).
func (r *sessionRun) turnCompleted() {
	r.mu.Lock()
	// Any turn completing after the probe was sent is evidence the worker
	// is alive (the queued goal turn finishing IS progress). Resolve the
	// probe and revive the execution even though the worker may still not
	// have touched files — the probe reply is direct proof of liveness.
	if r.probePending {
		r.probePending = false
		r.lastNudgeAt = time.Now()
		if r.monitor.revive() {
			r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, "recovered:liveness_probe")
			r.a.log.Info("liveness probe answered — revived", "execution", r.execRow.ID)
		}
	}
	if r.pendingTurns > 0 {
		r.pendingTurns--
	}
	if r.pendingTurns == 0 && !r.finished {
		r.settleFinish()
	}
	r.mu.Unlock()
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

// recordStreamError handles a session.error bus event: the turn failed at
// the model/API level, which mirrors the one-shot path failing the run on
// a JSON error event.
func (r *sessionRun) recordStreamError(evt BusEvent) {
	msg := ""
	if errObj, ok := evt.Properties["error"].(map[string]any); ok {
		if m, ok2 := errObj["message"].(string); ok2 {
			msg = m
		}
	}
	if msg == "" {
		msg = "opencode session error"
	}
	r.a.log.Warn("opencode session error", "execution", r.execRow.ID, "message", msg)
	r.mu.Lock()
	if r.lastStreamErr == "" {
		r.lastStreamErr = msg
	}
	r.mu.Unlock()
	r.callbacks.OnHealth(r.parentCtx, r.execRow.ID, "unhealthy")
	r.finish(false, "opencode_session_error: "+msg)
}

// onStall is the progress monitor's stall callback. Fatal signals abort
// the session and fail the execution (the response to a genuine
// hang/loop). The advisory no_file_progress signal sends a liveness probe
// instead of just a stalled notice.
func (r *sessionRun) onStall(reason string) {
	fatal := isFatalStall(reason)
	r.callbacks.OnStall(r.parentCtx, r.execRow.ID, reason, fatal)
	if fatal {
		r.a.log.Warn("fatal stall — aborting session", "execution", r.execRow.ID, "reason", reason)
		_ = r.client.Abort(r.parentCtx, r.sessionID)
		r.finish(false, reason)
		return
	}
	r.maybeNudge(reason)
}

// maybeNudge injects the liveness probe when the advisory no_file_progress
// stall trips and the nudge budget/cooldown allow it.
func (r *sessionRun) maybeNudge(reason string) {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	if r.nudgesSent >= nudgeMax() || now.Sub(r.lastNudgeAt) < nudgeCooldown() {
		r.mu.Unlock()
		return
	}
	r.nudgesSent++
	r.probePending = true
	r.probeDeadline = now.Add(nudgeReplyWindow())
	r.mu.Unlock()

	r.a.log.Info("advisory stall — sending liveness probe",
		"execution", r.execRow.ID, "reason", reason, "nudge", r.nudgesSent, "max", nudgeMax())
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
		case <-time.After(nudgeReplyWindow()):
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
