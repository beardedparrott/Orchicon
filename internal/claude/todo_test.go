package claude

// todo_test.go — the claude TASK-FAMILY todo parity suite. It runs on canned
// stream-json fixtures alone (no live API session, zero Anthropic spend) and
// proves that a claude TaskCreate/TaskUpdate lifecycle lands on the EXACT
// todowrite envelope internal/execution/todos.go parses, correlated by
// tool_use_id, with the same sidecar snapshot opencode writes.
//
// The live counterpart is TestClaudeTaskTodoLiveSmoke in live_smoke_test.go,
// gated ONLY by ORCHICON_TEST_LIVE_CLAUDE=1.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// latestTodoWire decodes the LATEST todowrite envelope's todos out of the
// recorded tool_use parts, reading it EXACTLY the way internal/execution/todos.go
// does (envelope {"part":…}, part.tool=="todowrite", items under part.state.input.todos).
// Newest wins (replacement semantics). It fails the test when no todowrite
// envelope was ever recorded — which is itself the parity failure.
func latestTodoWire(t *testing.T, rec *captureCallbacks) []map[string]any {
	t.Helper()
	parts := rec.partsOfKind(db.SessionPartToolUse)
	for i := len(parts) - 1; i >= 0; i-- {
		body, err := json.Marshal(parts[i].payload)
		if err != nil {
			t.Fatalf("marshal part: %v", err)
		}
		var env struct {
			Part struct {
				Tool  string `json:"tool"`
				State struct {
					Input struct {
						Todos []map[string]any `json:"todos"`
					} `json:"input"`
				} `json:"state"`
			} `json:"part"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("unmarshal part %s: %v", body, err)
		}
		if env.Part.Tool != "todowrite" {
			continue
		}
		return env.Part.State.Input.Todos
	}
	t.Fatalf("no todowrite envelope recorded across %d tool_use parts", len(parts))
	return nil
}

// todoByContent indexes the wire todos by their content label for order-free
// assertions.
func todoByContent(todos []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(todos))
	for _, td := range todos {
		if c, ok := td["content"].(string); ok {
			out[c] = td
		}
	}
	return out
}

// TestMapperTaskFamilyLifecycle drives create → activate → complete → remove and
// asserts the rendered list at each step, including that a removal DROPS the row
// (rather than leaving a stale one).
func TestMapperTaskFamilyLifecycle(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})

	// create two tasks (no allocated id yet — it arrives in the tool_result).
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-1","name":"TaskCreate","input":{"subject":"first","activeForm":"doing first"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-2","name":"TaskCreate","input":{"subject":"second"}}]}}`,
	)
	// the paired tool_results carry the allocated ids (#1, #2).
	feed(t, m,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tc-1","content":"Task #1 created successfully: first","is_error":false}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tc-2","content":"Task #2 created successfully: second","is_error":false}]}}`,
	)

	// Both surface on the todowrite channel — no claude-specific tool surface.
	s := rec.snap()
	if len(s.toolCalls) == 0 {
		t.Fatalf("no tool calls recorded")
	}
	for _, tc := range s.toolCalls {
		if tc.name != "todowrite" {
			t.Errorf("OnToolCall name = %q, want every task op on the todowrite channel", tc.name)
		}
	}

	todos := latestTodoWire(t, rec)
	if len(todos) != 2 {
		t.Fatalf("after create, todos = %+v, want 2", todos)
	}
	byC := todoByContent(todos)
	for _, want := range []string{"first", "second"} {
		if got, ok := byC[want]; !ok || got["status"] != "pending" {
			t.Errorf("created task %q = %+v, want pending", want, byC[want])
		}
	}

	// activate #1, complete #2 (spelled taskId then task_id).
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-1","name":"TaskUpdate","input":{"taskId":"1","status":"in_progress"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-2","name":"TaskUpdate","input":{"task_id":"2","status":"completed"}}]}}`,
	)
	byC = todoByContent(latestTodoWire(t, rec))
	if byC["first"]["status"] != "in_progress" {
		t.Errorf("first status = %v, want in_progress", byC["first"]["status"])
	}
	if byC["second"]["status"] != "completed" {
		t.Errorf("second status = %v, want completed", byC["second"]["status"])
	}

	// remove #1 — the row must actually disappear.
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-3","name":"TaskUpdate","input":{"taskId":"1","status":"deleted"}}]}}`)
	todos = latestTodoWire(t, rec)
	if len(todos) != 1 {
		t.Fatalf("after removal, todos = %+v, want 1 (the removed row must be GONE)", todos)
	}
	if todos[0]["content"] != "second" || todos[0]["status"] != "completed" {
		t.Errorf("survivor = %+v, want second/completed", todos[0])
	}
}

// TestMapperTaskFamilyCorrelatesAllocatedIDViaToolUseID proves the create→id
// correlation: the allocated id NEVER rides the tool_use block, so it is taken
// from the paired tool_result (matched by tool_use_id). After correlation an
// update by the ALLOCATED id hits the row, and the pre-correlation tool_use id
// no longer does.
func TestMapperTaskFamilyCorrelatesAllocatedIDViaToolUseID(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-1","name":"TaskCreate","input":{"subject":"one"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tc-1","content":"Task #7 created successfully: one","is_error":false}]}}`,
	)
	// update by the ALLOCATED id 7 → must hit the created row.
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-1","name":"TaskUpdate","input":{"taskId":"7","status":"completed"}}]}}`)
	byC := todoByContent(latestTodoWire(t, rec))
	if byC["one"]["status"] != "completed" {
		t.Fatalf("update by allocated id 7 missed the row: %+v", byC["one"])
	}
	// the pre-correlation tool_use id is NO LONGER the key: an update by it must
	// NOT match (proves the re-key actually happened).
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-2","name":"TaskUpdate","input":{"taskId":"tc-1","status":"deleted"}}]}}`)
	todos := latestTodoWire(t, rec)
	if len(todos) != 1 {
		t.Fatalf("a stale tool_use-id update removed or duplicated a row: %+v", todos)
	}
}

// TestMapperTaskFamilyFieldNameVariants covers the defensive spellings
// (taskId / task_id / id, activeForm / active_form), numeric and string ids,
// subject / content / description labels, and raw string statuses — an unknown
// status must be preserved verbatim and "cancelled" must remain VISIBLE (it is a
// real terminal status, not a removal).
func TestMapperTaskFamilyFieldNameVariants(t *testing.T) {
	m, rec := newTestMapper(t, scheduler.ExecutionManifest{})
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"TaskCreate","input":{"content":"alpha","activeForm":"doing alpha"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"TaskCreate","input":{"description":"beta","active_form":"doing beta"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"TaskCreate","input":{"subject":"gamma"}}]}}`,
	)
	// a → "1"; b → a NUMERIC id (JSON number); c → no id extractable.
	feed(t, m,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"Task #1 created","is_error":false}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"task 2 created","is_error":false}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"task created","is_error":false}]}}`,
	)
	// update by taskId (numeric), task_id (string) + an unknown status, and a
	// cancelled status that must NOT remove the row.
	feed(t, m,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u1","name":"TaskUpdate","input":{"taskId":1,"status":"in_progress","activeForm":"working"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u2","name":"TaskUpdate","input":{"task_id":"2","status":"novel_state"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u3","name":"TaskUpdate","input":{"id":"1","status":"cancelled"}}]}}`,
	)
	todos := latestTodoWire(t, rec)
	if len(todos) != 3 {
		t.Fatalf("todos = %+v, want all 3 rows", todos)
	}
	byC := todoByContent(todos)
	if byC["alpha"]["status"] != "cancelled" {
		t.Errorf("cancelled must stay VISIBLE (a real status, not a removal): %+v", byC["alpha"])
	}
	if byC["beta"]["status"] != "novel_state" {
		t.Errorf("unknown status must be preserved verbatim: %+v", byC["beta"])
	}
	if byC["gamma"]["status"] != "pending" {
		t.Errorf("uncorrelated row must still render as pending: %+v", byC["gamma"])
	}
}

// TestMapperTaskFamilyWritesSnapshot pins AC4: a claude task op leaves the SAME
// on-disk artifact an opencode todowrite does (.orchicon/todos.json), via the
// same worktree.SaveTodoSnapshot site.
func TestMapperTaskFamilyWritesSnapshot(t *testing.T) {
	dir := t.TempDir()
	m, _ := newTestMapper(t, scheduler.ExecutionManifest{ProjectDir: dir})
	feed(t, m, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-1","name":"TaskCreate","input":{"subject":"snap one"}}]}}`)

	got, err := worktree.TodoRead(worktree.BaseFor(dir), worktree.TodoReadArgs{})
	if err != nil {
		t.Fatalf("TodoRead: %v", err)
	}
	if !strings.Contains(got, "snap one") || !strings.Contains(got, "pending") {
		t.Errorf("claude task op wrote no todo snapshot: %q", got)
	}
}

// TestTodoTrackToolsAreOptedIn pins AC1 at the launch-shape layer: the
// task/todo tools are named in the PreToolUse matcher AND pre-approved in
// permissions.allow, both NON-BYPASS. (The behavioural half — the installed CLI
// actually streaming them — is the env-gated live smoke; the CLI is absent from
// this container.)
func TestTodoTrackToolsAreOptedIn(t *testing.T) {
	for _, n := range TodoTrackToolNames {
		if !strings.Contains(HookToolMatcher, n) {
			t.Errorf("HookToolMatcher %q does not cover the todo/task tool %q", HookToolMatcher, n)
		}
	}
	args, err := PermissionArgs(PermissionOptions{ProjectDir: t.TempDir(), HookBinary: "orchicon"})
	if err != nil {
		t.Fatalf("PermissionArgs: %v", err)
	}
	perm, _ := settingsDoc(t, args)["permissions"].(map[string]any)
	allow := stringSet(perm["allow"])
	for _, n := range TodoTrackToolNames {
		if !allow[n] {
			t.Errorf("permissions.allow is missing the todo/task tool %q (opt-in absent)", n)
		}
	}
}

// TestCanonicalToolNameLeavesTheTaskFamilyVerbatim proves the task family is NOT
// remapped, so the workerrestrict subagent deny (exact "Task"/"Agent") is
// neither widened nor narrowed by this feature.
func TestCanonicalToolNameLeavesTheTaskFamilyVerbatim(t *testing.T) {
	for _, n := range []string{"TaskCreate", "TaskUpdate", "TaskList", "TaskGet", "TaskOutput"} {
		if got := canonicalToolName(n); got != n {
			t.Errorf("canonicalToolName(%q) = %q, want the name verbatim", n, got)
		}
	}
}
