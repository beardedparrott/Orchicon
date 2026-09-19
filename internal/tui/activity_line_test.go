package tui

// activity_line_test.go — THE STREAM SAYS WHAT IT IS DOING, AND HOW FRESH IT IS.
//
// The operator: "I say we implement the watchdog and add the line at the bottom of the chat stream so a
// user knows activity is happening" — and then, when the line vanished mid-reply: "After the initial
// 'Orchicon is thinking...', streaming started and the 'Orchicon is thinking...' went away and never came
// back."
//
// So the line runs for the WHOLE turn and its verb follows the phase, and both halves are asserted here.
// The age comes from the SAME clock the watchdog reads (chat.Controller.SilenceSince, over the lastActivity
// the liveness check uses), so the line cannot claim a liveness the watchdog would contradict — that
// coupling is what makes it a report rather than a decoration.

import (
	"testing"
	"time"
)

// THE LINE ESCALATES WITH SILENCE, in the same bands for both phases.
//
// All the boundaries are asserted because the boundaries ARE the behaviour: a line that said "no output
// for 2s" would twitch on every chunk, and one that never escalated would leave the operator waiting on a
// dead stream with no warning at all.
func TestActivityNoticeEscalatesWithSilence(t *testing.T) {
	cases := []struct {
		silent time.Duration
		want   string
		why    string
	}{
		{0, "Orchicon is thinking…", "no activity recorded yet — the moment after sending"},
		{-time.Second, "Orchicon is thinking…", "a clock that stepped backwards is not a silence"},
		{time.Second, "Orchicon is thinking… · last activity 1s ago", "the age shows from the first second"},
		{4 * time.Second, "Orchicon is thinking… · last activity 4s ago", "an ordinary quiet stretch"},
		{24 * time.Second, "Orchicon is thinking… · last activity 24s ago", "still under the heartbeat margin"},
		{25 * time.Second, "Orchicon is thinking… · no output for 25s", "past the 15s heartbeat: worth stating"},
		{35 * time.Second, "Orchicon is thinking… · no output for 35s — the stream will re-attach if it stays silent",
			"the watchdog re-dials at 40s, so the operator is told before it happens"},
		{90 * time.Second, "Orchicon is thinking… · no output for 90s — the stream will re-attach if it stays silent",
			"a long stall reports the same thing rather than inventing a new state"},
	}
	for _, c := range cases {
		if got := turnActivityNotice(c.silent, true); got != c.want {
			t.Errorf("turnActivityNotice(%v, beforeContent=true) = %q, want %q (%s)", c.silent, got, c.want, c.why)
		}
	}
}

// THE VERB FOLLOWS THE PHASE, which is the fix for the operator's report.
//
// BEFORE content the GUI's own wording applies — "Orchicon is thinking…" is what the operator sees while
// their message is being read, and that parity is deliberately kept. AFTER content the turn is REPLYING:
// saying "thinking" beneath a half-written answer would be wrong, and dropping the line entirely (which is
// what it used to do) removed the only signal that the stream is still alive.
func TestActivityNoticeVerbFollowsThePhase(t *testing.T) {
	for _, silent := range []time.Duration{0, 2 * time.Second, 30 * time.Second, time.Minute} {
		if got := turnActivityNotice(silent, true); !containsStr(got, "thinking") {
			t.Errorf("before content, turnActivityNotice(%v) = %q — want the GUI's thinking wording", silent, got)
		}
		if got := turnActivityNotice(silent, false); !containsStr(got, "replying") {
			t.Errorf("after content, turnActivityNotice(%v) = %q — the turn is replying, and the line must "+
				"still be there saying so", silent, got)
		}
	}
}

// THE LINE IS PRESENT AT EVERY STAGE OF THE TURN — the operator's actual complaint, stated as the
// property: there is no point during a streaming turn where the transcript shows no activity line.
func TestThereIsAlwaysAnActivityLineDuringATurn(t *testing.T) {
	for _, beforeContent := range []bool{true, false} {
		for _, silent := range []time.Duration{0, time.Second, 5 * time.Second, 20 * time.Second, 40 * time.Second, time.Hour} {
			got := turnActivityNotice(silent, beforeContent)
			if got == "" {
				t.Errorf("turnActivityNotice(%v, beforeContent=%v) is empty — that is the state the operator "+
					"reported as the line \"never coming back\"", silent, beforeContent)
			}
			if !containsStr(got, "Orchicon is ") {
				t.Errorf("turnActivityNotice(%v, beforeContent=%v) = %q, which does not say what the turn is "+
					"doing", silent, beforeContent, got)
			}
		}
	}
}

// AND THE LINE NEVER CLAIMS ACTIVITY IT CANNOT SEE: a growing gap says "no output", never that the stream
// is receiving. A reassurance that cannot be true is worse than silence, because it hides the very failure
// this work exists to expose.
func TestTheLineNeverClaimsActivityItCannotSee(t *testing.T) {
	for _, beforeContent := range []bool{true, false} {
		for _, silent := range []time.Duration{26 * time.Second, 40 * time.Second, time.Minute, time.Hour} {
			got := turnActivityNotice(silent, beforeContent)
			if containsStr(got, "receiving") {
				t.Errorf("turnActivityNotice(%v, %v) = %q — it asserts the stream is receiving, which is exactly "+
					"the claim a stalled stream would make falsely", silent, beforeContent, got)
			}
		}
	}
}
