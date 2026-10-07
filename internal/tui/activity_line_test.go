package tui

// activity_line_test.go — THE STREAM SAYS WHAT IT IS DOING, AND HOW FRESH IT IS.
//
// The operator: "I say we implement the watchdog and add the line at the bottom of the chat stream so a
// user knows activity is happening" — and then, when the line vanished mid-reply: "After the initial
// 'Orchicon is thinking...', streaming started and the 'Orchicon is thinking...' went away and never came
// back."
//
// So the line runs for the WHOLE turn. Its verb ROTATES on the server's clock (chat/verbs.go), and its age
// comes from the SAME clock the watchdog reads (chat.Controller.SilenceSince, over the lastActivity the
// liveness check uses), so the line cannot claim a liveness the watchdog would contradict — that coupling is
// what makes it a report rather than a decoration.
//
// THE EARLIER ASSERTIONS HERE WERE LITERAL STRINGS, and rotation deliberately retires them. "Orchicon is
// thinking…" was the weaker form of a promise both clients have always been making; the stronger form is
// "both clients draw the same word for the same server time", which is what internal/tui/chat/verbs_test.go
// and frontend/src/lib/ask-verbs.test.ts now pin against one shared fixture. These tests are re-pointed at
// that invariant — a list member for a fixed server time — rather than weakened.

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// activityWordInList reports whether the WORD in an activity line is one of the rotation's entries — the
// membership half of the re-pointed parity assertion, shared by the tests that assert on a painted frame
// rather than on the notice function directly.
func activityWordInList(line string) bool {
	const prefix = "Orchicon is "
	i := strings.Index(line, prefix)
	if i < 0 {
		return false
	}
	rest := line[i+len(prefix):]
	end := strings.IndexAny(rest, " …·")
	if end < 0 {
		end = len(rest)
	}
	word := rest[:end]
	for _, v := range chat.AskVerbs {
		if v == word {
			return true
		}
	}
	return false
}

// activityTestServerTime is the fixed server stamp these tests index the rotation with. It is a real Unix
// millisecond value (2023-11-14T22:13:20Z) and deliberately NOT a multiple of the period, so it lands
// mid-period and a selector that ignored the stamp could not accidentally match.
const activityTestServerTime int64 = 1_700_000_123_456

// THE LINE ESCALATES WITH SILENCE, at the same bands regardless of the word.
//
// All the boundaries are asserted because the boundaries ARE the behaviour: a line that said "no output
// for 2s" would twitch on every chunk, and one that never escalated would leave the operator waiting on a
// dead stream with no warning at all. The verb is computed from the SAME fixed stamp as the selector, so
// the age bands stay asserted exactly while the word is no longer a literal.
func TestActivityNoticeEscalatesWithSilence(t *testing.T) {
	verb := "Orchicon is " + chat.VerbAt(activityTestServerTime) + "…"
	// THE CALLS BELOW PASS (summary="", width=0), which is exactly the state these cases describe: a turn
	// that counted no tool work (so the counter contributes nothing) and a pane wide enough to hold the
	// whole line (width 0 = unbounded in fitNotice). Every expected string is therefore UNCHANGED from
	// before the counter landed — the invariant this test pins is still "the band boundaries are the
	// behaviour", and the new arguments cannot mask a band regression: the counter cases live in
	// activity_summary_test.go.
	cases := []struct {
		silent time.Duration
		want   string
		why    string
	}{
		{0, verb, "no activity recorded yet — the moment after sending"},
		{-time.Second, verb, "a clock that stepped backwards is not a silence"},
		{time.Second, verb + " · last activity 1s ago", "the age shows from the first second"},
		{4 * time.Second, verb + " · last activity 4s ago", "an ordinary quiet stretch"},
		{24 * time.Second, verb + " · last activity 24s ago", "still under the heartbeat margin"},
		{25 * time.Second, verb + " · no output for 25s", "past the 15s heartbeat: worth stating"},
		{35 * time.Second, verb + " · no output for 35s — the stream will re-attach if it stays silent",
			"the watchdog re-dials at 40s, so the operator is told before it happens"},
		{90 * time.Second, verb + " · no output for 90s — the stream will re-attach if it stays silent",
			"a long stall reports the same thing rather than inventing a new state"},
	}
	for _, c := range cases {
		if got := turnActivityNotice(c.silent, activityTestServerTime, "", 0); got != c.want {
			t.Errorf("turnActivityNotice(%v, %d) = %q, want %q (%s)", c.silent, activityTestServerTime, got, c.want, c.why)
		}
	}
}

// AND THE WORD IS A LIST MEMBER, THE SAME ONE THE SELECTOR NAMES FOR THAT SERVER TIME.
//
// This is the re-pointed form of the old "the verb follows the phase" assertion. What is asserted is no
// longer two literals but the SHARED property: for one server time, one word — and it is one of the
// rotation's own entries, so the line can never invent a claim outside the reviewed list.
func TestActivityNoticeVerbFollowsTheServerClock(t *testing.T) {
	want := "Orchicon is " + chat.VerbAt(activityTestServerTime)
	for _, silent := range []time.Duration{0, 2 * time.Second, 30 * time.Second, time.Minute} {
		got := turnActivityNotice(silent, activityTestServerTime, "", 0)
		if !containsStr(got, want) {
			t.Errorf("turnActivityNotice(%v, %d) = %q — want it to open with %q, the word the selector names "+
				"for that server time", silent, activityTestServerTime, got, want)
		}
		if !containsStr(got, "Orchicon is ") {
			t.Errorf("turnActivityNotice(%v, %d) = %q — the line no longer says what the turn is doing",
				silent, activityTestServerTime, got)
		}
	}

	// A DIFFERENT SERVER TIME MUST BE ABLE TO NAME A DIFFERENT WORD, or the stamp is decoration. Two stamps
	// one period apart are exactly the pair that differs (see VerbAt's contract).
	other := activityTestServerTime + chat.VerbPeriodMS
	if chat.VerbAt(other) == chat.VerbAt(activityTestServerTime) {
		t.Errorf("VerbAt(%d) and VerbAt(%d) both name %q — the rotation is not reading the stamp",
			activityTestServerTime, other, chat.VerbAt(other))
	}
}

// THE LINE IS PRESENT AT EVERY STAGE OF THE TURN — the operator's actual complaint, stated as the
// property: there is no point during a streaming turn where the transcript shows no activity line.
//
// The PRE-HEARTBEAT case (stamp 0) is included deliberately: it is the first second after sending, before
// any heartbeat has arrived, which is exactly the moment the operator is most likely to be looking.
func TestThereIsAlwaysAnActivityLineDuringATurn(t *testing.T) {
	for _, serverTime := range []int64{0, activityTestServerTime, activityTestServerTime + 9_000} {
		for _, silent := range []time.Duration{0, time.Second, 5 * time.Second, 20 * time.Second, 40 * time.Second, time.Hour} {
			got := turnActivityNotice(silent, serverTime, "", 0)
			if got == "" {
				t.Fatalf("turnActivityNotice(%v, %d) is empty — that is the state the operator reported as "+
					"the line \"never coming back\"", silent, serverTime)
			}
			if !containsStr(got, "Orchicon is ") {
				t.Errorf("turnActivityNotice(%v, %d) = %q, which does not say what the turn is doing",
					silent, serverTime, got)
			}
			if !containsStr(got, "…") {
				t.Errorf("turnActivityNotice(%v, %d) = %q — the line lost its ellipsis", silent, serverTime, got)
			}
		}
	}
}

// AND THE LINE NEVER CLAIMS ACTIVITY IT CANNOT SEE: a growing gap says "no output", never that the stream
// is receiving. A reassurance that cannot be true is worse than silence, because it hides the very failure
// this work exists to expose. The verb is checked against the SAME ban as the list itself
// (chat/verbs_test.go's TestAskVerbsNeverClaimLiveness), so a liveness word cannot enter the rotation at all.
func TestTheLineNeverClaimsActivityItCannotSee(t *testing.T) {
	for _, serverTime := range []int64{0, activityTestServerTime} {
		for _, silent := range []time.Duration{26 * time.Second, 40 * time.Second, time.Minute, time.Hour} {
			got := turnActivityNotice(silent, serverTime, "", 0)
			if containsStr(got, "receiving") {
				t.Errorf("turnActivityNotice(%v, %d) = %q — it asserts the stream is receiving, which is exactly "+
					"the claim a stalled stream would make falsely", silent, serverTime, got)
			}
		}
	}
}
