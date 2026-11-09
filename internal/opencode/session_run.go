package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// sessionRun is the per-execution state for the session transport: one persistent opencode session,
// driven entirely through the serve's HTTP+SSE API instead of a one-shot `opencode run`.
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
	// probeGracePending is set while a completion-probe grace timer is armed: session.idle fired
	// without the decision marker.
	probeGracePending bool

	// Session-scoped nudge tuning (manifest value first, env fallback).
	nudgeMaxVal         int
	nudgeReplyWindowVal time.Duration
	nudgeCooldownVal    time.Duration

	// tuningInit records that the three fields above hold RESOLVED values (initNudgeTuning ran).
	tuningInit bool

	// In-flight tool-hang watchdog (Tier A): a tool call with no events for longer than
	// toolHangWindowVal is aborted-and-redirected.
	toolHangWindowVal time.Duration
	hangLatched       bool
	hangMu            sync.Mutex
	// tool tracking for the hang watchdog: the currently in-flight tool (name + last-activity time).
	toolTrackName   string
	toolTrackAt     time.Time
	toolInFlightNow bool
	// v2Tools remembers what 2.x announces in SEPARATE events from the one that resolves a call: the
	// tool NAME (session.tool.input.started) and its INPUT (session.tool.called).
	v2Tools map[string]v2ToolAnnounce
	// workStarted (probe-startup guard, 2026-09-09): set true the first time the session produces WORK
	// evidence — a tool call or any substantive text output (a non-empty completed text part beyond.
	workStarted bool
	// hangAbortAt records the last Abort WE initiated (tool-hang or stream-retry).
	hangAbortAt time.Time

	// streamRetries counts same-session stream-drop turn retries (Tier B), bounded by maxStreamRetries
	// before falling through to the existing kill path.
	streamRetries int
	// eventGen counts routed bus events; lastIdleAt marks the last session.idle.
	eventGen   int64
	lastIdleAt time.Time

	// Durable transcript (execution_session_parts): recorded as events
	// arrive, flushed in batches by a background goroutine and at finish.
	store        SessionStoreFunc
	muParts      sync.Mutex
	seq          int64
	pendingParts []db.SessionPart

	// thinkState demuxes folded think blocks out of completed text parts (think_demux.go): GLM-style
	// models emit thinking INLINE wrapped in the "think" tag pair.
	thinkState *completedThinkDemux

	// Unified warn→escalate→abort budget ladder (see compact.go).
	budget            *budgetAccumulator
	budgetSpec        budgetSpec
	startedAt         time.Time
	firedWarn         [dimCount][3]bool
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
	// defaultCompletionProbeGrace is how long the completion probe waits after a markerless
	// session.idle before interjecting.
	defaultCompletionProbeGrace = 30 * time.Second
	// maxStreamRetries bounds Tier B same-session stream-drop turn retries
	// before falling through to the existing kill path.
	maxStreamRetries = 2
	// hangAbortEchoWindow is how long after an Abort WE initiated a `session.error: Aborted` echo is
	// ignored (it is our own cancel landing, not session death).
	hangAbortEchoWindow = 30 * time.Second
	// defaultStreamDropQuiet is how long the SSE reconnect-drop detector waits for fresh events on the
	// new subscription before concluding the turn was cut mid-stream and retrying it.
	defaultStreamDropQuiet = 30 * time.Second
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

func completionProbeGrace() time.Duration {
	return envDuration("ORCHICON_COMPLETION_PROBE_GRACE", defaultCompletionProbeGrace)
}

// initNudgeTuning resolves the session's nudge knobs: manifest (tenant settings) value first,
// env-var fallback, then code default.
func toolHangWindow() time.Duration {
	if v := os.Getenv("ORCHICON_STALL_TOOL_HANG_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	if v := os.Getenv("ORCHICON_TOOL_HANG_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 180 * time.Second
}

func (r *sessionRun) initNudgeTuning() {
	r.nudgeMaxVal = nudgeMax()
	r.nudgeReplyWindowVal = nudgeReplyWindow()
	r.nudgeCooldownVal = nudgeCooldown()
	r.toolHangWindowVal = toolHangWindow()
	// From here the fields above hold RESOLVED values, so the accessors can tell
	// an explicit 0 (DISABLED) from "never initialised".
	r.tuningInit = true
	// Manifest fields are POINTERS: nil = the tenant left it BLANK, so the env/code default above
	// stands; non-nil = an explicit value, and 0 means DISABLED.
	if m := r.manifest.StallNudgeMax; m != nil && os.Getenv("ORCHICON_STALL_NUDGE_MAX") == "" {
		r.nudgeMaxVal = int(*m)
	}
	if m := r.manifest.StallNudgeReplyWindowSeconds; m != nil && os.Getenv("ORCHICON_STALL_NUDGE_REPLY_WINDOW") == "" {
		r.nudgeReplyWindowVal = time.Duration(*m) * time.Second
	}
	if m := r.manifest.StallNudgeCooldownSeconds; m != nil && os.Getenv("ORCHICON_STALL_NUDGE_COOLDOWN") == "" {
		r.nudgeCooldownVal = time.Duration(*m) * time.Second
	}
	// Tool-hang watchdog: nil keeps the env/code default (180s); an explicit 0
	// resolves to a zero duration, which the watchdog gates on — disabled.
	if m := r.manifest.StallToolHangSeconds; m != nil && os.Getenv("ORCHICON_STALL_TOOL_HANG_WINDOW") == "" && os.Getenv("ORCHICON_TOOL_HANG_WINDOW") == "" {
		r.toolHangWindowVal = time.Duration(*m) * time.Second
	}
}

// startToolHangWatchdog arms the in-flight tool-hang watchdog for this session: a background
// goroutine that watches for a tool call that has been silent for longer than toolHangWindowVal.
func (r *sessionRun) startToolHangWatchdog(ctx context.Context) func() {
	stop := make(chan struct{})
	go func() {
		// Poll every second; the watchdog is cheap and the window is
		// seconds-scale, so a 1s poll gives tight latencies.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-r.done:
				return
			case <-t.C:
				r.checkToolHang()
			}
		}
	}()
	return func() { close(stop) }
}

// checkToolHang is the Tier A ACTUATOR for the progressMonitor tool-hang signal: when a tool call
// has produced no events for longer than the window, it latches once and (a) records the hang.
func (r *sessionRun) checkToolHang() {
	r.hangMu.Lock()
	if r.finished || r.hangLatched || r.toolHangWindowVal <= 0 {
		r.hangMu.Unlock()
		return
	}
	r.hangMu.Unlock()

	// Detection is owned by the progressMonitor (pure, clock-injectable).
	// The hangMu fallback below covers runs/tests without a monitor.
	var tool string
	var tripped bool
	if r.monitor != nil {
		tool, tripped = r.monitor.checkToolHang()
	} else {
		if !r.toolInFlight() {
			return
		}
		if time.Since(r.lastToolActivity()) <= r.toolHangWindowVal {
			return
		}
		tool, tripped = r.lastToolName(), true
	}
	if !tripped {
		return
	}
	// The latch lives in actuateToolHang (single owner): pre-latching here would make the actuator
	// below a no-op and the redirect would never send.
	r.a.log.Warn("tool hang detected — aborting in-flight turn and redirecting",
		"execution", r.execRow.ID, "tool", tool, "window", r.toolHangWindowVal)
	r.callbacks.OnStall(r.parentCtx, r.execRow.ID, "stalled:tool_hang:"+tool, false)

	r.actuateToolHang(tool)
}

// actuateToolHang performs the Tier A abort-and-redirect intervention, latched once per execution.
func (r *sessionRun) actuateToolHang(tool string) {
	r.hangMu.Lock()
	if r.finished || r.hangLatched {
		r.hangMu.Unlock()
		return
	}
	r.hangLatched = true
	r.hangMu.Unlock()
	if tool == "" {
		tool = "unknown"
	}
	if r.client != nil && r.sessionID != "" {
		if err := r.client.Abort(r.parentCtx, r.sessionID); err != nil {
			r.a.log.Warn("tool-hang abort failed (redirect still queued)", "execution", r.execRow.ID, "error", err)
		} else {
			r.hangMu.Lock()
			r.hangAbortAt = time.Now()
			r.hangMu.Unlock()
		}
	}
	redirect := toolHangRedirectMessage(tool, r.toolHangWindowVal)
	if r.client != nil {
		if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, redirect); err != nil {
			r.a.log.Warn("tool-hang redirect send failed", "execution", r.execRow.ID, "error", err)
			return
		}
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": redirect, "source": "tool_hang_redirect"})
}

// observeToolStart records that a tool call began (called from handleEvent on the raw tool-start
// event).
func (r *sessionRun) observeToolStart(name string) {
	r.hangMu.Lock()
	// A v2 `session.tool.called` start carries NO name (2.x puts it on input.started), so an empty one
	// must not erase a name already remembered.
	if name != "" {
		r.toolTrackName = name
	}
	r.toolTrackAt = time.Now()
	r.toolInFlightNow = true
	// First tool call = the session has demonstrably begun its work. The
	// completion-probe gate arms from here (probe-startup guard).
	r.workStarted = true
	r.hangMu.Unlock()
	if r.monitor != nil {
		r.monitor.observeToolStart(name)
	}
}

// observeToolEnd records that the in-flight tool produced a completion event (or the call
// resolved); the hang window resets.
func (r *sessionRun) observeToolEnd() {
	r.hangMu.Lock()
	r.toolInFlightNow = false
	r.hangMu.Unlock()
	if r.monitor != nil {
		r.monitor.observeToolEnd()
	}
}

// noteToolActivity refreshes the Tier A hang window without disarming: the in-flight call (or the
// session around it) produced an event, so the zero-events-only trip condition restarts.
func (r *sessionRun) noteToolActivity() {
	if r.monitor != nil {
		r.monitor.noteToolActivity()
		return
	}
	r.hangMu.Lock()
	if r.toolInFlightNow {
		r.toolTrackAt = time.Now()
	}
	r.hangMu.Unlock()
}

func (r *sessionRun) toolInFlight() bool {
	r.hangMu.Lock()
	defer r.hangMu.Unlock()
	return r.toolInFlightNow
}

func (r *sessionRun) lastToolActivity() time.Time {
	r.hangMu.Lock()
	defer r.hangMu.Unlock()
	return r.toolTrackAt
}

func (r *sessionRun) lastToolName() string {
	r.hangMu.Lock()
	defer r.hangMu.Unlock()
	if r.toolTrackName == "" {
		return "unknown"
	}
	return r.toolTrackName
}

// v2ToolAnnounce is what 2.x tells us about a tool call BEFORE it resolves: the
// name (from session.tool.input.started) and the input (from session.tool.called).
type v2ToolAnnounce struct {
	name  string
	input any
}

// rememberV2Tool records what a 2.x tool event announced, ignoring absent fields
// so the separate events ACCUMULATE rather than overwrite one another.
func (r *sessionRun) rememberV2Tool(id, name, input any) {
	sid, _ := id.(string)
	if sid == "" {
		return
	}
	r.hangMu.Lock()
	defer r.hangMu.Unlock()
	if r.v2Tools == nil {
		r.v2Tools = map[string]v2ToolAnnounce{}
	}
	cur := r.v2Tools[sid]
	if n, _ := name.(string); n != "" {
		cur.name = n
	}
	if input != nil {
		cur.input = input
	}
	r.v2Tools[sid] = cur
}

// fillV2Tool applies the remembered name and input to a resolved 2.x tool_use part and forgets the
// entry (so the map holds only in-flight calls).
func (r *sessionRun) fillV2Tool(legacy map[string]any) {
	if t, _ := legacy["type"].(string); t != evtToolUse {
		return
	}
	part, _ := legacy["part"].(map[string]any)
	if part == nil {
		return
	}
	sid, _ := part["callID"].(string)
	if sid == "" {
		return
	}
	r.hangMu.Lock()
	ann, ok := r.v2Tools[sid]
	if ok {
		delete(r.v2Tools, sid)
	}
	r.hangMu.Unlock()
	if !ok {
		return
	}
	if _, has := part["tool"]; !has && ann.name != "" {
		part["tool"] = ann.name
	}
	if state, _ := part["state"].(map[string]any); state != nil {
		if _, has := state["input"]; !has && ann.input != nil {
			state["input"] = ann.input
		}
	}
}

// toolHangRedirectMessage builds the course-correcting redirect injected
// when a tool call exceeds the hang window.
func toolHangRedirectMessage(tool string, window time.Duration) string {
	return "Your tool call " + tool + " exceeded the " + window.String() +
		" tool-hang window with no events and was cancelled (cancelled: tool exceeded tool-hang window). " +
		"Re-harness it: kill or supersede the stuck call and retry with a timeout, " +
		"background-and-poll, or a bounded retry — then move on. Do not repeat the identical hung call."
}

// nudgeMax returns the session's resolved nudge budget (manifest value first, env fallback).
func (r *sessionRun) nudgeMax() int {
	if r.tuningInit {
		return r.nudgeMaxVal
	}
	return nudgeMax()
}

// nudgeReplyWindow returns the session's resolved nudge reply window.
func (r *sessionRun) nudgeReplyWindow() time.Duration {
	if r.tuningInit {
		return r.nudgeReplyWindowVal
	}
	return nudgeReplyWindow()
}

// nudgeCooldown returns the session's resolved nudge cooldown.
func (r *sessionRun) nudgeCooldown() time.Duration {
	if r.tuningInit {
		return r.nudgeCooldownVal
	}
	return nudgeCooldown()
}

// decisionMarker is the single marker signal every worker execution ends with (docs: the first word
// after it is success|failure).
const decisionMarker = "ORCHICON WORKER SUMMARY:"

// afterCompactReminderText is interjected into the session immediately after a mid-flight
// compaction (compact.go maybeCompact) so the model that just had its context summarized.
const afterCompactReminderText = "Your conversation was just compacted to keep it within budget — your task and progress notes are preserved in the compacted summary. " +
	"Continue working on your task exactly as before. " +
	"Resume your `todowrite` list NOW: emit a fresh full replacement array (`pending | in_progress | completed | cancelled`) reflecting current progress, and keep emitting it after every turn while you work — never wait until the end. " +
	"When you have actually finished — and only then — end your response with the literal line: " +
	"ORCHICON WORKER SUMMARY: success — <your summary of what you did>  (or  ORCHICON WORKER SUMMARY: failure — <the blocker>). " +
	"Do NOT emit that line as part of a plan; emit it only as your final sign-off when the deliverable is complete."

// placeholderMarkerBody reports whether the text following an ORCHICON WORKER SUMMARY marker is a
// doc/plan placeholder ("success — <summary>", "<reason>".
func placeholderMarkerBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	// Inline code (backtick-quoted) markers are seed/instruction echo, never a real sign-off.
	words := strings.Fields(rest)
	if len(words) > 0 {
		raw := words[0]
		before, _ := strings.CutPrefix(raw, "`")
		after, afterBacktick := strings.CutSuffix(before, "`")
		if afterBacktick {
			lower := strings.ToLower(after)
			if lower == "success" || lower == "failure" {
				return true
			}
		}
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

// realDecisionMarkerIn reports the index of the LAST real ORCHICON WORKER SUMMARY marker in output
// — one whose body is actual content, not a placeholder/template echo.
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

// probeReplyKind classifies the model's reply to a completion probe.
const (
	// probeReplyNone — no answer to the probe in the output (a bare status-line reply is NOT an answer:
	// it must not reset the probe budget.
	probeReplyNone = iota
	// probeReplySummary — the output carries a REAL decision marker.
	probeReplySummary
	// probeReplyWorking — the reply contains the standalone WORKING token:
	// the model's explicit "still working, leave me alone" answer.
	probeReplyWorking
)

// probeWorkingToken is the literal single-word answer the completion probe
// offers a still-working model. Word-bounded so "networking" never matches.
const probeWorkingToken = "working"

// completionProbeReply classifies whether the session output ANSWERS a completion probe:
// (probeReplySummary, idx>=0) for a real marker, (probeReplyWorking.
func completionProbeReply(output string) (int, int) {
	if idx := realDecisionMarkerIn(output); idx >= 0 {
		return probeReplySummary, idx
	}
	// WORKING token: word-bounded scan of the output TAIL only (the probe reply is by construction the
	// latest turn; earlier turns may legitimately contain the word).
	tail := output
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}
	lower := strings.ToLower(tail)
	search := 0
	for search < len(lower) {
		at := strings.Index(lower[search:], probeWorkingToken)
		if at < 0 {
			break
		}
		at += search
		before := byte(' ')
		if at > 0 {
			before = lower[at-1]
		}
		after := byte(' ')
		if at+len(probeWorkingToken) < len(lower) {
			after = lower[at+len(probeWorkingToken)]
		}
		isBoundary := func(b byte) bool {
			return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '.' || b == ',' || b == '!' || b == '?' || b == '"' || b == '\'' || b == '*' || b == '`' || b == '_' || b == '-' || b == ':' || b == ';'
		}
		if isBoundary(before) && isBoundary(after) {
			return probeReplyWorking, at
		}
		search = at + len(probeWorkingToken)
	}
	return probeReplyNone, -1
}

// completionProbeText is sent on session.idle when the worker's turn ended WITHOUT the decision
// marker — e.g. the final model response was truncated mid-stream (a step_finish with reason.
const completionProbeText = "Your response appears to have been cut off before your final ORCHICON WORKER SUMMARY was captured. " +
	"Please do not restart your work and do NOT reply with a status update. " +
	"Your NEXT reply must be one of exactly two things: " +
	"(1) your final summary, in this form: ORCHICON WORKER SUMMARY: success — <summary>  (or  failure — <reason>), or " +
	"(2) the single word WORKING (nothing else) if you still have work to do — you will then be left alone to continue working."

// streamRetryText is the short continue-turn re-prompt for a stream-drop retry (Tier B).
const streamRetryText = "Your previous turn was cut off mid-stream (model response stream truncated or event dropped). " +
	"Continue from where you stopped without repeating completed work. " +
	"If you had finished, reply with your final ORCHICON WORKER SUMMARY now."

func streamDropQuietWindow() time.Duration {
	return envDuration("ORCHICON_STREAM_DROP_QUIET_WINDOW", defaultStreamDropQuiet)
}

// run executes the whole session lifecycle.
func (r *sessionRun) run() error {
	client := r.client
	// The durable transcript writer must be set before any recordPart
	// (the session_info entry is the first one).
	r.store = r.a.sessionStore

	// The serve's published port can be refused for a beat after the docker-proxy binds it; retry the
	// session create briefly so a converging serve doesn't fail the execution.
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
	// Switch the session to THIS execution's worker agent.
	agentPromptCarried := false
	if client.AgentSelectionSupported() {
		agentName := adapter.WorkerAgentName(r.manifest.WorkerID)
		if aerr := client.SelectAgent(r.parentCtx, sid, agentName); aerr != nil {
			r.a.log.Warn("opencode session agent select failed — the worker runs on the serve's default agent and the message keeps the full prompt",
				"execution", r.execRow.ID, "session", sid, "agent", agentName, "error", aerr)
		} else if agentName != "" {
			agentPromptCarried = true
		}
	}
	// Persist the opencode session identity so the UI can show which serve/session a worker ran on
	// (troubleshooting + follow-up seed).
	r.recordPart(db.SessionPartSessionInfo, map[string]any{
		"session_id":   sid,
		"serve_url":    client.BaseURL(),
		"adapter_kind": adapter.KindOpencode,
	})
	// Persist the full system prompt sent to the worker (the per-message `system` field) so the session
	// chat can show exactly what the worker was told.
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

	// Progress monitor: same stall signals as the one-shot path (which shares the guardrails).
	r.initNudgeTuning()
	r.monitor = newProgressMonitor(r.execRow.ID, stallWindowsFromManifest(r.manifest))
	go r.monitor.run(r.parentCtx,
		func(_ string, reason string) { r.onStall(reason) },
		func(_, recovered string) { r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, recovered) },
	)
	defer r.monitor.close()

	// In-flight tool-hang watchdog (D6): cancels a silent tool call and
	// injects a course-correcting redirect, latched once per session.
	stopHangWatchdog := r.startToolHangWatchdog(r.parentCtx)
	defer stopHangWatchdog()

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

	// The RUN-LEVEL prefix travels here ONLY when the agent selected above did not take it: if the
	// agent carries it, it is trimmed off so the same text is not sent twice.
	goalSystem := r.system
	if agentPromptCarried {
		goalSystem = trimCarriedAgentPrompt(r.a.log, r.execRow.ID, goalSystem, r.manifest.AgentPromptLen)
	}
	if err := client.SendMessageCarryingSystem(r.parentCtx, sid, goalSystem, r.modelRef, r.manifest.Goal); err != nil {
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
	// A stream that ended mid-step or on a truncated final step is NOT a success — unless the output
	// still carries the decision signal (the completion probe may have salvaged it.
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
	// Props(), NOT Properties: the props live under `properties` on v1 and under `data` on v2, and
	// reading the wrong one yields an EMPTY map rather than an error.
	props := evt.Props()
	if sid, _ := props["sessionID"].(string); sid != "" && sid != r.sessionID {
		return
	}
	r.mu.Lock()
	r.eventGen++
	r.mu.Unlock()
	switch evt.Type {
	case "permission.asked":
		// Auto-approve (the server-side --auto equivalent) so tool calls
		// never block an unattended execution.
		if pid, _ := props["id"].(string); pid != "" {
			go func() { _ = r.client.ReplyPermission(r.parentCtx, r.sessionID, pid) }()
		}
		return
	case "session.status":
		// Lifecycle signal. The turn's END is a terminal event, not this.
		return
	}
	// THE TURN'S TERMINAL EVENTS, ON EITHER SURFACE — and the vocabulary lives in ONE place
	// (terminalTurnKind) rather than in these arms, because that is what makes it checkable.
	switch terminalTurnKind(evt.Type) {
	case turnEnd:
		r.turnEnded()
		return
	case turnFailure:
		r.recordStreamError(evt)
		return
	}
	// Mid-generation token deltas (streamed text/reasoning) are LIVENESS evidence: feed them to the
	// progress monitor so a long.
	if _, ok := TokenDeltaFromBus(evt); ok {
		r.resolveProbe()
		r.noteSessionProgress()
		// Token deltas are MODEL generation — the model only generates between tool calls, never while a
		// tool is in flight (it waits for the result).
		r.observeToolEnd()
		if r.monitor != nil {
			// observe("text") advances lastStepFinish — the no_progress signal.
			r.monitor.observe("text", nil)
		}
		return
	}
	// Feed the in-flight tool-hang watchdog (D6) from the RAW bus event, BEFORE the LegacyEventFromBus
	// completed/error filter.
	switch evt.Type {
	case "session.tool.input.started":
		r.rememberV2Tool(props["id"], props["name"], nil)
	case "session.tool.called":
		r.rememberV2Tool(props["id"], nil, props["input"])
	}
	if tool, isStart := ToolStartFromBus(evt); isStart {
		r.observeToolStart(tool)
	}
	if legacy, ok := LegacyEventFromBus(evt); ok {
		r.fillV2Tool(legacy)
		// ANY telemetry activity (text/tool/step/reasoning) after a probe is evidence the worker is alive —
		// resolve the probe and revive without waiting for a full turn (the false-positive case.
		r.resolveProbe()
		r.noteSessionProgress()
		// A resolved tool part (completed/error) disarms the hang window.
		if t, _ := legacy["type"].(string); t == evtToolUse {
			r.observeToolEnd()
		} else if t != "" {
			r.observeToolEnd()
		}
		// Folded-think segmentation (think_demux.go): a completed text part carrying a GLM-style inline
		// think block must not reach the output accumulator.
		evtType, _ := legacy["type"].(string)
		var thinkBodies []string
		if evtType == evtText {
			if part, _ := legacy["part"].(map[string]any); part != nil {
				if raw, _ := part["text"].(string); raw != "" {
					clean, bodies := r.splitThink(raw)
					thinkBodies = bodies
					part["text"] = clean
				}
			}
		}
		hasCleanText := true
		if evtType == evtText && len(thinkBodies) > 0 {
			if part, _ := legacy["part"].(map[string]any); part != nil {
				if txt, _ := part["text"].(string); txt == "" {
					hasCleanText = false
				}
			}
		}
		if hasCleanText {
			r.a.parseEvent(r.parentCtx, r.execRow, r.manifest, legacy, r.callbacks,
				r.monitor, &r.output, &r.lastStreamErr, &r.textSeq, r.stats, r.budget)
			// Record the raw part for the durable transcript, with the tool OUTPUT capped like the live forward
			// (a follow-up or a recovery-resumed session re-seeds this transcript as context.
			if t, _ := legacy["type"].(string); t != "" {
				r.recordPart(t, map[string]any{"part": r.a.capPartOutput(legacy["part"], r.execRow.ID, executionDir(r.manifest)), "error": legacy["error"]})
			}
		}
		for _, body := range thinkBodies {
			thinkLegacy := map[string]any{"type": evtReasoning, "part": map[string]any{"text": body}}
			r.a.parseEvent(r.parentCtx, r.execRow, r.manifest, thinkLegacy, r.callbacks,
				r.monitor, &r.output, &r.lastStreamErr, &r.textSeq, r.stats, r.budget)
			r.recordPart(evtReasoning, map[string]any{"part": r.a.capPartOutput(thinkLegacy["part"], r.execRow.ID, executionDir(r.manifest))})
		}
		// Unified warn→escalate→abort budget ladder, evaluated on its own event boundary: step_finish feeds
		// the spend accumulator and then drives the tokens/cost/time dimensions (the subsequent turn.
		switch et, _ := legacy["type"].(string); et {
		case evtStepFinish:
			// Tier B (b): a truncated mid-turn step-finish (reason unknown/empty + zero tokens) is a stream
			// drop, not a clean turn end.
			if part, _ := legacy["part"].(map[string]any); isTruncatedStepFinish(part) {
				r.mu.Lock()
				pend, fin := r.pendingTurns, r.finished
				r.mu.Unlock()
				if !fin && pend > 0 {
					r.a.log.Warn("truncated step-finish mid-turn — retrying turn on same session",
						"execution", r.execRow.ID)
					r.retryStreamTurn("truncated step_finish mid-turn (reason unknown/empty, zero tokens)")
				}
			}
			r.maybeEnforceLadder(dimTokens)
			r.maybeEnforceLadder(dimCost)
			r.maybeEnforceLadder(dimTime)
			r.maybeCompact() // context-hygiene turn-count gate only
		case evtToolUse:
			r.maybeEnforceLadder(dimTools)
		}
	}
}

// splitThink demuxes folded think blocks out of one completed text part (think_demux.go).
func (r *sessionRun) splitThink(text string) (string, []string) {
	if r.thinkState == nil {
		r.thinkState = newCompletedThinkDemux()
	}
	return r.thinkState.segment(text)
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

// resolveProbe clears a pending liveness probe and revives the execution (the advisory stall notice
// clears and every stall window restarts).
func (r *sessionRun) resolveProbe() {
	r.mu.Lock()
	if !r.probePending {
		// No probe is outstanding, but a completion-probe grace may be armed (session.idle fired without
		// the marker and we are waiting for the trailing final text to land).
		r.probeGracePending = false
		r.mu.Unlock()
		return
	}
	r.probePending = false
	r.probeGracePending = false
	r.lastNudgeAt = time.Now()
	// 2026-09-09 probe-loop fix (transcript 01M23C2MTF1ZYYHE8ACK17PMKV, native parity): the budget does
	// NOT reset on mere activity.
	if kind, idx := completionProbeReply(r.output.String()); kind != probeReplyNone && idx >= 0 {
		if kind == probeReplySummary || kind == probeReplyWorking {
			r.nudgesSent = 0
		}
	}
	revived := r.monitor.revive()
	r.mu.Unlock()
	if revived {
		r.callbacks.OnRecovered(r.parentCtx, r.execRow.ID, "recovered:liveness_probe")
		r.a.log.Info("liveness probe resolved — revived", "execution", r.execRow.ID)
	}
}

// allTurnsDone forces the outstanding-turn counter to zero and settles — called from session.idle,
// which the server emits only once every queued prompt has been answered.
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

// settleFinish finishes the execution after a short delay if no new message has arrived (a
// just-sent human message bumps pendingTurns and cancels the settle).
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

// sessionErrorMessage extracts the human-readable reason from a session.error bus event.
func sessionErrorMessage(evt BusEvent) string {
	// Props(), NOT Properties — see handleEvent: on v2 the props live under `data`, so reading
	// Properties here would lose the reason and report a generic "opencode session error" for exactly.
	props := evt.Props()
	if errObj, ok := props["error"].(map[string]any); ok {
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

// recordStreamError handles a session.error bus event: the turn failed at the model/API level.
func (r *sessionRun) recordStreamError(evt BusEvent) {
	msg := sessionErrorMessage(evt)
	r.a.log.Warn("opencode session error", "execution", r.execRow.ID, "message", msg)
	// Abort-echo guard: when the session was already terminated (e.g. a fatal stall recorded its reason
	// and Abort'd the session), the `session.error: Aborted` echo must not re-mark health.
	if r.isFinished() {
		return
	}
	// Our-own-Abort echo guard (Tier A/B): an `Aborted` error arriving within hangAbortEchoWindow of an
	// Abort WE initiated is the serve echoing our own cancel — not session death.
	if isAbortEcho(msg) && r.recentOwnAbort() {
		r.a.log.Info("ignoring own-abort echo", "execution", r.execRow.ID)
		return
	}
	// Tier B: a prematurely-ended model stream is retried against the same
	// session (bounded) instead of failing the execution.
	if isStreamDropError(msg) {
		r.retryStreamTurn(msg)
		return
	}
	r.mu.Lock()
	if r.lastStreamErr == "" {
		r.lastStreamErr = msg
	}
	r.mu.Unlock()
	r.callbacks.OnHealth(r.parentCtx, r.execRow.ID, "unhealthy")
	if isInfraModelTurnError(msg) {
		r.recycleOnInfraModelTurn(msg)
	} else {
		r.recycleOnWedgedServe(msg)
	}
	r.finish(false, "opencode_session_error: "+msg)
}

// isAbortEcho reports whether a session.error message is the serve echoing a turn abort (our own
// Abort or a user-initiated cancel).
func isAbortEcho(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	return strings.Contains(m, "abort") || strings.Contains(m, "cancel")
}

// recentOwnAbort reports whether WE initiated an Abort within
// hangAbortEchoWindow (tool-hang or stream-retry actuator).
func (r *sessionRun) recentOwnAbort() bool {
	r.hangMu.Lock()
	defer r.hangMu.Unlock()
	return !r.hangAbortAt.IsZero() && time.Since(r.hangAbortAt) <= hangAbortEchoWindow
}

// isStreamDropError classifies a session.error message as a premature stream end (truncation /
// dropped events / transport reset) rather than a clean error.
func isStreamDropError(msg string) bool {
	if msg == "" || isAbortEcho(msg) {
		return false
	}
	m := strings.ToLower(msg)
	// Guard: server-decision / per-request rejections fail cleanly.
	for _, term := range []string{
		"http 4", "unauthorized", "forbidden", "permission",
		"rate limit", "429", "insufficient", "quota", "policy",
		"invalid api key", "authentication", "not found", "404",
	} {
		if strings.Contains(m, term) {
			return false
		}
	}
	for _, sig := range []string{
		"truncat", "event dropped", "stream dropped", "stream reset",
		"stream closed", "stream ended", "connection reset", "eof",
		"broken pipe", "use of closed network", "unexpected eof",
		"context deadline exceeded", "incomplete stream", "partial stream",
		"message stream", "sse disconnect",
	} {
		if strings.Contains(m, sig) {
			return true
		}
	}
	// "stream" + "drop"/"cut"/"interrupt" in combination.
	if strings.Contains(m, "stream") && (strings.Contains(m, "drop") || strings.Contains(m, "cut") || strings.Contains(m, "interrupt") || strings.Contains(m, "lost")) {
		return true
	}
	return false
}

// isTruncatedStepFinish reports whether a step-finish part is a truncated mid-turn end: reason
// unknown/empty with zero tokens (the signature of an interrupted response.
func isTruncatedStepFinish(part map[string]any) bool {
	if part == nil {
		return false
	}
	reason, _ := part["reason"].(string)
	if reason != "unknown" && reason != "" {
		return false
	}
	tokens, _ := part["tokens"].(map[string]any)
	return allTokensZero(tokens)
}

// retryStreamTurn retries a dropped turn against the SAME session (Tier B): Abort any
// still-in-flight turn, then send the short continue-turn so the model resumes where it stopped.
func (r *sessionRun) retryStreamTurn(reason string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	if r.streamRetries >= maxStreamRetries {
		r.mu.Unlock()
		r.a.log.Warn("stream-drop retry budget spent — failing", "execution", r.execRow.ID, "retries", r.streamRetries)
		r.mu.Lock()
		if r.lastStreamErr == "" {
			r.lastStreamErr = reason
		}
		r.mu.Unlock()
		r.callbacks.OnHealth(r.parentCtx, r.execRow.ID, "unhealthy")
		r.finish(false, "opencode_session_error: "+reason)
		if r.client != nil && r.sessionID != "" {
			_ = r.client.Abort(r.parentCtx, r.sessionID)
		}
		return
	}
	r.streamRetries++
	n := r.streamRetries
	r.mu.Unlock()

	r.a.log.Warn("stream drop — retrying turn on same session", "execution", r.execRow.ID, "retry", n, "max", maxStreamRetries, "reason", reason)
	if r.client != nil && r.sessionID != "" {
		// Only abort when a turn is plausibly still in flight; Abort on an
		// idle session is a harmless no-op server-side.
		if err := r.client.Abort(r.parentCtx, r.sessionID); err != nil {
			r.a.log.Warn("stream-retry abort failed (retry still queued)", "execution", r.execRow.ID, "error", err)
		} else {
			r.hangMu.Lock()
			r.hangAbortAt = time.Now()
			r.hangMu.Unlock()
		}
		if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, streamRetryText); err != nil {
			r.a.log.Warn("stream-retry send failed — failing", "execution", r.execRow.ID, "error", err)
			r.finish(false, "opencode_session_error: "+reason)
			return
		}
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": streamRetryText, "source": "stream_retry"})
}

// recycleOnWedgedServe recycles the workflow's runtime container after a run of CONSECUTIVE
// model-layer session errors.
func (r *sessionRun) recycleOnWedgedServe(msg string) {
	threshold := sessionErrorRecycleThreshold()
	if threshold < 1 || r.manifest.RuntimeWorkflowID == "" || r.a.rt == nil {
		return
	}
	if !r.a.countSessionError() {
		return
	}
	sessionRecycleMetricsSingleton.recordWedgedConsecutive()
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

// recycleOnInfraModelTurn recycles the workflow's runtime container on the FIRST model-turn failure
// whose root is a socket/transport connect error (see isInfraModelTurnError).
func (r *sessionRun) recycleOnInfraModelTurn(msg string) {
	budget := sessionRepairBudget()
	if budget < 1 || r.manifest.RuntimeWorkflowID == "" || r.a.rt == nil {
		return
	}
	if r.a.infraModelTurnRecycleCount() >= budget {
		r.a.log.Warn("not recycling — infra model-turn repair budget exhausted",
			"run", r.manifest.RuntimeWorkflowID, "execution", r.execRow.ID,
			"budget", budget, "error", msg)
		sessionRecycleMetricsSingleton.recordInfraModelTurn(true)
		return
	}
	r.a.incrInfraModelTurnRecycles()
	r.a.log.Warn("recycling runtime container after infra model-turn connect failure",
		"run", r.manifest.RuntimeWorkflowID, "execution", r.execRow.ID,
		"budget", budget, "error", msg)
	sessionRecycleMetricsSingleton.recordInfraModelTurn(false)
	// Kill removes the container; the next dispatch's Create rebuilds it with
	// a fresh serve + store. Best-effort — a failed recycle is just a log.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.a.rt.Kill(ctx, r.manifest.RuntimeWorkflowID); err != nil {
		r.a.log.Warn("runtime container recycle failed", "run", r.manifest.RuntimeWorkflowID, "error", err)
	}
}

// countSessionError increments the consecutive session-error counter and returns true once the
// recycle threshold is reached (the counter is then reset so a still-wedged serve re-arms on later.
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

// infraModelTurnRecycleCount returns the current per-dispatch
// infra-model-turn recycle count (test helper).
func (a *Adapter) infraModelTurnRecycleCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.infraModelTurnRecycles
}

// incrInfraModelTurnRecycles increments the per-dispatch infra-model-turn
// recycle count.
func (a *Adapter) incrInfraModelTurnRecycles() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.infraModelTurnRecycles++
}

// noteSessionProgress resets the consecutive-session-error counter and the infra-model-turn
// per-dispatch recycle budget on any non-error session progress (a step, tool call.
func (r *sessionRun) noteSessionProgress() {
	r.a.mu.Lock()
	r.a.consecutiveSessionErrors = 0
	r.a.infraModelTurnRecycles = 0
	r.a.mu.Unlock()
}

// maybeCompact evaluates the soft-first compact gate at a quiet step boundary (after a step_finish
// feeds the spend accumulator).
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
	if !turnTriggered {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.doCompact(steps, "turn_count")
}

// doCompact performs one best-effort context compaction (the summarize round-trip) and, on success,
// re-states the summary contract so the resumed turns finish correctly.
func (r *sessionRun) doCompact(steps int, reason string) {
	r.mu.Lock()
	if r.compactsPerformed >= compactMax() {
		r.mu.Unlock()
		return
	}
	r.compactsPerformed++
	r.lastCompactStep = steps
	r.mu.Unlock()

	provider, model, ok := adapter.SplitForServe(r.modelRef)
	if !ok {
		r.a.log.Warn("compact skipped: malformed model ref",
			"execution", r.execRow.ID, "modelRef", r.modelRef)
		return
	}
	r.a.log.Info("compact triggering", "execution", r.execRow.ID, "step", steps, "reason", reason,
		"provider", provider, "model", model)
	if err := r.client.Compact(r.parentCtx, r.sessionID, provider, model); err != nil {
		r.a.log.Warn("compact failed (best-effort)", "execution", r.execRow.ID, "error", err)
		return
	}
	r.recordPart(db.SessionPartCompacted, map[string]any{
		"step": steps, "reason": reason, "provider": provider, "model": model,
	})
	r.sendAfterCompactReminder(steps)
}

// sendAfterCompactReminder re-states the summary contract + todowrite
// obligation after a lossy compact so the resumed turns finish correctly.
func (r *sessionRun) sendAfterCompactReminder(steps int) {
	// CARRIES THE SYSTEM PROMPT, and after a compact that is the point of it.
	if err := r.client.SendMessageCarryingSystem(r.parentCtx, r.sessionID, r.system, r.modelRef, afterCompactReminderText); err != nil {
		r.a.log.Warn("post-compact reminder send failed (best-effort)",
			"execution", r.execRow.ID, "error", err)
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": afterCompactReminderText, "source": "compact_reminder"})
	r.a.log.Info("post-compact reminder sent", "execution", r.execRow.ID, "step", steps)
}

// maybeEnforceLadder evaluates the UNIFIED warn→escalate→abort ladder for
// one dimension (tokens/cost/time on a step boundary; tool-calls on a tool
// use). It is the single enforcement point for every budget dimension:
//
//	levelWarn     → inject the warning message once (compact if enabled)
//	levelEscalate → inject the message once (compact if enabled)
//	levelFinal    → inject the FINAL message once (compact if enabled)
//	levelAbort    → KILL the session and fail the execution (HARD)
//
// Every non-abort tier ALWAYS injects its warning message when crossed;
// whether it ALSO compacts is decided by the operator's per-tier
// compact_tiers policy AND the shared re-arm latch (canCompactNow). The
// default policy compacts only at escalate/final, not warn, and the re-arm
// latch prevents compacts from different triggers firing on consecutive
// steps — so the lossy collapse no longer repeatedly interrupts the worker
// mid-flight while the spend climbs. Each warning tier is latched in
// firedWarn so it fires exactly once per execution (a dimension that crosses
// 25% then 50% then 75% sends all three, but never re-sends one). abort is
// terminal and recovers via the same path onStall's fatal branch uses
// (finish() first, THEN Abort, so the true reason survives the serve's
// session.error echo).
func (r *sessionRun) maybeEnforceLadder(d budgetDimension) {
	r.mu.Lock()
	if r.finished || r.budget == nil {
		r.mu.Unlock()
		return
	}
	frac := r.budgetSpec.fraction(d, r.budget, time.Since(r.startedAt), r.stats.toolUses)
	level := r.budgetSpec.levelFor(d, frac)
	if level == levelNone {
		r.mu.Unlock()
		return
	}
	if level == levelAbort {
		r.mu.Unlock()
		r.abortForLadder(d)
		return
	}
	idx := levelIndex(level)
	if r.firedWarn[d][idx] {
		r.mu.Unlock()
		return
	}
	r.firedWarn[d][idx] = true
	msg := r.budgetSpec.message(d, level, frac)
	// Whether this tier ALSO compacts is gated on the operator's per-tier compact_tiers policy, the
	// per-dimension compact_dims policy (so e.g. a tool-call-budget warning never triggers a lossy.
	compactPolicy := r.budgetSpec.compactsAt(level) && r.budgetSpec.compactsDim(d)
	r.mu.Unlock()

	r.injectBudgetWarning(d, level, msg)
	if compactPolicy && r.canCompactNow() {
		r.doCompact(r.budget.steps, "budget_ladder:"+dimName(d))
	}
}

// canCompactNow reports whether a context compaction is currently allowed: under the per-execution
// cap, past the min-turn floor, and more than min-turn turns since the last compact.
func (r *sessionRun) canCompactNow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.budget == nil {
		return false
	}
	if r.compactsPerformed >= compactMax() {
		return false
	}
	steps := r.budget.steps
	minT := compactMinTurns()
	if steps < minT {
		return false
	}
	if r.compactsPerformed > 0 && steps < r.lastCompactStep+minT {
		return false
	}
	return true
}

// injectBudgetWarning sends one configured warning message into the live
// session (best-effort) and records it in the transcript.
func (r *sessionRun) injectBudgetWarning(d budgetDimension, level warnLevel, msg string) {
	r.a.log.Warn("budget warning injected", "execution", r.execRow.ID,
		"dimension", dimName(d), "level", levelIndex(level), "message", strings.TrimSpace(strings.Split(msg, "\n")[0]))
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, msg); err != nil {
		r.a.log.Warn("budget warning send failed (best-effort)", "execution", r.execRow.ID, "error", err)
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": msg, "source": "budget_warning"})
}

// abortForLadder is the HARD-abort terminal for a dimension that reached its ceiling.
func (r *sessionRun) abortForLadder(d budgetDimension) {
	reason := "budget_abort:" + dimName(d)
	r.a.log.Warn("budget limit reached — aborting session",
		"execution", r.execRow.ID, "dimension", dimName(d), "reason", reason)
	r.finish(false, reason)
	_ = r.client.Abort(r.parentCtx, r.sessionID)
}

// onStall is the progress monitor's stall callback.
func (r *sessionRun) onStall(reason string) {
	fatal := isFatalStall(reason)
	r.callbacks.OnStall(r.parentCtx, r.execRow.ID, reason, fatal)
	// Tier A: the tool-hang signal is owned by the abort-and-redirect actuator, NOT the nudge path — a
	// nudge would queue behind the hung turn and never land.
	if tool, ok := strings.CutPrefix(reason, "stalled:tool_hang:"); ok {
		r.a.log.Warn("tool hang via monitor — aborting in-flight turn and redirecting",
			"execution", r.execRow.ID, "tool", tool)
		r.actuateToolHang(tool)
		return
	}
	if fatal {
		r.a.log.Warn("fatal stall — aborting session", "execution", r.execRow.ID, "reason", reason)
		// Record the terminal reason FIRST so the true cause survives the `session.error: Aborted` echo
		// that the serve emits when Abort lands below — finish() is first-arrival-wins.
		r.finish(false, reason)
		_ = r.client.Abort(r.parentCtx, r.sessionID)
		return
	}
	r.maybeNudge(reason)
}

// stallNudgeMessages returns the escalating liveness/stall messages, one per nudge attempt (index
// 0..nudgeMax-1).
var stallNudgeMessages = []string{
	"WARNING: You have gone quiet and are not making progress. You MUST work quickly and finish your work. " +
		"Continue your task NOW — if you stall again your session will be KILLED.",
	"CRITICAL: You are STILL not making progress. This is your final warning. " +
		"You MUST work quickly and finish your work to avoid exceeding your budget — your session WILL BE KILLED if you do not continue immediately.",
}

// maybeNudge injects an escalating liveness warning when an advisory stall (text_loop / repetition
// / no_file_progress) trips and the nudge budget/cooldown allow it.
func (r *sessionRun) maybeNudge(reason string) {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	if r.nudgesSent >= r.nudgeMax() || now.Sub(r.lastNudgeAt) < r.nudgeCooldown() {
		// Nudge budget spent (or inside the cooldown) and the worker is still tripping the advisory signal
		// — escalate to fatal.
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
	idx := r.nudgesSent - 1
	if idx >= len(stallNudgeMessages) {
		idx = len(stallNudgeMessages) - 1
	}
	msg := stallNudgeMessages[idx]
	r.mu.Unlock()

	r.a.log.Info("advisory stall — sending escalating liveness warning",
		"execution", r.execRow.ID, "reason", reason, "nudge", r.nudgesSent, "max", r.nudgeMax())
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, msg); err != nil {
		r.mu.Lock()
		r.probePending = false
		r.mu.Unlock()
		r.a.log.Warn("liveness probe send failed — plain advisory", "execution", r.execRow.ID, "error", err)
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": msg, "source": "nudge"})

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

// completionProbeDecision is the pure decision core of the completion-probe logic (testable without
// a live session):
func completionProbeDecision(output string, nudgesSent int, lastNudgeAt, now time.Time, nudgeMax int, nudgeCooldown time.Duration) (probe bool, fail bool) {
	if realDecisionMarkerIn(output) >= 0 {
		return false, false
	}
	if nudgesSent >= nudgeMax {
		// Probe budget spent AND the marker is still absent: the worker was given every chance (probes +
		// full reply windows) and never delivered the sign-off — fail honestly.
		return false, true
	}
	if now.Sub(lastNudgeAt) < nudgeCooldown {
		// Inside the cooldown after the last nudge/probe.
		return true, false
	}
	return true, false
}

// maybeProbeCompletion decides whether a session that went idle WITHOUT the ORCHICON WORKER SUMMARY
// decision signal can still be completed.
func (r *sessionRun) maybeProbeCompletion() bool {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return false
	}
	// Probe-startup guard (2026-09-09, transcript 01M23C2MTF1ZYYHE8ACK17PMKV): the probe fired 6s after
	// dispatch, BEFORE the model had streamed a single token.
	r.hangMu.Lock()
	started := r.workStarted
	r.hangMu.Unlock()
	if !started {
		if realDecisionMarkerIn(r.output.String()) >= 0 {
			r.mu.Unlock()
			return false // marker present → the normal idle path settles
		}
		r.a.log.Info("session idle without decision marker before any work — startup guard, waiting (no probe)",
			"execution", r.execRow.ID)
		r.mu.Unlock()
		return true
	}
	probe, fail := completionProbeDecision(r.output.String(), r.nudgesSent, r.lastNudgeAt, time.Now(), r.nudgeMax(), r.nudgeCooldown())
	// A run that has been compacted is, by contract, mid-task: the compact fired on a cost / turn-count
	// breach DURING the work, and the post-compact reminder explicitly tells the worker to keep going.
	if r.compactsPerformed > 0 {
		r.mu.Unlock()
		if probe || fail {
			// Marker absent (probe or fail is only set on a markerless idle).
			r.a.log.Info("session idle without decision marker after compact — mid-task pause, waiting for the next turn",
				"execution", r.execRow.ID, "nudges", r.nudgesSent)
			return true
		}
		return false
	}
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

	// A probe is warranted, but do NOT fire it immediately.
	if !r.probeGracePending {
		r.probeGracePending = true
		grace := completionProbeGrace()
		r.mu.Unlock()

		r.a.log.Info("session idle without decision marker — arming completion-probe grace",
			"execution", r.execRow.ID, "nudge", r.nudgesSent, "max", r.nudgeMax(), "grace", grace)
		go func() {
			select {
			case <-r.done:
				return
			case <-time.After(grace):
			}
			r.mu.Lock()
			if r.finished || r.probePending {
				r.mu.Unlock()
				return
			}
			r.probeGracePending = false
			// The marker may have landed during the grace (the trailing final
			// text part arrived) — if so, settle normally instead of probing.
			probe2, fail2 := completionProbeDecision(r.output.String(), r.nudgesSent, r.lastNudgeAt, time.Now(), r.nudgeMax(), r.nudgeCooldown())
			r.mu.Unlock()
			if fail2 {
				r.a.log.Warn("session idle without decision marker after grace — failing",
					"execution", r.execRow.ID, "nudges", r.nudgesSent)
				r.finish(false, "stalled:missing_decision_signal:completion_probe_no_response")
				return
			}
			if !probe2 {
				// Marker present now — the trailing text landed during the grace.
				r.a.log.Info("decision marker landed during completion-probe grace — settling",
					"execution", r.execRow.ID)
				r.allTurnsDone()
				return
			}
			r.sendCompletionProbe()
		}()
		return true
	}
	r.mu.Unlock()
	return true
}

// sendCompletionProbe sends the completion-probe interjection (the "your response appears to have
// been cut off" message) once the session has been genuinely idle and markerless through.
func (r *sessionRun) sendCompletionProbe() {
	r.mu.Lock()
	if r.finished || r.probePending {
		r.mu.Unlock()
		return
	}
	r.nudgesSent++
	r.probePending = true
	r.lastNudgeAt = time.Now() // the cooldown measures THIS probe's reply window
	r.probeDeadline = time.Now().Add(r.nudgeReplyWindow())
	r.mu.Unlock()

	r.a.log.Info("session idle without decision marker — sending completion probe",
		"execution", r.execRow.ID, "nudge", r.nudgesSent, "max", r.nudgeMax())
	if err := r.client.SendMessage(r.parentCtx, r.sessionID, r.system, r.modelRef, completionProbeText); err != nil {
		r.mu.Lock()
		r.probePending = false
		r.mu.Unlock()
		r.a.log.Warn("completion probe send failed — failing", "execution", r.execRow.ID, "error", err)
		r.finish(false, "stalled:missing_decision_signal:completion_probe_no_response")
		return
	}
	r.bumpPending()
	r.recordPart(db.SessionPartUserMessage, map[string]any{"text": completionProbeText, "source": "nudge"})

	// Verdict: no reply within the window → fail.
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
		// Tier B (c): the SSE transport dropped.
		r.mu.Lock()
		pend := r.pendingTurns
		genBefore := r.eventGen
		idleBefore := r.lastIdleAt
		r.mu.Unlock()
		if pend > 0 {
			go func(g int64, idle time.Time) {
				select {
				case <-r.done:
					return
				case <-r.subCtx.Done():
					return
				case <-time.After(streamDropQuietWindow()):
				}
				r.mu.Lock()
				fresh := r.eventGen != g
				idleLanded := r.lastIdleAt.After(idle)
				pendNow, fin := r.pendingTurns, r.finished
				r.mu.Unlock()
				if fin || pendNow == 0 || fresh || idleLanded {
					return
				}
				r.a.log.Warn("sse disconnect with pending turns and no fresh events — retrying turn on same session",
					"execution", r.execRow.ID)
				r.retryStreamTurn("sse disconnect with pending turns and no fresh events")
			}(genBefore, idleBefore)
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

// trimCarriedAgentPrompt removes the run-level prompt prefix from the message when the selected
// agent already carries it.
func trimCarriedAgentPrompt(log *slog.Logger, execID, system string, n int) string {
	if n <= 0 || n >= len(system) {
		return system
	}
	rest := system[n:]
	if !strings.HasPrefix(rest, "# Task") {
		log.Warn("agent prompt length did not line up with the composite — sending the full prompt",
			"execution", execID, "agent_prompt_len", n)
		return system
	}
	return rest
}

// turnEnd and turnFailure are the two terminal classes an opencode stream can end on, and
// terminalTurnKind is the ONE table mapping a raw event type to them.
const (
	turnEnd     = "end"
	turnFailure = "failure"
)

func terminalTurnKind(evtType string) string {
	switch evtType {
	case "session.idle", "session.execution.succeeded":
		return turnEnd
	case "session.error", "session.execution.failed", "session.step.failed":
		return turnFailure
	}
	return ""
}

// turnEnded is the shared handling for a turn-end signal, so the arm that acts on it has
// exactly one implementation.
func (r *sessionRun) turnEnded() {
	r.mu.Lock()
	r.lastIdleAt = time.Now()
	r.mu.Unlock()
	r.resolveProbe()
	if r.maybeProbeCompletion() {
		return
	}
	r.allTurnsDone()
}
