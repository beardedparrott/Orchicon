package toolclass

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DefaultWindow is the rolling window the activity line counts over.
//
// 30s IS THE OPERATOR'S CHOICE, and the reasoning is this line's own cadence: the transcript's
// activity line is repainted about once a second and the server heartbeats every 15s, so 30s is
// the shortest span that (a) still shows a burst of work after it has stopped — long enough not
// to blink out between repaints — and (b) is not outlived by the stall the same line is already
// reporting (the re-dial notice fires at 35s, internal/tui/app.go). It is a named constant, not a
// literal, because the Go caller and the GUI must count the SAME span.
const DefaultWindow = 30 * time.Second

// ledgerCall is the subset of the tool_calls JSON this package reads. The shape is
// internal/askorchicon/tool_ledger.go (id/type/function_name/arguments) plus the issue timestamp
// child 1 adds to each entry: `issued_at_unix_ms`, epoch MILLISECONDS — the clients' existing
// convention (chat.ParsedTool.At, internal/tui/chat/grouping.go).
//
// THE FIELD NAME IS NOT NEGOTIABLE, and it is the ONE thing this package must agree with the
// ledger on. internal/askorchicon/toolCallEntry stamps `json:"issued_at_unix_ms"`
// (tool_ledger.go:37) and carries it through askToolCallJSON in chathistory.go; if this tag ever
// drifts from that one, json.Unmarshal silently leaves the field 0, every entry is skipped as
// "no timestamp", and the line renders "" over a turn that is plainly working — with no error and
// no failing fixture, because the fixture is written in this package's own vocabulary. That is why
// TestSummarizeCountsTheRealLedgerShape (askorchicon side) unmarshals a REAL ledger snapshot.
type ledgerCall struct {
	FunctionName   string `json:"function_name"`
	IssuedAtUnixMs int64  `json:"issued_at_unix_ms"`
}

// Summarize renders the rolling-window activity summary for a turn's tool_calls JSON:
//
//	"5 modifies · 2 reads · 3 bash · last 30s"
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
// on one line cannot disagree.
func Summarize(callsJSON []byte, now time.Time, window time.Duration) string {
	var calls []ledgerCall
	if err := json.Unmarshal(callsJSON, &calls); err != nil {
		return ""
	}
	if window <= 0 {
		// A non-positive window is a caller slip, not a request for a blank line: "the last
		// zero seconds" is not a display anybody wants. Fall back to the named default rather
		// than silently rendering "" over a turn that is plainly working.
		window = DefaultWindow
	}
	nowMs := now.UnixMilli()
	var modifies, reads, bashes int
	newest := int64(0)
	counted := false
	for _, c := range calls {
		if c.IssuedAtUnixMs == 0 {
			// No timestamp: a pre-change row, or a synthesized one. An entry that cannot be
			// placed in a window is skipped, so an old conversation does not render a wrong count.
			continue
		}
		ageMs := nowMs - c.IssuedAtUnixMs
		if ageMs < 0 {
			ageMs = 0 // clock skew / a future stamp: clamp, never drop the work.
		}
		// INCLUSIVE on the left: an entry exactly `window` old IS counted. The window is "the
		// last 30s" and the canonical example's newest entry sits exactly on that edge.
		if time.Duration(ageMs)*time.Millisecond > window {
			continue
		}
		switch Classify(c.FunctionName) {
		case Modify:
			modifies++
		case Read:
			reads++
		case Bash:
			bashes++
		default:
			continue // Ignore: ask_user, permission.*, MCP, unknown names.
		}
		// Track the NEWEST COUNTED entry explicitly rather than seeding a 0 sentinel and taking
		// the max: a malformed negative stamp would then leave `newest` at 0 and the trailing age
		// would be measured from the epoch, not from the entry that was just counted.
		if !counted || c.IssuedAtUnixMs > newest {
			newest = c.IssuedAtUnixMs
		}
		counted = true
	}
	if !counted {
		return ""
	}

	// Ordering is FIXED (modify, read, bash) so the line does not reorder as counts change.
	parts := make([]string, 0, 4)
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
	ageMs := nowMs - newest
	if ageMs < 0 {
		ageMs = 0
	}
	// ageMs is MILLISECONDS: scale to a Duration, then apply the activity line's own rule
	// (internal/tui/app.go: `int(d.Round(time.Second)/time.Second)`).
	ageSecs := int((time.Duration(ageMs) * time.Millisecond).Round(time.Second) / time.Second)
	parts = append(parts, fmt.Sprintf("last %ds", ageSecs))
	return strings.Join(parts, " · ")
}

// countLabel renders one bucket's phrase with correct singular/plural.
func countLabel(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}
