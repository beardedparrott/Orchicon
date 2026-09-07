package askorchicon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeAskToolDefsCarryJSONSchema(t *testing.T) {
	r := testToolRegistry()
	r.Add(ToolDefinition{
		Name:        "ping",
		Description: "Ping",
		Properties:  map[string]PropertySchema{"target": {Type: "string", Description: "Host"}},
		Required:    []string{"target"},
	})
	p := (&Service{toolRegistry: r}).NativeAskTools()
	defs := p.AskToolDefs()
	var found bool
	for _, d := range defs {
		if d.Name != "ping" {
			continue
		}
		found = true
		var schema map[string]any
		if err := json.Unmarshal([]byte(d.ParamsJSON), &schema); err != nil {
			t.Fatalf("ping ParamsJSON is not JSON: %v", err)
		}
		if schema["type"] != "object" {
			t.Fatalf("schema = %v, want object", schema)
		}
		props, _ := schema["properties"].(map[string]any)
		if props["target"] == nil {
			t.Fatalf("schema properties = %v, want target", schema["properties"])
		}
	}
	if !found {
		t.Fatal("ping def missing from native Ask tools")
	}
}

func TestNativeAskToolExecuteUnknownErrors(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	if _, err := p.ExecuteAskTool(context.Background(), "nope", `{}`); err == nil ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown tool err = %v, want not-registered", err)
	}
}
