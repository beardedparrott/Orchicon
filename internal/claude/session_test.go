package claude

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// fakeProc is an in-memory ProcSession. It proves the long-lived streaming
// session end to end with ZERO real Anthropic spend: the test owns the
// stdout line channel and drives canned fixtures.
type fakeProc struct {
	lines  chan []byte
	stderr chan []byte
	done   chan struct{}
	once   sync.Once

	mu     sync.Mutex
	turns  [][]byte
	sigs   []string
	closed bool
}

func newFakeProc() *fakeProc {
	return &fakeProc{
		lines:  make(chan []byte, 64),
		stderr: make(chan []byte, 8),
		done:   make(chan struct{}),
	}
}

func (p *fakeProc) WriteTurn(b []byte) error {
	p.mu.Lock()
	p.turns = append(p.turns, append([]byte(nil), b...))
	p.mu.Unlock()
	return nil
}
func (p *fakeProc) Lines() <-chan []byte  { return p.lines }
func (p *fakeProc) Stderr() <-chan []byte { return p.stderr }
func (p *fakeProc) Signal(sig string) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	p.mu.Unlock()
	return nil
}
func (p *fakeProc) Wait() (int, error) { <-p.done; return 0, nil }
func (p *fakeProc) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.done)
	})
	return nil
}

func (p *fakeProc) push(line string) { p.lines <- []byte(line) }
func (p *fakeProc) end()             { close(p.lines) }
func (p *fakeProc) turnCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.turns)
}

// turnsSnapshot returns a copy of every frame written to the child's stdin, as
// strings. The Ask tests need to inspect the exact bytes of a control_response
// (its nesting is the contract), not just count them.
func (p *fakeProc) turnsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.turns))
	for _, t := range p.turns {
		out = append(out, string(t))
	}
	return out
}
func (p *fakeProc) signals() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sigs...)
}
func (p *fakeProc) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// recordingCallbacks records every ExecutionCallbacks invocation in order.
type recordingCallbacks struct {
	mu     sync.Mutex
	events []string
	result struct {
		succeeded bool
		output    string
		errMsg    string
	}
	files []string
}

func (r *recordingCallbacks) note(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}
func (r *recordingCallbacks) OnStarted(ctx context.Context, execID string) { r.note("started") }
func (r *recordingCallbacks) OnText(ctx context.Context, execID, text string) {
	r.note("text:" + text)
}
func (r *recordingCallbacks) OnToolCall(ctx context.Context, execID, toolName string, input, output []byte) {
	r.note("tool:" + toolName)
}
func (r *recordingCallbacks) OnWrittenFiles(ctx context.Context, execID string, files []string) {
	r.mu.Lock()
	r.files = append(r.files, files...)
	r.mu.Unlock()
	r.note("files:" + strings.Join(files, ","))
}
func (r *recordingCallbacks) OnHealth(ctx context.Context, execID, healthState string) {
	r.note("health:" + healthState)
}
func (r *recordingCallbacks) OnStall(ctx context.Context, execID, reason string, fatal bool) {
	r.note("stall")
}
func (r *recordingCallbacks) OnRecovered(ctx context.Context, execID, recovered string) {
	r.note("recovered")
}
func (r *recordingCallbacks) OnArtifact(ctx context.Context, execID, name, artifactType, content string) {
	r.note("artifact")
}
func (r *recordingCallbacks) OnResult(ctx context.Context, execID string, succeeded bool, output, errorMessage string) {
	r.mu.Lock()
	r.result.succeeded = succeeded
	r.result.output = output
	r.result.errMsg = errorMessage
	r.mu.Unlock()
	r.note("result")
}
func (r *recordingCallbacks) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness wires a Bridge whose spawn factory returns the given fake and
// captures the argv it was spawned with.
type harness struct {
	b    *Bridge
	argv []string
	rec  *recordingCallbacks
}

func newHarness(t *testing.T, factory func() *fakeProc) *harness {
	t.Helper()
	h := &harness{rec: &recordingCallbacks{}}
	h.b = New(quietLogger())
	h.b.SetSpawnOverride(func(ctx context.Context, spec procSpec) (ProcSession, error) {
		h.argv = spec.Argv
		return factory(), nil
	})
	return h
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

const (
	initLine  = `{"type":"system","subtype":"init","session_id":"sess-1"}`
	deltaOne  = `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`
	deltaTwo  = `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}}}`
	toolLine  = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"/w/a.go"}}]}}`
	toolRes   = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"written","is_error":false}]}}`
	resultOne = `{"type":"result","subtype":"success","session_id":"sess-1","result":"ok","total_cost_usd":0.02,"usage":{"input_tokens":5,"output_tokens":7}}`
)

// TestStartLongLivedSession proves the core acceptance: ONE spawn, callbacks
// in order, a turn boundary on `result` (NOT process exit), a mid-run
// injected turn on the SAME stdin, and a clean terminal OnResult.
func TestStartLongLivedSession(t *testing.T) {
	fp := newFakeProc()
	h := newHarness(t, func() *fakeProc { return fp })

	manifest := scheduler.ExecutionManifest{
		ExecutionID: "exec-1",
		Goal:        "do the thing",
		ModelRef:    "claude/anthropic/claude-sonnet-5",
		ProjectDir:  t.TempDir(),
	}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-1", TenantID: "t1"}, manifest, h.rec)
	}()

	waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn write")
	// argv: exactly the stream-json shape, model bound at start, never bare.
	joined := strings.Join(h.argv, " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--verbose", "--include-partial-messages", "--model claude-sonnet-5"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--bare") {
		t.Fatalf("argv must never be bare (bare drops the operator OAuth): %q", joined)
	}

	fp.push(initLine)
	fp.push(deltaOne)
	fp.push(deltaTwo)
	fp.push(toolLine)
	fp.push(toolRes)

	// A mid-run injected message must land on the SAME live stdin and keep
	// the session alive past the turn boundary.
	waitFor(t, func() bool { return h.b.IsExecutionActive("exec-1") }, "session live")
	if err := h.b.SendExecutionMessage(context.Background(), "exec-1", "keep going"); err != nil {
		t.Fatalf("SendExecutionMessage: %v", err)
	}
	waitFor(t, func() bool { return fp.turnCount() == 2 }, "second turn write")

	fp.push(resultOne)
	// The turn boundary is `result`, NOT process exit: the subprocess is
	// still alive and the injected turn keeps the session running.
	waitFor(t, func() bool { return h.b.IsExecutionActive("exec-1") }, "session still live after turn 1")
	if fp.isClosed() {
		t.Fatal("subprocess was closed at the turn boundary (turn boundary must not be process exit)")
	}

	// Turn 2 completes the execution.
	fp.push(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" done"}}}`)
	fp.push(resultOne)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after the terminal result")
	}

	ev := h.rec.snapshot()
	if len(ev) == 0 || ev[0] != "started" {
		t.Fatalf("first callback = %v, want started", ev)
	}
	if last := ev[len(ev)-1]; last != "result" {
		t.Fatalf("last callback = %q, want result (all: %v)", last, ev)
	}
	// OnText/OnToolCall/OnWrittenFiles all fired, in the required ORDER:
	// the file list follows the tool call that produced it.
	idx := func(want string) int {
		for i, e := range ev {
			if e == want {
				return i
			}
		}
		return -1
	}
	// The mapper canonicalizes claude's tool vocabulary onto opencode's
	// (`Write` → `write`), so the execution view renders identically.
	tool := idx("tool:write")
	files := idx("files:/w/a.go")
	text := idx("text:hello ")
	if tool < 0 || files < 0 || text < 0 {
		t.Fatalf("missing callbacks (text=%d tool=%d files=%d): %v", text, tool, files, ev)
	}
	if !(text < tool && tool < files && files < len(ev)-1) {
		t.Fatalf("callback order wrong: %v", ev)
	}
	if !h.rec.result.succeeded {
		t.Fatal("OnResult succeeded = false, want true")
	}
	if !strings.Contains(h.rec.result.output, "hello world") {
		t.Fatalf("OnResult output = %q", h.rec.result.output)
	}
	if len(h.rec.files) != 1 || h.rec.files[0] != "/w/a.go" {
		t.Fatalf("OnWrittenFiles = %v", h.rec.files)
	}
}

// TestStartFailureCapturesSessionAndResumes proves the fail→capture→resume
// round trip: an exit WITHOUT a terminal result is a failure, the captured
// session id is persisted, and the retry is spawned with --resume.
func TestStartFailureCapturesSessionAndResumes(t *testing.T) {
	fp1 := newFakeProc()
	var stored []db.SessionPart
	h := newHarness(t, func() *fakeProc { return fp1 })
	h.b.SetSessionStore(func(ctx context.Context, execID, tenantID string, parts []db.SessionPart) error {
		stored = append(stored, parts...)
		return nil
	})

	manifest := scheduler.ExecutionManifest{ExecutionID: "exec-f", ModelRef: "claude/anthropic/claude-sonnet-5", ProjectDir: t.TempDir()}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-f", TenantID: "t1"}, manifest, h.rec)
	}()
	waitFor(t, func() bool { return fp1.turnCount() == 1 }, "initial turn")
	fp1.push(initLine)
	waitFor(t, func() bool { return h.b.sessionOwner("sess-1") == "exec-f" }, "session id captured")
	fp1.end() // process exits with NO terminal result

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start returned nil for a session that exited without a terminal result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after process exit")
	}

	// The session identity was persisted for the resume.
	var found bool
	for _, p := range stored {
		if p.Kind == db.SessionPartSessionInfo && strings.Contains(string(p.Payload), `"sess-1"`) && strings.Contains(string(p.Payload), `"adapter_kind":"claude"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("session_info part not persisted: %+v", stored)
	}

	// Resume: a second Start resumes the captured session id.
	fp2 := newFakeProc()
	h2 := newHarness(t, func() *fakeProc { return fp2 })
	manifest2 := scheduler.ExecutionManifest{
		ExecutionID:           "exec-f2",
		ModelRef:              "claude/anthropic/claude-sonnet-5",
		ProjectDir:            t.TempDir(),
		SequenceContinue:      true,
		ContinueFromSessionID: "sess-1",
	}
	done2 := make(chan error, 1)
	go func() {
		done2 <- h2.b.Start(context.Background(), db.ExecutionRow{ID: "exec-f2", TenantID: "t1"}, manifest2, h2.rec)
	}()
	waitFor(t, func() bool { return fp2.turnCount() == 1 }, "resumed turn")
	joined := strings.Join(h2.argv, " ")
	if !strings.Contains(joined, "--resume sess-1") {
		t.Fatalf("resume argv = %q, want --resume sess-1", joined)
	}
	if !strings.Contains(joined, "--model claude-sonnet-5") {
		t.Fatalf("resume argv lost the model: %q", joined)
	}
	fp2.push(initLine)
	fp2.push(resultOne)
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("resumed Start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resumed Start did not return")
	}
}

// TestSecondSpawnForLiveSessionRefused proves the single-writer invariant.
func TestSecondSpawnForLiveSessionRefused(t *testing.T) {
	fp := newFakeProc()
	h := newHarness(t, func() *fakeProc { return fp })
	manifest := scheduler.ExecutionManifest{ExecutionID: "exec-a", ModelRef: "claude/anthropic/x", ProjectDir: t.TempDir()}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-a", TenantID: "t1"}, manifest, h.rec)
	}()
	waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn")
	fp.push(initLine)
	waitFor(t, func() bool { return h.b.sessionOwner("sess-1") == "exec-a" }, "session bound")

	// A second spawn targeting the same live session id is refused.
	err := h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-b", TenantID: "t1"}, scheduler.ExecutionManifest{
		ExecutionID:           "exec-b",
		ModelRef:              "claude/anthropic/x",
		ProjectDir:            t.TempDir(),
		SequenceContinue:      true,
		ContinueFromSessionID: "sess-1",
	}, &recordingCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "second writer") {
		t.Fatalf("second spawn for a live session id = %v, want refusal", err)
	}
	// A second spawn for the SAME execution id is also refused.
	if err := h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-a"}, manifest, &recordingCallbacks{}); err == nil {
		t.Fatal("second spawn for a live execution id should be refused")
	}

	fp.push(resultOne)
	<-done
}

// TestAbortSignalsAndNoop proves Aborter: SIGINT first, unknown exec no-op.
func TestAbortSignalsAndNoop(t *testing.T) {
	fp := newFakeProc()
	h := newHarness(t, func() *fakeProc { return fp })
	manifest := scheduler.ExecutionManifest{ExecutionID: "exec-ab", ModelRef: "claude/anthropic/x", ProjectDir: t.TempDir()}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: "exec-ab", TenantID: "t1"}, manifest, h.rec)
	}()
	waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn")
	fp.push(initLine)
	waitFor(t, func() bool { return h.b.IsExecutionActive("exec-ab") }, "session live")

	// Unknown execution: a safe no-op, never an error.
	if err := h.b.AbortExecution(context.Background(), "nope", "cancel"); err != nil {
		t.Fatalf("abort of unknown execution = %v, want nil", err)
	}
	if err := h.b.AbortExecution(context.Background(), "exec-ab", "cancel"); err != nil {
		t.Fatalf("abort = %v", err)
	}
	sigs := fp.signals()
	if len(sigs) == 0 || sigs[0] != "INT" {
		t.Fatalf("signals = %v, want INT first", sigs)
	}
	// An unfinished turn (no result) stays resumable: the session end is a
	// failure, and the session id is still resumable.
	fp.end()
	if err := <-done; err == nil {
		t.Fatal("aborted session with no terminal result should fail")
	}
}
