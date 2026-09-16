package execution

// run_flow_state.go — the RUNS pane's step cursor: which step is highlighted, and the jump from
// it to that step's execution.
//
// The operator: "you should be able to see live runs and if you hit enter on a particular step
// (whether it's complete or still running), it should take you to the execution."
//
// The cursor is keyed on the STEP ID, not the row index: the rows are rebuilt from the plane on
// every load (a live run's statuses change constantly), and an index would silently re-point the
// highlight at whichever step happened to land in that slot.

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

// setRows installs a run's rows, keeping the cursor on the same STEP when it survived the
// reload and otherwise seating it on the first row.
//
// Keeping the cursor by step is what makes the LIVE refresh usable: a run's flow is re-read
// whenever execution events arrive, and re-seating the cursor every time would make the flow
// unusable while it is running — which is exactly when the operator is watching it.
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
// superseded iterations: those are shown as history, but they are not something the operator
// navigates to.
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

// runStepOrder resolves the order a run's steps are DISPLAYED in: the steps of the workflow
// version the run executed, in FLOW order (run_flow.go's authoredStepOrder).
//
// Why it is worth an RPC on the detail path: the run's own step-run rows cannot carry this
// ordering. They arrive ordered by `created_at, id` — and every step run of a run is created in
// ONE statement, so they share a created_at and the ordering collapses to id order, which for a
// generated id (step-3rplua0d) is arbitrary and for a semantic one (step-devops-pr) is merely
// alphabetical. Neither is the order the steps RUN in, which is the only order that makes the flow
// readable — and reading it backwards is exactly what the operator reported.
//
// Best effort throughout: no client, a failed workflow read, a failed version list, or a version
// whose steps do not parse all return nil, and runStepRows then falls back to the run's own
// arrival order. The flow still renders; it just loses the order.
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
	// The version the RUN executed, which is not necessarily the newest: a run keeps the steps it
	// was started with, and ordering it by a later version's graph would misplace anything that
	// was renamed, added or removed since. pickFlowVersion's published-first preference is the
	// fallback for a version the list does not carry (an unpublished draft being the common case).
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
//
// It focuses the execution AND requests its detail — the same pair the Schedules jump uses, and
// for the same reason: the executions list is a page, so the execution may have no row to
// highlight, and a jump that silently does nothing is worse than no jump.
//
// A step with no execution is NAMED rather than silently ignored: "no execution linked" is the
// honest answer for a step that has not been dispatched yet (pending/blocked) or is not a worker
// step at all (an approval or a loop decision has no execution of its own).
func (m *Model) goToRunStepExecution() tea.Cmd {
	cur := m.runFlow.current()
	if cur == nil {
		return m.refuse("this run has no steps to jump from")
	}
	if cur.executionID == "" {
		return m.refuse(noExecutionReason(cur))
	}
	m.Base.SelectSource(srcExecutions)
	// ShowEntity, NOT SelectWhenLoaded: this is a JUMP, so the detail asked for here is the one
	// the pane must keep. The executions list is recent-first and paginated, so the step's
	// execution is often not on the loaded page — and the landing would otherwise load whatever
	// row sits at the top, which is the "not taking you to the execution for that step" report.
	m.Base.ShowEntity(srcExecutions, cur.executionID)
	m.notice = "jumped to execution " + cur.executionID
	return tea.Batch(m.Base.Refresh(srcExecutions), m.Base.RequestDetail(srcExecutions, cur.executionID))
}

// noExecutionReason explains WHY a step has nothing to jump to.
//
// It is worded from the step's own state, because "has not been dispatched" is FALSE for the case
// the operator actually hit. Ground truth, live SDLC run 01M2B8D9M0H8E59RKNADW9QH33: step-sse
// (Senior Software Engineer) is status=succeeded with worker_execution_id NULL, as is step-parallel.
// Telling the operator a step that plainly SUCCEEDED "has not been dispatched" is worse than
// saying nothing — it makes them distrust the status beside it.
//
// The proto cannot distinguish "the link was never written" from "the execution row is gone" —
// worker_execution_id is a single nullable column either way — so the message says what it DOES
// know (the step's status, which is authoritative) and names the one thing the operator can do
// about it: the execution is on the Executions pane if it exists at all.
//
// A step that genuinely has not run is still told so plainly; a non-worker step (a parallel
// marker, an approval, a loop decision) is named as such, because for those a missing execution is
// expected rather than suspicious.
func noExecutionReason(cur *runStepRow) string {
	switch {
	case cur.kind == "parallel":
		return cur.name + " is a parallel marker — it dispatches its branches and has no execution of its own"
	case cur.kind == "approval":
		return cur.name + " is an approval gate — it waits on a human decision and has no execution of its own"
	case cur.kind == "loop_decision":
		return cur.name + " is a loop decision — it reads its branches' results and has no execution of its own"
	case cur.status == "succeeded" || cur.status == "failed" || cur.status == "skipped":
		// It RAN. The step run just does not carry the execution's id.
		return cur.name + " finished (" + cur.status + ") but the run has no execution linked to it — " +
			"check the Executions pane for its worker's session"
	case cur.status == "running" || cur.status == "recovering":
		return cur.name + " is " + cur.status + " but no execution is linked yet"
	case cur.status == "blocked":
		return cur.name + " is blocked — it has not been dispatched, so there is no execution to open"
	}
	return cur.name + " has not been dispatched yet — there is no execution to open"
}

// repaintRunFlow redraws the run detail from its cached rows, so moving the cursor does not cost
// a round trip. It also scrolls the pane to FOLLOW the cursor: a real run's flow is taller than
// the pane, and a cursor the operator cannot see is a cursor they cannot use.
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
	m.Base.SetDetailBody(body, fields)
	if line, ok := offsets[m.runFlow.sel()]; ok {
		scroll := line - 2
		if scroll < 0 {
			scroll = 0
		}
		m.Base.SetDetailScrollTop(scroll)
	}
	return nil
}
