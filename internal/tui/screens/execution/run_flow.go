package execution

// run_flow.go — a workflow RUN as a vertical flow of its steps, with live status.
//
// The operator: "I think we should change the view on workflow runs in the tui to mimic a similar
// look to our new workflow view where we have the steps, and it should show next to the steps if
// it succeeded, failed, how many retries, etc. and you should be able to see live runs and if you
// hit enter on a particular step (whether it's complete or still running), it should take you to
// the execution."
//
// The visual language is the workflow flow view's (workflow_flow.go): one step per block, indented
// so dependencies read DOWN the page, with a marker on the cursor row. What differs is the
// CONTENT — a run has step RUNS, so each row carries its status, attempt count and timing rather
// than its authored config — and the interaction: on a run the cursor picks a step to JUMP FROM,
// on a workflow it picks a step to edit.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// runStepRow is one step RUN flattened for display.
//
// It carries the STEP ID as well as the step-run id because the cursor is keyed on the STEP: a
// step that looped has SEVERAL step runs (iterations), and the operator's cursor is on the step,
// not on one of its historical attempts. The row it resolves to is the ACTIVE one.
type runStepRow struct {
	stepID      string
	runID       string
	name        string
	kind        string
	status      string
	attempt     int32
	iteration   int32
	executionID string
	started     string
	ended       string
	// active is false for a SUPERSEDED iteration — it is shown (history matters) but it is not
	// what the cursor lands on and it cannot be jumped from.
	active bool
}

// runStepRows collapses a run's step runs into one row per STEP, in the workflow's AUTHORED order.
//
// ORDERING — this is the operator's "I think it is showing the results in reverse order":
// "It is showing the PR Reviewer and QA Engineer as first in the list."
//
// The list was sorted ALPHABETICALLY BY STEP ID (`sort.Strings`), which is not an order at all.
// The live SDLC run it was reported on has steps step-sse, step-parallel, step-branch-a,
// step-branch-b, step-loop, so sorting by id yields branch-a, branch-b, loop, parallel, sse — i.e.
// PR Reviewer, QA Engineer, Loop Decision, Parallel, Senior Software Engineer, which is exactly
// what the operator saw. Nothing about a random id encodes when a step runs, and the SEMANTIC ids
// (step-devops-pr, step-approval) sort just as arbitrarily.
//
// So the order comes from the WORKFLOW DEFINITION. `authored` is the version's steps in flow
// order (flowOrder — Kahn over `depends_on`, keeping the authored order among steps that are
// simultaneously ready), which is what the workflow view already draws and what the GUI maps run
// status onto.
//
// The server's own order is `created_at ASC, id ASC`, which for a run created in one statement
// (every step run of a run shares a created_at) collapses to ID order — the same defect. It is
// used here only as a FALLBACK, for the cases where authored order is unavailable: the workflow
// version could not be read, or the run is for a version whose steps no longer include a step the
// run recorded. A step the definition does not mention is appended AFTER the authored ones rather
// than dropped, so a renderer never silently hides a row.
func runStepRows(runs []*apiv1.WorkflowStepRun, authored []string) []runStepRow {
	byStep := map[string][]*apiv1.WorkflowStepRun{}
	appeared := []string{}
	for _, s := range runs {
		id := s.GetStepId()
		if id == "" {
			id = s.GetId()
		}
		if _, seen := byStep[id]; !seen {
			appeared = append(appeared, id)
		}
		byStep[id] = append(byStep[id], s)
	}

	// The authored order first, then anything the definition did not name — ordered by the
	// server's own arrival order, which is the best signal left for a step the version cannot
	// account for.
	order := make([]string, 0, len(appeared))
	seen := map[string]bool{}
	for _, id := range authored {
		if _, ok := byStep[id]; ok && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, id := range appeared {
		if !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}

	out := make([]runStepRow, 0, len(runs))
	for _, id := range order {
		group := byStep[id]
		// Active first: the newest iteration that is not superseded.
		sort.SliceStable(group, func(i, j int) bool {
			si, sj := group[i], group[j]
			if (si.GetSupersededBy() == "") != (sj.GetSupersededBy() == "") {
				return si.GetSupersededBy() == ""
			}
			return si.GetIteration() > sj.GetIteration()
		})
		for i, s := range group {
			out = append(out, runStepRow{
				stepID:      id,
				runID:       s.GetId(),
				name:        stepDisplayName(s),
				kind:        stepKindBadge(s.GetStepKind()),
				status:      runStatusWord(s.GetStatus()),
				attempt:     s.GetAttempt(),
				iteration:   s.GetIteration(),
				executionID: s.GetWorkerExecutionId(),
				started:     shortClock(s.GetStartedAt()),
				ended:       shortClock(s.GetEndedAt()),
				active:      i == 0 && s.GetSupersededBy() == "",
			})
		}
	}
	return out
}

// authoredStepOrder reads a workflow version's steps in FLOW order and returns their ids. It is
// the input that makes a run's step flow read top-to-bottom instead of alphabetically by id.
//
// Best effort: a version that cannot be read, or whose steps do not parse, yields nil and the
// caller falls back to the run's own arrival order. A run is still perfectly renderable without
// it — it just loses the one thing the operator asked for, which is the order.
func authoredStepOrder(stepsJSON string) []string {
	ordered := flowOrder(parseFlowSteps(stepsJSON))
	ids := make([]string, 0, len(ordered))
	for _, s := range ordered {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func stepDisplayName(s *apiv1.WorkflowStepRun) string {
	if n := s.GetStepName(); n != "" {
		return n
	}
	if id := s.GetStepId(); id != "" {
		return id
	}
	return s.GetId()
}

// runStatusWord trims the proto prefix, so the pane reads "succeeded", not
// "STEP_RUN_STATUS_SUCCEEDED".
func runStatusWord(s apiv1.StepRunStatus) string {
	w := strings.TrimPrefix(s.String(), "STEP_RUN_STATUS_")
	if w == "" || w == s.String() {
		return strings.ToLower(s.String())
	}
	return strings.ToLower(w)
}

// shortClock renders a timestamp as just the wall clock ("15:04:05"), because a flow row is
// about WHEN something happened, not which day — the run's own header carries the date.
func shortClock(ts interface {
	AsTime() time.Time
}) string {
	if ts == nil {
		return ""
	}
	t := ts.AsTime()
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04:05")
}

// stepKindBadge is the same short vocabulary the workflow flow uses, so a run and its definition
// read with one set of words.
func stepKindBadge(k apiv1.StepKind) string {
	w := strings.ToLower(strings.TrimPrefix(k.String(), "STEP_KIND_"))
	if w == "" {
		return "?"
	}
	return w
}

// runFlowLegend names the status vocabulary ONCE at the top, so the rows can stay terse.
func runFlowLegend() string {
	return "● running  ✓ succeeded  ✗ failed  ⊘ skipped  ‖ blocked  ⧗ approval  ⟳ recovering  · pending"
}

// renderRunFlow draws the run's steps as a vertical flow.
//
// It returns the body and a stepID → line offset map so the pane can FOLLOW the cursor: a real
// run has more steps than fit (SDLC is 11, each with up to three lines), so moving the cursor
// must scroll the pane or the operator edits something they cannot see.
func renderRunFlow(rows []runStepRow, width int, sel string) (string, map[string]int) {
	if len(rows) == 0 {
		return "", nil
	}
	offsets := map[string]int{}
	var b strings.Builder
	line := 0
	write := func(s string) {
		b.WriteString(s + "\n")
		line++
	}
	write(theme.HintText.Render(runFlowLegend()))
	write("")

	prevStep := ""
	for _, r := range rows {
		// A superseded iteration is indented UNDER its step, so the grouping is visible without a
		// second header row per step.
		sameStep := r.stepID == prevStep
		prevStep = r.stepID

		marker := "  "
		if !sameStep && r.stepID == sel {
			marker = "▸ "
		} else if sameStep && r.stepID == sel {
			marker = "  "
		}
		if !r.active {
			marker = "↳ " // a historical iteration of the step above it
		}

		if !sameStep {
			offsets[r.stepID] = line
		}
		head := marker + runStatusGlyph(r.status) + " " + r.name
		if r.iteration > 0 {
			head += theme.HintText.Render(fmt.Sprintf("  (iteration %d)", r.iteration))
		}
		write(head)

		// The meta line: kind, attempts, timing, and WHERE it went. Every piece here is
		// something the operator asked to see next to the step ("if it succeeded, failed, how
		// many retries").
		meta := []string{r.kind, r.status}
		if r.attempt > 0 {
			meta = append(meta, fmt.Sprintf("%d retries", r.attempt))
		}
		if r.started != "" {
			if r.ended != "" {
				meta = append(meta, r.started+" → "+r.ended)
			} else {
				meta = append(meta, "started "+r.started)
			}
		}
		if r.executionID != "" {
			meta = append(meta, "enter → execution")
		} else if r.active && r.status == "running" {
			meta = append(meta, "no execution linked yet")
		}
		write("   " + theme.HintText.Render(strings.Join(meta, " · ")))
		if !r.active {
			write("   " + theme.HintText.Render("superseded by a later iteration"))
		}
		write("")
	}
	return strings.TrimRight(b.String(), "\n"), offsets
}

// runStatusGlyph is the one-character status the flow rows lead with.
func runStatusGlyph(status string) string {
	switch status {
	case "running":
		return "●"
	case "succeeded":
		return "✓"
	case "failed":
		return "✗"
	case "skipped":
		return "⊘"
	case "blocked":
		return "‖"
	case "approval_pending":
		return "⧗"
	case "recovering":
		return "⟳"
	}
	return "·"
}
