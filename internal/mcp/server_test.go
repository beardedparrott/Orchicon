package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

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
