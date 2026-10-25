package main

// mcp_stdio_stdout_test.go — STDOUT BELONGS TO THE PROTOCOL.
//
// REPORTED: "Ask cards are now not rendering in both windows and Linux. The model in both operating
// systems in the GUI and the TUI claims it sent and rendered a card but no card popped up", with the
// operator's own diagnosis that the orchicon MCP server had failed inside opencode.
//
// It had, and opencode's log named the reason exactly:
//
//	level=WARN message="mcp transport error" server=orchicon
//	error="... unrecognized_keys ... keys: [\"time\",\"level\",\"msg\"] ...
//	        jsonrpc: Invalid input: expected \"2.0\" ..."
//
// That is OUR LOG LINE being validated as a JSON-RPC message. The process-wide logger is built with
// slog.NewJSONHandler(os.Stdout, ...) — right for every other subcommand — and `orchicon mcp` speaks
// JSON-RPC over stdout, so its very first record landed inside the protocol stream. opencode declared
// the server's transport invalid, which is why tool calls (and therefore every ask and consent card)
// could not reach this sidecar even though the model was told a card had been shown.
//
// The assertions below are the ones opencode makes: parse every frame we write, and require that no
// frame carries log-record keys.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestTheMCPLoggerWritesOnlyToItsWriter pins the helper the sidecar uses: JSON records, one per line,
// and none of them a JSON-RPC frame.
func TestTheMCPLoggerWritesOnlyToItsWriter(t *testing.T) {
	var buf bytes.Buffer
	log := mcpLogger(&buf)
	log.Info("mcp server started (stdio transport)")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("mcpLogger wrote nothing")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("the MCP log record is not JSON (%v): %q", err, line)
	}
	// The three keys opencode objected to are exactly a slog JSON record's.
	if _, ok := rec["msg"]; !ok {
		t.Errorf("expected a slog record with a msg key, got %v", rec)
	}
	if _, ok := rec["jsonrpc"]; ok {
		t.Errorf("a log record carries a jsonrpc key — it would be read as protocol frame: %v", rec)
	}
}

// TestRunMCPRoutesItsLoggerAwayFromStdout is the wiring assertion, at the source, because the failure
// is a control-flow property of one function.
//
// It cannot be a behavioural test of runMCP itself: that would have to exec the built binary and
// speak JSON-RPC to it, which is a slow and flaky thing to put in the unit gate — and the property
// being pinned (which writer the logger is handed) is visible in four lines of source.
func TestRunMCPRoutesItsLoggerAwayFromStdout(t *testing.T) {
	src := readFile(t, "main.go")

	i := strings.Index(src, "func runMCP(")
	if i < 0 {
		t.Fatal("runMCP is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}

	// THE REDIRECT, before anything can log.
	if !strings.Contains(body, "mcpStderrLogger()") {
		t.Error("runMCP does not build a stderr logger.\n" +
			"  The process-wide logger writes JSON to STDOUT, and stdout is this subcommand's\n" +
			"  JSON-RPC channel — so its first log record is parsed as a protocol frame, opencode\n" +
			"  reports an mcp transport error, and tool calls never reach the sidecar. That is the\n" +
			"  reported 'cards do not render' bug.")
	}
	if !strings.Contains(body, "slog.SetDefault(") {
		t.Error("runMCP does not install its logger as the PROCESS default.\n" +
			"  Several code paths reached from here (config load, db open, the KEK warning) log\n" +
			"  through slog.Default(), which is still the stdout logger — each of those would put a\n" +
			"  record into the protocol stream even with a local logger in place.")
	}

	// AND NOTHING IN runMCP MAY REACH FOR STDOUT EXPLICITLY — checked on CODE lines only.
	//
	// The comment above this function NAMES os.Stdout while explaining why it is forbidden, and a
	// Contains over the raw body matches that prose. (Measured: the first version of this assertion
	// failed on its own documentation.) Stripping comments first is what makes the check about
	// behaviour rather than about wording.
	for _, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "//") {
			continue
		}
		for _, bad := range []string{"os.Stdout", "fmt.Print"} {
			if strings.Contains(code, bad) {
				t.Errorf("runMCP references %q in code (%q) — stdout is the JSON-RPC channel for this "+
					"subcommand; anything printed there corrupts the stream", bad, code)
			}
		}
	}
}

// TestTheProcessLoggerIsStdoutForEveryOtherSubcommand guards the other side of the change: the fix
// must be LOCAL to the MCP sidecar, not a global move of the plane's logging to stderr.
func TestTheProcessLoggerIsStdoutForEveryOtherSubcommand(t *testing.T) {
	src := readFile(t, "main.go")
	if !strings.Contains(src, "slog.NewJSONHandler(os.Stdout,") {
		t.Error("the process logger no longer writes to stdout.\n" +
			"  Every other subcommand's output contract depends on it (the plane's own logs are read\n" +
			"  from stdout). Only the stdio MCP sidecar may differ, which is why the redirect lives in\n" +
			"  runMCP rather than at the process root.")
	}
}
