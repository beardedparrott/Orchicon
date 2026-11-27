package mcp

import (
	"context"
	"encoding/json"

	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/db"
)

// AskOrchiconRegistry adapts the askorchicon.ToolRegistry to the mcp.ToolRegistry interface.
type AskOrchiconRegistry struct {
	inner *askorchicon.ToolRegistry
}

func NewAskOrchiconRegistry(r *askorchicon.ToolRegistry) *AskOrchiconRegistry {
	return &AskOrchiconRegistry{inner: r}
}

func (a *AskOrchiconRegistry) List() []ToolDef {
	defs := a.inner.List()
	out := make([]ToolDef, 0, len(defs))
	for _, d := range defs {
		t := ToolDef{
			Name:        d.Name,
			Description: d.Description,
			Mutating:    d.Mutating,
		}
		if len(d.Properties) > 0 {
			props := make(map[string]propertySchema, len(d.Properties))
			for k, v := range d.Properties {
				props[k] = propertySchemaFromAsk(v)
			}
			t.Properties = props
			t.Required = d.Required
		}
		out = append(out, t)
	}
	return out
}

func (a *AskOrchiconRegistry) Execute(ctx context.Context, pool *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	return a.inner.Execute(ctx, pool, name, args)
}

// propertySchemaFromAsk carries EVERY field of an askorchicon property onto the wire shape, including
// `items`.
//
// THIS ADAPTER USED TO COPY TYPE AND DESCRIPTION ONLY, and that one omission made `ask_user` unusable on
// the antigravity transport: `options` is an array, and Google's tool-schema validator rejects an array
// whose `items` is absent with
//
//	400 INVALID_ARGUMENT ... properties[options].items: missing field
//
// which fails the WHOLE GenerateContentRequest — so every agy Ask turn that offered `ask_user` died before
// the model could call anything. The schema fix in askorchicon/tools.go was correct and did not help,
// because the field was dropped here on the way out; no unit test noticed, because only a live call
// surfaces a vendor schema validator. It is written as a recursive copy rather than a field list so a
// future PropertySchema field cannot be silently lost the same way.
func propertySchemaFromAsk(v askorchicon.PropertySchema) propertySchema {
	out := propertySchema{
		Type:        v.Type,
		Description: v.Description,
	}
	if v.Items != nil {
		items := propertySchemaFromAsk(*v.Items)
		out.Items = &items
	}
	return out
}
