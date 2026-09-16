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
	"sync"

	tea "github.com/charmbracelet/bubbletea"
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
		return m.refuse(cur.name + " has no execution linked — it has not been dispatched (or is not a worker step)")
	}
	m.Base.SelectSource(srcExecutions)
	m.Base.SelectWhenLoaded(srcExecutions, cur.executionID)
	m.notice = "jumped to execution " + cur.executionID
	return tea.Batch(m.Base.Refresh(srcExecutions), m.Base.RequestDetail(srcExecutions, cur.executionID))
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
