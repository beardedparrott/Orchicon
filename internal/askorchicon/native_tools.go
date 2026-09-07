package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// NativeAskTools exposes this service's product tool registry as the
// provider-substrate Ask tool surface (orchicon.AskToolProvider) so native
// Ask turns can query, read, and act exactly like host-serve turns. The
// substrate never imports this package (layering); the server wires the
// returned provider into the native bridge via SetAskTools.
func (s *Service) NativeAskTools() orchicon.AskToolProvider {
	return &nativeAskTools{pool: s.pool, registry: s.toolRegistry}
}

// nativeAskTools adapts ToolRegistry onto orchicon.AskToolProvider.
type nativeAskTools struct {
	pool     *db.Pool
	registry *ToolRegistry
}

// Compile-time proof the adapter satisfies the substrate contract.
var _ orchicon.AskToolProvider = (*nativeAskTools)(nil)

func (a *nativeAskTools) AskToolDefs() []orchicon.ToolDef {
	if a.registry == nil {
		return nil
	}
	defs := a.registry.List()
	out := make([]orchicon.ToolDef, 0, len(defs))
	for _, d := range defs {
		out = append(out, orchicon.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			ParamsJSON:  toolParamsSchema(d),
		})
	}
	return out
}

func (a *nativeAskTools) ExecuteAskTool(ctx context.Context, name, argsJSON string) (string, error) {
	if a.registry == nil {
		return "", fmt.Errorf("ask tools unavailable")
	}
	raw, err := a.registry.Execute(ctx, a.pool, name, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("ask tool %q is not registered", name)
	}
	return string(raw), nil
}

// toolParamsSchema renders the MCP-style property map as a JSON-schema
// parameters object for the provider wire (mirrors the MCP adapter's
// property mapping).
func toolParamsSchema(d ToolDefinition) string {
	schema := map[string]any{"type": "object"}
	if len(d.Properties) > 0 {
		props := make(map[string]any, len(d.Properties))
		for k, v := range d.Properties {
			prop := map[string]any{"type": v.Type}
			if v.Type == "" {
				prop["type"] = "string"
			}
			if v.Description != "" {
				prop["description"] = v.Description
			}
			props[k] = prop
		}
		schema["properties"] = props
	}
	if len(d.Required) > 0 {
		schema["required"] = d.Required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return `{"type":"object"}`
	}
	return string(b)
}
