package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// ------------------------------------------------------------------ recorder

// capturedSnapshot is the data half of captureCallbacks. It carries NO lock,
// so a consistent copy of it can be handed to a test goroutine that shares
// the recorder with a live session goroutine (see snap).
type capturedSnapshot struct {
	order     []string
	texts     []string
	toolCalls []capturedTool
	files     [][]string
	artifacts []capturedArtifact
	health    []string
	stalls    []capturedStall
	recovered []string
	results   []capturedResult

	parts  []capturedPart
	ledger []capturedLedger
	usage  []scheduler.UsageRecord
}

// captureCallbacks records every ExecutionCallbacks invocation with enough
// detail to assert the parity matrix. It is the shared recorder for
// parse_test.go and parity_matrix_test.go.
type captureCallbacks struct {
	mu sync.Mutex
	capturedSnapshot
}

type capturedTool struct {
	name   string
	input  string
	output string
}
type capturedArtifact struct{ name, typ, content string }
type capturedStall struct {
	reason string
	fatal  bool
}
type capturedResult struct {
	succeeded bool
	output    string
	errMsg    string
}
type capturedPart struct {
	kind    string
	payload map[string]any
}
type capturedLedger struct {
	tool  string
	input map[string]any
}

func (c *captureCallbacks) note(s string) {
	c.mu.Lock()
	c.order = append(c.order, s)
	c.mu.Unlock()
}

func (c *captureCallbacks) OnStarted(context.Context, string) { c.note("started") }

func (c *captureCallbacks) OnText(_ context.Context, _, text string) {
	c.mu.Lock()
	c.texts = append(c.texts, text)
	c.mu.Unlock()
	c.note("text")
}

func (c *captureCallbacks) OnToolCall(_ context.Context, _, toolName string, in, out []byte) {
	c.mu.Lock()
	c.toolCalls = append(c.toolCalls, capturedTool{name: toolName, input: string(in), output: string(out)})
	c.mu.Unlock()
	c.note("tool")
}

func (c *captureCallbacks) OnWrittenFiles(_ context.Context, _ string, files []string) {
	c.mu.Lock()
	c.files = append(c.files, append([]string(nil), files...))
	c.mu.Unlock()
	c.note("files")
}

func (c *captureCallbacks) OnHealth(_ context.Context, _, state string) {
	c.mu.Lock()
	c.health = append(c.health, state)
	c.mu.Unlock()
	c.note("health")
}

func (c *captureCallbacks) OnStall(_ context.Context, _, reason string, fatal bool) {
	c.mu.Lock()
	c.stalls = append(c.stalls, capturedStall{reason: reason, fatal: fatal})
	c.mu.Unlock()
	c.note("stall")
}

func (c *captureCallbacks) OnRecovered(_ context.Context, _, recovered string) {
	c.mu.Lock()
	c.recovered = append(c.recovered, recovered)
	c.mu.Unlock()
	c.note("recovered")
}

func (c *captureCallbacks) OnArtifact(_ context.Context, _, name, artifactType, content string) {
	c.mu.Lock()
	c.artifacts = append(c.artifacts, capturedArtifact{name: name, typ: artifactType, content: content})
	c.mu.Unlock()
	c.note("artifact")
}

func (c *captureCallbacks) OnResult(_ context.Context, _ string, succeeded bool, output, errMsg string) {
	c.mu.Lock()
	c.results = append(c.results, capturedResult{succeeded: succeeded, output: output, errMsg: errMsg})
	c.mu.Unlock()
	c.note("result")
}

// recordPart is the durable-transcript sink handed to MapperDeps.
func (c *captureCallbacks) recordPart(_ context.Context, kind string, payload map[string]any) {
	c.mu.Lock()
	c.parts = append(c.parts, capturedPart{kind: kind, payload: payload})
	c.mu.Unlock()
}

// fileEdit is the ledger sink handed to MapperDeps.
func (c *captureCallbacks) fileEdit(_ context.Context, _, _, _ string, toolName string, input map[string]any, _ string) {
	c.mu.Lock()
	c.ledger = append(c.ledger, capturedLedger{tool: toolName, input: input})
	c.mu.Unlock()
}

func (c *captureCallbacks) recordUsage(_ context.Context, in scheduler.UsageRecord) error {
	c.mu.Lock()
	c.usage = append(c.usage, in)
	c.mu.Unlock()
	return nil
}

// recordParts is the scheduler.SessionStoreFunc sink: it decodes each
// persisted part's marshaled payload back into a map so a test can assert
// the durable envelope EXACTLY as it lands in the transcript table.
func (c *captureCallbacks) recordParts(_ context.Context, _, _ string, parts []db.SessionPart) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range parts {
		var payload map[string]any
		_ = json.Unmarshal(p.Payload, &payload)
		c.parts = append(c.parts, capturedPart{kind: p.Kind, payload: payload})
	}
	return nil
}

// snap returns a consistent COPY of everything recorded so far. A test that
// shares the recorder with a live session goroutine MUST read through this
// (or through the locked accessors below) instead of touching the fields
// directly — the callbacks append under c.mu from that other goroutine.
func (c *captureCallbacks) snap() capturedSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.capturedSnapshot
	cp.order = append([]string(nil), c.order...)
	cp.texts = append([]string(nil), c.texts...)
	cp.toolCalls = append([]capturedTool(nil), c.toolCalls...)
	cp.files = append([][]string(nil), c.files...)
	cp.artifacts = append([]capturedArtifact(nil), c.artifacts...)
	cp.health = append([]string(nil), c.health...)
	cp.stalls = append([]capturedStall(nil), c.stalls...)
	cp.recovered = append([]string(nil), c.recovered...)
	cp.results = append([]capturedResult(nil), c.results...)
	cp.parts = append([]capturedPart(nil), c.parts...)
	cp.ledger = append([]capturedLedger(nil), c.ledger...)
	cp.usage = append([]scheduler.UsageRecord(nil), c.usage...)
	return cp
}

func (c *captureCallbacks) stallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stalls)
}

func (c *captureCallbacks) toolCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.toolCalls)
}

func (c *captureCallbacks) stallsSnapshot() []capturedStall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedStall(nil), c.stalls...)
}

func (c *captureCallbacks) recoveredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recovered)
}

func (c *captureCallbacks) recoveredSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.recovered...)
}

func (s *capturedSnapshot) filesFlat() []string {
	var out []string
	for _, f := range s.files {
		out = append(out, f...)
	}
	return out
}

func (s *capturedSnapshot) partsOfKind(kind string) []capturedPart {
	var out []capturedPart
	for _, p := range s.parts {
		if p.kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func (s *capturedSnapshot) hasOrder(want string) bool {
	for _, o := range s.order {
		if o == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ harness

// newTestMapper wires a Mapper over a captureCallbacks recorder with a
// temp-dir exec dir and optional manifest knobs.
func newTestMapper(t *testing.T, manifest scheduler.ExecutionManifest) (*Mapper, *captureCallbacks) {
	t.Helper()
	if manifest.ProjectDir == "" {
		manifest.ProjectDir = t.TempDir()
	}
	rec := &captureCallbacks{}
	m := NewMapper("exec-t", rec, MapperDeps{
		TenantID:      "t1",
		Model:         "claude-sonnet-5",
		ExecDir:       manifest.ProjectDir,
		Manifest:      manifest,
		FileEdits:     rec.fileEdit,
		UsageRecorder: rec.recordUsage,
		BindSession:   func(context.Context, string) {},
		Record:        rec.recordPart,
		Log:           quietLogger(),
	})
	return m, rec
}

// feed parses canned JSONL lines into the mapper and returns the terminal flag.
func feed(t *testing.T, m *Mapper, lines ...string) bool {
	t.Helper()
	terminal := false
	for _, l := range lines {
		ev, err := ParseLine([]byte(l))
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", l, err)
		}
		if m.Handle(context.Background(), ev) {
			terminal = true
		}
	}
	return terminal
}

// ------------------------------------------------------------------ helpers

func TestCanonicalToolName(t *testing.T) {
	cases := map[string]string{
		"TodoWrite":    "todowrite",
		"Write":        "write",
		"Edit":         "edit",
		"MultiEdit":    "edit",
		"NotebookEdit": "edit",
		"ApplyPatch":   "edit",
		"Read":         "read",
		"Bash":         "bash",
		"Grep":         "grep",
		"Glob":         "glob",
		// Identity-preserving passthroughs.
		"Task":        "Task",
		"Agent":       "Agent",
		"mcp__x__y":   "mcp__x__y",
		"SomeNewTool": "SomeNewTool",
	}
	for in, want := range cases {
		if got := canonicalToolName(in); got != want {
			t.Errorf("canonicalToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArtifactTypeFromPath(t *testing.T) {
	cases := map[string]string{
		"a.md":    "markdown",
		"a.MD":    "text", // extension match is case-sensitive, mirrors opencode
		"b.json":  "json",
		"c.yaml":  "yaml",
		"c.yml":   "yaml",
		"d.html":  "html",
		"e.csv":   "csv",
		"f.xml":   "xml",
		"g.svg":   "svg",
		"h.go":    "text",
		"no-ext":  "text",
		"dir/x.m": "text",
	}
	for in, want := range cases {
		if got := artifactTypeFromPath(in); got != want {
			t.Errorf("artifactTypeFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeTodoItemsIsDefensive(t *testing.T) {
	// claude's REPAIRED key variants (id / task_id / active_form / activeForm)
	// must be tolerated, plus an unknown status string preserved verbatim.
	input := map[string]any{
		"todos": []any{
			map[string]any{"content": "step one", "status": "in_progress", "priority": "high", "activeForm": "doing one", "id": "1"},
			map[string]any{"content": "step two", "status": "", "task_id": "2", "active_form": "doing two"},
			map[string]any{"content": "step three", "status": "weird-new-state"},
			map[string]any{"content": ""}, // skipped
			"not-a-map",                   // skipped
		},
	}
	items := normalizeTodoItems(input)
	if len(items) != 3 {
		t.Fatalf("items = %+v, want 3", items)
	}
	if items[0] != (worktree.TodoItem{Content: "step one", Status: "in_progress", Priority: "high"}) {
		t.Errorf("items[0] = %+v", items[0])
	}
	if items[1].Status != "pending" {
		t.Errorf("empty status must default to pending, got %q", items[1].Status)
	}
	if items[2].Status != "weird-new-state" {
		t.Errorf("unknown status must be preserved verbatim, got %q", items[2].Status)
	}
	if got := normalizeTodoItems(nil); got != nil {
		t.Errorf("nil input = %+v, want nil", got)
	}
	if got := normalizeTodoItems(map[string]any{"todos": "nope"}); got != nil {
		t.Errorf("non-array todos = %+v, want nil", got)
	}
}

func TestMapperLedgerInputUsesTheHooksKeyAndIsExecDirRelative(t *testing.T) {
	dir := t.TempDir()
	m, _ := newTestMapper(t, scheduler.ExecutionManifest{ProjectDir: dir})

	cases := []struct {
		desc string
		in   map[string]any
		want string
	}{
		{"absolute inside the execution dir → relative", map[string]any{"file_path": filepath.Join(dir, "a.md")}, "a.md"},
		{"notebook_path key", map[string]any{"notebook_path": filepath.Join(dir, "n.ipynb")}, "n.ipynb"},
		{"nested absolute inside the execution dir", map[string]any{"file_path": filepath.Join(dir, "sub", "b.go")}, "sub/b.go"},
		{"already relative passes through", map[string]any{"file_path": "pkg/c.go"}, "pkg/c.go"},
		// Outside the execution dir: left ALONE so the ledger observer's own
		// containment check (not this helper) is what drops the row.
		{"absolute outside the execution dir", map[string]any{"file_path": "/w/a.go"}, "/w/a.go"},
		{"no path", map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := m.ledgerInput(c.in); got["filePath"] != c.want {
			t.Errorf("%s: ledgerInput(%v) filePath = %q, want %q", c.desc, c.in, got["filePath"], c.want)
		}
	}
}

// TestMapperFileEditLedgerFiresOnlyAfterTheWriteLands pins the TIMING the
// diff pipeline depends on: the ledger hook reads the file's on-disk state, so
// it must NOT fire at tool_use time (the CLI has not written the file yet —
// the observer then diffs the file against itself and silently drops the row,
// leaving the execution's file-diff pane empty) but at the tool_result, once
// the file is written. A failed call must never ledger.
func TestMapperFileEditLedgerFiresOnlyAfterTheWriteLands(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	m := NewMapper("exec-ledger", &recordingCallbacks{}, MapperDeps{
		TenantID: "t1",
		ExecDir:  dir,
		Log:      quietLogger(),
		FileEdits: func(_ context.Context, _, _, execDir, toolName string, input map[string]any, _ string) {
			p, _ := input["filePath"].(string)
			body, _ := os.ReadFile(filepath.Join(execDir, filepath.FromSlash(p)))
			calls = append(calls, toolName+"|"+p+"|"+string(body))
		},
	})

	abs := filepath.Join(dir, "a.md")
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w-1","name":"Write","input":{"file_path":"`+abs+`","content":"# hi"}}]}}`)
	if len(calls) != 0 {
		t.Fatalf("the ledger fired at tool_use time, before the CLI wrote the file: %v", calls)
	}

	// The CLI performs the write; only then does the tool_result arrive.
	if err := os.WriteFile(abs, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	feed(t, m, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w-1","content":"File created","is_error":false}]}}`)
	if len(calls) != 1 {
		t.Fatalf("ledger calls = %v, want exactly one, at completion", calls)
	}
	if calls[0] != "write|a.md|# hi" {
		t.Errorf("ledger call = %q, want canonical name + exec-dir-relative path + the WRITTEN body", calls[0])
	}

	// A FAILED edit must never create a phantom row.
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e-1","name":"Edit","input":{"file_path":"`+filepath.Join(dir, "b.md")+`","old_string":"x","new_string":"y"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"e-1","content":"String to replace not found","is_error":true}]}}`,
	)
	if len(calls) != 1 {
		t.Errorf("ledger calls = %v, want the failed edit skipped", calls)
	}
}

// ------------------------------------------------------------------ mapper

func TestMapperSystemBindsAndReportsHealthy(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, initLine, initLine) // the second must NOT re-bind or re-report

	if len(rec.health) != 1 || rec.health[0] != "healthy" {
		t.Errorf("OnHealth = %v, want exactly one healthy", rec.health)
	}
	parts := rec.partsOfKind(db.SessionPartSessionInfo)
	if len(parts) == 0 {
		t.Fatalf("no session_info part recorded: %+v", rec.parts)
	}
	body, _ := json.Marshal(parts[0].payload)
	if !strings.Contains(string(body), `"sess-1"`) || !strings.Contains(string(body), `"adapter_kind":"claude"`) {
		t.Errorf("session_info payload = %s", body)
	}
}

func TestMapperTextStreamsAndCoalescesOnePart(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	terminal := feed(t, m, deltaOne, deltaTwo, resultOne)
	if !terminal {
		t.Fatal("terminal result did not return terminal=true")
	}
	if strings.Join(rec.texts, "") != "hello world" {
		t.Errorf("OnText = %v", rec.texts)
	}
	if got := m.Output(); !strings.Contains(got, "hello world") {
		t.Errorf("Output = %q", got)
	}
	texts := rec.partsOfKind(db.SessionPartText)
	if len(texts) != 1 {
		t.Fatalf("text parts = %d, want ONE coalesced part", len(texts))
	}
	body, _ := json.Marshal(texts[0].payload)
	if !strings.Contains(string(body), "hello world") {
		t.Errorf("text part payload = %s", body)
	}
	if len(rec.usage) != 1 || rec.usage[0].AdapterKind != "claude" || rec.usage[0].PromptTokens != 5 {
		t.Errorf("usage = %+v", rec.usage)
	}
}

func TestMapperWriteToolFansOutToFilesLedgerAndArtifact(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Write","input":{"file_path":"/w/notes.md","content":"# hi"}}]}}`,
		// the ledger rides the COMPLETION (see the timing test above).
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"File created","is_error":false}]}}`,
	)

	tools := rec.toolCalls
	// TWO events, like opencode's tool_use + its resolution: the call itself
	// (input, no output) and the resolved call (output, no input).
	if len(tools) != 2 || tools[0].name != "write" || tools[1].name != "write" {
		t.Fatalf("OnToolCall = %+v, want the canonical write + its resolution", tools)
	}
	if tools[1].output != "File created" {
		t.Errorf("resolved call = %+v", tools[1])
	}
	if files := rec.filesFlat(); len(files) != 1 || files[0] != "/w/notes.md" {
		t.Errorf("OnWrittenFiles = %v", files)
	}
	if len(rec.ledger) != 1 || rec.ledger[0].tool != "write" || rec.ledger[0].input["filePath"] != "/w/notes.md" {
		t.Errorf("ledger = %+v, want write + filePath (the hook's vocabulary)", rec.ledger)
	}
	if len(rec.artifacts) != 1 || rec.artifacts[0].name != "/w/notes.md" || rec.artifacts[0].typ != "markdown" || rec.artifacts[0].content != "# hi" {
		t.Errorf("OnArtifact = %+v", rec.artifacts)
	}
	// durable parts: the tool_use carries the canonical name + running status
	// and the tool_result resolves it to completed.
	parts := rec.partsOfKind(db.SessionPartToolUse)
	if len(parts) != 2 {
		t.Fatalf("tool_use parts = %+v, want the call + its completion", parts)
	}
	body, _ := json.Marshal(parts[0].payload)
	if !strings.Contains(string(body), `"tool":"write"`) || !strings.Contains(string(body), `"status":"running"`) {
		t.Errorf("durable part = %s", body)
	}
	resolved, _ := json.Marshal(parts[1].payload)
	if !strings.Contains(string(resolved), `"status":"completed"`) {
		t.Errorf("resolved part = %s", resolved)
	}
}

func TestMapperEditFamilyAllReachWrittenFiles(t *testing.T) {
	for _, name := range []string{"Edit", "MultiEdit", "NotebookEdit", "ApplyPatch"} {
		m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
		feed(t, m,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"`+name+`","input":{"file_path":"/w/a.go","old_string":"a","new_string":"b"}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"ok","is_error":false}]}}`,
		)
		files := rec.filesFlat()
		if len(files) != 1 || files[0] != "/w/a.go" {
			t.Errorf("%s: OnWrittenFiles = %v", name, files)
		}
		if len(rec.ledger) != 1 || rec.ledger[0].tool != "edit" {
			t.Errorf("%s: ledger = %+v, want canonical edit", name, rec.ledger)
		}
		if len(rec.artifacts) != 0 {
			t.Errorf("%s: only write produces an artifact, got %+v", name, rec.artifacts)
		}
	}
}

func TestMapperTodoWritePersistsParsableEnvelopeAndSnapshot(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"td-1","name":"TodoWrite","input":{"todos":[{"content":"step 1","status":"in_progress","priority":"high","activeForm":"stepping"}]}}]}}`)

	if len(rec.toolCalls) != 1 || rec.toolCalls[0].name != "todowrite" {
		t.Fatalf("OnToolCall = %+v", rec.toolCalls)
	}
	parts := rec.partsOfKind(db.SessionPartToolUse)
	if len(parts) != 1 {
		t.Fatalf("tool_use parts = %+v", parts)
	}
	body, err := json.Marshal(parts[0].payload)
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	// THE acceptance shape: {"part":…} with part.tool=="todowrite" and
	// part.state.input.todos.
	var env struct {
		Part struct {
			Tool  string `json:"tool"`
			State struct {
				Input struct {
					Todos []struct {
						Content  string `json:"content"`
						Status   string `json:"status"`
						Priority string `json:"priority"`
					} `json:"todos"`
				} `json:"input"`
			} `json:"state"`
		} `json:"part"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal part %s: %v", body, err)
	}
	if env.Part.Tool != "todowrite" {
		t.Fatalf("part.tool = %q, want todowrite (payload %s)", env.Part.Tool, body)
	}
	todos := env.Part.State.Input.Todos
	if len(todos) != 1 || todos[0].Content != "step 1" || todos[0].Status != "in_progress" || todos[0].Priority != "high" {
		t.Fatalf("part.state.input.todos = %+v (%s)", todos, body)
	}

	// The DB-less sidecar snapshot fired too.
	got, err := worktree.TodoRead(worktree.BaseFor(m.deps.ExecDir), worktree.TodoReadArgs{})
	if err != nil {
		t.Fatalf("TodoRead: %v", err)
	}
	if !strings.Contains(got, "step 1") {
		t.Errorf("todo snapshot = %q, want the saved items", got)
	}
}

func TestMapperToolResultErrorSurfaces(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-9","name":"Bash","input":{"command":"false"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-9","content":"boom","is_error":true}]}}`,
	)
	if len(rec.toolCalls) != 2 {
		t.Fatalf("tool calls = %+v", rec.toolCalls)
	}
	res := rec.toolCalls[1]
	if res.name != "bash" || res.output != "boom" {
		t.Errorf("resolved tool call = %+v, want bash/boom", res)
	}
	parts := rec.partsOfKind(db.SessionPartToolUse)
	if len(parts) != 2 {
		t.Fatalf("tool_use parts = %+v", parts)
	}
	body, _ := json.Marshal(parts[1].payload)
	if !strings.Contains(string(body), `"status":"error"`) || !strings.Contains(string(body), `"error":"boom"`) {
		t.Errorf("error part = %s", body)
	}
}

func TestMapperToolResultOKCarriesCompletedStatus(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m, toolLine, toolRes)
	parts := rec.partsOfKind(db.SessionPartToolUse)
	if len(parts) != 2 {
		t.Fatalf("parts = %+v", parts)
	}
	body, _ := json.Marshal(parts[1].payload)
	if !strings.Contains(string(body), `"status":"completed"`) || !strings.Contains(string(body), `"output":"written"`) {
		t.Errorf("completed part = %s", body)
	}
}

func TestMapperWrittenFilesDedupeAcrossTurns(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Write","input":{"file_path":"/w/same.go","content":"x"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"Edit","input":{"file_path":"/w/same.go"}}]}}`,
	)
	if files := rec.filesFlat(); len(files) != 1 {
		t.Errorf("OnWrittenFiles = %v, want exactly one (deduped)", files)
	}
}

// ---------------------------------------------------------------- stall

func TestStallMonitorFatalNoProgress(t *testing.T) {
	now := time.Now()
	one := int64(1)
	m := newStallMonitor(scheduler.ExecutionManifest{StallNoProgressWindowSeconds: &one}, now)
	if r, _ := m.Evaluate(now.Add(500 * time.Millisecond)); r != "" {
		t.Fatalf("early Evaluate = %q, want none", r)
	}
	r, fatal := m.Evaluate(now.Add(2 * time.Second))
	if r != reasonNoProgress || !fatal {
		t.Fatalf("Evaluate = %q fatal=%v, want %q fatal", r, fatal, reasonNoProgress)
	}
	// Once fired it must not repeat.
	if r, _ := m.Evaluate(now.Add(5 * time.Second)); r != "" {
		t.Fatalf("repeat Evaluate = %q, want none", r)
	}
}

func TestStallMonitorAdvisoryNoFileProgressThenRecovers(t *testing.T) {
	now := time.Now()
	one := int64(1)
	m := newStallMonitor(scheduler.ExecutionManifest{StallNoFileDiffWindowSeconds: &one}, now)
	m.sawOutput(now)
	r, fatal := m.Evaluate(now.Add(2 * time.Second))
	if r != reasonNoFileProgress || fatal {
		t.Fatalf("Evaluate = %q fatal=%v, want advisory %q", r, fatal, reasonNoFileProgress)
	}
	if !m.sawFileWrite(now.Add(3 * time.Second)) {
		t.Fatal("sawFileWrite must report it cleared the advisory trip")
	}
	if m.sawFileWrite(now.Add(4 * time.Second)) {
		t.Fatal("sawFileWrite must not report recovery twice")
	}
	if r, _ := m.Evaluate(now.Add(5 * time.Second)); r != "" {
		t.Fatalf("Evaluate after recovery = %q, want none", r)
	}
}

func TestStallMonitorZeroDisablesAndNilUsesDefault(t *testing.T) {
	now := time.Now()
	zero := int64(0)
	m := newStallMonitor(scheduler.ExecutionManifest{StallNoProgressWindowSeconds: &zero, StallNoFileDiffWindowSeconds: &zero}, now)
	if m.noProgressWindow != 0 || m.noFileWindow != 0 {
		t.Fatalf("explicit 0 must disable: %v / %v", m.noProgressWindow, m.noFileWindow)
	}
	if r, _ := m.Evaluate(now.Add(48 * time.Hour)); r != "" {
		t.Fatalf("disabled monitor tripped: %q", r)
	}
	d := newStallMonitor(scheduler.ExecutionManifest{}, now)
	if d.noProgressWindow != defaultStallNoProgressWindow || d.noFileWindow != defaultStallNoFileDiffWindow {
		t.Fatalf("nil must resolve to the adapter defaults: %v / %v", d.noProgressWindow, d.noFileWindow)
	}
}

func TestMapperTickRaisesOnStallWithFatalFlag(t *testing.T) {
	one := int64(1)
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{StallNoProgressWindowSeconds: &one})
	if reason := m.Tick(context.Background(), time.Now().Add(2*time.Second)); reason != reasonNoProgress {
		t.Fatalf("Tick = %q, want the fatal reason returned to the caller", reason)
	}
	if len(rec.stalls) != 1 || !rec.stalls[0].fatal || rec.stalls[0].reason != reasonNoProgress {
		t.Fatalf("OnStall = %+v", rec.stalls)
	}
}

func TestMapperTickAdvisoryReturnsEmpty(t *testing.T) {
	one := int64(1)
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{StallNoFileDiffWindowSeconds: &one})
	feed(t, m, deltaOne) // progress, no file write
	if reason := m.Tick(context.Background(), time.Now().Add(2*time.Second)); reason != "" {
		t.Fatalf("advisory Tick = %q, want \"\" (not fatal)", reason)
	}
	if len(rec.stalls) != 1 || rec.stalls[0].fatal || rec.stalls[0].reason != reasonNoFileProgress {
		t.Fatalf("OnStall = %+v", rec.stalls)
	}
	// A file write after the advisory trip clears it.
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w","name":"Write","input":{"file_path":"/w/a.go","content":"x"}}]}}`)
	// The recovery label is the RECOVERY signal (opencode parity:
	// "recovered:no_file_progress"), never the stall reason.
	if len(rec.recovered) != 1 || rec.recovered[0] != recoveredNoFilePrefix {
		t.Fatalf("OnRecovered = %v, want %q", rec.recovered, recoveredNoFilePrefix)
	}
}

func TestMapperHandleIgnoresUnknownAndMalformedShapes(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	// An unknown event type with no relevant fields must not panic or emit.
	feed(t, m, `{"type":"totally_new_event","whatever":1}`)
	if len(rec.order) != 0 {
		t.Fatalf("unknown event produced callbacks: %v", rec.order)
	}
}
