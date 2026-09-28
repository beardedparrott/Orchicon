package execution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/claude"
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

// claudeParityNoop is a do-nothing ExecutionCallbacks: the parity assertion
// below only cares about what the mapper PERSISTS, so the live-callback fan-out
// is deliberately inert.
type claudeParityNoop struct{}

func (claudeParityNoop) OnStarted(context.Context, string)                          {}
func (claudeParityNoop) OnResult(context.Context, string, bool, string, string)     {}
func (claudeParityNoop) OnWrittenFiles(context.Context, string, []string)           {}
func (claudeParityNoop) OnHealth(context.Context, string, string)                   {}
func (claudeParityNoop) OnStall(context.Context, string, string, bool)              {}
func (claudeParityNoop) OnRecovered(context.Context, string, string)                {}
func (claudeParityNoop) OnToolCall(context.Context, string, string, []byte, []byte) {}
func (claudeParityNoop) OnText(context.Context, string, string)                     {}
func (claudeParityNoop) OnArtifact(context.Context, string, string, string, string) {}

// TestClaudeMapperPayloadParsesWithoutAdapterBranch closes the loop the
// hand-mirrored claudeTodoEnvelope above deliberately leaves open: it drives
// the REAL claude Mapper over a canned stream-json line and feeds the payload
// the mapper actually persists into the SAME shared parser the RPC path uses.
// A drift in parse.go's durable envelope (rename the tool, move the todos
// under a different key, nest the state differently) therefore fails HERE,
// not just in a mirror that would silently keep matching itself.
func TestClaudeMapperPayloadParsesWithoutAdapterBranch(t *testing.T) {
	var (
		captured []byte
		kind     string
	)
	m := claude.NewMapper("exec-mapper-todo-parity", claudeParityNoop{}, claude.MapperDeps{
		ExecDir: t.TempDir(),
		Record: func(_ context.Context, k string, payload map[string]any) {
			if k != db.SessionPartToolUse {
				return
			}
			b, err := json.Marshal(payload)
			if err != nil {
				return
			}
			captured = b
			kind = k
		},
	})
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"td-1","name":"TodoWrite","input":{"todos":[{"content":"step 1","status":"in_progress","priority":"high","activeForm":"stepping"},{"content":"step 2","status":"","task_id":"2"}]}}]}}`)
	ev, err := claude.ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	m.Handle(context.Background(), ev)
	if captured == nil {
		t.Fatal("the mapper persisted no tool_use part for the TodoWrite block")
	}

	// The mapper's own payload must decode through the shared parser.
	got, ok := todoItemsFromPayload(captured)
	if !ok {
		t.Fatalf("the mapper's persisted claude todo payload does not parse: %s", captured)
	}
	if len(got) != 2 || got[0].Content != "step 1" || got[1].Content != "step 2" {
		t.Fatalf("parser read %+v, want step 1 + step 2", got)
	}
	// Empty streamed status still lands as pending (claude omits it).
	if got[1].Status != "pending" {
		t.Errorf("todos[1].Status = %q, want pending", got[1].Status)
	}

	// …and through the full RPC walk (DESC by seq) with zero adapter branch.
	todos := latestTodos([]db.SessionPart{{Kind: kind, Seq: 1, Payload: captured}})
	if len(todos) != 2 {
		t.Fatalf("latestTodos = %+v, want the claude list", todos)
	}
	if todos[0].Content != "step 1" {
		t.Errorf("latestTodos[0].Content = %q", todos[0].Content)
	}
}
