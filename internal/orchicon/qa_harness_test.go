package orchicon

// QA test harness: scripted mock provider + recording callbacks + mock
// tool registry, shared by the session-engine loop tests. All loop tests
// run against this mock — no live API in default tests.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// scriptedTurn is one provider turn: events, finish reason, usage, and an
// optional mid-stream error.
type scriptedTurn struct {
	events    []Event
	finish    StopReason
	usage     Usage
	streamErr error
	// bare opts out of the harness's automatic decision-marker delivery:
	// the scripted turn ends exactly as scripted (no synthesized
	// ORCHICON WORKER SUMMARY text). Tests of the decision-signal gate
	// set bare to script marker-less turns.
	bare bool
	// markerSuffix overrides the auto-delivered marker body (StopStop
	// turns only, bare=false). Empty → the harness default sign-off.
	markerSuffix string
}

// markerText is the canonical scripted sign-off for a finished worker turn
// (mirrors what a conforming worker model emits before ending a session).
func markerText(body string) string {
	return "\nORCHICON WORKER SUMMARY: success — " + body
}

// mockProvider pops scripted turns per StreamTurn call and records
// requests for assertions.
type mockProvider struct {
	mu                 sync.Mutex
	turns              []scriptedTurn
	preStreamErrOnCall int // 1-based: Nth StreamTurn returns a pre-stream error
	requests           []TurnRequest
	lastStream         *mockStream
}

func (m *mockProvider) StreamTurn(ctx context.Context, req TurnRequest) (TurnStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.preStreamErrOnCall > 0 && len(m.requests) == m.preStreamErrOnCall {
		return nil, fmt.Errorf("mock pre-stream failure")
	}
	if len(m.turns) == 0 {
		return nil, fmt.Errorf("mock provider exhausted scripted turns")
	}
	t := m.turns[0]
	m.turns = m.turns[1:]
	st := &mockStream{events: t.events, finish: t.finish, usage: t.usage, streamErr: t.streamErr, bare: t.bare, markerSuffix: t.markerSuffix}
	m.lastStream = st
	return st, nil
}

func (m *mockProvider) ListModels(ctx context.Context) ([]ModelInfo, error) { return nil, nil }
func (m *mockProvider) Capabilities() Capabilities                          { return Capabilities{Streaming: true, Tools: true} }

func (m *mockProvider) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockProvider) lastRequest() TurnRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.requests) == 0 {
		return TurnRequest{}
	}
	return m.requests[len(m.requests)-1]
}

// mockStream is an in-memory TurnStream.
type mockStream struct {
	events       []Event
	finish       StopReason
	usage        Usage
	streamErr    error
	bare         bool
	markerSuffix string
	markerDone   bool
	i            int
}

func (s *mockStream) Next(ctx context.Context) (Event, bool, error) {
	if s.streamErr != nil && s.i == len(s.events) {
		err := s.streamErr
		s.streamErr = nil
		return StreamError{Err: err}, true, nil
	}
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	if s.i == len(s.events) {
		// Decision-signal parity: a scripted turn that ends StopStop
		// without bare delivers the ORCHICON WORKER SUMMARY sign-off as
		// its final text — the default shape of a legitimately-finished
		// worker turn. Gate tests opt out via bare / markerSuffix.
		if s.finish == StopStop && !s.bare && !s.markerDone {
			s.markerDone = true
			body := s.markerSuffix
			if body == "" {
				body = "task completed"
			}
			return TextDelta{Text: markerText(body)}, true, nil
		}
		s.i++
		return Finish{StopReason: s.finish, Usage: s.usage}, true, nil
	}
	return nil, false, nil
}

func (s *mockStream) Close() error { return nil }

// --- recording callbacks ---------------------------------------------------

type resultCall struct {
	succeeded bool
	output    string
	errMsg    string
}

type recordedCallback struct {
	mu         sync.Mutex
	started    int
	text       []string
	toolCalls  []string
	written    []string
	artifacts  []string
	stalls     []string
	stallFatal []bool
	results    []resultCall
}

func (r *recordedCallback) OnStarted(ctx context.Context, execID string) {
	r.mu.Lock()
	r.started++
	r.mu.Unlock()
}
func (r *recordedCallback) OnText(ctx context.Context, execID string, text string) {
	r.mu.Lock()
	r.text = append(r.text, text)
	r.mu.Unlock()
}
func (r *recordedCallback) OnToolCall(ctx context.Context, execID, toolName string, input, output []byte) {
	r.mu.Lock()
	r.toolCalls = append(r.toolCalls, fmt.Sprintf("%s(%s)", toolName, string(input)))
	r.mu.Unlock()
}
func (r *recordedCallback) OnWrittenFiles(ctx context.Context, execID string, files []string) {
	r.mu.Lock()
	r.written = append(r.written, files...)
	r.mu.Unlock()
}
func (r *recordedCallback) OnArtifact(ctx context.Context, execID, name, artifactType, content string) {
	r.mu.Lock()
	r.artifacts = append(r.artifacts, name+":"+artifactType)
	r.mu.Unlock()
}
func (r *recordedCallback) OnHealth(ctx context.Context, execID, healthState string) {}
func (r *recordedCallback) OnStall(ctx context.Context, execID, reason string, fatal bool) {
	r.mu.Lock()
	r.stalls = append(r.stalls, reason)
	r.stallFatal = append(r.stallFatal, fatal)
	r.mu.Unlock()
}
func (r *recordedCallback) OnRecovered(ctx context.Context, execID, recovered string) {}
func (r *recordedCallback) OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string) {
	r.mu.Lock()
	r.results = append(r.results, resultCall{succeeded: succeeded, output: output, errMsg: errorMessage})
	r.mu.Unlock()
}

// --- test harness ----------------------------------------------------------

func testExecRow(id string) db.ExecutionRow {
	return db.ExecutionRow{
		ID:         id,
		TenantID:   "tnt_test",
		ProjectID:  "proj_test",
		TaskID:     "task_test",
		WorkerID:   "worker_test",
		WorkerName: "qa-worker",
	}
}

func testManifest(modelRef string) scheduler.ExecutionManifest {
	return scheduler.ExecutionManifest{
		ExecutionID:        "exec_test",
		TaskID:             "task_test",
		ProjectID:          "proj_test",
		WorkerID:           "worker_test",
		SystemPrompt:       "You are the QA test worker.",
		Goal:               "Write a test.",
		AcceptanceCriteria: "Tests pass.",
		ModelRef:           modelRef,
	}
}

func newQATestSession(t interface{ Helper() }, prov Provider, tools ToolRegistry) (*Session, string) {
	dir := os.TempDir() + "/orchicon-qa-" + fmt.Sprintf("%d", os.Getpid())
	_ = os.MkdirAll(dir, 0o755)
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_test"),
		Manifest:   testManifest("orchicon/mockprov/deepseek-v4-flash"),
		ProjectDir: dir,
		Provider:   prov,
		Tools:      tools,
		Log:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		panic(fmt.Sprintf("NewSession: %v", err))
	}
	return s, dir
}

// --- mock tool registry ----------------------------------------------------

type mockTools struct {
	mu       sync.Mutex
	results  map[string]string
	errs     map[string]string
	panics   map[string]bool
	executed []string
	defs     []ToolDef
}

func newMockTools() *mockTools {
	return &mockTools{
		results: map[string]string{},
		errs:    map[string]string{},
		panics:  map[string]bool{},
		defs: []ToolDef{
			{Name: "read", Description: "read a file", ParamsJSON: `{"type":"object","properties":{"path":{"type":"string"}}}`},
			{Name: "write", Description: "write a file", ParamsJSON: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`},
			{Name: "edit", Description: "edit a file", ParamsJSON: `{"type":"object","properties":{"path":{"type":"string"}}}`},
			{Name: "noop", Description: "noop", ParamsJSON: `{}`},
		},
	}
}

func (m *mockTools) Defs() []ToolDef { return m.defs }

func (m *mockTools) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	m.mu.Lock()
	m.executed = append(m.executed, name+" "+argsJSON)
	defer m.mu.Unlock()
	if m.panics[name] {
		panic(fmt.Sprintf("tool %s panicked", name))
	}
	if e, ok := m.errs[name]; ok {
		return "", fmt.Errorf("%s", e)
	}
	if r, ok := m.results[name]; ok {
		return r, nil
	}
	return fmt.Sprintf("result for %s", name), nil
}

func (m *mockTools) executedCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.executed...)
}

// snapshot returns the recorded callback state.
func (r *recordedCallback) snapshot() (started int, text []string, toolCalls []string, written []string, stalls []string, stallFatal []bool, results []resultCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, append([]string(nil), r.text...), append([]string(nil), r.toolCalls...),
		append([]string(nil), r.written...), append([]string(nil), r.stalls...),
		append([]bool(nil), r.stallFatal...), append([]resultCall(nil), r.results...)
}
