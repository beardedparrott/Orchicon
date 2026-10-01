package mcpclient

// e2eproof.go — the EXPORTED test seam over the in-repo MCP fixture server
// (fixture.go), for the end-to-end proof child.
//
// WHY IT IS NOT A _test.go FILE. The proof legs live in FIVE other packages
// (orchicon, claude, opencode, askorchicon, scheduler) and each one must drive
// the SAME fixture server, over the SAME two transports, with the SAME
// distinctive tool name. `fixtureServer()`/`fixtureArgs()`/`fixtureEnv()`/
// `fixtureRun()` are unexported, so without this seam every proof package would
// have to duplicate the server — and two copies of "the fixture" is exactly how
// a proof stops being about the production path. This file adds no product
// behaviour: it wraps the EXISTING fixture (never mutating it) and exports four
// thin constructors.
//
// THE DISTINCTIVE TOOL NAME (E2EProbeTool) is the point of the whole rig: a
// fixture server that is merely *reachable* proves plumbing, not configuration.
// A tool named `orchicon_e2e_probe` can only have come from THIS server — a
// stale, cached or other server cannot have supplied it — and every call
// carries a per-run NONCE that the result echoes, so a memoised answer is
// detectable rather than indistinguishable from a live one.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// E2EProbeTool is the distinctive probe tool the end-to-end proof calls. It
// carries both `orchicon` and `e2e`, so a wire name of the form
// `mcp__<serverID>__orchicon_e2e_probe` cannot be produced by any other server.
const E2EProbeTool = "orchicon_e2e_probe"

// E2EProbeResultPrefix prefixes every probe result. The result is
// Prefix + nonce, so the assertion proves the call reached THIS server (the
// prefix) AND that the answer belongs to THIS run (the nonce).
const E2EProbeResultPrefix = "mcpfixture:"

// E2EServer builds the fixture MCP server PLUS the probe tool. It calls the
// EXISTING fixtureServer() and adds the probe ON TOP — it never mutates it, so
// manager_test.go's assertions over `echo`/`fail`/`slow`/`die` are untouched.
func E2EServer() *mcp.Server {
	srv := fixtureServer()
	srv.AddTool(&mcp.Tool{
		Name:        E2EProbeTool,
		Description: "End-to-end proof probe: echoes the nonce it was given.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"nonce": map[string]any{"type": "string"},
			},
			"required": []string{"nonce"},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: E2EProbeResultPrefix + args.Nonce},
		}}, nil
	})
	return srv
}

// E2EHTTPFixture starts an in-process streamable-HTTP MCP fixture server (the
// E2EServer above) and returns its URL. No child process, no external network:
// the manager connects over a real JSON-RPC transport on a loopback socket,
// which is what makes the discovery and the call observations real.
//
// It serves with a plain net.Listener + http.Server rather than
// httptest.NewServer ON PURPOSE: this file is NOT a _test.go file (the proof
// packages outside mcpclient reuse E2EServer), so importing net/http/httptest
// here would link the test-only httptest package (and its hidden `-httptest.serve`
// flag) into the SHIPPED control-plane binary — a production-hygiene regression
// with no bearing on the proof.
func E2EHTTPFixture() (string, func()) {
	srv := E2EServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", func() {}
	}
	httpSrv := &http.Server{Handler: h}
	go func() { _ = httpSrv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = httpSrv.Close() }
}

// E2EHTTPFixtureT is the *testing.T-shaped convenience (starts the server and
// registers its cleanup), for the proof files that hold a *testing.T.
func E2EHTTPFixtureT(t interface{ Cleanup(func()) }) string {
	url, close := E2EHTTPFixture()
	t.Cleanup(close)
	return url
}

// E2EStdioSpec is the stdio ServerSpec re-executing THIS test binary as the
// fixture server. The re-exec entry point is E2EStdioReexec(), called from the
// host package's TestMain (a package may have exactly ONE TestMain, and
// TestMain runs before test selection, so the -test.run filter is belt-and-
// braces rather than the mechanism).
//
// The argv is built here rather than reusing fixtureArgs() on purpose: that
// helper runs mcpclient's bare fixture (no probe tool). The E2E leg needs the
// PROBE server on the stdio transport too, so it carries its OWN marker
// (E2EStdioMarkerEnv), which E2EStdioReexec honours and TestFixtureReexec
// ignores — so manager_test.go's stdio cases are untouched.
func E2EStdioSpec(id string) ServerSpec {
	return ServerSpec{
		ID:      id,
		Type:    TypeStdio,
		Command: []string{os.Args[0], "-test.run=" + E2EStdioReexecTestName},
		Env:     map[string]string{E2EStdioMarkerEnv: "1"},
	}
}

// E2EStdioMarkerEnv marks the stdio child that must serve the PROBE server.
const E2EStdioMarkerEnv = "ORCHICON_E2E_PROBE_FIXTURE"

// e2eReexecArgv reports whether this process was launched as the E2E stdio
// probe child, by looking for the -test.run filter in argv — the signal that
// survives storage as an mcp_servers.command/args pair.
func e2eReexecArgv() bool {
	for _, a := range os.Args {
		if strings.Contains(a, E2EStdioReexecTestName) {
			return true
		}
	}
	return false
}

// E2EStdioReexecTestName is the -test.run filter E2EStdioSpec launches.
const E2EStdioReexecTestName = "TestE2EFixtureReexec"

// E2EStdioReexec runs the fixture server over stdio when the re-exec marker is
// set, and returns (false) otherwise. A package's TestMain calls it as:
//
//	func TestMain(m *testing.M) {
//		if mcpclient.E2EStdioReexec() {
//			return
//		}
//		os.Exit(m.Run())
//	}
//
// A package may have exactly ONE TestMain, so this is the single line that
// gives any proof package a working stdio fixture child.
func E2EStdioReexec() bool {
	// TWO signals, because the marker CANNOT ride the DB row: a stored stdio
	// definition keeps only its command + args (mcp_servers.command/args), so an
	// env map set on the spec is lost the moment a real project-owned row is the
	// source. The argv, by contrast, IS the row's command — so the -test.run test
	// name is the durable signal, and the env marker is the convenience one.
	if os.Getenv(E2EStdioMarkerEnv) != "1" && !e2eReexecArgv() {
		return false
	}
	// The stdio child's stdout is the JSON-RPC stream, so the process must NOT
	// return to the test runner. E2EServer() — NOT fixtureRun() — because the
	// probe tool must be present on the stdio transport too; the transport is the
	// variable, the server is not.
	srv := E2EServer()
	ts := &mcp.StdioTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = srv.Run(ctx, ts)
	return true
}
