package orchicon

// progress.go is the NATIVE progress monitor — a direct port of the
// opencode adapter's stall detection (internal/opencode/progress.go) so
// both adapter engines run the SAME monitors with the SAME windows, the
// SAME advisory/fatal split, and the SAME nudge-first escalation.
//
// Three stall signals (configurable via env + manifest, opencode parity):
//   - no_progress:   no step_finish / no new tokens within the window
//     (ORCHICON_STALL_NO_PROGRESS_WINDOW, default 300s). FATAL.
//   - no_file_diff:  no file modifications within the window
//     (ORCHICON_STALL_NO_FILE_DIFF_WINDOW, default 15m). ADVISORY: a
//     non-terminal `stalled` health notice that revives (OnRecovered)
//     when file progress resumes — reviewers legitimately go long
//     stretches without touching files.
//   - repetition:    the same tool-call signature repeated more than
//     ORCHICON_STALL_REPETITION_COUNT times (default 5, tier 1:
//     error-status only) / 2× that (tier 2: any-status) within
//     ORCHICON_STALL_REPETITION_WINDOW (default 300s). Reset-on-progress:
//     a file write or completed step clears the history. ADVISORY-first:
//     nudge the live session, escalate after the nudge budget is spent.
//   - text_loop:     text flowing but no meaningful action within
//     ORCHICON_STALL_TEXT_LOOP_WINDOW (default 10m). ADVISORY-first.
//   - tool_hang:     an in-flight tool call with zero events for
//     ORCHICON_STALL_TOOL_HANG_WINDOW (default 180s). ADVISORY, latched.
//
// isFatalStall mirrors opencode exactly: ONLY no_progress is fatal (total
// silence — no responsive surface to nudge). Every looping-but-responsive
// signal nudges first and escalates through the nudge budget.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// stallWindows is the set of tunable stall thresholds (opencode parity).
// Loaded from the ExecutionManifest (tenant settings) with env fallback
// and built-in defaults.
type stallWindows struct {
	noProgress  time.Duration
	noFileDiff  time.Duration
	textLoop    time.Duration // pure text without meaningful action
	repetitionN int
	repetitionW time.Duration
	toolHang    time.Duration // in-flight tool call with zero events
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

// toolHangDefaultWindow resolves the tool-hang watchdog window
// (opencode parity): ORCHICON_STALL_TOOL_HANG_WINDOW, then the deprecated
// ORCHICON_TOOL_HANG_WINDOW fallback, then the 180s default. <=0 disables.
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
	return 180 * time.Second
}

// stallWindowsFromManifest builds stallWindows from ExecutionManifest
// settings (tenant settings), with env-var fallback and built-in defaults.
// Zero (unset) manifest values fall through to env, then defaults.
// noFileDiff/textLoop follow the opencode 0/negative convention: 0/unset →
// default, positive → override, negative → disabled (consumers gate on >0).
func stallWindowsFromManifest(noProgressSec, noFileDiffSec, textLoopSec int64, repCount int32, repWindowSec int64, toolHangSec int64) stallWindows {
	w := defaultStallWindows()
	if noProgressSec > 0 && os.Getenv("ORCHICON_STALL_NO_PROGRESS_WINDOW") == "" {
		w.noProgress = time.Duration(noProgressSec) * time.Second
	}
	if noFileDiffSec != 0 && os.Getenv("ORCHICON_STALL_NO_FILE_DIFF_WINDOW") == "" {
		w.noFileDiff = time.Duration(noFileDiffSec) * time.Second
	}
	if textLoopSec != 0 && os.Getenv("ORCHICON_STALL_TEXT_LOOP_WINDOW") == "" {
		w.textLoop = time.Duration(textLoopSec) * time.Second
	}
	if repCount > 0 && os.Getenv("ORCHICON_STALL_REPETITION_COUNT") == "" {
		w.repetitionN = int(repCount)
	}
	if repWindowSec > 0 && os.Getenv("ORCHICON_STALL_REPETITION_WINDOW") == "" {
		w.repetitionW = time.Duration(repWindowSec) * time.Second
	}
	if toolHangSec != 0 && os.Getenv("ORCHICON_STALL_TOOL_HANG_WINDOW") == "" && os.Getenv("ORCHICON_TOOL_HANG_WINDOW") == "" {
		w.toolHang = time.Duration(toolHangSec) * time.Second
	}
	return w
}

// progressMonitor tracks per-session progress signals and detects stalls.
// Fed by the session loop (turn events, tool calls, file writes) and runs
// a background ticker started by Run. Thread-safe.
type progressMonitor struct {
	mu sync.Mutex

	execID string
	w      stallWindows
	now    func() time.Time

	startedAt time.Time

	lastStepFinish       time.Time // step_finish = token progress
	lastFileDiff         time.Time // file write = file progress
	lastMeaningfulAction time.Time // tool_call / file write / step_finish
	lastTokenCt          int64     // cumulative tokens (no-NEW-token detection)

	// Tool-call signature history for repetition detection (two-tier,
	// opencode D5 parity):
	//   - sigsErr: ERROR-result tool calls only (result-aware, tier 1).
	//   - sigsAll: ANY-result identical signature (tier 2) — catches a
	//     completed-call loop (repeatedly re-reading the same file).
	// A file write or step finish resets both (reset-on-progress).
	sigsErr map[string][]time.Time
	sigsAll map[string][]time.Time

	// fired latches the FATAL stall (no_progress): fires once per session.
	fired bool

	// Tier A tool-hang tracking: the single in-flight tool call.
	toolHangName  string
	toolHangStart time.Time
	toolInFlight  bool
	toolHangFired bool

	// warned tracks an active ADVISORY no_file_progress stall (revivable).
	warned bool

	stop chan struct{}
}

func newProgressMonitor(execID string, w stallWindows) *progressMonitor {
	now := time.Now()
	return &progressMonitor{
		execID:               execID,
		w:                    w,
		now:                  time.Now,
		startedAt:            now,
		lastStepFinish:       now,
		lastFileDiff:         now,
		lastMeaningfulAction: now,
		sigsErr:              map[string][]time.Time{},
		sigsAll:              map[string][]time.Time{},
		stop:                 make(chan struct{}),
	}
}

// observeText feeds an assistant text/reasoning delta (counts as progress —
// reviewers produce text without step_finish for extended periods).
func (m *progressMonitor) observeText() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastStepFinish = m.now()
}

// observeStepFinish feeds a completed turn: token progress, resets the
// repetition history, tracks the cumulative token count.
func (m *progressMonitor) observeStepFinish(usage Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.lastStepFinish = now
	m.lastMeaningfulAction = now
	m.sigsErr = make(map[string][]time.Time)
	m.sigsAll = make(map[string][]time.Time)
	m.lastTokenCt += usage.InputTokens + usage.OutputTokens + usage.ReasoningTokens
}

// observeToolCall feeds one executed tool call with its result status.
func (m *progressMonitor) observeToolCall(name, argsJSON string, isError bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.lastMeaningfulAction = now
	// The in-flight slot is disarmed — the call resolved within the turn.
	m.toolInFlight = false
	sig := toolCallSignature(name, argsJSON, isError)
	cutoff := now.Add(-m.w.repetitionW)
	if isError {
		m.sigsErr[sig] = appendInWindow(m.sigsErr[sig], now, cutoff)
	} else {
		m.sigsErr = make(map[string][]time.Time)
	}
	m.sigsAll[sig] = appendInWindow(m.sigsAll[sig], now, cutoff)
}

// observeToolStart arms the Tier A tool-hang slot (called when a tool call
// begins executing).
func (m *progressMonitor) observeToolStart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if name != "" {
		m.toolHangName = name
	}
	m.toolHangStart = m.now()
	m.toolInFlight = true
}

// observeFileDiff feeds file progress (a write/edit landed) — resets the
// repetition history and the advisory no-file window.
func (m *progressMonitor) observeFileDiff() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.lastFileDiff = now
	m.lastMeaningfulAction = now
	m.sigsErr = make(map[string][]time.Time)
	m.sigsAll = make(map[string][]time.Time)
}

// run starts the background stall checker (opencode parity): ticks every
// poll interval and calls onStall with the first tripped reason. Fatal
// stalls end monitoring; advisory stalls keep ticking so the session can
// revive (onRecovered) when the missing progress resumes.
func (m *progressMonitor) run(onStall func(execID, reason string), onRecovered func(execID, recovered string)) {
	poll := m.w.noProgress
	if m.w.noFileDiff < poll && m.w.noFileDiff > 0 {
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
		case <-m.stop:
			return
		case <-ticker.C:
			reason := m.check()
			if reason != "" {
				onStall(m.execID, reason)
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

// check returns a non-empty stall reason if any signal has tripped.
func (m *progressMonitor) check() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fired {
		return ""
	}
	now := m.now()
	// tool_hang: in-flight tool call with zero events for the whole
	// window. Advisory, latched once. Checked BEFORE no_progress.
	if m.w.toolHang > 0 && m.toolInFlight && !m.toolHangFired && now.Sub(m.toolHangStart) > m.w.toolHang {
		m.toolHangFired = true
		name := m.toolHangName
		if name == "" {
			name = "unknown"
		}
		return "stalled:tool_hang:" + name
	}
	// no_progress: no token progress within the window. FATAL.
	if now.Sub(m.lastStepFinish) > m.w.noProgress {
		m.fired = true
		return "stalled:no_progress"
	}
	// no_file_diff: no file modifications within the window. ADVISORY
	// (revivable) — reviewers may never write files.
	if m.w.noFileDiff > 0 && now.Sub(m.lastFileDiff) > m.w.noFileDiff && !m.warned {
		m.warned = true
		return "stalled:no_file_progress"
	}
	// text_loop: talking without meaningful action. ADVISORY-first.
	if m.w.textLoop > 0 && now.Sub(m.lastMeaningfulAction) > m.w.textLoop {
		return "stalled:text_loop:You were talking in circles without making progress. On the next attempt, start fresh with a clear plan, make a concrete tool call or file edit within the first few turns."
	}
	// repetition: same signature repeated past the threshold within the
	// window. Tier 1 (error-status) >N; tier 2 (any-status) >2N. Both
	// ADVISORY-first (nudge, then escalate).
	if m.w.repetitionN > 0 {
		cutoff := now.Add(-m.w.repetitionW)
		for sig, ts := range m.sigsErr {
			kept := trimWindow(ts, cutoff)
			m.sigsErr[sig] = kept
			if len(kept) > m.w.repetitionN {
				return fmt.Sprintf("stalled:repetition:%s", sig)
			}
		}
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

// checkRevival clears an active advisory stall when file progress resumed
// (opencode parity — the reconciler flips the execution back to healthy).
func (m *progressMonitor) checkRevival() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.warned {
		return ""
	}
	if m.w.noFileDiff > 0 && m.now().Sub(m.lastFileDiff) <= m.w.noFileDiff {
		m.warned = false
		return "recovered:no_file_progress"
	}
	return ""
}

// close stops the background checker.
func (m *progressMonitor) close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

func appendInWindow(hist []time.Time, now, cutoff time.Time) []time.Time {
	kept := hist[:0]
	for _, t := range hist {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return append(kept, now)
}

func trimWindow(ts []time.Time, cutoff time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// toolCallSignature builds the repetition signature for a native tool
// call: tool name plus a stable canonical marshal of its (normalized)
// arguments. Identical retries collapse; distinct commands stay distinct.
// Normalization applies to ALL calls (opencode normalizes erroring calls;
// native signatures are already exact-args, but content payloads are
// volatile across retries — write/edit drop/fingerprint the payload so a
// retry loop with new content still collapses).
func toolCallSignature(tool, argsJSON string, isError bool) string {
	var args any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool + "|" + argsJSON
	}
	args = normalizeToolArgs(tool, args)
	b, err := json.Marshal(args)
	if err != nil {
		return tool + "|" + argsJSON
	}
	return tool + "|" + string(b)
}

// normalizeToolArgs strips volatile payloads from tool-call args so
// identical retries collapse to one signature while distinct calls stay
// distinct (opencode parity, adapted to the native host tool arg shapes):
//   - write -> key on filePath (drop the volatile content payload)
//   - edit  -> filePath + fingerprints of oldString/newString
//   - bash  -> scrubbed command (volatile tokens replaced)
func normalizeToolArgs(tool string, args any) any {
	m, ok := args.(map[string]any)
	if !ok {
		return args
	}
	switch tool {
	case "write":
		path, _ := m["filePath"].(string)
		if path == "" {
			path, _ = m["path"].(string)
		}
		return map[string]any{"filePath": path}
	case "edit":
		path, _ := m["filePath"].(string)
		if path == "" {
			path, _ = m["path"].(string)
		}
		oldS, _ := m["oldString"].(string)
		newS, _ := m["newString"].(string)
		return map[string]any{
			"filePath":  path,
			"oldString": fingerprint(oldS),
			"newString": fingerprint(newS),
		}
	case "bash":
		cmd, _ := m["command"].(string)
		return map[string]any{"command": scrubCommand(cmd)}
	}
	return args
}

// fingerprint returns a stable short hash so identical content collapses
// while different content stays distinct.
func fingerprint(s string) string {
	if s == "" {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum32())
}

// scrubCommand removes volatile tokens (long hex/digit runs, temp paths)
// from a bash command so retries collapse while distinct commands stay
// distinct (opencode parity).
func scrubCommand(cmd string) string {
	re := regexp.MustCompile(`(?:[0-9a-fA-F]{8,}|[0-9]{4,}|/tmp/[A-Za-z0-9._-]+)`)
	return re.ReplaceAllString(cmd, "<vol>")
}

// isFatalStall reports whether a stall reason terminates the session
// (opencode parity): ONLY no_progress is fatal — a looping worker is
// responsive and holds full context, so it is nudged first; total silence
// has no surface to nudge.
func isFatalStall(reason string) bool {
	return strings.HasPrefix(reason, "stalled:no_progress")
}

// envDuration parses a duration env var with a fallback (opencode parity
// helper).
func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// envInt parses an int env var with a fallback (opencode parity helper).
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if _, err := fmt.Sscanf(v, "%d", new(int)); err == nil {
			var n int
			_, _ = fmt.Sscanf(v, "%d", &n)
			if n > 0 {
				return n
			}
		}
	}
	return fallback
}

// stallNudgeMessages are the escalating liveness/stall nudges injected
// into the live session (opencode parity — the advisory signals reach a
// responsive worker through the mid-run injection queue before any fatal
// escalation).
var stallNudgeMessages = []string{
	"Are you still making progress? If you are stuck in a loop, change approach and move the task forward.",
	"You appear to be repeating the same actions without progress. Step back, pick a different approach, and deliver the work.",
	"This is the final check: break the pattern now. Complete the deliverable with the fewest possible remaining tool calls.",
}

// Nudge tuning (opencode parity): manifest (tenant settings) value first,
// env fallback, built-in defaults last.
const (
	defaultMaxNudges        = 2
	defaultNudgeReplyWindow = 300 * time.Second
	defaultNudgeCooldown    = 60 * time.Second
)

func nudgeMaxFromManifest(manifestMax int32) int {
	if manifestMax > 0 && os.Getenv("ORCHICON_STALL_NUDGE_MAX") == "" {
		return int(manifestMax)
	}
	return envInt("ORCHICON_STALL_NUDGE_MAX", defaultMaxNudges)
}

func nudgeReplyWindowFromManifest(manifestSec int64) time.Duration {
	if manifestSec > 0 && os.Getenv("ORCHICON_STALL_NUDGE_REPLY_WINDOW") == "" {
		return time.Duration(manifestSec) * time.Second
	}
	return envDuration("ORCHICON_STALL_NUDGE_REPLY_WINDOW", defaultNudgeReplyWindow)
}

func nudgeCooldownFromManifest(manifestSec int64) time.Duration {
	if manifestSec > 0 && os.Getenv("ORCHICON_STALL_NUDGE_COOLDOWN") == "" {
		return time.Duration(manifestSec) * time.Second
	}
	return envDuration("ORCHICON_STALL_NUDGE_COOLDOWN", defaultNudgeCooldown)
}

// defaultWallClockTimeout is the hard per-execution timeout applied when
// the merged budget omits wall_clock_seconds (opencode parity).
const defaultWallClockTimeout = 3600 * time.Second

// wallClockDeadline parses the merged budget JSON and returns the absolute
// deadline for the session (opencode parity). Explicit 0 (worker or tenant
// default) disables the hard timeout; unset falls back to the 3600s
// backstop so every session has one. The merged JSON (tenant defaults +
// worker overrides) carries wall_clock_seconds as a top-level number.
func wallClockDeadline(budgets []byte) (time.Time, bool) {
	if v := os.Getenv("ORCHICON_STALL_WALL_CLOCK_SECONDS"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
			return time.Now().Add(d), true
		}
	}
	var spec struct {
		WallClockSeconds *float64 `json:"wall_clock_seconds"`
	}
	if len(budgets) > 0 {
		if err := json.Unmarshal(budgets, &spec); err != nil {
			// Unparseable → same as empty (mirrors the opencode default).
			spec.WallClockSeconds = nil
		}
	}
	// Absent field (or empty/unparseable budgets) → default backstop.
	if spec.WallClockSeconds == nil {
		return time.Now().Add(defaultWallClockTimeout), true
	}
	// Explicit 0 (or negative) disables the hard timeout.
	if *spec.WallClockSeconds <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(*spec.WallClockSeconds * float64(time.Second))), true
}
