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
}

func defaultStallWindows() stallWindows {
	return stallWindows{
		noProgress:  envDuration("ORCHICON_STALL_NO_PROGRESS_WINDOW", 300*time.Second),
		noFileDiff:  envDuration("ORCHICON_STALL_NO_FILE_DIFF_WINDOW", 15*time.Minute),
		textLoop:    envDuration("ORCHICON_STALL_TEXT_LOOP_WINDOW", 10*time.Minute),
		repetitionN: envInt("ORCHICON_STALL_REPETITION_COUNT", 5),
		repetitionW: envDuration("ORCHICON_STALL_REPETITION_WINDOW", 300*time.Second),
	}
}

// stallWindowsFromManifest builds stallWindows from ExecutionManifest
// settings, with env-var fallback for dev overrides. Zero (unset) values in
// the manifest fall through to env vars, which fall through to code
// defaults.
//
// noFileDiff/textLoop are the two advisory windows Settings documents as
// "0 = disabled" — but 0 is also the manifest's unset value (a fresh/
// never-configured tenant_settings row), so a plain `> 0` guard cannot
// tell "never configured" from "explicitly disabled" and previously just
// treated both as unset (the built-in default always applied — "0 =
// disabled" was aspirational, nothing could ever actually disable these).
// A negative value is unambiguous and needs no schema change: 0/unset →
// default (safe — a never-configured tenant keeps real stall detection),
// positive → explicit override, negative → the resulting duration is
// itself <= 0, which every consumer below (the checker in this file)
// already gates on `> 0`, so it naturally reads as disabled with no
// change needed anywhere else.
func stallWindowsFromManifest(m scheduler.ExecutionManifest) stallWindows {
	w := defaultStallWindows()
	if m.StallNoProgressWindowSeconds > 0 {
		if v := time.Duration(m.StallNoProgressWindowSeconds) * time.Second; os.Getenv("ORCHICON_STALL_NO_PROGRESS_WINDOW") == "" {
			w.noProgress = v
		}
	}
	if m.StallNoFileDiffWindowSeconds != 0 {
		if v := time.Duration(m.StallNoFileDiffWindowSeconds) * time.Second; os.Getenv("ORCHICON_STALL_NO_FILE_DIFF_WINDOW") == "" {
			w.noFileDiff = v
		}
	}
	if m.StallTextLoopWindowSeconds != 0 {
		if v := time.Duration(m.StallTextLoopWindowSeconds) * time.Second; os.Getenv("ORCHICON_STALL_TEXT_LOOP_WINDOW") == "" {
			w.textLoop = v
		}
	}
	if m.StallRepetitionCount > 0 {
		if os.Getenv("ORCHICON_STALL_REPETITION_COUNT") == "" {
			w.repetitionN = int(m.StallRepetitionCount)
		}
	}
	if m.StallRepetitionWindowSeconds > 0 {
		if v := time.Duration(m.StallRepetitionWindowSeconds) * time.Second; os.Getenv("ORCHICON_STALL_REPETITION_WINDOW") == "" {
			w.repetitionW = v
		}
	}
	return w
}

// progressMonitor tracks per-execution progress signals and detects stalls.
// It is fed events from parseEvent (the session-transport bus mapping) and
// runs a background ticker that checks the stall windows. Thread-safe (the
// event goroutine + the ticker both touch it).
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
	// Only ERROR-status tool calls are recorded (result-aware repetition);
	// a completed call or file_diff resets the history (progress resets
	// the counters).
	sigs map[string][]time.Time

	// fired tracks whether the monitor has already raised a FATAL stall
	// (no_progress / text_loop / repetition) for this execution. A fatal
	// trigger fires once; the hard-kill + recovery it triggers is the
	// response (docs/06 §9 idempotency guards against re-trigger).
	fired bool

	// warned tracks an active ADVISORY stall (no_file_progress). Unlike a
	// fatal trip, an advisory trip does not end monitoring: the subprocess
	// is not killed, the execution stays running with a non-terminal
	// `stalled` health notice, and the monitor keeps ticking so it can
	// revive the execution to `healthy` (checkRevival) when the missing
	// file progress resumes — or trip a fatal signal if the worker truly
	// hangs. `warned` clears on revival so the window can trip again.
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
		sigs:                 make(map[string][]time.Time),
		stop:                 make(chan struct{}),
	}
	return m
}

// observe feeds a parsed event into the monitor. Called from the session
// transport's event dispatch (parseEvent). eventType matches opencode's
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
		// A step finishing is real progress — reset the repetition
		// signature history (the worker is making forward progress, not
		// looping).
		m.sigs = make(map[string][]time.Time)
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
		m.sigs = make(map[string][]time.Time)
	case "tool_use", "tool_call":
		m.lastMeaningfulAction = now
		// Result-aware repetition: only ERROR-status tool calls count
		// toward the repetition threshold. A completed call is real
		// progress — it resets the signature history (the normal
		// build-fix-iterate-debug loop).
		status, _ := part["state"].(map[string]any)["status"].(string)
		if status == "error" {
			sig := toolUseSignature(part)
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
		} else {
			// completed (or unknown) — progress resets the counters.
			m.sigs = make(map[string][]time.Time)
		}
	}
}

// toolUseSignature builds the repetition signature for a tool event: the
// tool name plus a stable, canonical marshal of its arguments. Repeating
// the exact same call (same tool, same args) is the loop signal.
//
// opencode's `--format json` emits tool parts with args nested under
// part.state.input (e.g. state.input.command for bash), NOT under a
// top-level part["args"] (progress.go's legacy tool_call case read the
// nonexistent top-level field, so every call of a tool collapsed to one
// signature like "bash|null"). LegacyEventFromBus (session.go) maps these
// to the "tool_use" event type. Fall back to the whole state when input is
// absent so signature extraction degrades gracefully.
//
// Go's encoding/json marshal of a map[string]any sorts map keys, so equal
// args maps produce identical bytes regardless of insertion order — this
// gives stable signatures for semantically identical calls.
//
// Conservative normalization (design A, gated through result-aware B):
// the signature is normalized ONLY for ERRORING calls, so a worker looping
// on a blocked write (each retry carrying different volatile content/path)
// collapses to a stable signature while genuinely distinct legitimate
// commands stay distinct. Successful calls keep the exact full-args
// signature (never collapsed) and reset the counter state in observe().
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

// scrubCommand removes volatile tokens from a bash command so a worker
// retrying the same command with different timestamps/temp IDs collapses to
// one signature, while distinct commands (git status vs git log) stay
// distinct. It replaces long hex/digit runs and common volatile patterns
// with a placeholder.
func scrubCommand(cmd string) string {
	re := regexp.MustCompile(`(?:[0-9a-fA-F]{8,}|[0-9]{4,}|/tmp/[A-Za-z0-9._-]+)`)
	return re.ReplaceAllString(cmd, "<vol>")
}

// run starts the background stall checker. It ticks every pollInterval
// and, on the first tripped signal, calls onStall once with the reason.
// A fatal stall ends monitoring (the subprocess is hard-killed). An
// advisory stall (no_file_progress) keeps monitoring so the execution can
// revive to healthy via onRecovered when the missing file progress resumes.
func (m *progressMonitor) run(ctx context.Context, onStall func(execID, reason string), onRecovered func(execID, recovered string)) {
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
	// no_progress: no step_finish (no token progress) within the window.
	if now.Sub(m.lastStepFinish) > m.w.noProgress {
		m.fired = true
		return "stalled:no_progress"
	}
	// no_file_diff: no file modifications within the window.
	// Skipped when noFileDiff is 0 or negative (disabled), because
	// reviewers/QA workers may never write files.
	//
	// ADVISORY (not fatal): trips into the revivable `warned` state rather
	// than `fired`. The subprocess is not killed and the execution is not
	// failed — it gets a non-terminal `stalled` health notice and revives
	// to healthy (checkRevival) once a file_diff arrives again. A reviewer
	// or analyst may legitimately go long stretches without touching files
	// while still producing output (observed: the SSE worker was flagged
	// yet completed successfully).
	if m.w.noFileDiff > 0 && now.Sub(m.lastFileDiff) > m.w.noFileDiff && !m.warned {
		m.warned = true
		return "stalled:no_file_progress"
	}
	// text_loop: text flowing but no meaningful action (tool call, file
	// diff, step finish) within the window. Catches workers that talk
	// forever ("but wait, let me reconsider…") without ever doing work.
	//
	// ADVISORY-first (nudge-first routing): the worker is generating text,
	// so a nudge can reach it. The monitor keeps ticking so the session's
	// onStall can nudge the live session; only after the nudge budget is
	// spent (and the worker still hasn't broken the pattern) does the
	// session escalate to a fatal kill + recovery.
	if m.w.textLoop > 0 && now.Sub(m.lastMeaningfulAction) > m.w.textLoop {
		return "stalled:text_loop:You were talking in circles without making progress. On the next attempt, start fresh with a clear plan, make a concrete tool call or file edit within the first few turns."
	}
	// repetition: same tool_call signature repeated more than the
	// threshold within the window.
	//
	// ADVISORY-first (nudge-first routing): same as text_loop — nudge the
	// live session first, escalate to fatal only after the nudge budget is
	// spent.
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
				return fmt.Sprintf("stalled:repetition:%s", sig)
			}
		}
	}
	return ""
}

// checkRevival returns a revival reason (and clears the `warned` state)
// when an active advisory stall has cleared — i.e. the missing progress
// signal arrived before the execution terminated. Currently the only
// advisory signal is no_file_progress, which clears when a file_diff event
// arrives again: lastFileDiff only advances on file_diff events, so the
// "back within the window" condition is exactly "a diff landed since the
// trip". The caller (run) turns this into an OnRecovered callback so the
// reconciler flips the execution back to healthy and clears the stall
// notice, while status stays running and the terminal OnResult still
// decides success/failure.
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

// revive clears an active advisory stall (warned) and resets every stall
// timestamp to now. Called when a liveness probe reply proves the worker
// is alive even though it has not touched files — the probe response IS
// the progress signal, so the advisory notice clears and the file window
// restarts. Returns whether an advisory stall was actually active.
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

// defaultWallClockTimeout is the hard per-execution timeout applied when a
// worker's final merged budget (tenant default + worker override) omits
// wall_clock_seconds. AGENTS.md documents this as the default (3600s) — the
// runaway-spend backstop that kills the subprocess even when the model is
// still producing output.
const defaultWallClockTimeout = 3600 * time.Second

// wallClockDeadline parses the execution's merged budget JSON
// (budget_overrides after tenant-default merge, docs/05 §8) and returns the
// absolute deadline for the execution. Returns ok=false if no wall-clock
// budget is set (no deadline). The deadline is enforced via
// context.WithDeadline in Start; when it hits, the subprocess is killed
// (exec.CommandContext) and OnResult(false) fires → recovery with reason
// "wall_clock_timeout" (docs/06 §2 budget overrun trigger).
//
// Precedence (applied before this call, in the reconciler): worker
// budget_overrides.wall_clock_seconds (explicit) → tenant
// default_budget_overrides.wall_clock_seconds → this default (3600). An
// explicit 0 in the worker OR the tenant default disables the hard timeout
// (relying solely on stall detection); an unset field falls back to
// defaultWallClockTimeout so every execution has a hard backstop.
//
// The wall-clock dimension shares the single budget parse (parseBudgetSpec)
// with the compaction gate so both read the same merged budget.
func wallClockDeadline(ctx context.Context, budgets []byte) (time.Time, bool) {
	if v := os.Getenv("ORCHICON_STALL_WALL_CLOCK_SECONDS"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
			return time.Now().Add(d), true
		}
	}
	spec := parseBudgetSpec(budgets)
	// Absent field (or empty/unparseable budgets) → default backstop (3600s).
	if spec.wallClockSeconds == nil {
		return time.Now().Add(defaultWallClockTimeout), true
	}
	// Explicit 0 (or negative) disables the hard timeout.
	if *spec.wallClockSeconds <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(*spec.wallClockSeconds * float64(time.Second))), true
}
