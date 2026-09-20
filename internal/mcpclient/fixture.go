package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Fixture server: an in-repo MCP server used by the tests over BOTH
// transports (stdio via TestMain re-exec, streamable HTTP via
// httptest + NewStreamableHTTPHandler). No external network.
//
// Tools:
//   - echo: returns the `message` argument verbatim.
//   - fail: returns a tool error (IsError) with a fixed message.
//   - slow: sleeps for `seconds` then returns "slow done".
//   - die:  causes the server process to exit.

const (
	fixtureToolEcho = "echo"
	fixtureToolFail = "fail"
	fixtureToolSlow = "slow"
	fixtureToolDie  = "die"
)

// fixtureRun is the re-exec entry point: run the fixture MCP server over
// stdio until the transport closes. It returns (rather than os.Exit) so it
// is also callable from the re-exec test hook.
func fixtureRun() {
	server := fixtureServer()
	ts := &mcp.StdioTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = server.Run(ctx, ts)
}

// fixtureServer builds the fixture MCP server with its tools.
func fixtureServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "orchicon-fixture",
		Version: "0.0.1",
	}, nil)

	server.AddTool(&mcp.Tool{
		Name:        fixtureToolEcho,
		Description: "Echoes back the given message.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
			"required": []string{"message"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Message}},
		}, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        fixtureToolFail,
		Description: "Always fails with a tool error.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := &mcp.CallToolResult{}
		res.SetError(fmt.Errorf("fixture failure"))
		return res, nil
	})

	server.AddTool(&mcp.Tool{
		Name:        fixtureToolSlow,
		Description: "Sleeps for the given seconds then returns.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"seconds": map[string]any{"type": "number"},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Seconds float64 `json:"seconds"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		sec := time.Duration(args.Seconds * float64(time.Second))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sec):
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "slow done"}},
			}, nil
		}
	})

	// die is registered but intentionally not used by the stdio tests via
	// re-exec (the child exits when the transport closes); it exists so the
	// HTTP transport can exercise server-side termination.
	_ = fixtureToolDie

	return server
}

// fixtureArgs returns the argv to re-exec this binary as the stdio
// fixture server.
func fixtureArgs() []string {
	return []string{os.Args[0], "-test.run=TestFixtureReexec"}
}

// fixtureEnv returns the child environment for the stdio fixture: the
// current environment plus the re-exec marker.
func fixtureEnv() []string {
	return append(os.Environ(), "ORCHICON_MCP_FIXTURE=1")
}
