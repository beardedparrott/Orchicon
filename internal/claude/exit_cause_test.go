package claude

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// exitCause must stay silent for an ordinary non-zero exit and loud for a transport failure: the
// difference is whether the CLI ever ran.
func TestExitCauseNamesOnlyTransportFailures(t *testing.T) {
	if got := exitCause(nil); got != "" {
		t.Errorf("exitCause(nil) = %q, want \"\"", got)
	}
	// A non-zero exit IS the child running; the code is already in the message.
	var exitErr *exec.ExitError
	if got := exitCause(&exec.ExitError{}); got != "" {
		t.Errorf("exitCause(*exec.ExitError) = %q, want \"\" — the exit code already carries it", got)
	}
	_ = exitErr
	if got := exitCause(errors.New("runtime stdio: stdio argv[0] not allowlisted: claude.exe")); !strings.Contains(got, "not allowlisted") {
		t.Errorf("exitCause(transport) = %q, want it to carry the cause", got)
	}
}

// waitErrProc is a ProcSession that dies with a TRANSPORT error and emits nothing at all: exactly the
// shape of the reported failure, where the supervisor refused the dispatch and no child existed. It is
// a separate type rather than a change to fakeProc so no existing test changes behaviour.
type waitErrProc struct {
	err error
}

func (p *waitErrProc) WriteTurn([]byte) error { return nil }
func (p *waitErrProc) Lines() <-chan []byte {
	ch := make(chan []byte)
	close(ch) // stdout closed → the session loop's "child exited" branch
	return ch
}
func (p *waitErrProc) Stderr() <-chan []byte { return make(chan []byte) }
func (p *waitErrProc) Signal(string) error   { return nil }
func (p *waitErrProc) Wait() (int, error)    { return 1, p.err }
func (p *waitErrProc) Close() error          { return nil }

// THE REPORTED MESSAGE, FIXED. The cause was in hand at every layer — the supervisor emitted it, the
// proc stored it — and the operator was shown "exited without a terminal result message (exit code 1);
// the CLI emitted no stderr at all before exiting". "The CLI emitted no stderr" is what sent the
// investigation to mounts and credentials; the CLI had never started.
func TestTransportErrorSurvivesIntoTheFailureMessage(t *testing.T) {
	cause := errors.New("runtime stdio: stdio argv[0] not allowlisted: C:\\Users\\justi\\claude.exe")
	h := newHarness(t, func() *fakeProc { return newFakeProc() })
	h.b.SetSpawnOverride(func(context.Context, procSpec) (ProcSession, error) {
		return &waitErrProc{err: cause}, nil
	})

	manifest := scheduler.ExecutionManifest{
		ExecutionID: "exec-wait-err",
		Goal:        "do the thing",
		ModelRef:    "claude/anthropic/claude-sonnet-5",
		ProjectDir:  t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := h.b.Start(ctx, db.ExecutionRow{ID: "exec-wait-err", TenantID: "t1"}, manifest, h.rec)
	if err == nil {
		t.Fatal("Start returned nil for a session whose transport failed — the failure was swallowed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not allowlisted") {
		t.Fatalf("failure message does not carry the transport cause:\n  got: %s\n  the supervisor's "+
			"refusal reached the proc and was dropped here, so the operator is told the CLI was silent "+
			"instead of that it never started", msg)
	}
	if !strings.Contains(msg, "transport:") {
		t.Errorf("failure message does not LABEL the cause as a transport failure: %s", msg)
	}
	// The misleading clause may still appear (there genuinely was no stderr) but only AFTER the real
	// cause, so it can no longer be read on its own.
	if i, j := strings.Index(msg, "not allowlisted"), strings.Index(msg, "no stderr at all"); j >= 0 && i > j {
		t.Errorf("the stderr clause precedes the cause — a reader stops at \"no stderr\" and blames the CLI: %s", msg)
	}
}
