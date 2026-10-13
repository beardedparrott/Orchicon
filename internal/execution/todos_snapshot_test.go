package execution

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/worktree"
)

// TestTodosFromSnapshotMapsTheProtoShape pins the run-state source's mapping.
//
// It is deliberately asserted through the SAME status/priority mappers the
// transcript parser uses: the panel must not render the same list differently
// depending on which source answered, and two independent mapping tables is
// exactly how that drifts.
func TestTodosFromSnapshotMapsTheProtoShape(t *testing.T) {
	snap := []worktree.TodoItem{
		{Content: "first", Status: "in_progress", Priority: "high"},
		{Content: "second", Status: "completed", Priority: "medium"},
		// An unknown status must degrade to UNSPECIFIED, not fail the render — the
		// tool accepts whatever the model sends, so the panel has to tolerate it.
		{Content: "third", Status: "banana", Priority: ""},
	}
	got := todosFromSnapshot(snap)
	if len(got) != 3 {
		t.Fatalf("todosFromSnapshot returned %d items, want 3", len(got))
	}
	if got[0].GetContent() != "first" || got[0].GetStatus() != apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS ||
		got[0].GetPriority() != apiv1.TodoPriority_TODO_PRIORITY_HIGH {
		t.Errorf("item 0 = %+v, want first/in_progress/high", got[0])
	}
	if got[1].GetStatus() != apiv1.TodoStatus_TODO_STATUS_COMPLETED {
		t.Errorf("item 1 status = %v, want completed", got[1].GetStatus())
	}
	if got[2].GetStatus() != apiv1.TodoStatus_TODO_STATUS_UNSPECIFIED {
		t.Errorf("an unknown status must map to UNSPECIFIED, got %v", got[2].GetStatus())
	}
	if got := todosFromSnapshot(nil); got != nil {
		t.Errorf("an empty snapshot must yield nil, got %+v", got)
	}
}
