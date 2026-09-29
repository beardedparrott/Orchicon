package claude

// Parity matrix (consolidated) — one subtest per acceptance-criterion surface
// of the claude → ExecutionCallbacks normalization layer, driven end to end
// through Bridge.Start with the fake ProcSession and CANNED stream-json
// fixtures. There is no live Claude API session by default (the live smoke
// lives in live_smoke_test.go behind ORCHICON_TEST_LIVE_CLAUDE=1).
//
// It mirrors internal/orchicon/parity_matrix_test.go: every acceptance
// surface is asserted in ONE named matrix so the reviewer / QA can point at a
// single place. The reference for "renders identically" is
// internal/opencode/adapter.go:1041-1073 (write → artifact, todowrite →
// snapshot, fileEdits with the canonical name).
//
// NOTE on reading the recorder: the callbacks are invoked from the session's
// run goroutine, so every assertion reads a LOCKED snapshot (`rec.snap()` or
// the locked count/slice accessors). Reading the fields directly would be a
// genuine data race (and `go test -race` catches it).

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
	"github.com/beardedparrott/orchicon/internal/fileedit"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

const (
	pmTodoLine = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"td-1","name":"TodoWrite","input":{"todos":[{"content":"step 1","status":"in_progress","priority":"high","activeForm":"stepping"},{"content":"step 2","status":"pending","priority":"low","task_id":"2"}]}}]}}`
	pmWrite    = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w-1","name":"Write","input":{"file_path":"a.md","content":"# hi"}}]}}`
	pmEdit     = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e-1","name":"Edit","input":{"file_path":"b.go","old_string":"a","new_string":"b"}}]}}`
	pmBash     = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b-1","name":"Bash","input":{"command":"false"}}]}}`
	pmBashErr  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b-1","content":"boom","is_error":true}]}}`

	// The completions. The file-edit ledger rides these (the observer diffs
	// the file's on-disk state, which only exists once the CLI has written it).
	pmWriteRes = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w-1","content":"File created successfully","is_error":false}]}}`
	pmEditRes  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"e-1","content":"The file has been updated","is_error":false}]}}`
)

// ledgerStore is the durable half of the file-edit ledger (the server wires a
// Postgres-backed store; this captures the rows instead) so the parity test
// can drive the REAL fileedit.Service for the claude delta.
type ledgerStore struct {
	mu   sync.Mutex
	rows []*db.FileEditLedgerRow
}

func (s *ledgerStore) Append(_ context.Context, tenantID, ownerKind, ownerID string, entries []fileedit.Entry) ([]*db.FileEditLedgerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*db.FileEditLedgerRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, &db.FileEditLedgerRow{
			ID:           db.NewID(),
			TenantID:     tenantID,
			OwnerKind:    ownerKind,
			OwnerID:      ownerID,
			Seq:          int64(len(s.rows) + len(out) + 1),
			Path:         e.Path,
			Kind:         e.Kind,
			UnifiedDiff:  e.UnifiedDiff,
			BeforeSize:   e.BeforeSize,
			AfterSize:    e.AfterSize,
			BeforeSHA256: e.BeforeSHA,
			AfterSHA256:  e.AfterSHA,
			Tool:         e.Tool,
		})
	}
	s.rows = append(s.rows, out...)
	return out, nil
}

func (s *ledgerStore) snapshot() []*db.FileEditLedgerRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*db.FileEditLedgerRow(nil), s.rows...)
}

// observerLedgerHook is the server's BUILT-IN write/edit ledger branch
// (internal/server/fileedit_hook.go): a claude delta carries no `file_edits`
// engine payload, so the hook falls through to the plane-side observer, which
// diffs the file against its last-known content. Wired over a REAL
// fileedit.Observer + Service here so the parity test exercises the same
// pipeline the server does rather than a mirror of it.
func observerLedgerHook(dir string, svc *fileedit.Service) FileEditHookFunc {
	o := fileedit.NewObserver(dir)
	return func(ctx context.Context, execID, tenantID, _, toolName string, input map[string]any, _ string) {
		p, _ := input["filePath"].(string)
		tool := fileedit.ToolOpenCodeWrite
		if toolName == "edit" {
			tool = fileedit.ToolOpenCodeEdit
		}
		if e := o.ObserveAfter(p, tool); e.Path != "" {
			svc.Record(ctx, tenantID, db.FileEditOwnerExecution, execID, []fileedit.Entry{e})
		}
	}
}

// pmSession starts a claude execution against a fake proc, wiring the
// recorder into every optional dependency the mapper consumes (transcript,
// ledger, usage).
func pmSession(t *testing.T, fp *fakeProc, manifest scheduler.ExecutionManifest, rec *captureCallbacks) <-chan error {
	t.Helper()
	h := newHarness(t, func() *fakeProc { return fp })
	h.b.SetSessionStore(rec.recordParts)
	h.b.SetFileEditHook(rec.fileEdit)
	h.b.SetUsageRecorder(rec.recordUsage)
	if manifest.ProjectDir == "" {
		manifest.ProjectDir = t.TempDir()
	}
	if manifest.ExecutionID == "" {
		manifest.ExecutionID = "exec-pm"
	}
	if manifest.ModelRef == "" {
		manifest.ModelRef = "claude/anthropic/claude-sonnet-5"
	}
	done := make(chan error, 1)
	go func() {
		done <- h.b.Start(context.Background(), db.ExecutionRow{ID: manifest.ExecutionID, TenantID: "t1"}, manifest, rec)
	}()
	waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn write")
	return done
}

func TestClaudeParityMatrix(t *testing.T) {
	// execution view: OnStarted → OnText(deltas) → OnResult(success), plus the
	// single health signal the session publishes when its id is captured.
	t.Run("execution_view", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(deltaOne)
		fp.push(deltaTwo)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if !s.hasOrder("started") {
			t.Errorf("OnStarted missing: %v", s.order)
		}
		if joined := strings.Join(s.texts, ""); joined != "hello world" {
			t.Errorf("OnText = %q, want %q", joined, "hello world")
		}
		if len(s.health) != 1 || s.health[0] != "healthy" {
			t.Errorf("OnHealth = %v, want [healthy]", s.health)
		}
		if len(s.results) != 1 || !s.results[0].succeeded {
			t.Fatalf("OnResult = %+v", s.results)
		}
		if !strings.Contains(s.results[0].output, "hello world") {
			t.Errorf("OnResult output = %q", s.results[0].output)
		}
		if len(s.order) == 0 || s.order[0] != "started" || s.order[len(s.order)-1] != "result" {
			t.Errorf("callback order = %v, want started … result", s.order)
		}
	})

	// todo panel: the TodoWrite round updates the tool surface AND leaves a
	// durable {"part":…} envelope the shared todos parser reads, plus the
	// DB-less sidecar snapshot.
	t.Run("todo_panel", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		dir := t.TempDir()
		done := pmSession(t, fp, scheduler.ExecutionManifest{ProjectDir: dir}, rec)
		fp.push(initLine)
		fp.push(pmTodoLine)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.toolCalls) != 1 || s.toolCalls[0].name != "todowrite" {
			t.Errorf("OnToolCall = %+v, want todowrite", s.toolCalls)
		}
		parts := s.partsOfKind(db.SessionPartToolUse)
		if len(parts) != 1 {
			t.Fatalf("tool_use parts = %+v", parts)
		}
		body, _ := json.Marshal(parts[0].payload)
		if !strings.Contains(string(body), `"tool":"todowrite"`) || !strings.Contains(string(body), `"todos"`) {
			t.Errorf("durable todo part = %s", body)
		}
		got, err := worktree.TodoRead(worktree.BaseFor(dir), worktree.TodoReadArgs{})
		if err != nil {
			t.Fatalf("TodoRead: %v", err)
		}
		if !strings.Contains(got, "step 1") || !strings.Contains(got, "in_progress") {
			t.Errorf("todo snapshot = %q", got)
		}
	})

	// file diffs: write + edit both reach OnWrittenFiles exactly once, and the
	// SHARED ledger hook is called with the canonical name + the filePath key
	// it actually reads (the diff pipeline's ingestion point).
	t.Run("file_diffs", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(pmWrite)
		fp.push(pmWriteRes)
		fp.push(pmEdit)
		fp.push(pmEditRes)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if files := s.filesFlat(); len(files) != 2 || files[0] != "a.md" || files[1] != "b.go" {
			t.Errorf("OnWrittenFiles = %v, want [a.md b.go]", files)
		}
		if len(s.ledger) != 2 {
			t.Fatalf("ledger calls = %+v, want 2", s.ledger)
		}
		if s.ledger[0].tool != "write" || s.ledger[0].input["filePath"] != "a.md" {
			t.Errorf("ledger[0] = %+v, want write + filePath=a.md", s.ledger[0])
		}
		if s.ledger[1].tool != "edit" || s.ledger[1].input["filePath"] != "b.go" {
			t.Errorf("ledger[1] = %+v, want edit + filePath=b.go", s.ledger[1])
		}
	})

	// file diffs (real ledger): the SAME diff pipeline the server wires — a
	// real fileedit.Observer over the execution dir under a real
	// fileedit.Service — must record a row for a claude Write's ABSOLUTE
	// file_path (claude's real shape) once the tool_result lands. This is the
	// execution view's file-diff pane data source; an empty ledger is a blank
	// diff pane.
	t.Run("file_diffs_ledger", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		dir := t.TempDir()
		store := &ledgerStore{}
		svc := fileedit.NewService(store, quietLogger())
		h := newHarness(t, func() *fakeProc { return fp })
		h.b.SetSessionStore(rec.recordParts)
		h.b.SetUsageRecorder(rec.recordUsage)
		h.b.SetFileEditHook(observerLedgerHook(dir, svc))

		manifest := scheduler.ExecutionManifest{
			ExecutionID: "exec-ledger",
			ProjectDir:  dir,
			ModelRef:    "claude/anthropic/claude-sonnet-5",
		}
		done := make(chan error, 1)
		go func() {
			done <- h.b.Start(context.Background(), db.ExecutionRow{ID: manifest.ExecutionID, TenantID: "t1"}, manifest, rec)
		}()
		waitFor(t, func() bool { return fp.turnCount() == 1 }, "initial turn write")

		fp.push(initLine)
		// claude reports the ABSOLUTE path — its real behaviour.
		fp.push(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w-abs","name":"Write","input":{"file_path":"` + filepath.Join(dir, "a.md") + `","content":"# hi\n"}}]}}`)
		// Wait for the mapper to have processed the tool_use BEFORE creating
		// the file, so the test cannot pass by racing the CLI's write.
		waitFor(t, func() bool { return rec.toolCallCount() == 1 }, "the tool_use to reach the mapper")
		// The CLI performs the write; only then does the tool_result arrive.
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fp.push(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w-abs","content":"File created successfully","is_error":false}]}}`)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}

		rows := store.snapshot()
		if len(rows) != 1 {
			t.Fatalf("ledger rows = %d (%+v), want the claude write recorded", len(rows), rows)
		}
		if rows[0].Path != "a.md" || rows[0].Tool != fileedit.ToolOpenCodeWrite {
			t.Errorf("ledger row = path %q tool %q, want a.md / %s", rows[0].Path, rows[0].Tool, fileedit.ToolOpenCodeWrite)
		}
		if !strings.Contains(rows[0].UnifiedDiff, "# hi") {
			t.Errorf("ledger diff = %q, want the written body", rows[0].UnifiedDiff)
		}
	})

	// artifact: a `write` is routed to OnArtifact with an extension-derived
	// type (mirrors opencode/adapter.go's built-in write routing).
	t.Run("artifact", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(pmWrite)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.artifacts) != 1 {
			t.Fatalf("OnArtifact = %+v", s.artifacts)
		}
		a := s.artifacts[0]
		if a.name != "a.md" || a.typ != "markdown" || a.content != "# hi" {
			t.Errorf("OnArtifact = %+v, want a.md/markdown/# hi", a)
		}
	})

	// tool errors: a tool_result with is_error surfaces on the tool-call
	// channel with the error text, and the durable part carries both
	// state.status="error" and a top-level "error" (there is no OnError).
	t.Run("tool_errors", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(pmBash)
		fp.push(pmBashErr)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.toolCalls) != 2 {
			t.Fatalf("tool calls = %+v", s.toolCalls)
		}
		if s.toolCalls[1].name != "bash" || s.toolCalls[1].output != "boom" {
			t.Errorf("resolved call = %+v, want bash/boom", s.toolCalls[1])
		}
		parts := s.partsOfKind(db.SessionPartToolUse)
		if len(parts) != 2 {
			t.Fatalf("tool_use parts = %+v", parts)
		}
		body, _ := json.Marshal(parts[1].payload)
		if !strings.Contains(string(body), `"status":"error"`) || !strings.Contains(string(body), `"error":"boom"`) {
			t.Errorf("error part = %s", body)
		}
	})

	// error terminal: a max-turns result still arrives via OnResult(false, …).
	t.Run("terminal_error_result", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(deltaOne)
		fp.push(`{"type":"result","subtype":"error_max_turns","session_id":"sess-1","result":"","usage":{"input_tokens":1,"output_tokens":2}}`)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.results) != 1 || s.results[0].succeeded {
			t.Fatalf("OnResult = %+v, want failure", s.results)
		}
		if !strings.Contains(s.results[0].errMsg, "error_max_turns") {
			t.Errorf("OnResult errMsg = %q", s.results[0].errMsg)
		}
	})

	// callbacks: a clean turn is exactly one terminal OnResult(success) and no
	// stall whatsoever.
	t.Run("callbacks", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(pmWrite)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.results) != 1 || !s.results[0].succeeded {
			t.Fatalf("OnResult = %+v", s.results)
		}
		if len(s.stalls) != 0 {
			t.Errorf("OnStall = %+v, want none on a clean run", s.stalls)
		}
	})

	// stall (fatal): total stdout silence past the no-progress window is a
	// hard hang — OnStall(reason, fatal=true), the child is SIGINTed, and the
	// execution ends with OnResult(false).
	t.Run("stall_fatal", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		one := int64(1)
		done := pmSession(t, fp, scheduler.ExecutionManifest{StallNoProgressWindowSeconds: &one}, rec)
		// No further lines: the watchdog alone must trip.
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("stalled session returned nil error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after the fatal stall")
		}
		s := rec.snap()
		if len(s.stalls) != 1 || !s.stalls[0].fatal || s.stalls[0].reason != reasonNoProgress {
			t.Fatalf("OnStall = %+v, want one fatal %q", s.stalls, reasonNoProgress)
		}
		sigs := fp.signals()
		found := false
		for _, sig := range sigs {
			if sig == "INT" {
				found = true
			}
		}
		if !found {
			t.Errorf("signals = %v, want INT (the child is hard-killed)", sigs)
		}
		if len(s.results) != 1 || s.results[0].succeeded {
			t.Fatalf("OnResult = %+v, want failure", s.results)
		}
	})

	// stall (advisory) + recovery: activity with no file write trips the
	// ADVISORY notice; a later file write clears it via OnRecovered and the
	// execution still succeeds.
	t.Run("stall_advisory_recovered", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		one := int64(1)
		done := pmSession(t, fp, scheduler.ExecutionManifest{StallNoFileDiffWindowSeconds: &one}, rec)
		fp.push(initLine)
		fp.push(pmBash) // progress, but nothing written
		waitFor(t, func() bool { return rec.stallCount() == 1 }, "advisory stall")
		stalls := rec.stallsSnapshot()
		if stalls[0].fatal || stalls[0].reason != reasonNoFileProgress {
			t.Fatalf("OnStall = %+v, want advisory %q", stalls, reasonNoFileProgress)
		}
		fp.push(pmWrite)
		waitFor(t, func() bool { return rec.recoveredCount() == 1 }, "recovery")
		// The recovery label is the RECOVERY signal, byte-identical to what BOTH
		// other bridges emit for the same cleared trip (opencode/progress.go and
		// the native engine's internal/orchicon/progress.go both report
		// "recovered:no_file_progress") — never the stall reason.
		if got := rec.recoveredSnapshot(); got[0] != recoveredNoFilePrefix {
			t.Errorf("OnRecovered = %v, want %q", got, recoveredNoFilePrefix)
		}
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		if len(s.results) != 1 || !s.results[0].succeeded {
			t.Fatalf("OnResult = %+v, want success (advisory stall is not fatal)", s.results)
		}
	})

	// defensive inputs: claude's repaired key names (id / task_id /
	// active_form / activeForm) and unknown status strings must not panic and
	// must still produce the canonical todo shape.
	t.Run("defensive_inputs", func(t *testing.T) {
		fp := newFakeProc()
		rec := &captureCallbacks{}
		done := pmSession(t, fp, scheduler.ExecutionManifest{}, rec)
		fp.push(initLine)
		fp.push(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"td-x","name":"TodoWrite","input":{"todos":[{"content":"a","status":"","task_id":"1","active_form":"doing a"},{"content":"b","status":"novel_state"}]}}]}}`)
		fp.push(resultOne)
		if err := <-done; err != nil {
			t.Fatalf("Start: %v", err)
		}
		s := rec.snap()
		parts := s.partsOfKind(db.SessionPartToolUse)
		if len(parts) != 1 {
			t.Fatalf("parts = %+v", parts)
		}
		body, _ := json.Marshal(parts[0].payload)
		if !strings.Contains(string(body), `"status":"pending"`) {
			t.Errorf("empty status must default to pending: %s", body)
		}
		if !strings.Contains(string(body), `"novel_state"`) {
			t.Errorf("unknown status must be preserved verbatim: %s", body)
		}
	})

	// ask_coupling: the bridge is Ask-capable as well as worker-execution
	// capable, so the dispatcher can route an Ask turn to it. This assertion used
	// to require the OPPOSITE (Ask-on-claude was an explicit follow-up); the
	// requirement changed and the assertion flipped with it, rather than being
	// dropped.
	t.Run("ask_coupling", func(t *testing.T) {
		b := New(quietLogger())
		if _, ok := any(b).(scheduler.ChatTurnClient); !ok {
			t.Fatal("claude.Bridge must implement scheduler.ChatTurnClient")
		}
	})
}
