package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// THE DISPATCH BUG, pinned. The daemon leases runtime containers by RUN id
// (daemonPool.containerForRun), so the stdio transport must name the RUN. Passing
// the EXECUTION id finds no container — and because the daemon could not answer
// while the client streamed a body that never ends, the call blocked FOREVER:
// observed live as an execution stuck in `dispatching` with zero tokens, no
// container child, and not one log line.
func TestWorkerSpawnSpecNamesTheRunNotTheExecution(t *testing.T) {
	var got procSpec
	h := newHarness(t, func() *fakeProc { return newFakeProc() })
	h.b.SetSpawnOverride(func(_ context.Context, spec procSpec) (ProcSession, error) {
		got = spec
		return newFakeProc(), nil
	})

	const (
		execID = "exec-spec-1"
		runID  = "run-spec-1"
	)
	manifest := scheduler.ExecutionManifest{
		ExecutionID:       execID,
		Goal:              "do the thing",
		ModelRef:          "claude/anthropic/claude-sonnet-5",
		ProjectDir:        t.TempDir(),
		RuntimeWorkflowID: runID,
	}
	// A cancelable ctx: the session loop runs until a terminal result, and this
	// fake proc never sends one, so the test must end it rather than wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(ctx, db.ExecutionRow{ID: execID, TenantID: "t1"}, manifest, h.rec)
	}()
	waitFor(t, func() bool { return got.WorkflowRunID != "" || got.ExecID != "" }, "the spawn spec to be captured")
	cancel() // end the session loop
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Not fatal: the assertion below is about the SPEC, and a session that
		// outlives the cancel is a different concern (covered elsewhere).
		t.Log("note: Start did not return after cancel; asserting on the captured spec anyway")
	}

	if got.WorkflowRunID != runID {
		t.Fatalf("spawn spec WorkflowRunID = %q, want the RUN id %q — the daemon leases by run id, "+
			"and naming the execution id finds no container", got.WorkflowRunID, runID)
	}
	if got.WorkflowRunID == got.ExecID {
		t.Fatalf("spawn spec names the EXECUTION id (%q) as the run — this is the bug that hung dispatch", got.ExecID)
	}
	if got.ExecID != execID {
		t.Errorf("spawn spec ExecID = %q, want %q", got.ExecID, execID)
	}
}

// recordingOpener captures the id the container transport NAMES, and returns a
// session that immediately ends — so the argument is what is under test, not the
// proxy.
type recordingOpener struct {
	gotWorkflowID string
	gotRequest    runtime.StdioRequest
	err           error
}

func (r *recordingOpener) Stdio(_ context.Context, workflowID string, req runtime.StdioRequest) (*runtime.StdioSession, error) {
	r.gotWorkflowID = workflowID
	r.gotRequest = req
	if r.err != nil {
		return nil, r.err
	}
	return nil, errors.New("stop here: the argument is the assertion")
}

// THE ARGUMENT PIN. A spec can be right while the CALL names the wrong field, and
// that is precisely what shipped: the spec carried the execution id and so did the
// request, so the daemon found no leased container and the dispatch hung forever.
// This asserts the id handed to Stdio is the RUN.
func TestContainerSpawnNamesTheRunIDToStdio(t *testing.T) {
	op := &recordingOpener{}
	_, err := newContainerProc(context.Background(), op, procSpec{
		ExecID:        "exec-abc",
		Argv:          []string{"claude", "-p"},
		Cwd:           t.TempDir(),
		ProjectDir:    t.TempDir(),
		WorkflowRunID: "run-xyz",
	})
	if err == nil {
		t.Fatal("expected the recording opener's sentinel error")
	}
	if op.gotWorkflowID != "run-xyz" {
		t.Fatalf("Stdio was called with workflowID=%q, want the RUN id %q — naming the execution id finds no leased container and hangs the dispatch",
			op.gotWorkflowID, "run-xyz")
	}
	if op.gotWorkflowID == op.gotRequest.ExecID {
		t.Fatalf("Stdio named the EXECUTION id as the container: %q", op.gotWorkflowID)
	}
	if op.gotRequest.ExecID != "exec-abc" {
		t.Errorf("the request's ExecID = %q, want exec-abc (the daemon tracks the child by it)", op.gotRequest.ExecID)
	}
}

// A container spawn with no run id must FAIL LOUDLY rather than send a request
// that cannot resolve a container. The local transport legitimately has no run id
// (it spawns on the host), which is why the check lives at the container call and
// not in the shared spec builder.
func TestContainerSpawnRequiresARunID(t *testing.T) {
	_, err := newContainerProc(context.Background(), &recordingOpener{}, procSpec{
		ExecID:        "exec-no-run",
		Argv:          []string{"claude", "-p"},
		Cwd:           t.TempDir(),
		ProjectDir:    t.TempDir(),
		WorkflowRunID: "",
	})
	if err == nil {
		t.Fatal("a container spawn with no run id was attempted; the daemon would find no container and (before the handshake bound) hang forever")
	}
	if !strings.Contains(err.Error(), "run id") {
		t.Errorf("the error does not name the missing run id: %v", err)
	}
}
