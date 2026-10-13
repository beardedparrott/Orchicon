package aigateway

// cli_error_test.go — the CLI's OWN words must survive a failure.
//
// THE BUG THIS PINS, measured against the real binary before it was fixed:
//
//	`opencode models --bogus-flag` -> ExitError.Error() = "exit status 1"
//	                                  ExitError.Stderr   = "opencode models [provider]\n\nlist all
//	                                  available models\n\nPositionals: ..."
//
// The discoverer reported only the first, so the operator's TUI showed
//
//	internal: list opencode models: opencode models: exit status 1
//
// and 900 bytes naming the command's arguments were discarded. "exit status 1" does not distinguish a
// version without the subcommand from a bad flag from a CLI that will not run in this environment, so
// there is nothing to act on — which is exactly the report that led here.
//
// The stubs are shell scripts rather than the real CLI: CI has no opencode, and a test that needs one
// installed is a test that silently does not run.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stubCLI writes an executable that prints stdoutText, writes stderrText to stderr, and exits with
// exitCode. An empty string means print nothing on that stream.
func stubCLI(t *testing.T, stdoutText, stderrText string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	// QUOTED HEREDOCS, deliberately. The first version used printf with the text in double quotes,
	// and a fixture containing backticks was COMMAND-SUBSTITUTED by the shell — the stub failed for a
	// reason invented by the test rather than by the code under test. A quoted delimiter expands
	// nothing, so the fixture text reaches the stream verbatim.
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if stdoutText != "" {
		b.WriteString("cat <<'STUB_STDOUT'\n" + stdoutText + "\nSTUB_STDOUT\n")
	}
	if stderrText != "" {
		b.WriteString("cat <<'STUB_STDERR' >&2\n" + stderrText + "\nSTUB_STDERR\n")
	}
	b.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestModelDiscoverySurfacesTheCLIsOwnError(t *testing.T) {
	const cliSaid = "opencode models [provider] — unknown option --verbose"
	bin := stubCLI(t, "", cliSaid, 1)

	d := NewModelDiscoverer(discardLog(), bin)
	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), cliSaid) {
		t.Errorf("the CLI's own explanation must be IN the error — it is the only thing that says "+
			"WHY. Before this was fixed the error was just \"exit status 1\".\n  got: %v", err)
	}
	// THE MINIMUM INVOCATION IS WHAT GETS NAMED, not the --verbose one. fetchModels now tries
	// `models --verbose` and falls back to `models`, and when both fail it reports the second: a
	// bare `models` is the invocation that cannot work at all, so naming the flag would point at
	// the wrong thing. Asserting the ABSENCE of --verbose is what keeps this meaningful — a plain
	// `strings.Contains(err, "opencode models")` would also pass for the old behaviour.
	if !strings.Contains(err.Error(), "opencode models:") {
		t.Errorf("the error must name the minimum invocation that failed, got: %v", err)
	}
	// THE INVOCATION PREFIX IS WHAT MUST BE FREE OF THE FLAG — not the whole message. The CLI's own
	// stderr is appended verbatim and may mention `--verbose` (an unknown-option complaint does
	// exactly that), so asserting its absence anywhere would fail on a CORRECT message. Measured:
	// the first version of this check failed against a message that was right.
	if strings.Contains(err.Error(), "opencode models --verbose") {
		t.Errorf("the invocation reported must be the minimum one, not the --verbose form, got: %v", err)
	}
}

func TestMCPDiscoverySurfacesTheCLIsOwnError(t *testing.T) {
	const cliSaid = "opencode debug — no such subcommand"
	bin := stubCLI(t, "", cliSaid, 1)

	d := NewMCPDiscoverer(discardLog(), bin)
	_, err := d.fetchMCPs(context.Background())
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), cliSaid) {
		t.Errorf("the MCP discoverer had the identical defect and must also keep the CLI's words; got: %v", err)
	}
}

func TestDiscoveryReportsStdoutWhenTheCLISaysNothingOnStderr(t *testing.T) {
	// Not every CLI writes failures to stderr. exec.Output() captured stdout anyway, so discarding it
	// because stderr was empty would lose the explanation a second time.
	const onStdout = "error: this build has no `models` command"
	bin := stubCLI(t, onStdout, "", 1)

	d := NewModelDiscoverer(discardLog(), bin)
	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), onStdout) {
		t.Errorf("with an empty stderr the stdout is the only explanation there is, so it must be "+
			"reported rather than dropped; got: %v", err)
	}
}

func TestDiscoveryStillFailsWhenTheCLISaysNothingAtAll(t *testing.T) {
	// The floor: silence must not become success, and the error must still name the invocation and
	// carry the exit status so the failure is visible even with nothing to quote.
	bin := stubCLI(t, "", "", 3)

	d := NewModelDiscoverer(discardLog(), bin)
	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("a silent non-zero exit must still be an error — silence is not success")
	}
	if !strings.Contains(err.Error(), "opencode models:") {
		t.Errorf("the error must name the invocation, got: %v", err)
	}
}

func TestTheCLIsOutputIsCapped(t *testing.T) {
	// A CLI can dump a megabyte onto stderr. This message reaches an RPC error and the TUI, so it is
	// capped — and says so, because a silently cut message is worse than a marked one.
	long := strings.Repeat("x", cliOutputCap+500)
	bin := stubCLI(t, "", long, 1)

	d := NewModelDiscoverer(discardLog(), bin)
	_, err := d.fetchModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := len(err.Error()); got > cliOutputCap+500 {
		t.Errorf("the error is %d bytes — the CLI's output must be capped near %d", got, cliOutputCap)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("a capped message must say it was truncated, got %d bytes", len(err.Error()))
	}
}
