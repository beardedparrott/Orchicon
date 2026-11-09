package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
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
//
// Fatal signals (no_progress / text_loop / repetition) fire at most once
// per execution — the hard-kill + recovery they trigger is the response.
// The no_file_progress signal is ADVISORY: it never kills the subprocess,
// puts the execution into a non-terminal `stalled` health notice instead
// of failing it, and revives the execution back to `healthy` via
// OnRecovered when file progress resumes (see checkRevival). A reviewer
// or analyst that legitimately produces output for long stretches without
// touching files must not be reaped mid-flight (observed: the SSE worker
// was flagged no_file_progress yet completed successfully moments later).

// stallWindows is the set of tunable stall thresholds. Loaded from env at
// adapter construction so operators can tighten/loosen per environment.
type stallWindows struct {
	noProgress  time.Duration
	noFileDiff  time.Duration
	textLoop    time.Duration // pure text without meaningful action
	repetitionN int
	repetitionW time.Duration
	toolHang    time.Duration // in-flight tool call with zero events (Tier A)
}

func defaultStallWindows() stallWindows {
	return stallWindows{
		noProgress:  envDuration("ORCHICON_STALL_NO_PROGRESS_WINDOW", 300*time.Second),
		noFileDiff:  envDuration("ORCHICON_STALL_NO_FILE_DIFF_WINDOW", 15*time.Minute),
		textLoop:    envDuration("ORCHICON_STALL_TEXT_LOOP_WINDOW", 10*time.Minute),
		repetitionN: envInt("ORCHICON_STALL_REPETITION_COUNT", 5),
		repetitionW: envDuration("ORCHICON_STALL_REPETITION_WINDOW", 300*time.Second),
		toolHang:    toolHangDefaultWindow(),
	}
}

// toolHangDefaultWindow resolves the Tier A in-flight tool-hang watchdog window: canonical
// ORCHICON_STALL_TOOL_HANG_WINDOW first.
func toolHangDefaultWindow() time.Duration {
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
	// RAISED FROM 180s: A LEGITIMATE COLD BUILD OUTLASTS IT.
	return 600 * time.Second
}

// stallWindowsFromManifest builds stallWindows from ExecutionManifest
// settings, with env-var fallback for dev overrides. Zero (unset) values in
// the manifest fall through to env vars, which fall through to code
// defaults.
//
// stallWindowsFromManifest builds stallWindows from ExecutionManifest settings,
// with env-var fallback for dev overrides.
//
// EVERY field is a POINTER, and the two states are different instructions:
//
//   - nil — the tenant left this dimension BLANK in Settings, so it has no
//     opinion and the env/code default below stands. A never-configured tenant
//     therefore keeps REAL stall detection rather than silently losing it.
//   - non-nil — an explicit value. A resolved duration <= 0 DISABLES that check,
//     which is what an explicit 0 means; every consumer below gates on `> 0`, so
//     0 needs no special case here. (A negative cannot arrive — the Settings API
//     rejects it — but would also read as disabled.)
//
// This replaces the old overloaded zero, under which 0 meant BOTH "unset" and,
// on most dimensions, nothing at all — while a negative was the only way to say
// "off". That made "disabled" unreachable without a counter-intuitive value and
// made a never-configured tenant indistinguishable from one that chose 0.
func stallWindowsFromManifest(m scheduler.ExecutionManifest) stallWindows {
	w := defaultStallWindows()
	if m.StallNoProgressWindowSeconds != nil && os.Getenv("ORCHICON_STALL_NO_PROGRESS_WINDOW") == "" {
		w.noProgress = time.Duration(*m.StallNoProgressWindowSeconds) * time.Second
	}
	if m.StallNoFileDiffWindowSeconds != nil && os.Getenv("ORCHICON_STALL_NO_FILE_DIFF_WINDOW") == "" {
		w.noFileDiff = time.Duration(*m.StallNoFileDiffWindowSeconds) * time.Second
	}
	if m.StallTextLoopWindowSeconds != nil && os.Getenv("ORCHICON_STALL_TEXT_LOOP_WINDOW") == "" {
		w.textLoop = time.Duration(*m.StallTextLoopWindowSeconds) * time.Second
	}
	if m.StallRepetitionCount != nil && os.Getenv("ORCHICON_STALL_REPETITION_COUNT") == "" {
		w.repetitionN = int(*m.StallRepetitionCount)
	}
	if m.StallRepetitionWindowSeconds != nil && os.Getenv("ORCHICON_STALL_REPETITION_WINDOW") == "" {
		w.repetitionW = time.Duration(*m.StallRepetitionWindowSeconds) * time.Second
	}
	// Tool-hang keeps its two env names (the deprecated one is still honoured for one release).
	if m.StallToolHangSeconds != nil && os.Getenv("ORCHICON_STALL_TOOL_HANG_WINDOW") == "" && os.Getenv("ORCHICON_TOOL_HANG_WINDOW") == "" {
		w.toolHang = time.Duration(*m.StallToolHangSeconds) * time.Second
	}
	return w
}

// progressMonitor tracks per-execution progress signals and detects stalls.
type progressMonitor struct {
	mu sync.Mutex

	execID string
	w      stallWindows
	now    func() time.Time

	startedAt time.Time

	lastStepFinish       time.Time // step_finish = token progress
	lastFileDiff         time.Time // file_diff = file progress
	lastMeaningfulAction time.Time // tool_call / file_diff / step_finish (not just text)
	lastTokenCt          int64     // cumulative tokens (for no-NEW-token detection)

	// tool-call signature history for repetition detection.
	// signature (tool+args hash) → timestamps within the window.
	// Two-tier model (D5):
	//   - sigsErr: ERROR-status tool calls only (result-aware, tier 1).
	//     A completed call or file_diff resets it (progress resets the
	//     counters).
	//   - sigsAll: ANY-status identical signature (tier 2). Catches a
	//     completed-call loop — e.g. repeatedly re-reading the same file
	//     without erroring. Resets on genuine forward progress (a
	//     file_diff / step_finish between identical calls resets the
	//     count), so legitimate long steps never false-positive.
	sigsErr map[string][]time.Time
	sigsAll map[string][]time.Time

	// fired tracks whether the monitor has already raised a FATAL stall (no_progress / text_loop /
	// repetition) for this execution.
	fired bool

	// In-flight tool-hang tracking (Tier A detection): the single currently in-flight tool call (name +
	// last-activity time).
	toolHangName  string
	toolHangStart time.Time
	toolInFlight  bool
	toolHangFired bool

	// warned tracks an active ADVISORY stall (no_file_progress).
	warned bool

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
		sigsErr:              make(map[string][]time.Time),
		sigsAll:              make(map[string][]time.Time),
		stop:                 make(chan struct{}),
	}
	return m
}

// observe feeds a parsed event into the monitor.
func (m *progressMonitor) observe(eventType string, part map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	// Tier A (tool-hang): ANY non-tool event while a tool call is in flight is activity — refresh the
	// hang start so only a call with zero events for the whole window trips.
	if m.toolInFlight && eventType != "tool_use" && eventType != "tool_call" {
		m.toolHangStart = now
	}
	switch eventType {
	case "text", "reasoning":
		// Text output counts as progress — reviewers produce text without
		// step_finish events for extended periods.
		m.lastStepFinish = now
	case "step_finish":
		m.lastStepFinish = now
		m.lastMeaningfulAction = now
		// A step finishing is real progress — reset the repetition signature history (the worker is making
		// forward progress, not looping).
		m.sigsErr = make(map[string][]time.Time)
		m.sigsAll = make(map[string][]time.Time)
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
		// File progress is real progress — reset the repetition history.
		m.sigsErr = make(map[string][]time.Time)
		m.sigsAll = make(map[string][]time.Time)
	case "tool_use", "tool_call":
		m.lastMeaningfulAction = now
		status, _ := part["state"].(map[string]any)["status"].(string)
		// Tier A: a tool part still in flight (running / no status) arms or refreshes the single in-flight
		// slot (a new start supersedes the previous one); a resolved part (completed/error) disarms it.
		if status == "completed" || status == "error" {
			m.toolInFlight = false
		} else {
			if tool, _ := part["tool"].(string); tool != "" {
				m.toolHangName = tool
			}
			m.toolHangStart = now
			m.toolInFlight = true
		}
		sig := toolUseSignature(part)
		cutoff := now.Add(-m.w.repetitionW)
		// Tier 1 (existing): only ERROR-status tool calls count toward the repetition threshold.
		if status == "error" {
			m.sigsErr[sig] = appendInWindow(m.sigsErr[sig], now, cutoff)
		} else {
			m.sigsErr = make(map[string][]time.Time)
		}
		// Tier 2 (D5): ANY-status identical signature — catches a completed-call loop (repeatedly
		// re-reading the same file, the same write).
		m.sigsAll[sig] = appendInWindow(m.sigsAll[sig], now, cutoff)
	}
}

// observeToolStart arms the Tier A tool-hang slot for one in-flight tool call (a new start
// supersedes the previous slot — only the longest/single in-flight call is tracked).
func (m *progressMonitor) observeToolStart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name != "" {
		m.toolHangName = name
	}
	m.toolHangStart = m.now()
	m.toolInFlight = true
}

// observeToolEnd disarms the Tier A tool-hang slot: the in-flight call
// resolved (completed/error), so the hang window resets.
func (m *progressMonitor) observeToolEnd() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolInFlight = false
}

// noteToolActivity refreshes the Tier A hang start without disarming: the in-flight call produced
// (or was followed by) an event, so the zero-events-only trip condition restarts.
func (m *progressMonitor) noteToolActivity() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.toolInFlight {
		m.toolHangStart = m.now()
	}
}

// checkToolHang reports the hung in-flight tool call when it has produced zero events for the whole
// toolHang window.
func (m *progressMonitor) checkToolHang() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkToolHangLocked()
}

// checkToolHangLocked is checkToolHang for callers that already hold m.mu
// (notably check()).
func (m *progressMonitor) checkToolHangLocked() (string, bool) {
	if m.w.toolHang <= 0 || !m.toolInFlight || m.toolHangFired {
		return "", false
	}
	if m.now().Sub(m.toolHangStart) <= m.w.toolHang {
		return "", false
	}
	m.toolHangFired = true
	name := m.toolHangName
	if name == "" {
		name = "unknown"
	}
	return name, true
}

// appendInWindow appends a timestamp to a signature history, dropping
// entries outside the repetition window.
func appendInWindow(hist []time.Time, now, cutoff time.Time) []time.Time {
	kept := hist[:0]
	for _, t := range hist {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return append(kept, now)
}

// Conservative normalization (design A, gated through result-aware B): the signature is normalized
// ONLY for ERRORING calls.
func toolUseSignature(part map[string]any) string {
	tool, _ := part["tool"].(string)
	args := part["state"]
	if state, ok := part["state"].(map[string]any); ok {
		if input, ok := state["input"]; ok {
			args = input
		}
	}
	status, _ := part["state"].(map[string]any)["status"].(string)
	if status == "error" {
		args = normalizeErrorArgs(tool, args)
	}
	argsJSON, _ := json.Marshal(args)
	return tool + "|" + string(argsJSON)
}

// normalizeErrorArgs strips volatile content from an ERRORING tool call's
// args so identical retries collapse to one signature while distinct
// commands stay distinct. Applied only to erroring calls (see
// toolUseSignature). It never collapses distinct legitimate commands:
//   - write -> key on path (drop the volatile content payload). opencode's
//     write tool emits {filePath, content}.
//   - edit  -> key on filePath + a stable fingerprint of oldString/newString
//     so genuinely different edits remain distinct while identical retries
//     to the same file collapse. opencode's edit tool emits {filePath,
//     oldString, newString, replaceAll}.
//   - bash  -> key on the scrubbed command: drop volatile args (timestamps,
//     temp IDs, long hex/digit tokens), preserving distinct commands
//     (git status != git log). opencode's shell tool emits {command, ...}.
func normalizeErrorArgs(tool string, args any) any {
	switch tool {
	case "write":
		if m, ok := args.(map[string]any); ok {
			path, _ := m["filePath"].(string)
			return map[string]any{"filePath": path}
		}
	case "edit":
		if m, ok := args.(map[string]any); ok {
			path, _ := m["filePath"].(string)
			oldS, _ := m["oldString"].(string)
			newS, _ := m["newString"].(string)
			return map[string]any{
				"filePath":  path,
				"oldString": fingerprint(oldS),
				"newString": fingerprint(newS),
			}
		}
	case "bash":
		if m, ok := args.(map[string]any); ok {
			cmd, _ := m["command"].(string)
			return map[string]any{"command": scrubCommand(cmd)}
		}
	}
	return args
}

// fingerprint returns a stable short hash of a string so identical content
// collapses while different content stays distinct (used for edit old/new).
func fingerprint(s string) string {
	if s == "" {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum32())
}

// scrubCommand removes volatile tokens from a bash command so a worker retrying the same command
// with different timestamps/temp IDs collapses to one signature.
func scrubCommand(cmd string) string {
	re := regexp.MustCompile(`(?:[0-9a-fA-F]{8,}|[0-9]{4,}|/tmp/[A-Za-z0-9._-]+)`)
	return re.ReplaceAllString(cmd, "<vol>")
}

// run starts the background stall checker.
func (m *progressMonitor) run(ctx context.Context, onStall func(execID, reason string), onRecovered func(execID, recovered string)) {
	// The tick interval is the SHORTEST ENABLED window, so a short window is never checked late.
	poll := 30 * time.Second
	if m.w.noProgress > 0 && m.w.noProgress < poll {
		poll = m.w.noProgress
	}
	if m.w.noFileDiff > 0 && m.w.noFileDiff < poll {
		poll = m.w.noFileDiff
	}
	if m.w.textLoop > 0 && m.w.textLoop < poll {
		poll = m.w.textLoop
	}
	if m.w.toolHang > 0 && m.w.toolHang < poll {
		poll = m.w.toolHang
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
				// Fatal: fire once and stop. Advisory: keep ticking so the
				// execution can revive when file progress picks back up.
				if isFatalStall(reason) {
					return
				}
				continue
			}
			if recovered := m.checkRevival(); recovered != "" {
				onRecovered(m.execID, recovered)
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
	// tool_hang (Tier A): an in-flight tool call with zero events for the whole window.
	if tool, ok := m.checkToolHangLocked(); ok {
		return "stalled:tool_hang:" + tool
	}
	// no_progress: no step_finish (no token progress) within the window.
	if m.w.noProgress > 0 && now.Sub(m.lastStepFinish) > m.w.noProgress {
		m.fired = true
		return "stalled:no_progress"
	}
	// no_file_diff: no file modifications within the window.
	if m.w.noFileDiff > 0 && now.Sub(m.lastFileDiff) > m.w.noFileDiff && !m.warned {
		m.warned = true
		return "stalled:no_file_progress"
	}
	// text_loop: text flowing but no meaningful action (tool call, file diff, step finish) within the
	// window.
	if m.w.textLoop > 0 && now.Sub(m.lastMeaningfulAction) > m.w.textLoop {
		return "stalled:text_loop:You were talking in circles without making progress. On the next attempt, start fresh with a clear plan, make a concrete tool call or file edit within the first few turns."
	}
	// repetition: same tool_call signature repeated more than the threshold within the window.
	if m.w.repetitionN > 0 {
		cutoff := now.Add(-m.w.repetitionW)
		// Tier 1: error-status repeats (result-aware).
		for sig, ts := range m.sigsErr {
			kept := trimWindow(ts, cutoff)
			m.sigsErr[sig] = kept
			if len(kept) > m.w.repetitionN {
				return fmt.Sprintf("stalled:repetition:%s", sig)
			}
		}
		// Tier 2 (D5): ANY-status identical signature — a completed-call loop (repeatedly re-reading the
		// same file, re-issuing the same write) is invisible to tier 1 (only erroring calls count) but
		// must.
		tier2N := m.w.repetitionN * 2
		if tier2N < m.w.repetitionN+3 {
			tier2N = m.w.repetitionN + 3
		}
		for sig, ts := range m.sigsAll {
			kept := trimWindow(ts, cutoff)
			m.sigsAll[sig] = kept
			if len(kept) > tier2N {
				return fmt.Sprintf("stalled:repetition:completed:%s", sig)
			}
		}
	}
	return ""
}

// trimWindow drops signature timestamps outside the repetition window.
func trimWindow(ts []time.Time, cutoff time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// checkRevival returns a revival reason (and clears the `warned` state) when an active advisory
// stall has cleared — i.e. the missing progress signal arrived before the execution terminated.
func (m *progressMonitor) checkRevival() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warned {
		return ""
	}
	now := m.now()
	if m.w.noFileDiff > 0 && now.Sub(m.lastFileDiff) <= m.w.noFileDiff {
		m.warned = false
		return "recovered:no_file_progress"
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

// revive clears an active advisory stall (warned) and resets every stall timestamp to now.
func (m *progressMonitor) revive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warned {
		return false
	}
	m.warned = false
	now := m.now()
	m.lastStepFinish = now
	m.lastMeaningfulAction = now
	m.lastFileDiff = now
	return true
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

// isFatalStall reports whether a stall reason warrants terminating the
// subprocess (hard kill → recovery) versus being advisory-only.
//
// Nudge-first routing (the core reframe): a LOOPING worker is responsive
// (tokens flowing, tool calls happening) and holds full context — killing
// it destroys that context and forces a cold recovery re-dispatch. So only
// a signal with NO responsive surface to nudge stays fatal:
//
//   - no_progress: FATAL. Total silence — there is no live session surface
//     to nudge, so the existing probe-then-escalate machinery handles it.
//   - text_loop / repetition / no_file_progress: ADVISORY-first. The
//     worker is generating text / issuing tool calls, so a nudge can reach
//     it. The session's onStall nudges the live session; only after the
//     nudge budget is spent (and the worker still hasn't broken the
//     pattern) does it escalate to a fatal kill + recovery.
func isFatalStall(reason string) bool {
	return strings.HasPrefix(reason, "stalled:no_progress")
}

// wallClockDeadline parses the execution's merged budget JSON (budget_overrides after
// tenant-default merge, docs/05 §8) and returns the absolute deadline for the execution.
func wallClockDeadline(ctx context.Context, budgets []byte) (time.Time, bool) {
	if v := os.Getenv("ORCHICON_STALL_WALL_CLOCK_SECONDS"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
			return time.Now().Add(d), true
		}
	}
	spec := parseBudgetSpec(budgets)
	// Absent field (or empty/unparseable budgets) → NO deadline.
	if spec.wallClockSeconds == nil {
		return time.Time{}, false
	}
	// Explicit 0 (or negative) also means no deadline.
	if *spec.wallClockSeconds <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(*spec.wallClockSeconds * float64(time.Second))), true
}
