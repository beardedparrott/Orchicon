package execution

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// This test is the claude leg of the todo-panel parity criterion: the payload
// the claude Mapper persists for a `TodoWrite` must parse through the SAME
// unexported todoItemsFromPayload the RPC path uses, with ZERO
// adapter-specific branch. The payloads below are built EXACTLY the way
// internal/claude/parse.go emits them ({"part": …} envelope, canonical tool
// name "todowrite", part.state.input.todos), so if the shared parser ever
// grows an adapter branch this test starts failing.

// claudeTodoEnvelope reproduces the exact durable part claude's mapper
// writes for a TodoWrite tool_use block.
func claudeTodoEnvelope(t *testing.T, items []worktree.TodoItem) []byte {
	t.Helper()
	env := map[string]any{
		"part": map[string]any{
			"type": "tool_use",
			"tool": "todowrite",
			"state": map[string]any{
				"status": "running",
				"input":  map[string]any{"todos": items},
			},
		},
		"error": nil,
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

func TestClaudeTodoPayloadParsesWithoutAdapterBranch(t *testing.T) {
	items := []worktree.TodoItem{
		{Content: "step 1", Status: "in_progress", Priority: "high"},
		{Content: "step 2", Status: "pending", Priority: "low"},
	}
	payload := claudeTodoEnvelope(t, items)

	got, ok := todoItemsFromPayload(payload)
	if !ok {
		t.Fatalf("claude todo payload did not parse: %s", payload)
	}
	if len(got) != 2 {
		t.Fatalf("todos = %+v, want 2", got)
	}
	if got[0].Content != "step 1" || got[0].Status != "in_progress" || got[0].Priority != "high" {
		t.Errorf("todos[0] = %+v", got[0])
	}
	if got[1].Content != "step 2" || got[1].Status != "pending" || got[1].Priority != "low" {
		t.Errorf("todos[1] = %+v", got[1])
	}
}

// TestClaudeTodosReachLatestTodos proves the full RPC walk (latestTodos over
// kind='tool_use' parts, DESC by seq) surfaces the claude list. The
// tool_result part claude writes at the turn boundary carries no
// state.input.todos and must simply be skipped, not shadow the list.
func TestClaudeTodosReachLatestTodos(t *testing.T) {
	running := db.SessionPart{Kind: db.SessionPartToolUse, Payload: claudeTodoEnvelope(t, []worktree.TodoItem{
		{Content: "step 1", Status: "in_progress", Priority: "high"},
	})}
	completed, err := json.Marshal(map[string]any{
		"part": map[string]any{
			"type": "tool_use",
			"tool": "todowrite",
			"state": map[string]any{
				"status": "completed",
				"output": "Todos have been modified successfully",
			},
		},
		"error": nil,
	})
	if err != nil {
		t.Fatalf("marshal completed part: %v", err)
	}
	// DESC-by-seq order: the resolved tool_result part comes first.
	parts := []db.SessionPart{
		{Kind: db.SessionPartToolUse, Payload: completed},
		running,
	}
	todos := latestTodos(parts)
	if len(todos) != 1 {
		t.Fatalf("latestTodos = %+v, want the claude todo list", todos)
	}
	if todos[0].Content != "step 1" {
		t.Errorf("todo content = %q", todos[0].Content)
	}
}

// TestClaudeNonTodoPartsAreSkipped pins that ordinary claude tool_use parts
// (which share the same envelope) never produce a todo list.
func TestClaudeNonTodoPartsAreSkipped(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"part": map[string]any{
			"type":  "tool_use",
			"tool":  "write",
			"state": map[string]any{"status": "running", "input": map[string]any{"file_path": "a.md"}},
		},
		"error": nil,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, ok := todoItemsFromPayload(payload); ok {
		t.Fatal("a non-todowrite claude part must not parse as a todo list")
	}
	if todos := latestTodos([]db.SessionPart{{Kind: db.SessionPartToolUse, Payload: payload}}); todos != nil {
		t.Fatalf("latestTodos = %+v, want nil", todos)
	}
}
