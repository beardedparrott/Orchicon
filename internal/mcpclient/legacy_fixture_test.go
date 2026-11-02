package mcpclient

// legacy_fixture_test.go — a server that behaves like the reported Python one.
//
// THE REAL FAILURE, reproduced faithfully: an older-spec MCP server receives `server/discover`, does not
// know the method, fails validation and EXITS 1. It is happy to run the `initialize` handshake it has always
// supported. That is the whole shape, and it is what makes "the client is on a newer protocol" look exactly
// like "this server is broken".
//
// The child is hand-rolled JSON-RPC rather than the SDK's server, deliberately: the point is to be an OLD
// server, and building it with the current SDK would let the SDK's own compatibility keep it working — the
// test would then pass for a reason unrelated to the fix.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// legacyFixtureEnv marks a re-exec child that should run the legacy server instead of tests.
const legacyFixtureEnv = "ORCHICON_MCP_LEGACY_FIXTURE"

// legacyFixtureLogEnv names a file the child appends the method of every request it receives to, so a test
// can assert WHICH handshake was attempted (not merely that the result looked right).
const legacyFixtureLogEnv = "ORCHICON_MCP_LEGACY_FIXTURE_LOG"

// legacyFixtureArgs is the argv that re-executes this binary as the legacy server.
func legacyFixtureArgs() []string {
	return []string{os.Args[0], "-test.run=TestLegacyFixtureReexec"}
}

// TestLegacyFixtureReexec is the child entry point, reached by re-executing this binary with
// `-test.run=TestLegacyFixtureReexec` (see legacyFixtureArgs). It never returns to the test runner: stdout IS
// the JSON-RPC stream, so the process exits rather than letting the runner print a PASS line into it.
//
// DISPATCHED FROM HERE, NOT FROM TestMain, and that is deliberate. A TestMain that returned early for this
// marker would exit BEFORE m.Run() and this function would never be called — the child would exit 0 having
// answered nothing, which is exactly how the first version of this fixture failed (silently, as "connection
// closed … EOF").
func TestLegacyFixtureReexec(t *testing.T) {
	if os.Getenv(legacyFixtureEnv) != "1" {
		return
	}
	runLegacyFixture()
	os.Exit(0)
}

// runLegacyFixture speaks just enough MCP to be an old server.
func runLegacyFixture() {
	logPath := os.Getenv(legacyFixtureLogEnv)
	record := func(method string) {
		if logPath == "" {
			return
		}
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(method + "\n")
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		record(req.Method)

		switch req.Method {
		case "server/discover":
			// THE REPORTED BEHAVIOUR, verbatim in spirit: an unknown method is a VALIDATION failure for an
			// old server, it says so on stderr, and it dies. It does NOT return a JSON-RPC error, which is
			// why the SDK's own fallback can never be reached in a useful way.
			fmt.Fprintln(os.Stderr, "CallToolRequest.method | Input should be 'tools/call' [input_value='server/discover']")
			os.Exit(1)
		case "initialize":
			// The legacy handshake this server has always supported.
			writeResult(req.ID, map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "legacy-fixture", "version": "1.0.0"},
			})
		case "notifications/initialized":
			// A notification: never answered.
		case "tools/list":
			writeResult(req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "legacy_echo",
					"description": "the tool a legacy server really does offer",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			})
		case "tools/call":
			writeResult(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			})
		default:
			// An unknown NOTIFICATION is ignored; an unknown REQUEST gets a method-not-found error, which
			// is the WELL-BEHAVED case (the SDK handles that one itself). Kept distinct from
			// server/discover on purpose, so the fixture proves the difference matters.
			if len(req.ID) > 0 && string(req.ID) != "null" {
				writeError(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
}

func writeResult(id json.RawMessage, result any) {
	if len(id) == 0 || string(id) == "null" {
		return
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return
	}
	fmt.Println(string(b))
}

func writeError(id json.RawMessage, code int, msg string) {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	if err != nil {
		return
	}
	fmt.Println(string(b))
}
