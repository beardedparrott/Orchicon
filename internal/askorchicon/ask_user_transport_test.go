package askorchicon_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askorchicon"
	"github.com/beardedparrott/orchicon/internal/mcp"
)

// TestAskUserReachesBothTransportsFromOneRegistration is the AC that no
// transport-specific wiring is needed. The TWO enumerations that ARE the two Ask
// transports must both carry ask_user, produced from the SINGLE allTools
// registration:
//
//   - native  — nativeAskTools.AskToolDefs, which enumerates
//     toolRegistry.List() and is injected into the native bridge;
//   - opencode — mcp.NewAskOrchiconRegistry(toolReg).List(), the registry the
//     `orchicon mcp` stdio sidecar serves to opencode as orchicon_<name>.
//
// An external test package (askorchicon_test) so it can import internal/mcp
// without an import cycle.
func TestAskUserReachesBothTransportsFromOneRegistration(t *testing.T) {
	reg := askorchicon.NewToolRegistry(nil, slog.Default(), nil)

	// Native transport.
	svc := askorchicon.New(nil, slog.Default(), nil, nil, nil)
	native := svc.NativeAskTools().AskToolDefs(context.Background())
	var nativeHas bool
	for _, d := range native {
		if d.Name == "ask_user" {
			nativeHas = true
		}
	}
	if !nativeHas {
		t.Error("the native Ask tool surface (AskToolDefs) does not offer ask_user")
	}

	// opencode transport (via the `orchicon mcp` sidecar registry).
	var mcpHas bool
	for _, d := range mcp.NewAskOrchiconRegistry(reg).List() {
		if d.Name == "ask_user" {
			mcpHas = true
		}
	}
	if !mcpHas {
		t.Error("the opencode Ask tool surface (orchicon mcp registry) does not offer ask_user")
	}
}
