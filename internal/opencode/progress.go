package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// Stall detection (docs/06 §2: "stalled health state | no progress within
// stall window"; docs/03 §5: HealthMonitor recomputes from progress rate
// + error rate). A worker stuck looping — repeating the same tool calls,
// making no file changes, or making no token progress — must trigger
// recovery, not run forever.
//
// v0.1 implements this in the OpenCode adapter bridge (the component that
// parses opencode's stdout telemetry). A full control-plane HealthMonitor
// that recomputes health from union signals (heartbeat freshness, progress
// rate, error rate, context-window usage) arrives with Phase 8
// (telemetry); this is the recovery-trigger floor.
//
// Three stall signals (configurable via env, docs/06 §2):
//   - no_progress:   no step_finish / no new tokens within the window
//     (ORCHICON_STALL_NO_PROGRESS_WINDOW, default 120s).
//   - no_file_diff:  no file_diff event within the window
//     (ORCHICON_STALL_NO_FILE_DIFF_WINDOW, default 180s). A worker that
//     hasn't modified files in X is likely stuck (docs/06 §2 "stalled").
//   - repetition:    the same tool_call signature (tool+args) repeated
//     more than ORCHICON_STALL_REPETITION_COUNT times (default 5) within
//     the window. Detects the "saying/doing the same things over and
//     over" loop the user described.
//
// When any signal trips, the monitor calls OnStall(execID, reason) which
// the TaskReconciler uses to trigger recovery (idempotent — docs/06 §9).
// The monitor fires at most once per execution (a stall trigger that
// itself loops would be a bug; the recovery it triggers is the response).

// stallWindows is the set of tunable stall thresholds. Loaded from env at
// adapter construction so operators can tighten/loosen per environment.
type stallWindows struct {
	noProgress    time.Duration
	noFileDiff    time.Duration
	textLoop      time.Duration // pure text without meaningful action
	repetitionN  int
	repetitionW  time.Duration
}

func defaultStallWindows() stallWindows {
	return stallWindows{
		noProgress:   envDuration("ORCHICON_STALL_NO_PROGRESS_WINDOW", 120*time.Second),
		noFileDiff:   envDuration("ORCHICON_STALL_NO_FILE_DIFF_WINDOW", 15*time.Minute),
		textLoop:     envDuration("ORCHICON_STALL_TEXT_LOOP_WINDOW", 10*time.Minute),
		repetitionN:  envInt("ORCHICON_STALL_REPETITION_COUNT", 5),
		repetitionW:  envDuration("ORCHICON_STALL_REPETITION_WINDOW", 300*time.Second),
	}
}

// progressMonitor tracks per-execution progress signals and detects stalls.
// It is fed events from parseStdoutLine and runs a background ticker that
// checks the stall windows. Thread-safe (the stdout reader + the ticker
// goroutine both touch it).
type progressMonitor struct {
	mu sync.Mutex

	execID  string
	w       stallWindows
	now     func() time.Time

	startedAt time.Time

	lastStepFinish     time.Time // step_finish = token progress
	lastFileDiff       time.Time // file_diff = file progress
	lastMeaningfulAction time.Time // tool_call / file_diff / step_finish (not just text)
	lastTokenCt        int64     // cumulative tokens (for no-NEW-token detection)

	// tool-call signature history for repetition detection.
	// signature (tool+args hash) → timestamps within the window.
	sigs map[string][]time.Time

	// fired tracks whether the monitor has already raised a stall for this
	// execution. A stall trigger fires once; the recovery it triggers is
	// the response (docs/06 §9 idempotency guards against re-trigger).
	fired bool

	stop chan struct{}
}

// newProgressMonitor constructs a monitor for one execution. Callers must
// call Close to stop the background ticker.
func newProgressMonitor(execID string, w stallWindows) *progressMonitor {
	m := &progressMonitor{
		execID:               execID,
		w:                    w,
		now:                  time.Now,
		startedAt:            time.Now(),
		lastStepFinish:       time.Now(),
		lastFileDiff:         time.Now(),
		lastMeaningfulAction: time.Now(),
		sigs:                 make(map[string][]time.Time),
		stop:                 make(chan struct{}),
	}
	return m
}

// observe feeds a parsed stdout event into the monitor. Called from the
// stdout reader goroutine (parseStdoutLine). eventType matches opencode's
// `--format json` event types (docs/04 §6.1).
func (m *progressMonitor) observe(eventType string, part map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	switch eventType {
	case "text", "reasoning":
		// Text output counts as progress — reviewers produce text without
		// step_finish events for extended periods.
		m.lastStepFinish = now
	case "step_finish":
		m.lastStepFinish = now
		m.lastMeaningfulAction = now
		// Track cumulative token count so no-new-tokens is detectable even
		// if step_finish fires without a token delta.
		if tokens, ok := part["tokens"].(map[string]any); ok {
			if in, ok := tokens["input"].(float64); ok {
				m.lastTokenCt += int64(in)
			}
			if out, ok := tokens["output"].(float64); ok {
				m.lastTokenCt += int64(out)
			}
		}
	case "file_diff":
		m.lastFileDiff = now
		m.lastMeaningfulAction = now
	case "tool_call":
		m.lastMeaningfulAction = now
		// Signature = tool name + marshaled args. Repeating the exact same
		// call (same tool, same args) is the loop signal.
		tool, _ := part["tool"].(string)
		argsJSON, _ := json.Marshal(part["args"])
		sig := tool + "|" + string(argsJSON)
		cutoff := now.Add(-m.w.repetitionW)
		hist := m.sigs[sig]
		// drop entries outside the window
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

// run starts the background stall checker. It ticks every pollInterval
// and, on the first tripped signal, calls onStall once with the reason.
func (m *progressMonitor) run(ctx context.Context, onStall func(execID, reason string)) {
	poll := m.w.noProgress
	if m.w.noFileDiff < poll && m.w.noFileDiff > 0 {
		poll = m.w.noFileDiff
	}
	if m.w.textLoop > 0 && m.w.textLoop < poll {
		poll = m.w.textLoop
	}
	if poll > 30*time.Second {
		poll = 30 * time.Second
	}
	if poll < 5*time.Second {
		poll = 5 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			reason := m.check()
			if reason != "" {
				onStall(m.execID, reason)
				return // fire once
			}
		}
	}
}

// check returns a non-empty stall reason if any signal has tripped, else "".
func (m *progressMonitor) check() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fired {
		return ""
	}
	now := m.now()
	// no_progress: no step_finish (no token progress) within the window.
	if now.Sub(m.lastStepFinish) > m.w.noProgress {
		m.fired = true
		return "stalled:no_progress"
	}
	// no_file_diff: no file modifications within the window.
	// Skipped when noFileDiff is 0 or negative (disabled), because
	// reviewers/QA workers may never write files.
	if m.w.noFileDiff > 0 && now.Sub(m.lastFileDiff) > m.w.noFileDiff {
		m.fired = true
		return "stalled:no_file_progress"
	}
	// text_loop: text flowing but no meaningful action (tool call, file
	// diff, step finish) within the window. Catches workers that talk
	// forever ("but wait, let me reconsider…") without ever doing work.
	if m.w.textLoop > 0 && now.Sub(m.lastMeaningfulAction) > m.w.textLoop {
		m.fired = true
		return "stalled:text_loop:You were talking in circles without making progress. On the next attempt, start fresh with a clear plan, make a concrete tool call or file edit within the first few turns."
	}
	// repetition: same tool_call signature repeated more than the
	// threshold within the window.
	if m.w.repetitionN > 0 {
		cutoff := now.Add(-m.w.repetitionW)
		for sig, ts := range m.sigs {
			kept := ts[:0]
			for _, t := range ts {
				if t.After(cutoff) {
					kept = append(kept, t)
				}
			}
			m.sigs[sig] = kept
			if len(kept) > m.w.repetitionN {
				m.fired = true
				return fmt.Sprintf("stalled:repetition:%s", sig)
			}
		}
	}
	return ""
}

// close stops the background ticker. Called when the execution terminates.
func (m *progressMonitor) close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

// envDuration parses a duration env var with a fallback.
func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// envInt parses an int env var with a fallback.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// keep domain referenced for the health-state constant used by the
// adapter's existing OnHealth path.
var _ = domain.HealthStalled

// wallClockDeadline parses the worker's budget_overrides.wall_clock_seconds
// (docs/05 §8) and returns the absolute deadline for the execution.
// Returns ok=false if no wall-clock budget is set (no deadline). The
// deadline is enforced via context.WithDeadline in Start; when it hits,
// the subprocess is killed (exec.CommandContext) and OnResult(false)
// fires → recovery with reason "wall_clock_timeout" (docs/06 §2 budget
// overrun trigger).
//
// A worker may set wall_clock_seconds to 0 to disable the hard timeout
// (relying solely on stall detection); this is unusual and discouraged.
func wallClockDeadline(ctx context.Context, budgets []byte) (time.Time, bool) {
	if len(budgets) == 0 {
		return time.Time{}, false
	}
	var b struct {
		WallClockSeconds float64 `json:"wall_clock_seconds"`
	}
	if err := json.Unmarshal(budgets, &b); err != nil {
		return time.Time{}, false
	}
	if b.WallClockSeconds <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(b.WallClockSeconds * float64(time.Second))), true
}
