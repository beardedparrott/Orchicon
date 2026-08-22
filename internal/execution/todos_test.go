package execution

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// toolPart builds a persisted tool_use part payload in the exact shape the
// session runner records: `{"part": <opencode part>, "error": ...}` with the
// opencode part carrying the tool name and its state.
func toolPart(tool string, status string, state map[string]any) []byte {
	part := map[string]any{
		"tool":   tool,
		"callID": "call-" + tool,
	}
	if state != nil {
		part["state"] = state
	}
	payload, _ := json.Marshal(map[string]any{
		"part":  part,
		"error": nil,
	})
	return payload
}

// todowritePart builds a completed todowrite tool_use payload with the given
// raw todos array.
func todowritePart(todos []any) []byte {
	return toolPart("todowrite", "completed", map[string]any{
		"status": "completed",
		"input":  map[string]any{"todos": todos},
	})
}

func item(content, status, priority string) any {
	return map[string]any{
		"content":  content,
		"status":   status,
		"priority": priority,
	}
}

func sessionParts(kind string, seq int64, payload []byte) db.SessionPart {
	return db.SessionPart{Kind: kind, Seq: seq, Payload: payload}
}

// TestLatestTodosParsesCorrectPayload verifies the happy path: a single
// todowrite part yields its full todos array with statuses/priorities mapped
// to the proto enums.
func TestLatestTodosParsesCorrectPayload(t *testing.T) {
	payload := todowritePart([]any{
		item("Read the acceptance criteria", "completed", "high"),
		item("Implement the RPC", "in_progress", "high"),
		item("Add frontend card", "pending", "medium"),
	})
	parts := []db.SessionPart{sessionParts(db.SessionPartToolUse, 3, payload)}
	got := latestTodos(parts)
	if len(got) != 3 {
		t.Fatalf("got %d todos, want 3", len(got))
	}
	if got[0].Content != "Read the acceptance criteria" || got[0].Status != apiv1.TodoStatus_TODO_STATUS_COMPLETED || got[0].Priority != apiv1.TodoPriority_TODO_PRIORITY_HIGH {
		t.Errorf("todo[0] = %+v, want completed/high", got[0])
	}
	if got[1].Content != "Implement the RPC" || got[1].Status != apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS || got[1].Priority != apiv1.TodoPriority_TODO_PRIORITY_HIGH {
		t.Errorf("todo[1] = %+v, want in_progress/high", got[1])
	}
	if got[2].Content != "Add frontend card" || got[2].Status != apiv1.TodoStatus_TODO_STATUS_PENDING || got[2].Priority != apiv1.TodoPriority_TODO_PRIORITY_MEDIUM {
		t.Errorf("todo[2] = %+v, want pending/medium", got[2])
	}
}

// TestLatestTodosOrderingBySeq verifies "latest wins": when multiple
// todowrite calls exist, the one with the highest sequence number is
// returned (replacement semantics — the newest call supersedes older ones).
// Parts are passed in DESC-by-seq order (the LatestToolUseParts contract).
func TestLatestTodosOrderingBySeq(t *testing.T) {
	parts := []db.SessionPart{
		sessionParts(db.SessionPartToolUse, 8, todowritePart([]any{
			item("New item A", "in_progress", "high"),
			item("New item B", "pending", "low"),
		})),
		sessionParts(db.SessionPartToolUse, 3, []byte(`{"part":{"text":"thinking"}}`)),
		sessionParts(db.SessionPartToolUse, 2, todowritePart([]any{
			item("Old item", "completed", "high"),
		})),
	}
	got := latestTodos(parts)
	if len(got) != 2 {
		t.Fatalf("got %d todos, want 2 (the latest todowrite call)", len(got))
	}
	if got[0].Content != "New item A" || got[0].Status != apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS {
		t.Errorf("expected the newest list, got %+v", got[0])
	}
	if got[1].Content != "New item B" || got[1].Priority != apiv1.TodoPriority_TODO_PRIORITY_LOW {
		t.Errorf("expected the newest list, got %+v", got[1])
	}
}

// TestLatestTodosMixedTranscript verifies a transcript with other tools
// interspersed: the parser skips non-todowrite tool calls and returns the
// first todowrite payload it finds walking from the highest seq.
func TestLatestTodosMixedTranscript(t *testing.T) {
	parts := []db.SessionPart{
		sessionParts(db.SessionPartToolUse, 4, toolPart("edit", "completed", map[string]any{"status": "completed", "input": map[string]any{"filePath": "x.go"}})),
		sessionParts(db.SessionPartToolUse, 3, todowritePart([]any{
			item("Do the thing", "pending", "medium"),
		})),
		sessionParts(db.SessionPartToolUse, 2, toolPart("bash", "completed", map[string]any{"status": "completed", "input": map[string]any{"command": "git status"}})),
		sessionParts(db.SessionPartToolUse, 1, toolPart("read", "completed", map[string]any{"status": "completed", "input": map[string]any{"filePath": "x.go"}})),
	}
	got := latestTodos(parts)
	if len(got) != 1 || got[0].Content != "Do the thing" {
		t.Fatalf("got %+v, want the single todo from the interspersed transcript", got)
	}
}

// TestLatestTodosEmptyTranscript verifies an empty transcript (or a
// transcript with no tool_use parts) returns nil without error.
func TestLatestTodosEmptyTranscript(t *testing.T) {
	if got := latestTodos(nil); got != nil {
		t.Errorf("nil parts should yield nil todos, got %+v", got)
	}
	parts := []db.SessionPart{
		sessionParts(db.SessionPartText, 1, []byte(`{"part":{"text":"hello"}}`)),
		sessionParts(db.SessionPartUserMessage, 2, []byte(`{"text":"hi"}`)),
	}
	if got := latestTodos(parts); got != nil {
		t.Errorf("no tool parts should yield nil todos, got %+v", got)
	}
}

// TestLatestTodosMalformedParts verifies the parser never fails on malformed
// payloads: junk JSON, missing part, missing state, missing input, missing
// todos, and a non-array todos are all skipped. A later valid part must
// still be found. (An EMPTY todos array is a valid todowrite call — the
// worker explicitly cleared the list — so it is not treated as malformed.)
func TestLatestTodosMalformedParts(t *testing.T) {
	parts := []db.SessionPart{
		sessionParts(db.SessionPartToolUse, 8, todowritePart([]any{
			item("Only valid one", "completed", "low"),
		})),
		sessionParts(db.SessionPartToolUse, 7, toolPart("todowrite", "completed", map[string]any{"status": "completed", "input": map[string]any{"todos": "oops"}})), // todos not an array
		sessionParts(db.SessionPartToolUse, 6, toolPart("todowrite", "completed", map[string]any{"status": "completed", "input": map[string]any{}})),            // no todos
		sessionParts(db.SessionPartToolUse, 5, toolPart("todowrite", "completed", map[string]any{"status": "completed"})),                                       // no input
		sessionParts(db.SessionPartToolUse, 4, toolPart("todowrite", "completed", map[string]any{})),                                                             // empty state
		sessionParts(db.SessionPartToolUse, 3, []byte(`{"part":{"tool":"todowrite"}}`)), // no state
		sessionParts(db.SessionPartToolUse, 2, []byte(`{"part":123}`)),
		sessionParts(db.SessionPartToolUse, 1, []byte("not json")),
	}
	got := latestTodos(parts)
	if len(got) != 1 || got[0].Content != "Only valid one" {
		t.Fatalf("got %+v, want the single valid todo after malformed parts", got)
	}
}

// TestLatestTodosEmptyArrayIsValid verifies an EMPTY todos array is a valid
// todowrite call: the worker cleared the list, so the latest list is empty
// (not nil). The frontend hides the card for an empty list, so the UX is
// "no todos shown" either way.
func TestLatestTodosEmptyArrayIsValid(t *testing.T) {
	parts := []db.SessionPart{
		sessionParts(db.SessionPartToolUse, 2, todowritePart([]any{})),
		sessionParts(db.SessionPartToolUse, 1, todowritePart([]any{
			item("Older item", "completed", "high"),
		})),
	}
	got := latestTodos(parts)
	if got == nil {
		t.Fatal("an empty todowrite call should yield an empty (non-nil) list, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d todos", len(got))
	}
}

// TestLatestTodosUnknownStatusPriority verifies forward compatibility:
// unknown status/priority strings map to UNSPECIFIED (never an error), and a
// todowrite part with an error status is still parsed (the todos payload is
// authoritative).
func TestLatestTodosUnknownStatusPriority(t *testing.T) {
	payload := toolPart("todowrite", "error", map[string]any{
		"status": "error",
		"input": map[string]any{"todos": []any{
			map[string]any{"content": "weird status", "status": "paused", "priority": "urgent"},
		}},
	})
	got := latestTodos([]db.SessionPart{sessionParts(db.SessionPartToolUse, 1, payload)})
	if len(got) != 1 {
		t.Fatalf("got %d todos, want 1", len(got))
	}
	if got[0].Status != apiv1.TodoStatus_TODO_STATUS_UNSPECIFIED {
		t.Errorf("unknown status should map to UNSPECIFIED, got %v", got[0].Status)
	}
	if got[0].Priority != apiv1.TodoPriority_TODO_PRIORITY_UNSPECIFIED {
		t.Errorf("unknown priority should map to UNSPECIFIED, got %v", got[0].Priority)
	}
}