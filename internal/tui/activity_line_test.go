package tui

// activity_line_test.go — THE STREAM SAYS WHAT IT IS DOING, AND HOW FRESH IT IS.

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// activityWordInList reports whether the WORD in an activity line is one of the rotation's entries
// — the membership half of the re-pointed parity assertion.
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

// activityTestServerTime is the fixed server stamp these tests index the rotation with.
const activityTestServerTime int64 = 1_700_000_123_456

// THE LINE ESCALATES WITH SILENCE, at the same bands regardless of the word.
func TestActivityNoticeEscalatesWithSilence(t *testing.T) {
	verb := "Orchicon is " + chat.VerbAt(activityTestServerTime) + "…"
	// THE CALLS BELOW PASS (summary="", width=0), which is exactly the state these cases describe.
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

// AND THE LINE NEVER CLAIMS ACTIVITY IT CANNOT SEE: a growing gap says "no output", never that the
// stream is receiving.
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
