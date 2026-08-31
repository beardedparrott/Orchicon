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
	defaultAskMCPToolWedgeWindow    = 30 * time.Second
	defaultAskMCPReconnectAttempts  = 1
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
func resolveChatStallNoProgressWindow(settingsSeconds int64) time.Duration {
	if settingsSeconds > 0 && os.Getenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW") == "" {
		return time.Duration(settingsSeconds) * time.Second
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
}

// newChatStallMonitor builds a stall monitor for one chat turn.
// settingsNoProgressSeconds is the tenant's stall_no_progress_window_seconds
// (0 when unset/unknown); the effective window is resolved by
// resolveChatStallNoProgressWindow so a chat turn honors the same tenant
// setting executions do.
func newChatStallMonitor(modelRef string, settingsNoProgressSeconds int64) *chatStallMonitor {
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
	if now.Sub(m.lastActivity) > m.noProgressWindow {
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
