package aigateway

// cli.go — running the opencode CLI, and REPORTING WHY IT FAILED.
//
// WHY THIS FILE EXISTS. Both discoverers shelled out like this:
//
//	out, err := cmd.Output()
//	if err != nil {
//		return nil, fmt.Errorf("opencode models: %w", err)
//	}
//
// `%w` on an *exec.ExitError renders as "exit status 1" — the status, and nothing else. The CLI's
// OWN explanation was already in hand and thrown away: exec.Output() points the child's stderr at a
// bounded buffer, so the ExitError that comes back CARRIES the message that says what went wrong.
//
// MEASURED against the real CLI, so this is not a theory about what might be in there. Invoking
// `opencode models --bogus-flag` and reading the two values:
//
//	err.Error()            = "exit status 1"
//	ee.Stderr              = "opencode models [provider]\n\nlist all available models\n\nPositionals: ..."
//
// The operator sees the first line in the TUI ("internal: list opencode models: opencode models:
// exit status 1") and the second — 900 bytes naming the command's actual arguments and options — is
// discarded. Neither they nor a model can act on "exit status 1": it does not distinguish a version
// without the subcommand from a bad flag from a CLI that refuses to run in this environment.
//
// This is the same defect, in the same shape, that the plane's model-discovery message and the
// execution guard's refusal had. It is worth stating as a rule: when a subprocess fails, the
// subprocess's own words are the most useful thing available, and dropping them in favour of an exit
// status is always a loss.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// cliOutputCap bounds how much of a failing CLI's output is put into an error. The message is
// surfaced in an RPC error and rendered in the TUI, so it has to stay readable; the HEAD is kept
// because a CLI's first line is the one that names the problem (opencode's usage text opens with
// the command's own synopsis).
const cliOutputCap = 2000

// runOpencodeCLI runs the CLI and returns its stdout, with the CLI's own
// explanation included in any error. dir is the working directory — empty means
// inherit the caller's, which callers should avoid (see the note at the call sites: the plane's cwd
// is whatever directory the operator ran the installer from).
func runOpencodeCLI(ctx context.Context, binary, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}

	invocation := "opencode " + strings.Join(args, " ")

	// exec.Output() populates ExitError.Stderr (bounded, and it keeps both ends), so the child's
	// words are normally here.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if detail := excerpt(ee.Stderr); detail != "" {
			return nil, &cliError{invocation: invocation, err: err, detail: detail}
		}
		// Some CLIs report a failure on STDOUT and write nothing to stderr — a bad flag handled by
		// a wrapper, for instance. Output() captured it, so it is checked before giving up on
		// explaining anything.
		if detail := excerpt(out); detail != "" {
			return nil, &cliError{invocation: invocation, err: err, detail: detail, viaStdout: true}
		}
	}
	// A timeout or a start failure: the status is genuinely all there is, and ctx.Err() names the
	// deadline case, which "exit status 1" would otherwise hide.
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("%s: %w (%v)", invocation, err, cerr)
	}
	// NO DETAIL ATTACHED — the CLI exited without saying anything. This is the TRANSIENT shape (the managed
	// service mid-eviction), and it is recorded as a STRUCTURED FACT rather than left to be inferred from the
	// message text.
	//
	// IT WAS INFERRED FROM TEXT, AND THAT WAS WRONG. The retry classifier first asked "does the message look
	// like it carries a diagnosis?" by matching phrases and string length — and mis-classified exactly the case
	// that matters: "list opencode MCP servers: opencode debug config: exit status 1" is 63 characters with a
	// ": " in it, so it read as explained and was NOT retried, while a genuinely silent failure is precisely
	// what a retry exists for. The wrapper is the only code that knows whether it attached a detail, so it says
	// so here and the classifier reads the fact instead of guessing at the prose.
	return nil, &cliError{invocation: invocation, err: err}
}

// cliError is a CLI invocation failure that RECORDS whether the CLI explained itself.
//
// detail == "" means the CLI exited without saying why, which is the retryable shape; a non-empty detail is the
// CLI's own diagnosis, which is deterministic and must be surfaced at once rather than re-collected.
type cliError struct {
	invocation string
	err        error
	detail     string
	viaStdout  bool
}

func (e *cliError) Unwrap() error { return e.err }

func (e *cliError) Error() string {
	switch {
	case e.detail == "":
		return e.invocation + ": " + e.err.Error()
	case e.viaStdout:
		return e.invocation + ": " + e.err.Error() + " (no stderr; stdout said): " + e.detail
	default:
		return e.invocation + ": " + e.err.Error() + ": " + e.detail
	}
}

// excerpt trims a CLI's output down to something an error message can carry: whitespace collapsed
// to the end, capped, with the truncation stated rather than silent.
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return ""
	}
	if len(s) > cliOutputCap {
		return s[:cliOutputCap] + " … (truncated)"
	}
	return s
}
