package opencode

// busevent_v2_test.go — THE v2 EVENT STREAM, USING THE REAL PAYLOADS.
//
// This is the piece that made Ask Orchicon "hang" on opencode 2.x while the model had in fact replied.
// The stream changed in TWO independent ways, and either one alone breaks the turn:
//
//	1. THE ENVELOPE. v1 events carry `properties`; v2 events carry `data`. Every consumer read
//	   `Properties`, which is nil on a v2 stream — so `sessionID` came back "" and each event was
//	   filtered out. BusEvent.Props() now reads whichever the surface used.
//	2. THE VOCABULARY. v1 ends a turn with `session.idle` and streams with `message.part.delta`; v2
//	   ends with `session.execution.succeeded`/`.failed` and streams with `session.text.delta`. Both
//	   are now classified.
//
// EVERY PAYLOAD BELOW IS VERBATIM from a real 2.0.26 serve, captured by running a turn and reading
// /api/event — so this test is anchored to observed output rather than to a shape I inferred.

import (
	"encoding/json"
	"testing"
)

// v2Event unmarshals one captured event exactly as the SSE reader does.
func v2Event(t *testing.T, raw string) BusEvent {
	t.Helper()
	var evt BusEvent
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return evt
}

const (
	// The envelope difference, in one pair: both carry sessionID, under different keys.
	v2TextDelta  = `{"id":"evt_12e9df0c1001fI6VGHh4oCXgOM","created":1791783465153,"type":"session.text.delta","location":{"directory":"/tmp"},"data":{"sessionID":"ses_ed1621c1fffew2Lw7AmUQIinbh","assistantMessageID":"msg_12e9debf8001X2jZCDTvBzj4kR","ordinal":0,"delta":"Hi! What can I help you with today?"},"durable":{"aggregateID":"ses_ed1621c1fffew2Lw7AmUQIinbh","seq":9,"version":1}}`
	v2TextEnded  = `{"id":"evt_12e9df0c2001n9q5vvtHTo9vfa","created":1791783465154,"type":"session.text.ended","location":{"directory":"/tmp"},"data":{"sessionID":"ses_ed1621c1fffew2Lw7AmUQIinbh","assistantMessageID":"msg_12e9debf8001X2jZCDTvBzj4kR","ordinal":0,"text":"Hi! What can I help you with today?"},"durable":{"aggregateID":"ses_ed1621c1fffew2Lw7AmUQIinbh","seq":10,"version":1}}`
	v2Succeeded  = `{"id":"evt_12e9df0c7001l4HrpxTCrl5qiA","created":1791783465159,"type":"session.execution.succeeded","data":{"sessionID":"ses_ed1621c1fffew2Lw7AmUQIinbh"}}`
	v2Failed     = `{"id":"evt_12e9cea29001fAmaylJS0kwidS","created":1791783397929,"type":"session.execution.failed","data":{"sessionID":"ses_ed16323f4ffeLHx1guLXUFefhK","error":{"type":"provider.invalid-request","message":"Command Code API error 400: You have insufficient credits to make this request.","status":400}}}`
	v2StepFailed = `{"id":"evt_12e9cea270016WRqzV5tJpDPLw","created":1791783397927,"type":"session.step.failed","data":{"sessionID":"ses_ed16323f4ffeLHx1guLXUFefhK","assistantMessageID":"msg_12e9ce56d0016I2FDPsrLTWThN","error":{"type":"provider.invalid-request","message":"Command Code API error 400: You have insufficient credits to make this request."}}}`
)

func TestV2EnvelopeIsRead(t *testing.T) {
	// THE ENVELOPE. Before Props() every one of these returned "", and a "" session id means the
	// collector DROPS the event (it filters to its own session).
	for name, raw := range map[string]string{
		"session.text.delta":          v2TextDelta,
		"session.text.ended":          v2TextEnded,
		"session.execution.succeeded": v2Succeeded,
		"session.execution.failed":    v2Failed,
	} {
		evt := v2Event(t, raw)
		if got, _ := evt.Props()["sessionID"].(string); got == "" {
			t.Errorf("%s: Props() has no sessionID — every event on a v2 stream would be filtered out "+
				"by session, which is the hang", name)
		}
	}
}

func TestV2CompletionClassifiesAsIdle(t *testing.T) {
	// THE COMPLETION SIGNAL. Without this arm the collector never learns the turn ended: it waits out
	// its window while the model's reply sits in the session — the reported behaviour exactly.
	got := classifyBusEvent(v2Event(t, v2Succeeded))
	if got == nil {
		t.Fatal("session.execution.succeeded classified as NOTHING — a complete turn would never be " +
			"collected on v2")
	}
	if got.Kind != "idle" {
		t.Errorf("kind = %q, want %q (the v1 equivalent)", got.Kind, "idle")
	}
	if got.SessionID != "ses_ed1621c1fffew2Lw7AmUQIinbh" {
		t.Errorf("session id = %q, want the one in the envelope", got.SessionID)
	}
}

func TestV2FailureClassifiesAsAnErrorWithItsReason(t *testing.T) {
	// v2 nests the reason as an OBJECT with `message`; v1 used a string. An error event that carries no
	// text renders as a bare "opencode session error", which tells the operator nothing.
	for name, raw := range map[string]string{
		"session.execution.failed": v2Failed,
		"session.step.failed":      v2StepFailed,
	} {
		got := classifyBusEvent(v2Event(t, raw))
		if got == nil || got.Kind != "error" {
			t.Fatalf("%s: classified as %+v, want an error — a failed turn must not look like silence", name, got)
		}
		if got.Text == "" || got.Text == "opencode session error" {
			t.Errorf("%s: error text = %q — the provider's own message must survive", name, got.Text)
		}
	}
}

func TestV2TextDeltaAndEndedAreUsable(t *testing.T) {
	// The delta drives liveness and the live mirror; the ended text is the completed block. Both must
	// yield the model's words, or the reply never appears.
	if delta, kind, ok := TokenDeltaInfoFromBus(v2Event(t, v2TextDelta)); !ok || delta == "" {
		t.Errorf("session.text.delta produced (%q, %q, %v) — the delta is the only liveness evidence a "+
			"v2 turn emits, so without it the stall monitor fires on a healthy turn", delta, kind, ok)
	}
	legacy, ok := LegacyEventFromBus(v2Event(t, v2TextEnded))
	if !ok {
		t.Fatal("session.text.ended produced no completed part — the reply would never be recorded")
	}
	if t2, _ := legacy["type"].(string); t2 != "text" {
		t.Errorf("completed part type = %q, want %q", t2, "text")
	}
	part, _ := legacy["part"].(map[string]any)
	if txt, _ := part["text"].(string); txt == "" {
		t.Error("the completed part carries no text")
	}
}

func TestAV1EnvelopeStillTakesPrecedence(t *testing.T) {
	// v1 must be byte-identical: its payload is read from `properties`, and the v2 `data` field on the
	// same event must not shadow it.
	evt := BusEvent{
		Type:       "session.idle",
		Properties: map[string]any{"sessionID": "ses_v1"},
		Data:       map[string]any{"sessionID": "ses_v2"},
	}
	if got, _ := evt.Props()["sessionID"].(string); got != "ses_v1" {
		t.Errorf("Props() = %q, want the v1 payload — a v1 serve's behaviour must not change", got)
	}
}
