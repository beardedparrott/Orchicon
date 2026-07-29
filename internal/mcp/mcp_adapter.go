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
		out = append(out, ToolDef{
			Name:        d.Name,
			Description: d.Description,
			Mutating:    d.Mutating,
		})
	}
	return out
}

func (a *AskOrchiconRegistry) Execute(ctx context.Context, pool *db.Pool, name string, args json.RawMessage) (json.RawMessage, error) {
	return a.inner.Execute(ctx, pool, name, args)
}
