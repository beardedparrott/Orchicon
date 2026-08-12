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
)

func askStallNoProgressWindow() time.Duration {
	if v := os.Getenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultAskStallNoProgressWindow
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
	mu sync.Mutex

	noProgressWindow time.Duration
	repetitionCount  int
	repetitionWindow time.Duration
	now              func() time.Time

	// lastActivity advances on ANY activity signal (text, reasoning,
	// step_finish, tool_use) — a model that keeps producing output, even
	// long reasoning, is never reaped.
	lastActivity time.Time

	// tool-call signature history for repetition detection.
	// signature (tool+args) → timestamps within the window.
	sigs map[string][]time.Time

	// fired latches a trip so the stall is reported once.
	fired bool
}

func newChatStallMonitor(modelRef string) *chatStallMonitor {
	return &chatStallMonitor{
		modelRef:         modelRef,
		noProgressWindow: askStallNoProgressWindow(),
		repetitionCount:  askStallRepetitionCount(),
		repetitionWindow: askStallRepetitionWindow(),
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
	case "tool_use":
		m.lastActivity = now
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
