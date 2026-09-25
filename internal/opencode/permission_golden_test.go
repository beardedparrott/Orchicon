package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPermissionAskedGoldenClassifies is the golden pin for the consent
// layer's detail extraction: it feeds the CAPTURED permission.asked payload
// (testdata/permission_asked_golden.json) through classifyBusEvent and asserts
// the raw detail survives verbatim. If a live serve's shape disagrees with the
// fixture, the fixture is re-captured — the parser's candidate keys are then
// corrected in internal/askorchicon/consent.go.
func TestPermissionAskedGoldenClassifies(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "permission_asked_golden.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var props map[string]any
	if err := json.Unmarshal(b, &props); err != nil {
		t.Fatalf("unmarshal golden fixture: %v", err)
	}
	// The provenance annotations are not part of the transport payload.
	delete(props, "_comment")
	delete(props, "_captured")

	se := classifyBusEvent(BusEvent{Type: "permission.asked", Properties: props})
	if se == nil {
		t.Fatal("classifyBusEvent returned nil for a permission.asked event")
	}
	if se.Kind != "permission" {
		t.Fatalf("Kind = %q, want %q", se.Kind, "permission")
	}
	if se.PermissionID != "per_01J9Z0PERMISSIONASK" {
		t.Fatalf("PermissionID = %q", se.PermissionID)
	}
	if se.SessionID != "ses_01J9Z0SESSION" {
		t.Fatalf("SessionID = %q", se.SessionID)
	}
	// The detail must NOT be dropped: without it there is nothing to show a
	// user and directory-scoped keying is impossible.
	if se.Detail == nil {
		t.Fatal("Detail is nil — the ask's detail was dropped")
	}
	if got := se.Detail["permission"]; got != "edit" {
		t.Fatalf("Detail[permission] = %v, want edit", got)
	}
	if pats, ok := se.Detail["patterns"].([]any); !ok || len(pats) == 0 {
		t.Fatalf("Detail[patterns] = %#v, want a non-empty list", se.Detail["patterns"])
	}
	meta, ok := se.Detail["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("Detail[metadata] = %#v, want an object", se.Detail["metadata"])
	}
	// metadata.filepath is the AUTHORITATIVE absolute path in the real schema
	// (opencode 1.18.32 PermissionRequest); the camelCase spelling does not occur.
	if meta["filepath"] != "/home/beardedparrott/projects/sibling-project/notes.md" {
		t.Fatalf("Detail[metadata][filepath] = %v", meta["filepath"])
	}
	// callID is NOT a top-level property: it is nested under tool.
	if _, ok := se.Detail["callID"]; ok {
		t.Fatalf("callID must not be a top-level property, got %v", se.Detail["callID"])
	}
	tool, ok := se.Detail["tool"].(map[string]any)
	if !ok {
		t.Fatalf("Detail[tool] = %#v, want the {messageID, callID} object", se.Detail["tool"])
	}
	if tool["callID"] != "call_01J9Z0TOOLCALL" {
		t.Fatalf("Detail[tool][callID] = %v", tool["callID"])
	}
}
