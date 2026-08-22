package execution

import (
	"encoding/json"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// latestTodos returns the worker's most recent todo list for an execution by
// walking its session-transcript tool_use parts from the highest sequence
// downward and returning the FIRST todowrite payload's full todos array.
//
// Parts are passed in DESC-by-seq order (the shape LatestToolUseParts
// returns), so the first todowrite payload is the LATEST recorded list —
// later todowrite calls fully replace earlier ones (replacement semantics),
// and a terminal execution naturally falls back to the last recorded list.
//
// The parser is deliberately tolerant: non-todowrite tool parts, parts with
// missing/malformed payloads, and todowrite parts with a missing or
// non-array todos field are all skipped rather than failing the parse — a
// single malformed part must never break the RPC. Returns nil when no
// todowrite call was ever recorded (or the transcript is empty).
func latestTodos(parts []db.SessionPart) []*apiv1.TodoItem {
	for _, p := range parts {
		if p.Kind != db.SessionPartToolUse {
			continue
		}
		raw, ok := todoItemsFromPayload(p.Payload)
		if !ok {
			continue
		}
		out := make([]*apiv1.TodoItem, 0, len(raw))
		for _, t := range raw {
			out = append(out, &apiv1.TodoItem{
				Content:  t.Content,
				Status:   todoStatusFromString(t.Status),
				Priority: todoPriorityFromString(t.Priority),
			})
		}
		return out
	}
	return nil
}

// rawTodo is the minimal shape of one todowrite todo entry. The parser keeps
// status/priority as raw strings and maps them to the proto enums with
// forward-compatible fallbacks (unknown values → UNSPECIFIED).
type rawTodo struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// todoItemsFromPayload decodes a persisted tool_use part payload
// (`{"part": <opencode part>, "error": ...}`) and returns the todos array of
// the part IF it is a completed todowrite call. ok=false means "not a usable
// todowrite payload" (different tool, missing part, missing todos, malformed
// JSON) — the caller skips it.
func todoItemsFromPayload(payload []byte) ([]rawTodo, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	var env struct {
		Part json.RawMessage `json:"part"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || len(env.Part) == 0 {
		return nil, false
	}
	var part struct {
		Tool  string          `json:"tool"`
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(env.Part, &part); err != nil || part.Tool != "todowrite" {
		return nil, false
	}
	if len(part.State) == 0 {
		return nil, false
	}
	var state struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(part.State, &state); err != nil || len(state.Input) == 0 {
		return nil, false
	}
	var input struct {
		Todos json.RawMessage `json:"todos"`
	}
	if err := json.Unmarshal(state.Input, &input); err != nil || len(input.Todos) == 0 {
		return nil, false
	}
	var todos []rawTodo
	if err := json.Unmarshal(input.Todos, &todos); err != nil || todos == nil {
		return nil, false
	}
	return todos, true
}

// todoStatusFromString maps a todowrite status string to the proto enum.
// Unknown values map to UNSPECIFIED (forward-compatible: the frontend renders
// those as pending/dimmed rather than failing).
func todoStatusFromString(s string) apiv1.TodoStatus {
	switch s {
	case "pending":
		return apiv1.TodoStatus_TODO_STATUS_PENDING
	case "in_progress":
		return apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS
	case "completed":
		return apiv1.TodoStatus_TODO_STATUS_COMPLETED
	case "cancelled":
		return apiv1.TodoStatus_TODO_STATUS_CANCELLED
	default:
		return apiv1.TodoStatus_TODO_STATUS_UNSPECIFIED
	}
}

// todoPriorityFromString maps a todowrite priority string to the proto enum.
// Unknown values map to UNSPECIFIED.
func todoPriorityFromString(s string) apiv1.TodoPriority {
	switch s {
	case "high":
		return apiv1.TodoPriority_TODO_PRIORITY_HIGH
	case "medium":
		return apiv1.TodoPriority_TODO_PRIORITY_MEDIUM
	case "low":
		return apiv1.TodoPriority_TODO_PRIORITY_LOW
	default:
		return apiv1.TodoPriority_TODO_PRIORITY_UNSPECIFIED
	}
}