package opencode

import (
	"encoding/json"
	"testing"
)

// TestTerminalEventsAreClassified is the regression guard for the HANG a 2.x
// execution hit.
//
// The runner recognised v1's terminal verbs (`session.idle` to end a turn,
// `session.error` to fail one) and NOTHING of v2's. MEASURED on 2.0.26: a v2 stream
// ends with `session.execution.succeeded` / `.failed` and reports a failed step with
// `session.step.failed`, and NEVER emits a bare `session.idle`. So a 2.x turn's end
// arrived, no arm recognised it, and the run waited for a signal that surface does not
// send — for a SUCCESSFUL turn as much as a failed one. Its text and tool parts still
// flowed, which is why it looked productive rather than stuck.
//
// The vocabulary is a TABLE (terminalTurnKind) rather than switch arms precisely so
// this test can exist: a missing switch arm is invisible, a missing row fails here.
func TestTerminalEventsAreClassified(t *testing.T) {
	want := map[string]string{
		// v1
		"session.idle":  turnEnd,
		"session.error": turnFailure,
		// v2 — the verbs whose absence was the hang.
		"session.execution.succeeded": turnEnd,
		"session.execution.failed":    turnFailure,
		"session.step.failed":         turnFailure,
	}
	for evt, kind := range want {
		if got := terminalTurnKind(evt); got != kind {
			t.Errorf("terminalTurnKind(%q) = %q, want %q — a terminal event the runner does not recognise makes the run wait for a signal that surface never sends", evt, got, kind)
		}
	}

	// NON-terminal events must classify as NOTHING. A false positive here would end a
	// turn early, or fail a healthy run — the opposite error, and just as bad.
	for _, evt := range []string{
		"session.status", "permission.asked", "session.text.delta", "session.text.ended",
		"message.part.updated", "session.created", "message.updated", "",
	} {
		if got := terminalTurnKind(evt); got != "" {
			t.Errorf("terminalTurnKind(%q) = %q, want no class — this event does NOT end or fail a turn", evt, got)
		}
	}
}

// TestEveryTerminalEventSurvivesThePropsEnvelope pins the OTHER half of the same bug,
// which was a second silent failure waiting behind the first.
//
// The runner reads both the session filter and the failure message through BusEvent.Props(),
// because the payload lives under `properties` on v1 and under `data` on v2. Reading
// `Properties` directly yields an EMPTY map on v2 rather than an error, so the session
// filter would stop filtering and the failure reason would collapse to a generic string —
// both invisible until an operator needs the message.
func TestEveryTerminalEventSurvivesThePropsEnvelope(t *testing.T) {
	for _, evtType := range []string{
		"session.idle", "session.error", "session.execution.succeeded",
		"session.execution.failed", "session.step.failed",
	} {
		for _, key := range []string{"properties", "data"} {
			raw, _ := json.Marshal(map[string]any{
				"type": evtType,
				key: map[string]any{
					"sessionID": "ses_test",
					"error":     map[string]any{"message": "the provider refused"},
				},
			})
			var evt BusEvent
			if err := json.Unmarshal(raw, &evt); err != nil {
				t.Fatalf("unmarshal %s: %v", evtType, err)
			}
			props := evt.Props()
			if props == nil {
				t.Fatalf("%s under %q: Props() is nil — the session filter and the error message both read through it", evtType, key)
			}
			if got, _ := props["sessionID"].(string); got != "ses_test" {
				t.Errorf("%s under %q: Props()[sessionID] = %q, want ses_test (the filter would stop filtering)", evtType, key, got)
			}
		}
	}
}

// TestSessionErrorMessageReadsTheV2Envelope pins that a v2 failure is DIAGNOSABLE, not
// merely detected. The message lives under the surface-specific envelope, so reading the
// wrong one yields the generic fallback for exactly the failures an operator most needs
// named.
func TestSessionErrorMessageReadsTheV2Envelope(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type": "session.execution.failed",
		"data": map[string]any{
			"sessionID": "ses_test",
			"error":     map[string]any{"message": "OpenCode's free tier can only be used from within OpenCode"},
		},
	})
	var evt BusEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatal(err)
	}
	if got := sessionErrorMessage(evt); got != "OpenCode's free tier can only be used from within OpenCode" {
		t.Errorf("sessionErrorMessage = %q — a v2 failure must name its cause, not fall back to a generic string", got)
	}
}
