package toolclass

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DefaultWindow is the rolling window the activity line counts over.
const DefaultWindow = 30 * time.Second

// ledgerCall is the subset of the tool_calls JSON this package reads.
type ledgerCall struct {
	FunctionName   string `json:"function_name"`
	IssuedAtUnixMs int64  `json:"issued_at_unix_ms"`
}

// Summarize renders the rolling-window activity summary for a turn's tool_calls JSON:
//
//	"5 modifies · 2 reads · 3 bash · newest call 30s ago"
//
// It is PURE and TOTAL: no I/O, no clock of its own (the caller passes now), and it never panics.
// An empty, nil, unparseable, JSON-null or non-array input returns "".
//
// A class with count 0 is omitted entirely and the separator never dangles. If every class is 0
// the result is the EMPTY STRING — load-bearing, because the caller APPENDS this to an existing
// line and "0 modifies" would be a false claim that work is happening.
//
// The trailing age is the age of the NEWEST COUNTED entry, rounded with the SAME rule as the
// activity line (internal/tui/app.go: `int(d.Round(time.Second)/time.Second)`) so the two numbers
// on one line cannot disagree. It is WORDED `newest call Ns ago`, not a bare `last Ns`, so it
// cannot be misread as the WINDOW's length (AC7): the window is 30s and a bare `last 12s` on the
// same line reads like a different window.
func Summarize(callsJSON []byte, now time.Time, window time.Duration) string {
	var calls []ledgerCall
	if err := json.Unmarshal(callsJSON, &calls); err != nil {
		return ""
	}
	in := make([]Call, 0, len(calls))
	for _, c := range calls {
		in = append(in, Call{ToolName: c.FunctionName, AtMs: c.IssuedAtUnixMs})
	}
	return SummarizeCalls(in, now, window)
}

// Call is one tool call the ACTIVITY LINE can count: the name to classify and the epoch-ms instant
// it was issued.
type Call struct {
	// ToolName is the tool's name as the model issued it ("write", "orchicon_bash", …). It is
	// classified by Classify, so an unknown name is Ignore rather than a new bucket.
	ToolName string
	// AtMs is when the call was issued, epoch MILLISECONDS — the clients' existing convention
	// (chat.ParsedTool.At). 0 means "no timestamp" and is SKIPPED, never treated as the epoch.
	AtMs int64
}

// SummarizeCalls is Summarize over calls the caller already holds: the SAME rendering rule, no
// JSON.
func SummarizeCalls(calls []Call, now time.Time, window time.Duration) string {
	if window <= 0 {
		// A non-positive window is a caller slip, not a request for a blank line: "the last zero seconds"
		// is not a display anybody wants.
		window = DefaultWindow
	}
	nowMs := now.UnixMilli()
	var modifies, reads, bashes, others int
	newest := int64(0)
	counted := false
	for _, c := range calls {
		if c.AtMs == 0 {
			// No timestamp: a pre-change row, or a synthesized one. An entry that cannot be
			// placed in a window is skipped, so an old conversation does not render a wrong count.
			continue
		}
		ageMs := nowMs - c.AtMs
		if ageMs < 0 {
			ageMs = 0 // clock skew / a future stamp: clamp, never drop the work.
		}
		// INCLUSIVE on the left: an entry exactly `window` old IS counted. The window is "the
		// last 30s" and the canonical example's newest entry sits exactly on that edge.
		if time.Duration(ageMs)*time.Millisecond > window {
			continue
		}
		switch Classify(c.ToolName) {
		case Modify:
			modifies++
		case Read:
			reads++
		case Bash:
			bashes++
		case Other:
			others++
		default:
			continue // Ignore: ask_user, permission.*, todowrite, the empty name.
		}
		// Track the NEWEST COUNTED entry explicitly rather than seeding a 0 sentinel and taking the max: a
		// malformed negative stamp would then leave `newest` at 0 and the trailing age would be measured.
		if !counted || c.AtMs > newest {
			newest = c.AtMs
		}
		counted = true
	}
	if !counted {
		return ""
	}

	// Ordering is FIXED (modify, read, bash, other) so the line does not reorder as counts change.
	parts := make([]string, 0, 5)
	if modifies > 0 {
		parts = append(parts, countLabel(modifies, "modify", "modifies"))
	}
	if reads > 0 {
		parts = append(parts, countLabel(reads, "read", "reads"))
	}
	if bashes > 0 {
		// "bash" is deliberately INVARIABLE: one row in the TUI has no room for "bash calls",
		// and the shared string is the compact form both clients render.
		parts = append(parts, countLabel(bashes, "bash", "bash"))
	}
	if others > 0 {
		// The catch-all goes LAST (it is the residual bucket) and the three named buckets precede it, so
		// the line still never reorders as counts change (AC6).
		parts = append(parts, countLabel(others, "other tool", "other tools"))
	}
	ageMs := nowMs - newest
	if ageMs < 0 {
		ageMs = 0
	}
	// ageMs is MILLISECONDS: scale to a Duration, then apply the activity line's own rule
	// (internal/tui/app.go: `int(d.Round(time.Second)/time.Second)`).
	ageSecs := int((time.Duration(ageMs) * time.Millisecond).Round(time.Second) / time.Second)
	// `newest call Ns ago` NAMES what is being aged. A bare `last Ns` read as "the window is Ns
	// long" (AC7) — the operator's own misreading of `last 12s` against a 30s window.
	parts = append(parts, fmt.Sprintf("newest call %ds ago", ageSecs))
	return strings.Join(parts, " · ")
}

// countLabel renders one bucket's phrase with correct singular/plural.
func countLabel(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
