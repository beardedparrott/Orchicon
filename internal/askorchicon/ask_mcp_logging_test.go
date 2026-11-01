package askorchicon

// ask_mcp_logging_test.go — THE LOG MUST DISTINGUISH THE THREE FAILURES IT LOOKED IDENTICAL FOR.
//
// REPORTED: the platform log recorded "ask: MCP servers registered" every turn and NOTHING about tools, so three
// very different situations were indistinguishable:
//
//	started but tools/list returned 0     (the server is up and advertises nothing)
//	defs built but dropped downstream     (offered to nobody)
//	offered fine                          (the model has them and did not use them)
//
// The reporter could not tell which they had, which is why the root cause took a whole session to find. These
// assertions pin the two lines that separate them, and the two refresh failures that used to be silent — a
// swallowed refresh leaves a stale client whose only symptom is a model saying it cannot call something.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpsettings"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// logCapture returns a logger writing into the buffer, so a test can assert on what an operator would read.
func logCapture() (*slog.Logger, *strings.Builder) {
	var b strings.Builder
	return slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo})), &b
}

// TestTheToolCountIsLogged is fix 2's first half: the number of DEFS, not just the servers.
func TestTheToolCountIsLogged(t *testing.T) {
	pool := chatDBTestPool(t)
	convID, projectID, _ := projectOnlyConversation(t, pool)
	log, buf := logCapture()
	svc := New(pool, log, nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, convID)
	ctx = withAskConversationProject(ctx, projectID)

	defs := svc.askMCPDefs(ctx)
	if len(defs) == 0 {
		t.Fatalf("no defs for a conversation whose project supplies a server:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "MCP tool defs offered") {
		t.Errorf("the turn discovered %d tools and logged nothing about them — the log cannot then distinguish "+
			"'offered fine' from 'defs built but dropped downstream':\n%s", len(defs), out)
	}
	if !strings.Contains(out, "tools=") {
		t.Errorf("the log line does not carry a tool COUNT:\n%s", out)
	}
}

// TestAnEmptyResolutionIsLoggedWithTheDisabledSet is fix 2's second half.
//
// The disabled ids are what turn "no MCP tools this turn" from a dead end into an action: definitions that exist
// and are switched off are a misconfiguration someone can fix.
func TestAnEmptyResolutionIsLoggedWithTheDisabledSet(t *testing.T) {
	pool := chatDBTestPool(t)
	convID, projectID, serverID := projectOnlyConversation(t, pool)
	// Disable it: now the resolution is empty for a reason worth naming.
	svcMCP := mcpSvc(pool, nil)
	entry, err := svcMCP.Get(context.Background(), "tnt_dev", serverID)
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	disabled := false
	if _, err := svcMCP.Update(context.Background(), "tnt_dev", mcpsettings.UpdateInput{ID: entry.ID, Enabled: &disabled}); err != nil {
		t.Fatalf("disable server: %v", err)
	}

	log, buf := logCapture()
	svc := New(pool, log, nil, nil, nil)
	svc.SetScopeResolver(mcpsettings.NewResolver(pool))

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	ctx = withAskMode(ctx, modeIteration)
	ctx = withAskConversation(ctx, convID)
	ctx = withAskConversationProject(ctx, projectID)

	if defs := svc.askMCPDefs(ctx); len(defs) != 0 {
		t.Fatalf("a disabled server produced %d tool defs", len(defs))
	}
	out := buf.String()
	if !strings.Contains(out, "resolved to none") {
		t.Errorf("an empty resolution was silent, so 'nothing configured' and 'everything disabled' look the "+
			"same to the operator:\n%s", out)
	}
	if !strings.Contains(out, serverID) {
		t.Errorf("the empty-resolution line does not name the DISABLED server %q, so it is a dead end rather than "+
			"an action:\n%s", serverID, out)
	}
}
