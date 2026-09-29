package claude

import (
	"encoding/json"
	"testing"
)

// These pin the control_response WIRE SHAPE byte for byte.
//
// They are exact-string assertions on purpose. The frame is matched by the CLI
// on a nesting that is invisible to a type check: `request_id` must sit INSIDE
// `response`, and the verdict must sit at `response.response`. A refactor that
// flattens either — or a "tidier" hand-built map — still compiles, still passes
// any structural round-trip, and silently fails to match at runtime, leaving the
// tool call parked until the turn is cancelled. That failure mode is an Ask turn
// hanging with a card the operator already answered, so the bytes are the
// contract.
//
// Ground truth (read from the installed binary 2.1.261):
//
//	return {type:"control_response", response:{subtype:"success", request_id:e, response:r}}
//	Expected {behavior: 'allow', updatedInput?: object} or {behavior: 'deny', message: string}.

func TestAllowResponsePinsTheExactWireShape(t *testing.T) {
	got := string(AllowResponse("req_abc123", ""))
	want := `{"type":"control_response","response":{"subtype":"success","request_id":"req_abc123","response":{"behavior":"allow"}}}`
	if got != want {
		t.Fatalf("allow frame =\n  %s\nwant\n  %s\n"+
			"(request_id must be nested inside `response`, and the verdict at response.response)", got, want)
	}
	// An allow carries NO message: the schema's allow arm has no such field, and
	// a stray one is at best noise on the wire.
	var doc map[string]any
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("frame is not valid JSON: %v", err)
	}
	inner := doc["response"].(map[string]any)["response"].(map[string]any)
	if _, present := inner["message"]; present {
		t.Error("an allow verdict must not carry a message")
	}
}

func TestDenyResponseCarriesTheRequiredMessage(t *testing.T) {
	got := string(DenyResponse("req_9", "outside the conversation's scope", DecisionUserReject))
	want := `{"type":"control_response","response":{"subtype":"success","request_id":"req_9","response":{"behavior":"deny","message":"outside the conversation's scope","decisionClassification":"user_reject"}}}`
	if got != want {
		t.Fatalf("deny frame =\n  %s\nwant\n  %s", got, want)
	}
}

// A deny with no message is a schema violation (the CLI validates
// {behavior:"deny", message: string}), so the encoder must never emit one — a
// refusal the model cannot read is worse than the refusal.
func TestDenyResponseAlwaysEmitsAMessage(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(DenyResponse("req_1", "", ""), &doc); err != nil {
		t.Fatalf("frame is not valid JSON: %v", err)
	}
	inner := doc["response"].(map[string]any)["response"].(map[string]any)
	if inner["behavior"] != BehaviorDeny {
		t.Fatalf("behavior = %v, want deny", inner["behavior"])
	}
	msg, _ := inner["message"].(string)
	if msg == "" {
		t.Fatal("a deny verdict with no message violates the CLI's schema (message is required)")
	}
}

func TestAllowResponseRecordsTheDecisionClassification(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(AllowResponse("req_2", DecisionUserTemporary), &doc); err != nil {
		t.Fatalf("frame is not valid JSON: %v", err)
	}
	inner := doc["response"].(map[string]any)["response"].(map[string]any)
	if inner["decisionClassification"] != DecisionUserTemporary {
		t.Errorf("decisionClassification = %v, want %s (the tool_decision vocabulary the CLI documents)",
			inner["decisionClassification"], DecisionUserTemporary)
	}
}

// The cancel settles an ask without answering it (a superseded turn, a deleted
// conversation). It is a real value the CLI itself emits, so the host's use of it
// is not an invention.
func TestCancelResponse(t *testing.T) {
	got := string(CancelResponse("req_3"))
	want := `{"type":"control_response","response":{"subtype":"success","request_id":"req_3","response":{"behavior":"cancelled"}}}`
	if got != want {
		t.Fatalf("cancel frame =\n  %s\nwant\n  %s", got, want)
	}
}

// The frame must always be answerable to the ask that raised it: encoding a
// request id and reading it back through the same nesting the CLI uses
// (`f.response.response`) is the correlation contract.
func TestResponseRequestIDRoundTripsAtTheNestingTheCLIUses(t *testing.T) {
	const id = "req_correlate_me"
	var doc struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(AllowResponse(id, ""), &doc); err != nil {
		t.Fatalf("frame is not valid JSON: %v", err)
	}
	if doc.Type != "control_response" {
		t.Errorf("type = %q, want control_response", doc.Type)
	}
	if doc.Response.Subtype != "success" {
		t.Errorf("response.subtype = %q, want success", doc.Response.Subtype)
	}
	if doc.Response.RequestID != id {
		t.Errorf("response.request_id = %q, want %q — the CLI matches the answer on this", doc.Response.RequestID, id)
	}
	if doc.Response.Response.Behavior != BehaviorAllow {
		t.Errorf("response.response.behavior = %q, want allow", doc.Response.Response.Behavior)
	}
}
