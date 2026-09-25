package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readGoldenProps reads a golden fixture's properties object, dropping the
// provenance annotations (they are not part of the transport payload).
func readGoldenProps(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var props map[string]any
	if err := json.Unmarshal(b, &props); err != nil {
		t.Fatalf("unmarshal golden fixture: %v", err)
	}
	delete(props, "_comment")
	delete(props, "_captured")
	return props
}

// TestPermissionAskCorrelatesMCPToolCallArgs pins the fix for the ask shape
// that carries no detail of its own: the MCP/host-suite ask the Ask prompt's
// `orchicon_*` tools raise (permission=tool key, patterns ["*"], metadata {}).
// Its args exist only on the bus, in the tool part that precedes the ask, so
// the index correlates them by callID; without them the consent layer had
// nothing to key or show and approved a sibling-path write silently.
func TestPermissionAskCorrelatesMCPToolCallArgs(t *testing.T) {
	props := readGoldenProps(t, "permission_asked_mcp_golden.json")
	if props["permission"] != "orchicon_write" {
		t.Fatalf("fixture permission = %v", props["permission"])
	}

	calls := newToolCallIndex()
	toolPart := func(input map[string]any) BusEvent {
		return BusEvent{Type: "message.part.updated", Properties: map[string]any{
			"sessionID": "ses_01J9Z0SESSION",
			"part": map[string]any{
				"type":   "tool",
				"tool":   "orchicon_write",
				"callID": "call_01J9Z0MCPCALL",
				"state":  map[string]any{"status": "running", "input": input},
			},
		}}
	}
	// An early input-less update must not erase a later one, and a later
	// update MERGES into the args already seen (the serve streams a tool
	// part's input progressively).
	calls.observe(toolPart(nil))
	calls.observe(toolPart(map[string]any{"filePath": "/home/beardedparrott/projects/sibling-project/notes.md"}))
	calls.observe(toolPart(map[string]any{"mode": "create"}))

	se := classifyBusEventIndexed(BusEvent{Type: "permission.asked", Properties: props}, calls)
	if se == nil || se.Kind != "permission" {
		t.Fatalf("classifyBusEventIndexed: %+v", se)
	}
	in, ok := se.Detail["toolInput"].(map[string]any)
	if !ok {
		t.Fatalf("Detail[toolInput] = %#v — the ask has no key-able detail without it", se.Detail["toolInput"])
	}
	if in["filePath"] != "/home/beardedparrott/projects/sibling-project/notes.md" {
		t.Fatalf("toolInput[filePath] = %v", in["filePath"])
	}
	if in["mode"] != "create" {
		t.Fatalf("toolInput[mode] = %v — a later input update must merge, not replace", in["mode"])
	}
	// The transport payload itself is never mutated.
	if _, mutated := props["toolInput"]; mutated {
		t.Fatal("classifyBusEventIndexed mutated the raw bus properties")
	}

	// Without correlation nothing is invented: the ask keeps exactly what the
	// serve sent (and the consent layer fails closed on it).
	if se2 := classifyBusEvent(BusEvent{Type: "permission.asked", Properties: props}); se2.Detail["toolInput"] != nil {
		t.Fatalf("uncorrelated ask carries toolInput = %#v", se2.Detail["toolInput"])
	}
	// A non-tool part and a part without a callID are ignored.
	empty := newToolCallIndex()
	empty.observe(BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"part": map[string]any{"type": "text", "callID": "call_x", "state": map[string]any{"input": map[string]any{"a": 1}}},
	}})
	empty.observe(BusEvent{Type: "message.part.updated", Properties: map[string]any{
		"part": map[string]any{"type": "tool", "state": map[string]any{"input": map[string]any{"a": 1}}},
	}})
	if got := empty.get("call_x"); got != nil {
		t.Fatalf("a non-tool part must not be indexed: %#v", got)
	}
}

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
