package tui

// activity_line_test.go — THE STREAM SAYS IT IS ALIVE, AND HOW FRESH IT IS.
//
// The operator: "I say we implement the watchdog and add the line at the bottom of the chat stream so
// a user knows activity is happening."
//
// The watchdog lives in the chat controller — it re-dials a stream that has gone silent past its
// timeout (see internal/tui/chat/liveness_watch_test.go). This file covers the other half: the LINE.
//
// Why the line matters as much as the watchdog. A bare "Orchicon is thinking…" is STATIC TEXT — it
// looks identical whether the stream is delivering a chunk a second or has been dead for a minute.
// That is precisely why the operator had to read two screens side by side to work out something was
// wrong: the GUI showed "Last activity 1s ago" and the TUI showed nothing that changed.
//
// The age comes from the SAME clock the watchdog reads (chat.Controller.SilenceSince, over the
// lastActivity the liveness check uses), so the line cannot claim a liveness the watchdog would
// contradict. That coupling is what makes it a report rather than a decoration.

import (
	"testing"
	"time"
)

// THE LINE IS QUIET UNTIL SILENCE IS WORTH MENTIONING, then states the age, then names the verdict.
//
// All the boundaries are asserted because the boundaries ARE the behaviour: a line that said "no
// output for 2s" would twitch on every chunk, and one that never escalated would leave the operator
// waiting on a dead stream with no warning at all.
func TestThinkingNoticeEscalatesWithSilence(t *testing.T) {
	cases := []struct {
		silent time.Duration
		want   string
		why    string
	}{
		{0, "Orchicon is thinking…", "no activity recorded yet — the moment after sending"},
		{-time.Second, "Orchicon is thinking…", "a clock that stepped backwards is not a silence"},
		{2 * time.Second, "Orchicon is thinking… · last activity 2s ago", "the age is shown from one second: the operator asked for the line to be visible right away, not after a pause"},
		{4 * time.Second, "Orchicon is thinking… · last activity 4s ago", "an ordinary quiet stretch"},
		{time.Second, "Orchicon is thinking… · last activity 1s ago", "the first second: the line is visible immediately, which is what the operator asked for"},
		{12 * time.Second, "Orchicon is thinking… · last activity 12s ago", "an ordinary quiet stretch"},
		{24 * time.Second, "Orchicon is thinking… · last activity 24s ago", "still under the heartbeat margin"},
		{25 * time.Second, "Orchicon is thinking… · no output for 25s", "past the 15s heartbeat: worth stating"},
		{34 * time.Second, "Orchicon is thinking… · no output for 34s", "approaching the watchdog's verdict"},
		{35 * time.Second, "Orchicon is thinking… · no output for 35s — the stream will re-attach if it stays silent",
			"the watchdog re-dials at 40s, so the operator is told before it happens"},
		{90 * time.Second, "Orchicon is thinking… · no output for 90s — the stream will re-attach if it stays silent",
			"a long stall still reports the same thing rather than inventing a new state"},
	}
	for _, c := range cases {
		if got := thinkingNotice(c.silent); got != c.want {
			t.Errorf("thinkingNotice(%v) = %q, want %q (%s)", c.silent, got, c.want, c.why)
		}
	}
}

// EVERY BAND KEEPS THE INDICATOR, so the line continues the state the operator already knows rather
// than replacing it — the pane does not appear to change what it is doing as the seconds climb.
func TestEveryBandStillSaysItIsThinking(t *testing.T) {
	for _, silent := range []time.Duration{0, time.Second, 5 * time.Second, 30 * time.Second, time.Minute} {
		if got := thinkingNotice(silent); !containsStr(got, "Orchicon is thinking…") {
			t.Errorf("thinkingNotice(%v) = %q, which no longer says the turn is in progress", silent, got)
		}
	}
}

// AND THE LINE NEVER CLAIMS ACTIVITY IT CANNOT SEE: the wording for a growing gap says "no output",
// never that the stream is receiving. A reassurance that cannot be true is worse than silence,
// because it hides the very failure this work exists to expose.
func TestTheLineNeverClaimsActivityItCannotSee(t *testing.T) {
	for _, silent := range []time.Duration{26 * time.Second, 40 * time.Second, time.Minute, time.Hour} {
		got := thinkingNotice(silent)
		if containsStr(got, "receiving") || containsStr(got, "still receiving") {
			t.Errorf("thinkingNotice(%v) = %q — it asserts the stream is receiving, which is exactly the "+
				"claim a stalled stream would be making falsely", silent, got)
		}
	}
}
