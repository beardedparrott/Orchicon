package execution

// run_flow_state.go — the RUNS pane's step cursor: which step is highlighted, and the jump from it
// to that step's execution.

import (
	"context"
	"sync"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// runFlowState holds the step-flow rows for the run currently in the detail pane, plus the
// cursor. Written from the detail fetch goroutine, read from Update, hence the mutex.
type runFlowState struct {
	mu       sync.Mutex
	stepRows []runStepRow
	// runID is the run these rows belong to, so a late fetch for a run the operator has left
	// cannot install its rows under a different run's cursor.
	runID   string
	curStep string // the step id the cursor is on ("" = the first row)
}

// setRows installs a run's rows, keeping the cursor on the same STEP when it survived the reload
// and otherwise seating it on the first row.
func (s *runFlowState) setRows(rows []runStepRow, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID {
		s.curStep = ""
	}
	s.runID = runID
	s.stepRows = rows
	if s.curStep != "" {
		for _, r := range rows {
			if r.stepID == s.curStep {
				return // the step is still there: keep the cursor
			}
		}
		s.curStep = ""
	}
}

func (s *runFlowState) rows() []runStepRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepRows
}

func (s *runFlowState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stepRows)
}

// sel returns the step id the renderer should mark.
func (s *runFlowState) sel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curStep != "" {
		return s.curStep
	}
	// No cursor yet: mark the first ACTIVE row, so the pane shows where `enter` will go the
	// moment it can be used.
	for _, r := range s.stepRows {
		if r.active {
			return r.stepID
		}
	}
	return ""
}

// current returns the row the cursor is on (nil when the flow is empty).
func (s *runFlowState) current() *runStepRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stepRows) == 0 {
		return nil
	}
	sel := s.curStep
	if sel == "" {
		for i := range s.stepRows {
			if s.stepRows[i].active {
				return &s.stepRows[i]
			}
		}
		return &s.stepRows[0]
	}
	for i := range s.stepRows {
		if s.stepRows[i].stepID == sel && s.stepRows[i].active {
			return &s.stepRows[i]
		}
	}
	// A SUPERSEDED-only step (every run of it superseded) is still shown, so it is still
	// selectable — history is worth being able to look at.
	for i := range s.stepRows {
		if s.stepRows[i].stepID == sel {
			return &s.stepRows[i]
		}
	}
	return nil
}

// moveSteps walks the cursor over the rows that REPRESENT a step (the active ones), skipping
// superseded iterations: those are shown as history.
func (s *runFlowState) moveSteps(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The navigable positions: the ACTIVE rows, in order.
	var idx []int
	for i, r := range s.stepRows {
		if r.active {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return
	}
	pos := 0
	for i, row := range idx {
		if s.stepRows[row].stepID == s.curStep {
			pos = i
			break
		}
	}
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(idx) {
		pos = len(idx) - 1
	}
	s.curStep = s.stepRows[idx[pos]].stepID
}

// runStepOrder resolves the order a run's steps are DISPLAYED in: the steps of the workflow version
// the run executed, in FLOW order (run_flow.go's authoredStepOrder).
func (m *Model) runStepOrder(ctx context.Context, run *apiv1.WorkflowRun) []string {
	if m.cl == nil || m.cl.Workflows == nil || run == nil || run.GetWorkflowId() == "" {
		return nil
	}
	resp, err := m.cl.Workflows.ListWorkflowVersions(ctx, connect.NewRequest(&apiv1.ListWorkflowVersionsRequest{
		WorkflowId: run.GetWorkflowId(),
	}))
	if err != nil {
		return nil
	}
	// The version the RUN executed, which is not necessarily the newest: a run keeps the steps it was
	// started with, and ordering it by a later version's graph would misplace anything.
	var v *apiv1.WorkflowVersion
	for _, cand := range resp.Msg.GetVersions() {
		if cand.GetVersion() == run.GetWorkflowVersion() {
			v = cand
			break
		}
	}
	if v == nil {
		v = pickFlowVersion(resp.Msg.GetVersions())
	}
	if v == nil {
		return nil
	}
	return authoredStepOrder(v.GetSteps())
}

// goToRunStepExecution switches to the Executions pane and jumps to the cursor step's execution.
func (m *Model) goToRunStepExecution() tea.Cmd {
	cur := m.runFlow.current()
	if cur == nil {
		return m.refuse("this run has no steps to jump from")
	}
	if cur.executionID == "" {
		return m.refuse(noExecutionReason(cur))
	}
	m.Base.SelectSource(srcExecutions)
	// ShowEntity, NOT SelectWhenLoaded: this is a JUMP, so the detail asked for here is the one the
	// pane must keep.
	m.Base.ShowEntity(srcExecutions, cur.executionID)
	m.notice = "jumped to execution " + cur.executionID
	return tea.Batch(m.Base.Refresh(srcExecutions), m.Base.RequestDetail(srcExecutions, cur.executionID))
}

// noExecutionReason explains WHY a step has nothing to jump to.
func noExecutionReason(cur *runStepRow) string {
	switch {
	case cur.kind == "parallel":
		return cur.name + " is a parallel marker — it dispatches its branches and has no execution of its own"
	case cur.kind == "approval":
		return cur.name + " is an approval gate — it waits on a human decision and has no execution of its own"
	case cur.kind == "loop_decision":
		return cur.name + " is a loop decision — it reads its branches' results and has no execution of its own"
	case cur.forced:
		// It did NOT run.
		return cur.name + " was marked " + cur.status + " by the manual force-progress escape hatch, " +
			"without being dispatched — there is no execution behind it"
	case cur.status == "succeeded" || cur.status == "failed" || cur.status == "skipped":
		// It RAN but the link is missing — see the note above on how that happens.
		return cur.name + " finished (" + cur.status + ") but the run no longer carries its execution link " +
			"(the execution was removed, or the link was lost to a test fixture) — it cannot be opened"
	case cur.status == "running" || cur.status == "recovering":
		return cur.name + " is " + cur.status + " but no execution is linked yet"
	case cur.status == "blocked":
		return cur.name + " is blocked — it has not been dispatched, so there is no execution to open"
	}
	return cur.name + " has not been dispatched yet — there is no execution to open"
}

// repaintRunFlow redraws the run detail from its cached rows, so moving the cursor does not cost a
// round trip.
func (m *Model) repaintRunFlow() tea.Cmd {
	rows := m.runFlow.rows()
	if len(rows) == 0 {
		return nil
	}
	body, offsets := renderRunFlow(rows, m.w, m.runFlow.sel())
	_, fields, _ := m.Base.DetailForTest()
	if cur := m.runFlow.current(); cur != nil {
		for i := range fields {
			if fields[i].Key == "selected" {
				if cur.executionID != "" {
					fields[i].Value = cur.name + " → enter: execution " + cur.executionID
				} else {
					fields[i].Value = cur.name + " — no execution linked"
				}
			}
		}
	}
	m.Base.SetDetailBodyLaidOut(body, fields)
	if line, ok := offsets[m.runFlow.sel()]; ok {
		scroll := line - 2
		if scroll < 0 {
			scroll = 0
		}
		m.Base.SetDetailScrollTop(scroll)
	}
	return nil
}
