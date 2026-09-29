package claude

import "testing"

// The control protocol is the whole basis of Ask support: the CLI raises a
// can_use_tool control_request when a tool needs permission and no handler
// refused it outright, and the Ask transport answers it. If ParseLine drops or
// mis-decodes that line, an Ask session would hang on an unanswerable tool
// call with nothing on screen — so the decoding is pinned here.
//
// The fixture mirrors the wire shape documented in the installed binary
// (2.1.261): {"type":"control_request","request_id":…,"request":{subtype,
// tool_name, input, permission_suggestions, blocked_path, decision_reason}}.
func TestParseLineDecodesCanUseToolControlRequest(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"req_abc123",` +
		`"request":{"subtype":"can_use_tool","tool_name":"Bash",` +
		`"input":{"command":"rm -rf /tmp/target","description":"clean up"},` +
		`"permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash"}]}],` +
		`"blocked_path":"/tmp/target",` +
		`"decision_reason":"outside the conversation's project"}}`)

	ev, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}

	if !ev.IsControlRequest {
		t.Fatal("IsControlRequest = false, want true")
	}
	if !ev.IsToolPermissionAsk() {
		t.Fatal("IsToolPermissionAsk = false, want true — the Ask transport would never raise a consent card")
	}
	if ev.ControlRequestID != "req_abc123" {
		t.Errorf("ControlRequestID = %q, want req_abc123 (the id a control_response must echo)", ev.ControlRequestID)
	}
	if ev.ControlSubtype != "can_use_tool" {
		t.Errorf("ControlSubtype = %q, want can_use_tool", ev.ControlSubtype)
	}
	if ev.ControlToolName != "Bash" {
		t.Errorf("ControlToolName = %q, want Bash", ev.ControlToolName)
	}
	// The input is what the consent card renders (the command/path). Losing it
	// would leave an approvable card with nothing to approve.
	if got := ev.ControlInput["command"]; got != "rm -rf /tmp/target" {
		t.Errorf("ControlInput[command] = %v, want the command string", got)
	}
	if len(ev.ControlSuggestions) != 1 {
		t.Errorf("ControlSuggestions = %v, want 1 suggestion (the CLI's own suggested rule)", ev.ControlSuggestions)
	}
	if ev.ControlBlockedPath != "/tmp/target" {
		t.Errorf("ControlBlockedPath = %q, want /tmp/target", ev.ControlBlockedPath)
	}
	if ev.ControlDecisionReason == "" {
		t.Error("ControlDecisionReason is empty — the human-readable reason would be lost from the card")
	}
	// The raw line is retained for the durable transcript.
	if len(ev.Raw) == 0 {
		t.Error("Raw is empty; the transcript would lose the control message")
	}
}

// A control message that is NOT a permission ask must not be mistaken for one:
// treating every control_request as an ask would raise consent cards for
// protocol housekeeping and wedge the turn on an operator answer nobody needs.
func TestParseLineIgnoresNonAskControlRequests(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"control_request","request_id":"req_2","request":{"subtype":"interrupt"}}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if !ev.IsControlRequest {
		t.Fatal("IsControlRequest = false, want true")
	}
	if ev.IsToolPermissionAsk() {
		t.Fatal("IsToolPermissionAsk = true for an interrupt control_request, want false")
	}
}

// The cancel sighting: the binary documents a control_cancel_request as how an
// in-flight request is settled (a pending can_use_tool after an interrupted
// turn, or one another client already answered). The Ask transport uses it to
// clear a card that can no longer be answered, so it must be decodable.
func TestParseLineDecodesControlCancel(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"control_cancel_request","request_id":"req_abc123"}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if !ev.IsControlCancel {
		t.Fatal("IsControlCancel = false, want true")
	}
	if ev.ControlRequestID != "req_abc123" {
		t.Errorf("ControlRequestID = %q, want the cancelled request id", ev.ControlRequestID)
	}
	if ev.IsToolPermissionAsk() {
		t.Fatal("a cancel must not read as a fresh permission ask")
	}
}

// No regression: an ordinary assistant line is not a control message, and a
// malformed control_request degrades rather than failing the session loop (a
// CLI version that reshapes the request must not break the stream).
func TestParseLineControlDoesNotDisturbOrdinaryEvents(t *testing.T) {
	ev, err := ParseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if ev.IsControlRequest || ev.IsControlCancel || ev.IsToolPermissionAsk() {
		t.Fatalf("an assistant line read as a control message: %+v", ev)
	}

	shapeless, err := ParseLine([]byte(`{"type":"control_request"}`))
	if err != nil {
		t.Fatalf("ParseLine on a shapeless control_request: %v", err)
	}
	if !shapeless.IsControlRequest {
		t.Fatal("IsControlRequest = false for a request with no `request` object")
	}
	if shapeless.IsToolPermissionAsk() {
		t.Fatal("a request with no subtype must not read as a tool ask")
	}
}
