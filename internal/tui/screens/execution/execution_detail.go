package execution

// execution_detail.go — the Executions pane's detail: the facts the GUI shows about a run, the
// worker's TODO LIST, and the follow-up box.
//
// The operator: "Executions should definitely mimic the GUI as much as possible in the detail pane
// and also have the nudge/follow up box including the full recorded or live stream of the worker
// and context, token, cost, tool, todo list etc. information just like in the GUI (within reason
// of the limitation of a text interface of course)."
//
// What already existed and is NOT rebuilt here: the merged TRANSCRIPT (durable
// GetExecutionSession parts + live StreamExecutionEvents, phase-grouped via chat.MergeSessionItems
// — the same merge the GUI's SessionChatPane does) and the NUDGE (the `i` interject form, which
// calls SendExecutionMessage exactly as the GUI's composer does).
//
// What was missing and is added here: the run's own facts beyond id/status/tokens (the GUI's
// context strip), the TODO LIST (GetExecutionTodos), and the FOLLOW-UP path
// (ContinueExecutionSession — the GUI's composer on a session that is no longer live).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// execTodosMsg carries a fetched todo list.
type execTodosMsg struct {
	execID string
	todos  []*apiv1.TodoItem
	err    error
}

// execFollowUpMsg carries a one-shot follow-up reply.
type execFollowUpMsg struct {
	execID string
	reply  string
	err    error
}

// todosCache holds each execution's todo list. A todo list changes only when the worker writes a
// new one, so it is re-read on the detail fetch (which already polls) rather than on a timer of
// its own.
type todosCache struct {
	mu sync.Mutex
	m  map[string][]*apiv1.TodoItem
}

func (c *todosCache) put(id string, todos []*apiv1.TodoItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]*apiv1.TodoItem{}
	}
	c.m[id] = todos
}

func (c *todosCache) get(id string) []*apiv1.TodoItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id]
}

// loadTodos fetches an execution's todo list. Best effort: a failure leaves the previous list
// alone and never turns "open an execution" into an error — the list is context, not the subject.
func (m *Model) loadTodos(execID string) tea.Cmd {
	if m.cl == nil || m.cl.Executions == nil || execID == "" {
		return nil
	}
	client := m.cl.Executions
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		resp, err := client.GetExecutionTodos(ctx, connect.NewRequest(&apiv1.GetExecutionTodosRequest{ExecutionId: execID}))
		if err != nil {
			return execTodosMsg{execID: execID, err: err}
		}
		return execTodosMsg{execID: execID, todos: resp.Msg.GetTodos()}
	}
}

// renderTodos draws the worker's todo list as a section of the detail body.
//
// The marks mirror the opencode statuses the proto carries, so the list reads the same way here
// as in the GUI and in the worker's own view of its todos.
func renderTodos(todos []*apiv1.TodoItem, width int) string {
	if len(todos) == 0 {
		return ""
	}
	done := 0
	for _, t := range todos {
		if t.GetStatus() == apiv1.TodoStatus_TODO_STATUS_COMPLETED {
			done++
		}
	}
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render(fmt.Sprintf("todo (%d/%d done)", done, len(todos))) + "\n")
	for _, t := range todos {
		mark := "[ ]"
		switch t.GetStatus() {
		case apiv1.TodoStatus_TODO_STATUS_COMPLETED:
			mark = "[x]"
		case apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS:
			mark = "[~]"
		case apiv1.TodoStatus_TODO_STATUS_CANCELLED:
			mark = "[-]"
		}
		line := "  " + mark + " " + t.GetContent()
		// Priority is shown only when it is HIGH: a list where every row carries a tag is a list
		// where the tags stop meaning anything.
		if t.GetPriority() == apiv1.TodoPriority_TODO_PRIORITY_HIGH {
			line += "  (!)"
		}
		if t.GetStatus() == apiv1.TodoStatus_TODO_STATUS_COMPLETED {
			// A completed item is dimmed rather than removed: seeing what was done is the point of
			// a list, and removing rows makes the pane jump around while it is being watched.
			b.WriteString(theme.HintText.Render(line) + "\n")
			continue
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- the run's facts ---------------------------------------------------------

// executionFields is the GUI's context strip as a text detail pane: who ran it, on what, where,
// for how much.
//
// Every field is OMITTED when it carries nothing, rather than rendered blank or as a zero the
// operator has to interpret: an execution with no worktree is not "worktree: (empty)", it simply
// did not use one.
func executionFields(e *apiv1.WorkerExecution, meta string) []screenkit.Field {
	fields := []screenkit.Field{{Key: "id", Value: e.GetId()}}
	if meta != "" {
		fields = append(fields, screenkit.Field{Key: "status", Value: meta})
	}
	if h := strings.ToLower(strings.TrimPrefix(e.GetHealthState().String(), "HEALTH_STATE_")); h != "" && h != "unspecified" {
		fields = append(fields, screenkit.Field{Key: "health", Value: h})
	}
	// Worker: the NAME the operator recognises, with the id kept for traceability (the same
	// answer the runs pane gives for workflow and work-item names).
	switch {
	case e.GetWorkerName() != "":
		fields = append(fields, screenkit.Field{Key: "worker", Value: e.GetWorkerName() + "  (" + e.GetWorkerId() + ")"})
	case e.GetWorkerId() != "":
		fields = append(fields, screenkit.Field{Key: "worker", Value: e.GetWorkerId()})
	}
	if e.GetWorkflowName() != "" {
		fields = append(fields, screenkit.Field{Key: "workflow", Value: e.GetWorkflowName()})
	}
	// The bound TICKET: WorkerExecution carries TaskId; the work item is the task. Named "work
	// item" because that is what the operator calls it everywhere else in the TUI.
	if e.GetTaskId() != "" {
		fields = append(fields, screenkit.Field{Key: "work item", Value: e.GetTaskId()})
	}
	if e.GetIteration() > 0 {
		fields = append(fields, screenkit.Field{Key: "iteration", Value: screenkit.FmtInt(int(e.GetIteration()))})
	}
	fields = append(fields, screenkit.Field{Key: "tokens", Value: screenkit.FmtInt64(e.GetTokenUsage())})
	if e.GetCostUsd() > 0 {
		fields = append(fields, screenkit.Field{Key: "cost", Value: fmt.Sprintf("$%.4f", e.GetCostUsd())})
	}
	if e.GetWorktreeBranch() != "" {
		branch := e.GetWorktreeBranch()
		if e.GetWorktreeStatus() != "" {
			branch += "  (" + e.GetWorktreeStatus() + ")"
		}
		fields = append(fields, screenkit.Field{Key: "branch", Value: branch})
	}
	if e.GetPrUrl() != "" {
		pr := e.GetPrUrl()
		if e.GetPrState() != "" {
			pr += "  (" + e.GetPrState() + ")"
		}
		fields = append(fields, screenkit.Field{Key: "pr", Value: pr})
	}
	fields = append(fields,
		screenkit.Field{Key: "started", Value: screenkit.FmtTime(e.GetStartedAt())},
		screenkit.Field{Key: "ended", Value: screenkit.FmtTime(e.GetEndedAt())},
	)
	if e.GetErrorMessage() != "" {
		fields = append(fields, screenkit.Field{Key: "error", Value: truncateForField(e.GetErrorMessage(), 200)})
	}
	return fields
}

// truncateForField keeps a long value from swallowing the pane. The FULL text stays in the body,
// so nothing is lost — the field is a signpost, not the content.
func truncateForField(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// --- the follow-up -----------------------------------------------------------

// beginFollowUp opens the follow-up box for a session that is no longer LIVE.
//
// The GUI's composer does both jobs on one control: nudge a running session (SendExecutionMessage)
// or follow up on a finished one (ContinueExecutionSession). The TUI already had the nudge (`i`);
// this is its complement, and it is a SEPARATE chord because the two are different acts with
// different preconditions — a nudge needs a live session, a follow-up needs a session that still
// exists.
func (m *Model) beginFollowUp(execID string) tea.Cmd {
	f := kit2.NewForm("Follow up on "+execID,
		kit2.FieldSpec{Name: "message", Label: "Question about this run", Kind: kit2.KTextArea, Required: true,
			Placeholder: "ask about what it did — the reply joins the session transcript, no new execution"})
	f.Focused = true
	f.Width = 66
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		msg := strings.TrimSpace(v["message"])
		id := execID
		return m.Mutate(mutate.Request{
			Name: "follow up on " + id, Source: srcExecutions,
			Do: func(ctx context.Context) error {
				resp, err := m.cl.Executions.ContinueExecutionSession(ctx, connect.NewRequest(&apiv1.ContinueExecutionSessionRequest{
					ExecutionId: id, Message: msg,
				}))
				if err != nil {
					return err
				}
				// The reply is shown in the notice, which is the one surface the shell never
				// truncates — a follow-up's answer is the whole point of asking, and the body is
				// re-read from the transcript anyway.
				reply := strings.TrimSpace(resp.Msg.GetReply())
				if reply == "" {
					reply = "(the model returned an empty reply)"
				}
				m.notice = "follow-up reply: " + reply
				return nil
			},
		}), nil
	}
	m.form = f
	m.notice = ""
	return nil
}

// followUpAvailable reports why a follow-up cannot run, or "" when it can. The precondition is
// the SESSION, not the run: a follow-up re-attaches to a session that still exists.
func (m *Model) followUpAvailable(meta string) string {
	if m.cl == nil || m.cl.Executions == nil {
		return "no execution client"
	}
	if meta == "" {
		return "select an execution first"
	}
	return ""
}
