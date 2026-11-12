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
	// 300s, RAISED FROM 120s — 2 minutes aborted live sessions, and the operator found it: "sessions
	// dying.
	defaultAskStallNoProgressWindow = 600 * time.Second

	// defaultAskStallNoToolNoProgressWindow is the window used when NO TOOL IS OPEN.
	//
	// THE LONG WINDOW EXISTS FOR ONE REASON: a tool that is legitimately running emits NO events, so
	// silence during a tool is expected and must not be read as a stall. That reasoning is why it was
	// raised to 300s, and it is sound — for a turn that has a tool open.
	//
	// IT DOES NOT APPLY TO A TURN WITH NOTHING OPEN, and applying it there is what the operator
	// experienced as a hang. MEASURED, from the plane's own record of a turn they reported as hanging:
	//
	//	stalled:no_progress (6m40s with no activity from model opencode/opencode/big-pickle)
	//
	// 6m40s is 400s — their tenant's stall_no_progress_window_seconds — and the turn had NO tool open
	// and no model output at all. Nothing was running that could be silent, so the long window bought
	// nothing except six and a half minutes of an operator watching an activity line that stays alive
	// because it is fed by PLANE heartbeats, not by the model.
	//
	// So when nothing is open, the question stops being "might a tool be quietly working?" and becomes
	// "is this provider answering at all?" — which deserves a shorter leash than an open tool.
	//
	// BUT 120s WAS FAR TOO SHORT, and the operator said so directly: "2 minutes is far too aggressive
	// as a default." 120s was carried over from before tools raised the other window, on the theory
	// that "nothing is open, so silence means nothing is happening". THAT THEORY IS WRONG about the
	// one thing it most needs to be right about: a REASONING model emits NOTHING while it thinks.
	// DeepSeek/GLM-class models routinely reason for several minutes on a hard question, and every
	// second of that is a healthy turn that 120s would abort — with a stall message the operator can
	// do nothing about except retry into the same wall.
	//
	// So the two windows are now proportional rather than merely ordered: nothing may be silent for
	// 300s, a tool may be silent for 600s. Both remain far inside the 30-minute reply window, and a
	// tenant or environment override still wins, so this is a DEFAULT rather than a policy.
	defaultAskStallNoToolNoProgressWindow = 300 * time.Second
	defaultAskStallRepetitionCount        = 5
	defaultAskStallRepetitionWindow       = 300 * time.Second
	// 2026-09-09 (operator: "the conversation session wedged on a tool (bash) and could not be
	// recovered after 2 attempt(s) — please retry" killed live Ask sessions).
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

// resolveChatStallNoProgressWindow returns the effective chat no-progress stall window.
func resolveChatStallNoProgressWindow(settingsSeconds *int64) time.Duration {
	if settingsSeconds != nil && os.Getenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW") == "" {
		return time.Duration(*settingsSeconds) * time.Second
	}
	return askStallNoProgressWindow()
}

// askStallNoToolNoProgressWindow is the effective no-tool window. Env override
// ORCHICON_ASK_STALL_NO_TOOL_NO_PROGRESS_WINDOW is a dev/test knob.
func askStallNoToolNoProgressWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_STALL_NO_TOOL_NO_PROGRESS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskStallNoToolNoProgressWindow
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

// askMCPToolWedgeWindow bounds how long a single tool call may stay open (started on the serve bus,
// never resolved) before it is treated as an MCP wedge.
func askMCPToolWedgeWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskMCPToolWedgeWindow
}

// askMCPReconnectAttempts bounds how many times a wedged session is recycled (abort + fresh seeded
// session + re-dispatch) within one turn before the turn is failed with a clear retryable error.
func askMCPReconnectAttempts() int {
	if v := os.Getenv("ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultAskMCPReconnectAttempts
}

// chatStallMonitor tracks per-turn progress signals and detects stalls.
type chatStallMonitor struct {
	// modelRef is the model this turn dispatched on (provider/model), carried so the stall reason can
	// name it — the single most useful diagnostic when a turn wedges (a rate-limited or unavailable.
	modelRef string
	mu       sync.Mutex

	noProgressWindow time.Duration
	repetitionCount  int
	repetitionWindow time.Duration
	toolWedgeWindow  time.Duration
	now              func() time.Time

	// lastActivity advances on ANY activity signal (text, reasoning, step_finish, tool_use) — a model
	// that keeps producing output, even long reasoning, is never reaped.
	lastActivity time.Time

	// openToolTime is when the current (unresolved) tool call was issued, and openToolName its name.
	openToolTime time.Time
	openToolName string

	// tool-call signature history for repetition detection.
	// signature (tool+args) → timestamps within the window.
	sigs map[string][]time.Time

	// fired latches a trip so the stall is reported once.
	fired bool

	// awaitingConsent gates the tool-wedge signal while a consent ask is outstanding: the tool call
	// behind the ask is HELD BY THE HUMAN, not wedged.
	awaitingConsent bool

	// locallyBounded names the tools this turn's transport executes ITSELF, on the process's own clock,
	// so that a silent call is not treated as a wedged one.
	locallyBounded map[string]bool
}

// setLocallyBoundedTools marks the tool NAMES whose calls this turn's transport executes in-process
// under its own hard deadline.
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
		m.lastActivity = m.now()
	}
}

// closeTool marks the open tool call as RESOLVED, so it can never be judged a wedge.
func (m *chatStallMonitor) closeTool() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openToolTime = time.Time{}
	m.openToolName = ""
	// AND IT IS PROGRESS, so the no-progress clock restarts HERE too — which is what makes suspending
	// that signal during a locally-bounded call safe (see stallReason).
	m.lastActivity = m.now()
}

// newChatStallMonitor builds a stall monitor for one chat turn.
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

// observe feeds one decoded telemetry event into the monitor.
func (m *chatStallMonitor) observe(etype string, part map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	switch etype {
	case "text", "reasoning", "step_finish":
		m.lastActivity = now
		// A completed text/reasoning/step boundary closes any open tool: the
		// model moved past it, so it is not wedged.
		m.openToolTime = time.Time{}
		m.openToolName = ""
	case "tool_use":
		m.lastActivity = now
		// A completed tool_use resolves the open tool.
		m.openToolTime = time.Time{}
		m.openToolName = ""
		// Signature = tool name + args.
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
		// A SIGNATURE NEEDS IDENTITY: A TOOL NAME *AND* ITS ARGS.
		if tool == "" || args == nil {
			return
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

// observeToolStart marks a tool call as ISSUED (active on the serve bus but not yet resolved).
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

// toolWedge reports (name, true) when a tool call has been open (issued, never resolved) and silent
// for the tool-wedge window.
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

// stallReason returns a non-empty stall reason when a signal has tripped (once, latched), else "".
func (m *chatStallMonitor) stallReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fired {
		return ""
	}
	now := m.now()

	// NO PROGRESS IS NOT JUDGED WHILE THE TRANSPORT IS ALREADY TIMING SOMETHING.
	localToolOpen := !m.openToolTime.IsZero() && m.locallyBounded[m.openToolName]

	// Gated on > 0: an explicit 0 in Settings means the operator DISABLED this check.
	window := m.noProgressWindow
	if m.openToolTime.IsZero() {
		if short := askStallNoToolNoProgressWindow(); short < window {
			window = short
		}
	}
	if !localToolOpen && !m.awaitingConsent && window > 0 && now.Sub(m.lastActivity) > window {
		m.fired = true
		return fmt.Sprintf("stalled:no_progress (%s with no activity from model %s)", window, m.modelRef)
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
