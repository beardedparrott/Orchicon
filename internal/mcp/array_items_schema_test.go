package mcp

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askorchicon"
)

// WHY THIS FILE EXISTS.
//
// A QA worker reported, from a live run:
//
//	"A live agy Ask turn fails with Gemini 400 INVALID_ARGUMENT: an MCP tool's `options` array has
//	 no `items` (probably internal/askorchicon/tools.go:235)."
//
// It was right. `ask_user.options` is `type: "array"` and neither PropertySchema nor the wire
// propertySchema had an `items` field, so the schema the platform EMITTED could never describe the
// array's elements — and Google's schema validator rejects that outright, 400ing the entire tool
// list. Every agy Ask turn offering ask_user failed, and no unit test could see it because no test
// serialised the schema and looked.
//
// This test does exactly that: it takes the REAL registry, serialises a tool property to the wire
// JSON, and asserts an array carries `items`.
func TestArrayPropertiesCarryItemsToTheWire(t *testing.T) {
	reg := askorchicon.NewToolRegistry(nil, slog.Default(), nil)

	var checked int
	for _, def := range reg.List() {
		for name, prop := range def.Properties {
			if prop.Type != "array" {
				continue
			}
			checked++
			b, err := json.Marshal(prop)
			if err != nil {
				t.Fatalf("%s.%s: marshal: %v", def.Name, name, err)
			}
			if !strings.Contains(string(b), `"items"`) {
				t.Errorf("%s.%s is type \"array\" but serialises without `items`:\n  %s\n"+
					"Gemini rejects the whole tool schema with 400 INVALID_ARGUMENT when an array has no "+
					"items, which fails every agy Ask turn that offers this tool.", def.Name, name, b)
			}
		}
	}
	if checked == 0 {
		t.Skip("no array-typed tool properties in the registry — the guard has nothing to inspect")
	}
}

// TestAskUserOptionsDeclaresItsElements names the specific property that broke, so a regression is
// unambiguous rather than "one of the array properties".
func TestAskUserOptionsDeclaresItsElements(t *testing.T) {
	reg := askorchicon.NewToolRegistry(nil, slog.Default(), nil)
	for _, def := range reg.List() {
		if def.Name != "ask_user" {
			continue
		}
		opts, ok := def.Properties["options"]
		if !ok {
			t.Fatal("ask_user no longer declares an options property")
		}
		if opts.Items == nil {
			t.Error("ask_user.options has no Items — this is the exact defect that 400ed every live " +
				"agy Ask turn. An array schema must describe its elements.")
		}
		return
	}
	t.Skip("ask_user not registered")
}
