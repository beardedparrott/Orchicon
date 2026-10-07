package askorchicon

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// Chat-turn stall detection (ADR-ASK-1). Worker executions run a
// progressMonitor; chat turns previously had none — a model that hangs
// mid-generation (provider silent, no events) or loops on tool calls
// produced text (so no timeout fired) but never a session.idle, and the
// collector sat until the 30-minute reply window. The user watched a
// spinner for what was perceptually forever.
//
// The chatStallMonitor mirrors the executions' no_progress and repetition
// signals but deliberately drops text_loop / no_file_diff: in brainstorm
// mode text output IS the work, and a long reasoning streak is legitimate.
// ANY activity resets the clock; only total silence or an identical-call
// loop trips.
//
//	no_progress: no text / reasoning / step_finish / tool_use for our
//	             session within the window (ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW, default 120s)
//	repetition:  the same tool_call signature repeated more than N times
//	             within the window (ORCHICON_ASK_STALL_REPETITION_COUNT / _WINDOW, default 5 / 300s)
//
// On a trip the collector calls SessionClient.Abort (the same abort the Stop
// button uses — it interrupts the model NOW) and fails the turn with a
// clear, retryable error. Unlike executions there is no recovery loop: the
// turn ends, the user retries or interjects.
const (
	defaultAskStallNoProgressWindow = 120 * time.Second
	defaultAskStallRepetitionCount  = 5
	defaultAskStallRepetitionWindow = 300 * time.Second
	// 2026-09-09 (operator: "the conversation session wedged on a tool
	// (bash) and could not be recovered after 2 attempt(s) — please
	// retry" killed live Ask sessions): the wedge detector fires when a
	// tool part has been open with NO further events for the window. The
	// serve emits NO streaming events while a tool runs (tool_part fires
	// once at issue, tool_use once at completion), so a legitimately slow
	// tool — a bash build/test, a gh merge, a long MCP call, 60-120s — is
	// INDISTINGUISHABLE from a hung tool for the whole window. 30s tripped
	// on every slow-but-healthy tool: the session was aborted, the same
	// message re-dispatched, the tool re-issued, tripped again, and the
	// turn FAILED (default reconnect budget 1) — the exact "wedged on a
	// tool (bash) and could not be recovered after 2 attempt(s)"
	// self-destruct. 120s still catches a genuinely hung tool (the
	// no_progress window is also 120s) without killing slow-but-alive
	// calls, and the recycle budget is raised so one recycle is never a
	// death sentence.
	defaultAskMCPToolWedgeWindow   = 120 * time.Second
	defaultAskMCPReconnectAttempts = 3
)

func askStallNoProgressWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskStallNoProgressWindow
}

// resolveChatStallNoProgressWindow returns the effective chat no-progress
// stall window. The tenant's stall_no_progress_window_seconds setting wins
// when set, UNLESS the ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW env override is
// pinned (env beats the DB setting — exactly the precedence executions use in
// stallWindowsFromManifest, so a chat turn and a worker execution on the same
// tenant agree on what "no progress" means). 0/unset falls back to the env
// override, then the 120s default.
// settingsSeconds is a POINTER: nil = the tenant left this blank, so the
// env/code default applies; non-nil = an explicit value, and 0 means DISABLED
// (a zero window, which the monitor reads as off).
func resolveChatStallNoProgressWindow(settingsSeconds *int64) time.Duration {
	if settingsSeconds != nil && os.Getenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW") == "" {
		return time.Duration(*settingsSeconds) * time.Second
	}
	return askStallNoProgressWindow()
}

func askStallRepetitionCount() int {
	if v := os.Getenv("ORCHICON_ASK_STALL_REPETITION_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultAskStallRepetitionCount
}

func askStallRepetitionWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_STALL_REPETITION_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskStallRepetitionWindow
}

// askMCPToolWedgeWindow bounds how long a single tool call may stay open (started
// on the serve bus, never resolved) before it is treated as an MCP wedge. The
// default is generous enough that a legitimately slow tool that still streams
// activity never trips it (any activity resets the clock), but far shorter than
// the 120s no_progress window so a wedged tool is healed, not silently timed
// out. Env override ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW.
func askMCPToolWedgeWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskMCPToolWedgeWindow
}

// askMCPReconnectAttempts bounds how many times a wedged session is recycled
// (abort + fresh seeded session + re-dispatch) within one turn before the turn
// is failed with a clear retryable error (no unbounded loop). Env override
// ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS.
func askMCPReconnectAttempts() int {
	if v := os.Getenv("ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultAskMCPReconnectAttempts
}

// chatStallMonitor tracks per-turn progress signals and detects stalls. It
// is fed the already-decoded telemetry events (text / reasoning /
// step_finish / tool_use) AFTER the message was accepted (sent == true) and
// only for events already filtered to our session id. The event goroutine
// and the stall ticker both touch it, so it is mutex-guarded.
type chatStallMonitor struct {
	// modelRef is the model this turn dispatched on (provider/model), carried
	// so the stall reason can name it — the single most useful diagnostic
	// when a turn wedges (a rate-limited or unavailable model looks exactly
	// like a "stuck" model to the user).
	modelRef string
	mu       sync.Mutex

	noProgressWindow time.Duration
	repetitionCount  int
	repetitionWindow time.Duration
	toolWedgeWindow  time.Duration
	now              func() time.Time

	// lastActivity advances on ANY activity signal (text, reasoning,
	// step_finish, tool_use) — a model that keeps producing output, even
	// long reasoning, is never reaped.
	lastActivity time.Time

	// openToolTime is when the current (unresolved) tool call was issued, and
	// openToolName its name. Zero value = no tool is open. A tool is closed by
	// a terminating event (step_finish, a completed tool_use, completed text).
	// The tool-wedge signal (ADR-0002 D2) fires when a tool has been open and
	// silent for toolWedgeWindow — the exact MCP-wedge signature no_progress
	// cannot see (tool_use-as-activity only fires on a COMPLETED tool, so a
	// wedged tool is invisible to it).
	openToolTime time.Time
	openToolName string

	// tool-call signature history for repetition detection.
	// signature (tool+args) → timestamps within the window.
	sigs map[string][]time.Time

	// fired latches a trip so the stall is reported once.
	fired bool

	// awaitingConsent gates the tool-wedge signal while a consent ask is
	// outstanding: the tool call behind the ask is HELD BY THE HUMAN, not
	// wedged, so reclaiming it (toolWedge) would kill the very call the user
	// is deciding on. Cleared when the ask is answered or the turn ends.
	awaitingConsent bool

	// locallyBounded names the tools this turn's transport executes ITSELF, on the process's own clock, so
	// that a silent call is not treated as a wedged one. Empty means the wedge inference applies to every
	// tool, which is the correct default for a transport that dispatches them to a session serve.
	//
	// See setLocallyBoundedTools for why the distinction is the signal's whole validity condition.
	locallyBounded map[string]bool
}

// setLocallyBoundedTools marks the tool NAMES whose calls this turn's transport executes in-process under its
// own hard deadline.
//
// THE WEDGE SIGNAL IS AN INFERENCE FROM ABSENCE — "this call has been issued and silent past the window, and
// no completion came, so it is wedged" — and it is only sound where absence is the only evidence available.
// That is true of a call dispatched to a session serve, which is what the signal was built for (AC1, an MCP
// tool whose serve never answers). It is NOT true of a call the transport is running itself: the transport
// holds the call, and for the host suite's bash the deadline is its own (bashTimeoutDefault 120s,
// bashTimeoutMax 600s), enforced by exec.CommandContext.
//
// WHAT THE FALSE POSITIVE COST: the wedge is healed by RECYCLING — abort the session, create a fresh one,
// re-dispatch the same message — so every false trip erases the turn's in-session context. The prod plane's
// log held 13 of them in two days, every one tool="bash" on the native (in-process) transport, each firing
// 395-894s after the last permission ask was raised, i.e. on a shell command that was simply still running.
// The operator: "The model is constantly losing its brain. It doesn't know it's already done things and then
// tries to do them again."
//
// TOOLS NOT NAMED HERE KEEP THE WEDGE, deliberately: the native transport also runs MCP tools, which leave
// this process and can genuinely wedge, so recovery for the case the signal exists for is preserved.
func (m *chatStallMonitor) setLocallyBoundedTools(names []string) {
	if m == nil || len(names) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locallyBounded == nil {
		m.locallyBounded = make(map[string]bool, len(names))
	}
	for _, n := range names {
		m.locallyBounded[n] = true
	}
}

// setAwaitingConsent arms/disarms the consent gate on the tool-wedge signal.
//
// DISARMING RESTARTS THE OPEN TOOL'S CLOCK, which is the whole reason this is not a plain assignment.
//
// A tool held by a human is NOT WEDGED: it has not been RUNNING, it has been WAITING, and this clock
// measures a tool's silence — not the operator's reading speed. The signal used to be merely SUPPRESSED
// while an ask was open (toolWedge returns early), while the clock kept ticking from the tool's start. So
// the moment a decision landed, an already-expired clock was re-read on the next tick (the ticker runs at
// ≤30s) and the collector declared an MCP wedge: it ABORTED the session, created a FRESH one and
// re-dispatched the same message, which the operator experiences as the model losing its memory.
//
// It is the operator's "The model is constantly losing its brain. It doesn't know it's already done things
// and then tries to do them again", measured in the prod plane's own log: every one of 13
// "session wedged on a tool — recycling to a fresh session" entries named bash — the tool that raises the
// asks — and each fired 395-894s after the last ask was raised, i.e. 3-7x this window, because the human's
// wait was counted in full against a tool that had not started running yet.
//
// Only a tool that is ACTUALLY OPEN is restarted: with no tool in flight there is nothing to be silent, and
// arming one here would invent a wedge for the next tick to find.
func (m *chatStallMonitor) setAwaitingConsent(v bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	was := m.awaitingConsent
	m.awaitingConsent = v
	if was && !v {
		if !m.openToolTime.IsZero() {
			m.openToolTime = m.now()
		}
		// AND THE NO-PROGRESS CLOCK, for exactly the reason above — which this reset was MISSING.
		//
		// `lastActivity` keeps aging while the ask is open (nothing about an ask advances it), so the
		// instant the gate lifts an ALREADY-EXPIRED clock is re-read on the next tick and the turn dies
		// there instead. The death would move one tick, not go away — and it would still be attributed
		// to a consent the operator had just answered.
		//
		// Restarting it is the honest reading: the turn was not idle, it was WAITING, and the model's
		// own clock starts again from the decision. A model that then genuinely goes quiet is still
		// caught, a full window later.
		m.lastActivity = m.now()
	}
}

// closeTool marks the open tool call as RESOLVED, so it can never be judged a wedge.
//
// THE NATIVE ADAPTER NEEDS THIS AND observe() COULD NOT DO IT FOR IT. It resolves a tool with a typed
// "tool_result" event (name, args, output, error) and emits NO LegacyEventFromBus "tool_use" part, so the
// only closeTool paths that existed — the "text"/"reasoning"/"step_finish"/"tool_use" arms of observe —
// never ran for it. A bash call therefore left this slot armed from its START until the model's next text,
// which means any silent command longer than the wedge window was declared an MCP wedge and recycled the
// session with no ask involved at all: a build or a test suite was enough.
func (m *chatStallMonitor) closeTool() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openToolTime = time.Time{}
	m.openToolName = ""
}

// newChatStallMonitor builds a stall monitor for one chat turn.
// settingsNoProgressSeconds is the tenant's stall_no_progress_window_seconds
// (0 when unset/unknown); the effective window is resolved by
// resolveChatStallNoProgressWindow so a chat turn honors the same tenant
// setting executions do.
func newChatStallMonitor(modelRef string, settingsNoProgressSeconds *int64) *chatStallMonitor {
	return &chatStallMonitor{
		modelRef:         modelRef,
		noProgressWindow: resolveChatStallNoProgressWindow(settingsNoProgressSeconds),
		repetitionCount:  askStallRepetitionCount(),
		repetitionWindow: askStallRepetitionWindow(),
		toolWedgeWindow:  askMCPToolWedgeWindow(),
		now:              time.Now,
		lastActivity:     time.Now(),
		sigs:             make(map[string][]time.Time),
	}
}

// observe feeds one decoded telemetry event into the monitor. Only events
// belonging to THIS turn (post-accept, our session) must be fed. etype is
// the LegacyEventFromBus type string ("text", "reasoning", "step_finish",
// "tool_use", ...).
func (m *chatStallMonitor) observe(etype string, part map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	switch etype {
	case "text", "reasoning", "step_finish":
		m.lastActivity = now
		// A completed text/reasoning/step boundary closes any open tool: the
		// model moved past it, so it is not wedged (ADR-0002 D2).
		m.openToolTime = time.Time{}
		m.openToolName = ""
	case "tool_use":
		m.lastActivity = now
		// A completed tool_use resolves the open tool.
		m.openToolTime = time.Time{}
		m.openToolName = ""
		// Signature = tool name + args. Repeating the exact same call (same
		// tool, same args) is the loop signal (mirrors progress.go). The
		// opencode v1.x tool part nests the input under `state.input` (see
		// LegacyEventFromBus), NOT `input`/`args` — reading the top-level
		// fields made every bash call collapse to `bash|null` and tripped
		// repetition on legitimately distinct commands (8 different git
		// commands → "8 repeats of bash|null"). Fall back to the legacy
		// fields for test-shaped parts.
		tool, _ := part["tool"].(string)
		var args any
		if state, ok := part["state"].(map[string]any); ok {
			args = state["input"]
		}
		if args == nil {
			args = part["input"]
		}
		if args == nil {
			args = part["args"]
		}
		argsJSON, _ := json.Marshal(args)
		sig := tool + "|" + string(argsJSON)
		cutoff := now.Add(-m.repetitionWindow)
		hist := m.sigs[sig]
		kept := hist[:0]
		for _, t := range hist {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		kept = append(kept, now)
		m.sigs[sig] = kept
	}
}

// observeToolStart marks a tool call as ISSUED (active on the serve bus but
// not yet resolved). The chat collector feeds this from the raw bus event for
// a tool part whose status is not terminal (LegacyEventFromBus only emits a
// tool_use on completion, so a wedged tool would otherwise be invisible to the
// stall monitor). name is the tool being called. Only the FIRST open tool is
// tracked: a single turn blocks on one tool at a time, so a second start while
// one is open is treated as a fresh open (the model moved on) — kept simple so
// the wedge signal is precise.
func (m *chatStallMonitor) observeToolStart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	// Any tool-start is activity.
	m.lastActivity = now
	// Only latch the earliest open time: if a tool is already open, a later
	// start (e.g. a re-issued call) must not reset the wedge clock.
	if m.openToolTime.IsZero() {
		m.openToolTime = now
		m.openToolName = name
	} else if m.openToolName != name && !m.openToolTime.IsZero() {
		// A DIFFERENT tool starting closes the previous open tool (the model
		// moved on) — but only when the previous one was open.
		m.openToolTime = now
		m.openToolName = name
	}
}

// toolWedge reports (name, true) when a tool call has been open (issued,
// never resolved) and silent for the tool-wedge window. This is the AC1
// MCP-wedge signal: precise, because any activity (token delta, a completed
// tool, text) resets lastActivity and closes the open tool, so a legitimately
// slow tool that streams activity never trips it. name is the stalled tool,
// returned so the surfaced error can name it.
func (m *chatStallMonitor) toolWedge() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fired || m.openToolTime.IsZero() {
		return "", false
	}
	if m.awaitingConsent {
		// The open tool is a consent ask awaiting the human — not a wedge.
		return "", false
	}
	if m.locallyBounded[m.openToolName] {
		// The transport is RUNNING this call itself, under its own deadline: silence here is a tool still at
		// work, not a transport that never answered. See setLocallyBoundedTools.
		return "", false
	}
	if m.now().Sub(m.openToolTime) > m.toolWedgeWindow {
		return m.openToolName, true
	}
	return "", false
}

// stallReason returns a non-empty stall reason when a signal has tripped
// (once, latched), else "". The ticker calls it on its interval. The reason
// names the model the turn dispatched on so the surfaced error tells the
// user WHICH model wedged — a rate-limited or unavailable provider is the
// most common cause of a "stalled" Ask Orchicon turn.
func (m *chatStallMonitor) stallReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fired {
		return ""
	}
	now := m.now()
	// Gated on > 0: an explicit 0 in Settings means the operator DISABLED this
	// check, and without the guard `now.Sub(...) > 0` holds on every tick — a
	// disabled check would fire instantly on the first tick.
	//
	// AND GATED ON awaitingConsent, the arm the wedge signal already had and this clock did not.
	//
	// A TURN WAITING ON THE OPERATOR IS NOT A STALLED TURN. `lastActivity` advances on token progress,
	// and an ASK does not produce any — so an unanswered consent counted as "no activity from the model"
	// and the turn was ABORTED at the window. Worse, the abort then resolved the outstanding ask as
	// consentCancelled, so the MODEL was told its approval had been cancelled when in fact the stall
	// monitor had killed the turn. That is precisely the operator's report: "No card ever came to me. That
	// is why you may have been waiting for approval" — a conversation that died waiting for a card
	// surfaces as a consent error, and the two are indistinguishable from the transcript.
	//
	// The tool behind the ask is HELD BY THE HUMAN, not silent, and how long a person takes to read a card
	// is not the model's progress. Repetition stays armed below: it is about the MODEL looping, which a
	// pending ask neither causes nor excuses.
	if !m.awaitingConsent && m.noProgressWindow > 0 && now.Sub(m.lastActivity) > m.noProgressWindow {
		m.fired = true
		return fmt.Sprintf("stalled:no_progress (%s with no activity from model %s)", m.noProgressWindow, m.modelRef)
	}
	if m.repetitionCount > 0 {
		cutoff := now.Add(-m.repetitionWindow)
		for sig, ts := range m.sigs {
			kept := ts[:0]
			for _, t := range ts {
				if t.After(cutoff) {
					kept = append(kept, t)
				}
			}
			m.sigs[sig] = kept
			if len(kept) > m.repetitionCount {
				m.fired = true
				return fmt.Sprintf("stalled:repetition (%d repeats of %s within %s on model %s)", len(kept), sig, m.repetitionWindow, m.modelRef)
			}
		}
	}
	return ""
}
