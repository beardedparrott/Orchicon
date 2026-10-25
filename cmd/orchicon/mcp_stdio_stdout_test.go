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
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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

// TestEveryOrchiconMCPServerSpawnGoesThroughTheMCPSubcommand is the "did this break the OTHER
// adapters" assertion, and it is the one that matters most for blast radius.
//
// There are FOUR places that register `orchicon` (or the plane/worktree sidecars) as a stdio MCP
// server for somebody else to spawn:
//
//	internal/opencode/config.go   orchiconMCPServer   — the opencode Ask serve AND worker serves
//	internal/opencode/config.go   planeMCPServer      — role-bound worker executions
//	internal/opencode/config.go   worktreeMCPServer   — the composite file tools in a runtime
//	internal/claude/mcpconfig.go  the claude bridge   — Ask and worker, both v1 and v2 shapes
//
// and every one of them must name the `mcp` SUBCOMMAND, because that is the only entry point that
// redirects logging to stderr. A fifth registration using a different subcommand — or none — would
// put stdout log records into the JSON-RPC stream again, for THAT adapter only, and it would be
// invisible to the Ask-path tests.
//
// THIS IS SLOW-BY-NATURE TO TEST BEHAVIOURALLY (it needs opencode 1.x, opencode 2.x and claude all
// installed and driven), so it is asserted where the contract is made: the argv each adapter
// registers.
func TestEveryOrchiconMCPServerSpawnGoesThroughTheMCPSubcommand(t *testing.T) {
	for _, tc := range []struct{ path, what string }{
		{"../../internal/opencode/config.go", "opencode (Ask serve + worker serves)"},
		{"../../internal/claude/mcpconfig.go", "claude (Ask + worker)"},
	} {
		src := readFile(t, tc.path)
		// The registered argv is always [{binary}, "mcp"] for a local stdio server.
		if !strings.Contains(src, `"mcp"`) {
			t.Errorf("%s no longer registers the `mcp` subcommand for its Orchicon MCP server.\n"+
				"  Every Orchicon MCP sidecar must be spawned as `orchicon mcp`: that is the only\n"+
				"  entry point that keeps logging OFF stdout, and stdout is the JSON-RPC channel.\n"+
				"  A different subcommand reintroduces the transport error for this adapter only.",
				tc.what)
		}
	}

	// AND THERE IS EXACTLY ONE HANDLER. If a second `case "mcp"` appeared somewhere, one of them
	// would inevitably miss the stderr redirect.
	main := readFile(t, "main.go")
	if n := strings.Count(main, `case "mcp":`); n != 1 {
		t.Errorf("main.go has %d `case \"mcp\":` arms, want exactly 1 — the stderr redirect lives in "+
			"runMCP, so a second dispatch arm would bypass it", n)
	}
}

// TestTheOrchiconMCPChildWritesOnlyProtocolToStdout is the end-to-end shape of the same rule, run
// against the REAL sidecar: speak JSON-RPC at it and require that every line on stdout parses as a
// protocol frame.
//
// IT USES A SPAWNED BINARY rather than the logger in isolation, because the defect was not "the
// logger is wrong" but "something printed a log record on the protocol channel" — and only the whole
// process can prove that nothing else does it either.
func TestTheOrchiconMCPChildWritesOnlyProtocolToStdout(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the sidecar and needs a reachable Postgres")
	}
	bin := os.Getenv("ORCHICON_TEST_BINARY")
	if bin == "" {
		t.Skip("set ORCHICON_TEST_BINARY to a built orchicon to run the sidecar over real stdio")
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Stdin = strings.NewReader(init)
	cmd.Env = append(os.Environ(), "ORCHICON_MCP_TENANT_ID=tnt_dev")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run() // the sidecar exits when stdin closes

	lines := 0
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Errorf("stdout line %d is not JSON: %q", lines, line)
			continue
		}
		if _, ok := frame["jsonrpc"]; !ok {
			t.Errorf("stdout line %d is not a JSON-RPC frame (no jsonrpc key): %q\n"+
				"  THIS IS THE REPORTED BUG: opencode validates each frame and reports an mcp\n"+
				"  transport error for keys like time/level/msg, which is a LOG RECORD on the\n"+
				"  protocol channel.", lines, line)
		}
	}
	if lines == 0 {
		t.Fatalf("the sidecar wrote nothing to stdout (stderr: %s)", stderr.String())
	}
}
