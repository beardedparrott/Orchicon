package execution

// execution_detail.go — the Executions pane's detail: the facts the GUI shows about a run, the
// worker's TODO LIST, and the follow-up box.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
	"github.com/beardedparrott/orchicon/internal/verify"
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

// todosCache holds each execution's todo list, and WHEN each was fetched.
type todosCache struct {
	mu sync.Mutex
	m  map[string][]*apiv1.TodoItem
	at map[string]time.Time
}

func (c *todosCache) put(id string, todos []*apiv1.TodoItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]*apiv1.TodoItem{}
		c.at = map[string]time.Time{}
	}
	c.m[id] = todos
	c.at[id] = time.Now()
}

func (c *todosCache) get(id string) []*apiv1.TodoItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id]
}

// fetchedAt reports when an execution's list was last fetched, and whether it ever was.
func (c *todosCache) fetchedAt(id string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.at[id]
	return t, ok
}

// todosTTL is how long a cached todo list is drawn before it is re-read.
const todosTTL = 10 * time.Second

// execDetailState is one execution's detail, held as its SEPARATE writers rather than as one
// rendered string.
type execDetailState struct {
	mu   sync.Mutex
	byID map[string]*execDetailParts
}

// execDetailParts is one execution's record plus its session ITEMS.
type execDetailParts struct {
	facts  []screenkit.Field
	record string // error + output, already styled
	items  []chat.ChatItem
}

// put records an execution's fetched facts and record.
func (s *execDetailState) put(id string, e *apiv1.WorkerExecution, facts []screenkit.Field) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[string]*execDetailParts{}
	}
	p, ok := s.byID[id]
	if !ok {
		p = &execDetailParts{}
		s.byID[id] = p
	}
	p.facts = facts
	p.record = renderExecutionRecord(e)
}

// putTranscript records the merged session ITEMS, leaving the record alone.
func (s *execDetailState) putTranscript(id string, items []chat.ChatItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[string]*execDetailParts{}
	}
	p, ok := s.byID[id]
	if !ok {
		p = &execDetailParts{}
		s.byID[id] = p
	}
	p.items = items
}

// blockItems returns an execution's session items, for callers that need the CURRENT transcript
// length (the cursor clamp and the toggle both do).
func (s *execDetailState) blockItems(id string) []chat.ChatItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil
	}
	return p.items
}

// parts returns a COPY of an execution's parts (nil when nothing has arrived).
func (s *execDetailState) parts(id string) *execDetailParts {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return nil
	}
	cp := *p
	return &cp
}

// renderExecutionRecord renders the run's own prose: the error, then the output.
func renderExecutionRecord(e *apiv1.WorkerExecution) string {
	var b strings.Builder
	if msg := e.GetErrorMessage(); msg != "" {
		if verify.IsNotice(msg) {
			// The notice already carries its own sentence; strip the internal marker and label it neutrally.
			b.WriteString(theme.HintText.Render("additional info: "+
				strings.TrimPrefix(strings.TrimSpace(msg), verify.NoticePrefix)) + "\n\n")
		} else {
			b.WriteString(theme.ErrorText.Render("error: "+msg) + "\n\n")
		}
	}
	if out := strings.TrimSpace(e.GetOutput()); out != "" {
		b.WriteString(theme.ListTitle.Render("output") + "\n" + out + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// composeExecutionBodyFor composes the pane body for an execution whose parts are already stored,
// falling back to the empty string when nothing has arrived yet.
func (m *Model) composeExecutionBodyFor(id string) string {
	body, _ := m.composeExecutionBody(id)
	return body
}

// composeExecutionBody composes the execution pane's body and fields from its parts, in reading
// order: the todo list (context for reading the transcript), then the record (error/output).
func (m *Model) composeExecutionBody(id string) (string, []screenkit.Field) {
	p := m.execDetail.parts(id)
	if p == nil {
		return "", nil
	}
	sections := []string{}
	if u := m.execUsage.get(id); u != nil {
		if ctxSection := renderUsage(u, m.w); ctxSection != "" {
			sections = append(sections, ctxSection)
		}
	}
	if todo := renderTodos(m.todos.get(id), m.w); todo != "" {
		sections = append(sections, todo)
	}
	if p.record != "" {
		sections = append(sections, p.record)
	}
	// The transcript is drawn as COLLAPSIBLE BLOCKS with the cursor, and the COMPOSER is its last
	// position — so walking down past the last block reaches the input box (execution_blocks.go).
	if len(p.items) > 0 {
		blocks := blocksFromItems(p.items, m.w)
		m.blocks.reset(id)
		m.clampBlockCursor(len(blocks))
		if body, _ := renderBlocks(blocks, &m.blocks, m.w, m.blocks.cursor); body != "" {
			sections = append(sections, body)
		}
	}
	return strings.Join(sections, "\n\n"), p.facts
}

// execMeta is the execution's list-row status, which decides what the composer will DO when it is
// submitted (message a live worker vs ask a follow-up on a finished one).
func (m *Model) execMeta(id string) string {
	if it, ok := m.Base.SourceItem(srcExecutions, id); ok {
		return it.Meta
	}
	return ""
}

// clampBlockCursor keeps the cursor inside the transcript's CURRENT length: a live transcript
// grows, and a re-fetch can shrink one.
func (m *Model) clampBlockCursor(n int) {
	if m.blocks.cursor.idx >= n {
		m.blocks.cursor.idx = n - 1
	}
	if m.blocks.cursor.idx < 0 {
		m.blocks.cursor.idx = 0
	}
}

// blockItems is the pane's current transcript, for the cursor and the toggle.
func (m *Model) blockItems() []chat.ChatItem {
	return m.execDetail.blockItems(m.Base.DetailID())
}

// repaintExecutionDetail recomposes the execution pane from its current parts without asking the
// server for anything.
func (m *Model) repaintExecutionDetail() tea.Cmd {
	id := m.Base.DetailID()
	if id == "" || m.Base.ActiveSourceName() != srcExecutions {
		return nil
	}
	body, fields := m.composeExecutionBody(id)
	if len(fields) == 0 {
		return nil // the record has not landed; there is nothing to draw yet
	}
	m.Base.SetDetailContentLaidOut("Execution "+id, fields, body)
	return nil
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

// todosRefreshCmd returns the todo fetch for an execution WHEN its cached list is stale, and nil
// otherwise.
func (m *Model) todosRefreshCmd(execID string) tea.Cmd {
	if m.cl == nil || m.cl.Executions == nil || execID == "" {
		return nil
	}
	at, ever := m.todos.fetchedAt(execID)
	if !ever || time.Since(at) > todosTTL {
		return m.loadTodos(execID)
	}
	return nil
}

// renderTodos draws the worker's todo list as a section of the detail body.
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

// executionFields is the GUI's context strip as a text detail pane: who ran it, on what, where, for
// how much.
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
	if msg := e.GetErrorMessage(); msg != "" {
		// Same split as the prose render: a notice is "notes", not "error".
		key, value := "error", msg
		if verify.IsNotice(msg) {
			key = "notes"
			value = strings.TrimPrefix(strings.TrimSpace(msg), verify.NoticePrefix)
		}
		fields = append(fields, screenkit.Field{Key: key, Value: truncateForField(value, 200)})
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
