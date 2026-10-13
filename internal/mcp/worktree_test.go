package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/worktree"
)

// allToolNames is the full worktree tool suite: 3 batch tools, 4 single-op
// wrappers, `list`, and the `todowrite`/`todoread` todo pair (D1/D2).
//
// `todowrite` is OURS as of the 2.x work: opencode 1.x had it natively and this
// registry exposed only the read half, but 2.x deleted the tool outright, so a
// prompt that orders it every turn (ours does) was ordering a tool that does not
// exist. See the tool's comment in worktree.go.
var allToolNames = []string{
	"batch_read", "batch_grep", "batch_write",
	"read", "grep", "write", "edit",
	"list", "todowrite", "todoread",
}

func TestWorktreeRegistryExposesFullToolSuite(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "", "")
	defs := r.List()
	if len(defs) != len(allToolNames) {
		t.Fatalf("expected %d tools, got %d", len(allToolNames), len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, n := range allToolNames {
		if !names[n] {
			t.Fatalf("missing tool %s", n)
		}
	}
	// The batch tools must be present in the registry; the single-op
	// wrappers must be Mutating=false/true as appropriate.
	//
	// todowrite is deliberately NOT in this map: it writes only the ephemeral
	// todo sidecar under .orchicon/, never project content — the same
	// bookkeeping class as todoread's sibling state. "Mutating" describes a
	// change to the worker's deliverable, and marking the todo list as one would
	// be a false claim about it (and a gate-tripping lie if a caller ever
	// enforces the field).
	mutating := map[string]bool{"batch_write": true, "write": true, "edit": true}
	for _, d := range defs {
		if d.Mutating != mutating[d.Name] {
			t.Fatalf("tool %s: mutating=%v want %v", d.Name, d.Mutating, mutating[d.Name])
		}
	}
}

func TestWorktreeRegistryExecuteBatchRead(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewWorktreeRegistry(base, "", "")
	args, _ := json.Marshal(map[string]any{"paths": []string{"a.txt"}})
	res, err := r.Execute(context.Background(), nil, "batch_read", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(string(res), "hello") {
		t.Fatalf("batch_read result missing content: %s", res)
	}
}

func TestWorktreeRegistrySingleOpWrappers(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewWorktreeRegistry(base, "", "")
	ctx := context.Background()

	// read wrapper
	args, _ := json.Marshal(map[string]any{"path": "a.txt"})
	res, err := r.Execute(ctx, nil, "read", args)
	if err != nil || !strings.Contains(string(res), "hello world") {
		t.Fatalf("read: err=%v res=%s", err, res)
	}
	// grep wrapper
	args, _ = json.Marshal(map[string]any{"pattern": "world"})
	res, err = r.Execute(ctx, nil, "grep", args)
	if err != nil || !strings.Contains(string(res), "a.txt") {
		t.Fatalf("grep: err=%v res=%s", err, res)
	}
	// write wrapper
	args, _ = json.Marshal(map[string]any{"filePath": "b.txt", "content": "new content"})
	if _, err := r.Execute(ctx, nil, "write", args); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "b.txt")); string(got) != "new content" {
		t.Fatalf("write did not persist: %q", got)
	}
	// edit wrapper
	args, _ = json.Marshal(map[string]any{"filePath": "b.txt", "oldString": "new", "newString": "edited"})
	if _, err := r.Execute(ctx, nil, "edit", args); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "b.txt")); string(got) != "edited content" {
		t.Fatalf("edit did not apply: %q", got)
	}
}

func TestWorktreeRegistryList(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(base, "a.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(base, "sub", "b.txt"), []byte("hi"), 0o644)
	r := NewWorktreeRegistry(base, "", "")
	args, _ := json.Marshal(map[string]any{})
	res, err := r.Execute(context.Background(), nil, "list", args)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := string(res)
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub/") {
		t.Fatalf("list missing entries: %s", out)
	}
}

func TestWorktreeRegistryTodoRead(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "", "")
	// No snapshot yet — must return a graceful empty message, not an error.
	res, err := r.Execute(context.Background(), nil, "todoread", nil)
	if err != nil {
		t.Fatalf("todoread empty: %v", err)
	}
	if !strings.Contains(string(res), "no todo list") {
		t.Fatalf("todoread empty message: %s", res)
	}
	// Write a snapshot the way the adapter would, then read it back.
	snap := `[{"content":"first item","status":"in_progress","priority":"high"}]`
	if err := os.MkdirAll(filepath.Join(base, ".orchicon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".orchicon", "todos.json"), []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = r.Execute(context.Background(), nil, "todoread", nil)
	if err != nil {
		t.Fatalf("todoread: %v", err)
	}
	if !strings.Contains(string(res), "first item") || !strings.Contains(string(res), "in_progress") {
		t.Fatalf("todoread snapshot: %s", res)
	}
}

func TestWorktreeRegistryExecuteRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "", "")
	args, _ := json.Marshal(map[string]any{"paths": []string{"../etc/passwd"}})
	if _, err := r.Execute(context.Background(), nil, "batch_read", args); err == nil {
		t.Fatal("expected a path-traversal error")
	}
	// Single-op wrappers must inherit the same confinement.
	args, _ = json.Marshal(map[string]any{"path": "../../etc/passwd"})
	if _, err := r.Execute(context.Background(), nil, "read", args); err == nil {
		t.Fatal("expected a path-traversal error for read")
	}
}

func TestWorktreeRegistryExecuteUnknownTool(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "", "")
	if _, err := r.Execute(context.Background(), nil, "nope", nil); err == nil {
		t.Fatal("expected an unknown-tool error")
	}
}

// TestWorktreeRegistryExecuteProjectRootRead pins AC1 at the registry level
// with a NON-EMPTY project root (the exact wiring RuntimeServeConfig injects:
// worktree under <proj>/.orchicon-worktrees/<run>, project root = <proj>). A
// worker must be able to batch_read a run-state .orchicon/<run>/ file by both
// a `..`-relative path and an absolute project-root path — without the tools
// it would shell out (or re-derive facts). This closes the coverage gap the
// PR step noted (the worktree unit tests cover the mechanism, not the wiring).
func TestWorktreeRegistryExecuteProjectRootRead(t *testing.T) {
	proj := t.TempDir()
	run := "run-qa"
	wt := filepath.Join(proj, ".orchicon-worktrees", run)
	if err := os.MkdirAll(filepath.Join(proj, ".orchicon", run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "FACTS LEARNED: the project root is readable.\n"
	if err := os.WriteFile(filepath.Join(proj, ".orchicon", run, "facts_learned"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewWorktreeRegistry(wt, proj, "")
	ctx := context.Background()

	// `..`-relative project-root path.
	args, _ := json.Marshal(map[string]any{"paths": []string{"../../.orchicon/" + run + "/facts_learned"}})
	res, err := r.Execute(ctx, nil, "batch_read", args)
	if err != nil {
		t.Fatalf("relative project-root read should be allowed: %v", err)
	}
	if !strings.Contains(string(res), content) {
		t.Fatalf("relative read missing run-file content: %s", res)
	}

	// Absolute project-root path.
	abs := filepath.Join(proj, ".orchicon", run, "facts_learned")
	argsAbs, _ := json.Marshal(map[string]any{"paths": []string{abs}})
	resAbs, err := r.Execute(ctx, nil, "batch_read", argsAbs)
	if err != nil {
		t.Fatalf("absolute project-root read should be allowed: %v", err)
	}
	if !strings.Contains(string(resAbs), content) {
		t.Fatalf("absolute read missing run-file content: %s", resAbs)
	}
}

// TestWorktreeRegistryExecuteScratchWrite pins AC2 at the registry level: a
// batch_write (and its read-back) to the sanctioned scratch dir /tmp/orchicon
// must be permitted — the tools are not confined to the worktree for scratch.
func TestWorktreeRegistryExecuteScratchWrite(t *testing.T) {
	if err := os.MkdirAll(worktree.DefaultScratchDir, 0o755); err != nil {
		t.Skipf("cannot create scratch dir %s: %v", worktree.DefaultScratchDir, err)
	}
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "", "")
	target := filepath.Join(worktree.DefaultScratchDir, "reg-scratch-"+filepath.Base(t.TempDir())+".txt")
	defer os.Remove(target)

	args, _ := json.Marshal(map[string]any{"writes": []map[string]any{{"path": target, "mode": "create", "content": "scratch-ok"}}})
	if _, err := r.Execute(context.Background(), nil, "batch_write", args); err != nil {
		t.Fatalf("scratch write should be allowed: %v", err)
	}
	got, rerr := os.ReadFile(target)
	if rerr != nil || string(got) != "scratch-ok" {
		t.Fatalf("scratch write did not persist: %q err=%v", got, rerr)
	}
}

// TestWorktreeTodowriteTodoreadRoundTrip proves the todo pair works end to end
// over MCP: a worker writes its list and reads the SAME list back.
//
// This is the regression guard for opencode 2.x having REMOVED the native
// `todowrite`. Our composite prompt orders a todowrite at every turn boundary,
// so on 2.x that obligation had no tool to satisfy it: the model's calls came
// back as errors, and repeated error-status calls are exactly what the
// repetition stall counts. Owning the tool here makes the todo list independent
// of which tools the agent surface happens to ship.
func TestWorktreeTodowriteTodoreadRoundTrip(t *testing.T) {
	base := t.TempDir()
	r := NewWorktreeRegistry(base, "", "")

	args, _ := json.Marshal(map[string]any{"todos": []map[string]any{
		{"content": "wire the search bar", "status": "completed", "priority": "high"},
		{"content": "add jump-to-top", "status": "in_progress", "priority": "medium"},
	}})
	out, err := r.Execute(context.Background(), nil, "todowrite", args)
	if err != nil {
		t.Fatalf("todowrite: %v", err)
	}
	if !strings.Contains(string(out), `"count":2`) {
		t.Fatalf("todowrite did not report the 2 items it stored: %s", out)
	}

	// The read half must return what the write half stored — this is the
	// property the execution UI and the DB-less sidecar both depend on.
	read, err := r.Execute(context.Background(), nil, "todoread", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("todoread: %v", err)
	}
	for _, want := range []string{"wire the search bar", "add jump-to-top", "in_progress"} {
		if !strings.Contains(string(read), want) {
			t.Fatalf("todoread did not return %q after a todowrite:\n%s", want, read)
		}
	}
}

// TestWorktreeTodowriteRejectsMalformedArgs pins the failure mode: bad args are
// a tool error the model can read and correct, not a silent no-op. A todowrite
// that quietly did nothing would leave the operator staring at a stale plan.
func TestWorktreeTodowriteRejectsMalformedArgs(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "", "")
	if _, err := r.Execute(context.Background(), nil, "todowrite", json.RawMessage(`{"todos":"not-an-array"}`)); err == nil {
		t.Fatal("a malformed todos payload must fail the call, not be silently ignored")
	}
}

// TestTodowriteWritesTheRunStateSnapshotAndTodoreadSeesIt is the round trip
// through the RUN-STATE file — the copy the execution view's todo panel reads.
//
// It has to be this file and not the worktree one: the worktree is PRUNED when
// the run ends, so a worktree-scoped snapshot is unreadable by the time anyone
// looks at the run's plan. The run-state dir (<project>/.orchicon/<runID>/) is
// where the plane already keeps facts_learned/status/summary, and it survives.
func TestTodowriteWritesTheRunStateSnapshotAndTodoreadSeesIt(t *testing.T) {
	project := t.TempDir()
	runState := worktree.RunStateDir(project, "01RUN")
	if runState == "" {
		t.Fatal("RunStateDir returned empty for a project dir + run id")
	}
	r := NewWorktreeRegistry(t.TempDir(), project, runState)

	args, _ := json.Marshal(map[string]any{"todos": []map[string]any{
		{"content": "wire the panel", "status": "in_progress", "priority": "high"},
	}})
	if _, err := r.Execute(context.Background(), nil, "todowrite", args); err != nil {
		t.Fatalf("todowrite: %v", err)
	}

	// THE FILE ITSELF, at the path the plane derives — not merely "something was
	// written somewhere". The reader and the writer must agree by construction, so
	// this asserts the plane's own resolution, not a copy of it.
	path := filepath.Join(runState, worktree.TodoSnapshotName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the run-state snapshot is missing at %s: %v", path, err)
	}
	if !strings.Contains(string(raw), "wire the panel") {
		t.Fatalf("the run-state snapshot does not carry the item: %s", raw)
	}
	// And the plane's OWN reader resolves the same content.
	items, ok := worktree.ReadRunTodoSnapshot(runState)
	if !ok || len(items) != 1 || items[0].Content != "wire the panel" {
		t.Fatalf("ReadRunTodoSnapshot = %+v ok=%v, want the one item", items, ok)
	}

	// todoread must prefer the run-state copy.
	out, err := r.Execute(context.Background(), nil, "todoread", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("todoread: %v", err)
	}
	if !strings.Contains(string(out), "wire the panel") {
		t.Fatalf("todoread did not surface the run-state list:\n%s", out)
	}
}

// TestTodowriteSaysSoWhenTheRunStateChannelIsMissing pins the failure mode that
// made this invisible.
//
// When the sidecar is not told its run, the run-state write cannot happen. The
// tool must STILL succeed (the worktree copy is a legitimate fallback for
// todoread, and the worker's next move must not be blocked), but it must SAY the
// panel will not update. The previous behaviour returned `ok:true` and nothing
// else, which is exactly how an empty panel coexisted with a successful tool call
// for as long as it did.
func TestTodowriteSaysSoWhenTheRunStateChannelIsMissing(t *testing.T) {
	r := NewWorktreeRegistry(t.TempDir(), "", "")
	args, _ := json.Marshal(map[string]any{"todos": []map[string]any{
		{"content": "something", "status": "pending"},
	}})
	out, err := r.Execute(context.Background(), nil, "todowrite", args)
	if err != nil {
		t.Fatalf("todowrite must not fail when the run-state channel is absent: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("todowrite result is not JSON: %s", out)
	}
	if res["ok"] != true {
		t.Errorf("ok = %v, want true — the worktree copy still landed", res["ok"])
	}
	warn, _ := res["warning"].(string)
	if warn == "" {
		t.Fatalf("a missing run-state channel reported NO warning, so the empty panel is unexplainable: %s", out)
	}
	if !strings.Contains(warn, "panel") {
		t.Errorf("the warning does not name the consequence for the operator: %q", warn)
	}
}
