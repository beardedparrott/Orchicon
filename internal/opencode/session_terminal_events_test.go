package opencode

import (
	"encoding/json"
	"testing"
)

// MEASURED on 2.0.26: a v2 stream ends with `session.execution.succeeded` / `.failed` and reports a
// failed step with `session.step.failed`, and NEVER emits a bare `session.idle`.
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

// TestEveryTerminalEventSurvivesThePropsEnvelope pins the OTHER half of the same bug, which was a
// second silent failure waiting behind the first.
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

// TestSessionErrorMessageReadsTheV2Envelope pins that a v2 failure is DIAGNOSABLE, not merely
// detected.
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
