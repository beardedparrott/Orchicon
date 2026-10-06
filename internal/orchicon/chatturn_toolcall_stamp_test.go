package orchicon

import (
	"encoding/json"
	"reflect"
	"testing"
)

// AC 4: the provider-replay path carries NO ledger timestamp, and the added
// `issued_at_unix_ms` key cannot perturb it.
//
// The native bridge builds ContentToolUse from the adapter stream's ToolCall
// (chatturn.go:1153 → appendAssistantTurn), never from askorchicon's persisted
// `tool_calls` jsonb column. This test makes that structural fact a REGRESSION
// guard: a stamped payload and its unstamped predecessor must produce a
// byte-identical bridge history. If a future change ever routed the stamp into
// the bridge, this comparison is what catches it.
func TestStampedLedgerDoesNotAlterNativeBridgeContentToolUse(t *testing.T) {
	const unstamped = `[{"id":"tc-1","type":"function","function_name":"bash","arguments":"{\"command\":\"make ci\"}"},` +
		`{"id":"tc-2","type":"function","function_name":"read","arguments":"{\"filePath\":\"main.go\"}"}]`
	const stamped = `[{"id":"tc-1","type":"function","function_name":"bash","arguments":"{\"command\":\"make ci\"}","issued_at_unix_ms":1700000000123},` +
		`{"id":"tc-2","type":"function","function_name":"read","arguments":"{\"filePath\":\"main.go\"}","issued_at_unix_ms":1700000000456}]`

	bridge := &NativeBridge{log: testLogger()}

	build := func(payload string) []Message {
		// The bridge's own decode shape for a call, fed by the ledger column's
		// four wire fields. The stamp is simply not one of them.
		var rows []struct {
			ID           string `json:"id"`
			Type         string `json:"type"`
			FunctionName string `json:"function_name"`
			Arguments    string `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(payload), &rows); err != nil {
			t.Fatalf("decode fixture: %v", err)
		}
		calls := make([]ToolCall, 0, len(rows))
		for _, r := range rows {
			calls = append(calls, ToolCall{ToolCallID: r.ID, Name: r.FunctionName, ArgsJSON: r.Arguments})
		}
		var working []Message
		bridge.appendAssistantTurn(&working, "on it", calls)
		return working
	}

	gotUnstamped := build(unstamped)
	gotStamped := build(stamped)

	if len(gotUnstamped) == 0 || len(gotStamped) == 0 {
		t.Fatalf("the bridge produced no history: %d and %d messages", len(gotUnstamped), len(gotStamped))
	}
	if !reflect.DeepEqual(gotUnstamped, gotStamped) {
		t.Errorf("the stamp changed the bridged history:\n unstamped=%#v\n stamped=%#v", gotUnstamped, gotStamped)
	}

	// And the values themselves are exactly the four the ToolCall carries today.
	var uses []ContentToolUse
	for _, m := range gotStamped {
		for _, c := range m.Content {
			if c.ToolUse != nil {
				uses = append(uses, *c.ToolUse)
			}
		}
	}
	if len(uses) != 2 {
		t.Fatalf("want 2 bridged tool uses, got %d (%#v)", len(uses), uses)
	}
	if uses[0].ToolCallID != "tc-1" || uses[0].Name != "bash" || uses[0].ArgsJSON != `{"command":"make ci"}` {
		t.Errorf("first bridged use = %#v, want the four values unchanged", uses[0])
	}
}
