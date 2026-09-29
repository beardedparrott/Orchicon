package execution

// todos_claude_fixture_test.go — AC2/AC3: a claude task list must be readable by
// the ONE existing parse site. These fixtures are the EXACT durable envelopes a
// claude TaskCreate/TaskUpdate sequence persists (produced by the claude mapper's
// emitTodos, byte-shape-identical to opencode's todowrite part); latestTodos must
// read them with no adapter-specific branch. There is no second parser.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

func toolUsePart(payload string) db.SessionPart {
	return db.SessionPart{Kind: db.SessionPartToolUse, Payload: []byte(payload)}
}

func TestLatestTodosReadsClaudeTaskEnvelope(t *testing.T) {
	// DESC-by-seq order, exactly what db.LatestToolUseParts returns.
	parts := []db.SessionPart{
		// new
		toolUsePart(`{"part":{"type":"tool_use","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"alpha","status":"in_progress","priority":"high"},{"content":"beta","status":"pending","priority":""}]}}},"error":null}`),
		// an unrelated claude tool part (a TaskCreate passthrough) is skipped.
		toolUsePart(`{"part":{"type":"tool_use","tool":"read","state":{"status":"completed"}},"error":null}`),
		// the previous list (older) — must be ignored.
		toolUsePart(`{"part":{"type":"tool_use","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"stale","status":"pending"}]}}},"error":null}`),
	}

	got := latestTodos(parts)
	if len(got) != 2 {
		t.Fatalf("latestTodos = %+v, want the two claude items", got)
	}
	if got[0].Content != "alpha" || got[0].Status != apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS || got[0].Priority != apiv1.TodoPriority_TODO_PRIORITY_HIGH {
		t.Errorf("todos[0] = %+v, want alpha/in_progress/high", got[0])
	}
	if got[1].Content != "beta" || got[1].Status != apiv1.TodoStatus_TODO_STATUS_PENDING {
		t.Errorf("todos[1] = %+v, want beta/pending", got[1])
	}
}

// A claude REMOVAL re-emits a shorter list; the parse site must surface the
// shorter list (the removed row is gone rather than stale).
func TestLatestTodosReflectsAClaudeRemoval(t *testing.T) {
	parts := []db.SessionPart{
		toolUsePart(`{"part":{"type":"tool_use","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"survivor","status":"completed"}]}}},"error":null}`),
		toolUsePart(`{"part":{"type":"tool_use","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"survivor","status":"pending"},{"content":"removed","status":"pending"}]}}},"error":null}`),
	}
	got := latestTodos(parts)
	if len(got) != 1 || got[0].Content != "survivor" {
		t.Fatalf("latestTodos = %+v, want only the survivor", got)
	}
}

// An empty todowrite payload (every row removed) yields an EMPTY list, never a
// stale one and never nil-with-error.
func TestLatestTodosEmptyClaudeList(t *testing.T) {
	parts := []db.SessionPart{
		toolUsePart(`{"part":{"type":"tool_use","tool":"todowrite","state":{"status":"completed","input":{"todos":[]}}},"error":null}`),
	}
	got := latestTodos(parts)
	if len(got) != 0 {
		t.Fatalf("latestTodos = %+v, want an empty list", got)
	}
}
