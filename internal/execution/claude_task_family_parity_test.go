package execution

// claude_task_family_parity_test.go — the QA end-to-end leg for the claude
// TASK-family (TaskCreate/TaskUpdate) todo parity. The claude-package suite
// (internal/claude/todo_test.go) asserts the emitted envelope with its own
// decoder; this test closes the loop the same way
// TestClaudeMapperPayloadParsesWithoutAdapterBranch does for TodoWrite: it
// drives the REAL claude Mapper over a canned stream-json lifecycle, feeds the
// payloads the mapper ACTUALLY persists into the shared parser the RPC path
// uses (`latestTodos`), and asserts the rendered values. A drift in
// emitTodos (tool name, envelope nesting, the todos key) therefore fails HERE
// rather than only in a mirror that keeps matching itself.

import (
	"context"
	"encoding/json"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/db"
)

// taskFamilyMapperParts drives the real mapper over the canned task-family
// lifecycle below and returns every tool_use part it persisted, in emission
// order, plus the raw JSON of each for failure output.
func taskFamilyMapperParts(t *testing.T) ([]db.SessionPart, []string) {
	t.Helper()
	var parts []db.SessionPart
	var raw []string
	seq := int64(0)
	m := claude.NewMapper("exec-qa-task-parity", claudeParityNoop{}, claude.MapperDeps{
		ExecDir: t.TempDir(),
		Record: func(_ context.Context, kind string, payload map[string]any) {
			if kind != db.SessionPartToolUse {
				return
			}
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal persisted part: %v", err)
			}
			seq++
			parts = append(parts, db.SessionPart{Kind: kind, Seq: seq, Payload: b})
			raw = append(raw, string(b))
		},
	})

	lines := []string{
		// create two tasks; the CLI-allocated id is NOT in the tool_use block.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-1","name":"TaskCreate","input":{"subject":"alpha","activeForm":"doing alpha","priority":"high"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tc-2","name":"TaskCreate","input":{"subject":"beta"}}]}}`,
		// the paired tool_results carry the allocated ids (#1, #2).
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tc-1","content":"Task #1 created successfully: alpha","is_error":false}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tc-2","content":"Task #2 created successfully: beta","is_error":false}]}}`,
		// activate #1, complete #2 — spelled taskId then task_id.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-1","name":"TaskUpdate","input":{"taskId":"1","status":"in_progress"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-2","name":"TaskUpdate","input":{"task_id":"2","status":"completed"}}]}}`,
		// remove #1 — the row must actually DROP from the rendered list.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u-3","name":"TaskUpdate","input":{"taskId":"1","status":"deleted"}}]}}`,
	}
	for _, l := range lines {
		ev, err := claude.ParseLine([]byte(l))
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", l, err)
		}
		m.Handle(context.Background(), ev)
	}
	return parts, raw
}

// TestClaudeTaskFamilyLifecycleReachesLatestTodos is the end-to-end acceptance
// check for normalization (AC2), full parity through the single parse site
// (AC3) and the complete lifecycle including removal (AC5).
func TestClaudeTaskFamilyLifecycleReachesLatestTodos(t *testing.T) {
	parts, raw := taskFamilyMapperParts(t)
	if len(parts) == 0 {
		t.Fatal("the mapper persisted no tool_use part for the task family")
	}

	// Every persisted part must be a todowrite envelope — a claude-specific
	// tool name in the transcript is exactly the "second parse site" failure.
	for i, p := range parts {
		var env struct {
			Part struct {
				Tool string `json:"tool"`
			} `json:"part"`
		}
		if err := json.Unmarshal(p.Payload, &env); err != nil {
			t.Fatalf("part %d is not valid JSON: %s", i, raw[i])
		}
		if env.Part.Tool != "todowrite" {
			t.Errorf("part %d carries tool %q, want todowrite (%s)", i, env.Part.Tool, raw[i])
		}
	}

	// Feed them DESC-by-seq, exactly what db.LatestToolUseParts returns.
	desc := make([]db.SessionPart, len(parts))
	for i := range parts {
		desc[i] = parts[len(parts)-1-i]
	}
	todos := latestTodos(desc)
	if len(todos) != 1 {
		t.Fatalf("latestTodos = %+v, want ONLY the surviving task after the removal (%v)", todos, raw)
	}
	got := todos[0]
	if got.Content != "beta" || got.Status != apiv1.TodoStatus_TODO_STATUS_COMPLETED {
		t.Errorf("surviving todo = %+v, want beta/completed", got)
	}
}

// TestClaudeTaskFamilyIntermediateStatesReachLatestTodos walks the SAME
// persisted transcript prefix-by-prefix (as the live RPC would see it during the
// run) and asserts the created→pending, activated→in_progress,
// completed→completed transitions are all visible at the right step.
func TestClaudeTaskFamilyIntermediateStatesReachLatestTodos(t *testing.T) {
	parts, raw := taskFamilyMapperParts(t)
	at := func(n int) []*apiv1.TodoItem {
		desc := make([]db.SessionPart, 0, n)
		for i := n - 1; i >= 0; i-- {
			desc = append(desc, parts[i])
		}
		return latestTodos(desc)
	}
	byContent := func(todos []*apiv1.TodoItem) map[string]*apiv1.TodoItem {
		out := make(map[string]*apiv1.TodoItem, len(todos))
		for _, td := range todos {
			out[td.GetContent()] = td
		}
		return out
	}

	// step 1: the first TaskCreate persists → one pending row.
	if todos := at(1); len(todos) != 1 || todos[0].GetStatus() != apiv1.TodoStatus_TODO_STATUS_PENDING {
		t.Fatalf("after the first create, todos = %+v, want one pending row (%v)", todos, raw[0])
	}
	// step 2: second create → two pending rows.
	if todos := at(2); len(todos) != 2 {
		t.Fatalf("after two creates, todos = %+v, want two rows", todos)
	}
	// steps 3-4 are the tool_result re-keys: still two pending rows.
	if todos := at(4); len(todos) != 2 {
		t.Fatalf("after the create tool_results, todos = %+v, want two rows (raw=%v)", todos, raw[:4])
	}
	for _, td := range at(4) {
		if td.GetStatus() != apiv1.TodoStatus_TODO_STATUS_PENDING {
			t.Errorf("post-create %q status = %v, want pending", td.GetContent(), td.GetStatus())
		}
	}
	// step 5: activate #1 by its ALLOCATED id → in_progress.
	if td := byContent(at(5))["alpha"]; td == nil || td.GetStatus() != apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS {
		t.Fatalf("after activating #1, alpha = %+v (raw=%v)", td, raw[4])
	}
	// step 6: complete #2 by a '#'-stripped allocated id → completed.
	if td := byContent(at(6))["beta"]; td == nil || td.GetStatus() != apiv1.TodoStatus_TODO_STATUS_COMPLETED {
		t.Fatalf("after completing #2, beta = %+v (raw=%v)", td, raw[5])
	}
	// step 7: the removal actually drops the row.
	if todos := at(7); len(todos) != 1 {
		t.Fatalf("after the removal, todos = %+v, want one row (%v)", todos, raw[6])
	}
}
