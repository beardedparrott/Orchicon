package claude

// progress.go is the claude stall/health monitor — the same progress
// watchdog the opencode adapter and the native engine run (see
// internal/opencode/progress.go and internal/orchicon/progress.go), reduced
// to the signals the claude stream-json transport can actually observe.
//
// Claude Code emits NO step_finish event: a turn's boundary is the streamed
// `result` message and everything in between is text deltas and tool
// round-trips. So "progress" here is ANY stdout activity (text or a tool
// call/result) rather than a token report, and the signals are:
//
//   - no_progress: no stdout activity AT ALL within the no-progress window
//     (manifest StallNoProgressWindowSeconds, env fallback, default 300s).
//     FATAL — a totally silent child has no surface to nudge, so the caller
//     hard-kills it, raises OnStall(reason, fatal=true) and fails the
//     execution.
//   - no_file_progress: activity, but no file write within the no-file
//     window (manifest StallNoFileDiffWindowSeconds, default 120s).
//     ADVISORY: a non-terminal `stalled` health notice; the child keeps
//     running and a later file write clears it through OnRecovered.
//   - repetition: the same tool-call signature repeated past
//     StallRepetitionCount times (default 5) within
//     StallRepetitionWindowSeconds (default 300s). ADVISORY-first, exactly
//     like opencode: a looping-but-responsive worker is told to change
//     approach rather than killed. Reset-on-progress: a file write clears the
//     history, and a completed non-error call clears the error tier.
//
// isFatalStall mirrors opencode exactly: ONLY no_progress is fatal.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

const (
	reasonNoProgress      = "stalled:no_progress"
	reasonNoFileProgress  = "stalled:no_file_progress"
	reasonRepetitionPfx   = "stalled:repetition:"
	reasonRepetitionDone  = "stalled:repetition:completed:"
	recoveredNoFilePrefix = "recovered:no_file_progress"

	// Adapter defaults, used when the tenant left the dimension blank
	// (manifest pointer nil). An explicit 0 DISABLES the dimension.
	defaultStallNoProgressWindow = 300 * time.Second
	defaultStallNoFileDiffWindow = 120 * time.Second
	defaultStallRepetitionCount  = 5
	defaultStallRepetitionWindow = 300 * time.Second
)

// isFatalStall reports whether a stall reason terminates the session
// (opencode parity): ONLY no_progress is fatal — a looping worker is
// responsive and holds full context, so it is nudged instead.
func isFatalStall(reason string) bool {
	return strings.HasPrefix(reason, reasonNoProgress)
}

// stallMonitor is the pure progress watchdog: total silence past the
// no-progress window is FATAL (the caller hard-kills the child), while
// ongoing activity with no file write past the no-file window is ADVISORY
// (the subprocess keeps running; a later file write clears it). A repeated
// tool-call signature is ADVISORY too. It is driven from the session's
// single run goroutine (stream events + the watchdog tick), so it needs no
// internal locking.
type stallMonitor struct {
	started       time.Time
	lastOutput    time.Time
	lastFileWrite time.Time

	noProgressWindow time.Duration
	noFileWindow     time.Duration

	repetitionN int
	repetitionW time.Duration

	// sigsErr holds ERROR-result signatures (tier 1); sigsAll holds
	// ANY-result signatures (tier 2). repFired latches one report per
	// signature until progress resets the history.
	sigsErr  map[string][]time.Time
	sigsAll  map[string][]time.Time
	repFired map[string]bool

	progressed    bool
	advisoryFired bool
	fatalFired    bool
}

// newStallMonitor resolves the windows from the manifest. nil pointer = the
// tenant has no opinion → the adapter default; non-nil = explicit, and 0
// disables that dimension.
func newStallMonitor(m scheduler.ExecutionManifest, now time.Time) *stallMonitor {
	return &stallMonitor{
		started:          now,
		lastOutput:       now,
		lastFileWrite:    now,
		noProgressWindow: resolveWindow(m.StallNoProgressWindowSeconds, defaultStallNoProgressWindow),
		noFileWindow:     resolveWindow(m.StallNoFileDiffWindowSeconds, defaultStallNoFileDiffWindow),
		repetitionN:      resolveRepCount(m.StallRepetitionCount, defaultStallRepetitionCount),
		repetitionW:      resolveWindow(m.StallRepetitionWindowSeconds, defaultStallRepetitionWindow),
		sigsErr:          map[string][]time.Time{},
		sigsAll:          map[string][]time.Time{},
		repFired:         map[string]bool{},
	}
}

// resolveWindow resolves one manifest window: nil = the tenant left it
// blank (env override, then the adapter default); non-nil = explicit, and
// <= 0 DISABLES the dimension.
func resolveWindow(v *int64, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	if *v <= 0 {
		return 0
	}
	return time.Duration(*v) * time.Second
}

// resolveRepCount resolves the repetition count: nil = blank (env then
// default); non-nil <= 0 disables the signal.
func resolveRepCount(v *int32, def int) int {
	if v == nil {
		return envInt("ORCHICON_STALL_REPETITION_COUNT", def)
	}
	if *v <= 0 {
		return 0
	}
	return int(*v)
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// sawOutput records progress (any text or tool activity).
func (s *stallMonitor) sawOutput(now time.Time) {
	s.lastOutput = now
	s.progressed = true
}

// sawFileWrite records a file write and reports whether it cleared an
// outstanding ADVISORY stall (which the caller surfaces via OnRecovered).
// A file write also clears the repetition history (reset-on-progress).
func (s *stallMonitor) sawFileWrite(now time.Time) bool {
	s.lastFileWrite = now
	s.resetRepetition()
	if s.advisoryFired {
		s.advisoryFired = false
		return true
	}
	return false
}

// sawToolCall records one completed tool round-trip with its result status
// for repetition detection (opencode parity: content payloads are fingerprinted
// so a retry with new content still collapses onto one signature).
func (s *stallMonitor) sawToolCall(now time.Time, name, argsJSON string, isError bool) {
	if name == "" {
		return
	}
	sig := toolCallSignature(name, argsJSON, isError)
	cutoff := now.Add(-s.repetitionW)
	if isError {
		s.sigsErr[sig] = appendInWindow(s.sigsErr[sig], now, cutoff)
	} else {
		// Tier 1 is ERROR-status only: a completed call clears the error
		// history (the worker recovered from that failure mode).
		s.sigsErr = map[string][]time.Time{}
	}
	s.sigsAll[sig] = appendInWindow(s.sigsAll[sig], now, cutoff)
}

// resetRepetition clears the signature history and its latches.
func (s *stallMonitor) resetRepetition() {
	s.sigsErr = map[string][]time.Time{}
	s.sigsAll = map[string][]time.Time{}
	s.repFired = map[string]bool{}
}

// Evaluate returns the signal to raise ("" = none) and whether it is fatal.
func (s *stallMonitor) Evaluate(now time.Time) (string, bool) {
	if s.noProgressWindow > 0 && !s.fatalFired && now.Sub(s.lastOutput) > s.noProgressWindow {
		s.fatalFired = true
		return reasonNoProgress, true
	}
	if s.noFileWindow > 0 && !s.advisoryFired && !s.fatalFired && s.progressed &&
		now.Sub(s.lastFileWrite) > s.noFileWindow {
		s.advisoryFired = true
		return reasonNoFileProgress, false
	}
	// repetition is ADVISORY (nudge-first, opencode parity): the worker is
	// responsive, so it is told to change approach rather than killed.
	if reason := s.evaluateRepetition(now); reason != "" {
		return reason, false
	}
	return "", false
}

func (s *stallMonitor) evaluateRepetition(now time.Time) string {
	if s.repetitionN <= 0 {
		return ""
	}
	cutoff := now.Add(-s.repetitionW)
	for sig, ts := range s.sigsErr {
		kept := trimWindow(ts, cutoff)
		s.sigsErr[sig] = kept
		if len(kept) > s.repetitionN {
			key := "err:" + sig
			if !s.repFired[key] {
				s.repFired[key] = true
				return reasonRepetitionPfx + sig
			}
		}
	}
	tier2 := s.repetitionN * 2
	if tier2 < s.repetitionN+3 {
		tier2 = s.repetitionN + 3
	}
	for sig, ts := range s.sigsAll {
		kept := trimWindow(ts, cutoff)
		s.sigsAll[sig] = kept
		if len(kept) > tier2 {
			key := "all:" + sig
			if !s.repFired[key] {
				s.repFired[key] = true
				return reasonRepetitionDone + sig
			}
		}
	}
	return ""
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

// toolCallSignature builds the repetition signature for a claude tool call:
// the canonical tool name plus a stable canonical marshal of its normalized
// arguments, so identical retries collapse while distinct calls stay
// distinct (opencode parity).
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

// normalizeToolArgs strips volatile payloads from claude's built-in tool
// arguments so a retry with fresh content still collapses onto one
// signature (opencode parity, adapted to claude's arg vocabulary):
//   - write -> file_path (the volatile content payload is dropped)
//   - edit  -> file_path + fingerprints of old_string/new_string
//   - bash  -> scrubbed command
func normalizeToolArgs(tool string, args any) any {
	m, ok := args.(map[string]any)
	if !ok {
		return args
	}
	switch tool {
	case "write":
		return map[string]any{"filePath": strField(m, "file_path")}
	case "edit":
		return map[string]any{
			"filePath":  strField(m, "file_path"),
			"oldString": fingerprint(strField(m, "old_string")),
			"newString": fingerprint(strField(m, "new_string")),
		}
	case "bash":
		return map[string]any{"command": scrubCommand(strField(m, "command"))}
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
