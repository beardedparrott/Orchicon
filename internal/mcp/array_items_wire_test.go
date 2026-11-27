package mcp

// array_items_wire_test.go — the WIRE half of the `items` guard.
//
// array_items_schema_test.go checks that `askorchicon`'s own PropertySchema carries `items`. That test was
// written after a live 400 and it passed while the bug was still live, because the defect was one layer
// further out: `AskOrchiconRegistry.List` (mcp_adapter.go) rebuilt each property from Type and Description
// ONLY, so `items` — and any future field — was dropped between the registry and `tools/list`.
//
// THAT IS THE SHAPE OF BUG THIS FILE EXISTS TO CATCH: a correct fix in one layer, silently discarded in the
// next, invisible to both, and only ever visible to a vendor's schema validator on a live call. So this test
// never touches the registry's own structs: it drives the SAME adapter the `orchicon mcp` sidecar drives and
// inspects the JSON that would actually go on the wire.

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askorchicon"
)

// TestArrayItemsSurviveTheAdapterToTheWire is the regression guard for the dropped `items` field.
func TestArrayItemsSurviveTheAdapterToTheWire(t *testing.T) {
	reg := NewAskOrchiconRegistry(askorchicon.NewToolRegistry(nil, slog.Default(), nil))

	var checked int
	for _, def := range reg.List() {
		for name, prop := range def.Properties {
			if prop.Type != "array" {
				continue
			}
			checked++
			// THE FULL PATH: property → wire JSON, exactly as handleToolsList serialises it.
			b, err := json.Marshal(struct {
				InputSchema inputSchema `json:"inputSchema"`
			}{inputSchema{Type: "object", Properties: map[string]propertySchema{name: prop}}})
			if err != nil {
				t.Fatalf("%s.%s: marshal: %v", def.Name, name, err)
			}
			var decoded struct {
				InputSchema struct {
					Properties map[string]struct {
						Type  string          `json:"type"`
						Items json.RawMessage `json:"items"`
					} `json:"properties"`
				} `json:"inputSchema"`
			}
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("%s.%s: unmarshal: %v", def.Name, name, err)
			}
			got := decoded.InputSchema.Properties[name]
			if got.Type != "array" {
				t.Errorf("%s.%s: wire type = %q, want array", def.Name, name, got.Type)
			}
			if len(got.Items) == 0 || string(got.Items) == "null" {
				t.Errorf("%s.%s reaches the wire as an array with NO `items`:\n  %s\n"+
					"AskOrchiconRegistry.List must carry every field of the registry's property; Gemini "+
					"rejects the whole tool list with 400 INVALID_ARGUMENT otherwise, which fails EVERY "+
					"agy Ask turn that offers this tool.", def.Name, name, b)
			}
		}
	}
	if checked == 0 {
		t.Skip("no array-typed tool properties reached the adapter — the guard has nothing to inspect")
	}
}
