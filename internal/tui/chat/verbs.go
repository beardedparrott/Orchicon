package chat

import (
	"os"
	"strings"
)

// verbs.go — THE ACTIVITY VERB ROTATES, INDEXED ON THE SERVER'S CLOCK.
//
// The operator: "I think we could also liven up the conversations by rotating through a series of words
// that means 'orchicon is thinking' but variations like 'inquisiting, contemplating, planning, etc.' that
// changes every few seconds. We should have a ton of them."
//
// THAT BRIEF WAS WRONG ABOUT THE CADENCE, AND THIS RECORD CORRECTS IT. It set the period to 4s and
// explicitly rejected the heartbeat-aligned value as too slow. The operator's verdict on shipping it:
// "the values are changing WAY too often. Every single update is a new word for 'thinking'." The line
// repaints about once a second (the TUI on every stream chunk and the 1s askTurnPollInterval), so a 4s
// period swapped the word on roughly every fourth repaint — churn on a line the operator stares at for
// minutes. The period is now 15s, the server heartbeat cadence (askHeartbeatInterval).
//
// Before this, the TUI drew a fixed pair of literals ("Orchicon is thinking…" before content, "Orchicon is
// replying…" after), so a long quiet phase looked byte-identical for minutes. That is the complaint.
//
// THE INDEX IS A PURE FUNCTION OF SERVER TIME. AskVerbs/VerbAt below take the timestamp the server already
// puts on the wire — Heartbeat.server_time_unix_ms (emitted at internal/askorchicon/chat.go, whose own doc
// says it exists so a client can measure socket age/skew). Two clients holding the same server stamp draw
// the SAME word, with no shared state, no new RPC and no server-side counter; and because the client is not
// consulting its own wall clock for the index, a client whose clock is skewed still draws the same word
// (the callers apply only a DELTA since their own receipt instant, to keep the word advancing between
// heartbeats without ever inventing a clock).

// AskVerbs is the rotation. "A ton of them": 72 entries, all present participles that read as thinking or
// working, and every one of them is a grammatical continuation of "Orchicon is …".
//
// SHAPE IS A CONSTRAINT, NOT A PREFERENCE. The line is a ONE-ROW TUI footer, so every entry is a single
// lowercase a-z word of at most VerbCellCap (14) characters — no spaces, no punctuation, no emoji, nothing
// the `ascii` theme could not draw (the longest approved entry, "contemplating"/"investigating"/
// "hypothesizing", is 13). And NOTHING IS A LIVENESS CLAIM: the rotation says what the turn is DOING to
// the problem, never that the stream is receiving or the work is progressing, because a stalled stream
// would make such a word false. (TestAskVerbsNeverClaimLiveness pins that; it mirrors
// activity_line_test.go's TestTheLineNeverClaimsActivityItCannotSee.)
//
// THE LIST IS PINNED TO frontend/src/lib/ask-verbs.json, byte for byte and in ORDER, by
// TestAskVerbsMatchTheSharedFixture — the same one-fixture-both-languages binding the Schedules pane uses
// (frontend/src/lib/schedules-fixture.json read by internal/tui/screens/execution/schedules_queued_test.go).
// A drift between the Go and TS lists fails a test in BOTH languages.
var AskVerbs = []string{
	"thinking", "pondering", "contemplating", "considering", "deliberating",
	"musing", "ruminating", "cogitating", "reasoning", "reflecting",
	"meditating", "mulling", "brooding", "speculating", "hypothesizing",
	"theorizing", "analyzing", "scrutinizing", "examining", "inspecting",
	"investigating", "inquiring", "inquisiting", "questioning", "probing",
	"exploring", "researching", "studying", "surveying", "scouting",
	"mapping", "charting", "plotting", "planning", "scheming",
	"strategizing", "devising", "designing", "drafting", "sketching",
	"outlining", "organizing", "arranging", "sorting", "sifting",
	"parsing", "dissecting", "unraveling", "untangling", "synthesizing",
	"assembling", "composing", "crafting", "constructing", "shaping",
	"refining", "polishing", "honing", "pruning", "distilling",
	"conjuring", "calculating", "computing", "enumerating", "estimating",
	"weighing", "balancing", "verifying", "validating", "rehearsing",
	"preparing", "calibrating",
}

// VerbCellCap is the widest entry the one-row footer will accept. It is asserted against the list, not
// merely documented (TestAskVerbsAreAsciiShortAndUnique), so a future entry that does not fit is a test
// failure rather than a clipped or wrapped footer.
const VerbCellCap = 14

// VerbPeriodMS is how long one word stays up: 15s, the server's heartbeat cadence (askHeartbeatInterval,
// internal/askorchicon/chat.go), so the word advances about once per heartbeat.
//
// IT WAS 4s FIRST, AND THAT WAS THE BUG. The original brief asked for a word that changed "every few
// seconds" and explicitly rejected the heartbeat-aligned value as too slow, and 4s was chosen to satisfy
// it. The operator's verdict on shipping it: "the values are changing WAY too often. Every single update
// is a new word for 'thinking'." The activity line repaints about once a second — the TUI on every stream
// chunk and on the 1s askTurnPollInterval — so a 4s period swapped the word on roughly every fourth
// repaint: churn on a line the operator stares at for minutes. 15s, the operator's decision, fixes that.
//
// The *source* of the index is still the server stamp: between heartbeats each client extrapolates from
// its own receipt instant (applying only a DELTA), so no client invents a clock, no client needs to hear
// a heartbeat to keep drawing the same word, and the word does not lag a whole period behind a late
// heartbeat.
const VerbPeriodMS int64 = 15000

// VerbAt is the pure selector: the same timestamp always yields the same word, on any client, in any
// process. No clock, no RNG, no global mutable state — integer division and modulo only, so Go and JS
// cannot disagree on the result.
//
// t <= 0 IS THE "NO HEARTBEAT YET" SENTINEL (a real server stamp is Unix milliseconds and always
// positive), and it returns AskVerbs[0]. That is the fallback for the moment right after the operator
// sends — before-content, first second — which is exactly when they are most likely to be looking: the
// line must never be empty and must never panic.
func VerbAt(serverTimeUnixMs int64) string {
	if serverTimeUnixMs <= 0 {
		return AskVerbs[0]
	}
	return AskVerbs[(serverTimeUnixMs/VerbPeriodMS)%int64(len(AskVerbs))]
}

// VerbRotationOn reports whether the rotation may advance. It is the accessibility off-switch, and it is
// part of the feature rather than a follow-up: the rotation IS animation.
//
// MECHANISM: ORCHICON_TUI_NO_ROTATE, truthy when set to "1" or "true" (case-insensitive). An env knob
// rather than a persisted preference, matching the idiom already in this codebase (chat.go's
// ORCHICON_ASK_HEARTBEAT_INTERVAL) and cheap to assert on both sides of the parity line (the GUI's mirror
// is prefers-reduced-motion, see frontend/src/lib/ask-verbs.ts).
func VerbRotationOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ORCHICON_TUI_NO_ROTATE"))) {
	case "1", "true":
		return false
	default:
		return true
	}
}

// ActivityVerb is what the activity line asks for: the rotating word for this server time, or the still
// first entry when the operator has turned the rotation off. Keeping the off-switch here (rather than at
// the call site) means one code path produces the word, and both the pre-heartbeat fallback and the
// disabled case land on AskVerbs[0] — "the line is present and says something true" is the same promise in
// both.
func ActivityVerb(serverTimeUnixMs int64) string {
	if !VerbRotationOn() {
		return AskVerbs[0]
	}
	return VerbAt(serverTimeUnixMs)
}
