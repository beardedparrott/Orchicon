package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/db"
)

// stubRegistry returns a canned success for any tool call, so a server.Run
// with a large tools/call payload exercises only the stdio reader path.
type stubRegistry struct{}

func (stubRegistry) List() []ToolDef { return nil }

func (stubRegistry) Execute(_ context.Context, _ *db.Pool, _ string, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

// TestServerRunHandlesLargePayload pins the large-payload batch_write fix
// (AC5): a single JSON-RPC tools/call line whose content is well over 64 KiB
// (the old bufio.Scanner.MaxScanTokenSize cap) must be read and dispatched
// without the server exiting with a "buffer too long" error. The old scanner
// returned bufio.ErrTooLong for the whole line, killing the MCP server.
func TestServerRunHandlesLargePayload(t *testing.T) {
	big := strings.Repeat("x", 200_000)
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "batch_write",
			"arguments": map[string]any{
				"writes": []any{map[string]any{"path": "big.txt", "mode": "create", "content": big}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	line := append(payload, '\n')

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origStdin, origStdout }()

	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, stubRegistry{})

	// Feed the (large) line in a goroutine so a full pipe buffer never
	// deadlocks the reader; Run consumes it until EOF.
	go func() {
		_, _ = inW.Write(line)
		inW.Close()
	}()

	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("Run returned an error (token too long?): %v", err)
	}
	outW.Close()
	out, _ := io.ReadAll(outR)
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("expected a result for the large payload call, got: %s", out)
	}
}

// scopeCapturingRegistry records the conversation scope each tool call ran under
// (and how many ran), so the stdio boundary can be asserted from the outside.
type scopeCapturingRegistry struct {
	scope askmode.ConversationScope
	calls int
	sawID bool
}

func (c *scopeCapturingRegistry) List() []ToolDef { return nil }

func (c *scopeCapturingRegistry) Execute(ctx context.Context, _ *db.Pool, _ string, _ json.RawMessage) (json.RawMessage, error) {
	c.scope = askmode.ConversationScopeFromContext(ctx)
	c.sawID = c.scope.ConversationID != ""
	c.calls++
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

// driveToolCall runs one tools/call through the REAL stdio path (Run), so an
// assertion covers everything between the env var and the tool's context.
//
// The pool is the SERVER's — handleToolsCall passes its own to the registry, not the
// registry's — so a tool that reaches the DB needs it supplied here.
func driveToolCall(t *testing.T, reg ToolRegistry, pool *db.Pool, toolName string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": toolName, "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	line := append(payload, '\n')
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origStdin, origStdout }()

	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), pool, reg)
	go func() {
		_, _ = inW.Write(line)
		inW.Close()
	}()
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	outW.Close()
	out, _ := io.ReadAll(outR)
	return string(out)
}

// THE SIDECAR IS A CHILD PROCESS. An Ask turn stamps its conversation scope on the
// turn's CONTEXT, which cannot cross a stdio boundary — so the scope arrives on the
// ENVIRONMENT and must be restored onto each tool call. Without this every Orchicon
// tool in a claude Ask session ran on an unstamped context, and get_current_conversation
// (the only way Quick Work can read the model_ref to offer) failed with
// "no conversation is stamped on this turn".
func TestToolsCallRestoresTheConversationScopeFromEnv(t *testing.T) {
	t.Setenv("ORCHICON_MCP_CONVERSATION_ID", "conv-ask-1")
	t.Setenv("ORCHICON_MCP_CONVERSATION_PROJECT_ID", "proj-ask-1")
	reg := &scopeCapturingRegistry{}
	driveToolCall(t, reg, nil, "get_current_conversation")

	if reg.calls != 1 {
		t.Fatalf("tool calls = %d, want 1", reg.calls)
	}
	if reg.scope.ConversationID != "conv-ask-1" {
		t.Errorf("ConversationID = %q, want conv-ask-1 — the tool would answer \"no conversation is stamped on this turn\"", reg.scope.ConversationID)
	}
	if reg.scope.ProjectID != "proj-ask-1" {
		t.Errorf("ProjectID = %q, want proj-ask-1", reg.scope.ProjectID)
	}
}

// The OTHER half, and the one a careless implementation gets wrong: a sidecar that
// serves NO conversation (a worker execution, or the plane's own MCP) must leave the
// context UNSTAMPED. Filling in an empty scope would replace the honest zero value —
// "not stamped", which a tool fails loud on — with a scope that CLAIMS to be a
// conversation, and every worker's get_current_conversation would then answer about a
// conversation that does not exist.
func TestToolsCallLeavesTheScopeUnstampedWithoutTheEnv(t *testing.T) {
	t.Setenv("ORCHICON_MCP_CONVERSATION_ID", "")
	t.Setenv("ORCHICON_MCP_CONVERSATION_PROJECT_ID", "")
	reg := &scopeCapturingRegistry{}
	driveToolCall(t, reg, nil, "get_current_conversation")

	if reg.calls != 1 {
		t.Fatalf("tool calls = %d, want 1", reg.calls)
	}
	if reg.sawID {
		t.Errorf("a conversation id was stamped from nowhere: %+v", reg.scope)
	}
}

// A PROJECT WITHOUT A CONVERSATION IS NOT A SCOPE, and this is the edge a lazy guard
// gets wrong. "Stamp if EITHER var is set" looks equivalent to "stamp if the conversation
// is set" when both are empty — a zero scope reads back as the zero value either way —
// but it is not: an orphan project would then be stamped as this session's project, and a
// tool that scopes its work by the conversation's project would act on a project
// belonging to no session at all. The producer never emits that combination
// (OrchiconMCPConversationEnv returns nil without a conversation id), so the invariant has
// to be held HERE, where a hand-wired `orchicon mcp` also lands.
func TestToolsCallDoesNotStampAProjectWithoutAConversation(t *testing.T) {
	t.Setenv("ORCHICON_MCP_CONVERSATION_ID", "")
	t.Setenv("ORCHICON_MCP_CONVERSATION_PROJECT_ID", "p-orphan")
	reg := &scopeCapturingRegistry{}
	driveToolCall(t, reg, nil, "get_current_conversation")

	if reg.scope.ProjectID != "" {
		t.Errorf("ProjectID = %q leaked into a session with no conversation: %+v", reg.scope.ProjectID, reg.scope)
	}
	if reg.scope.ConversationID != "" {
		t.Errorf("ConversationID = %q was stamped from nowhere: %+v", reg.scope.ConversationID, reg.scope)
	}
}
