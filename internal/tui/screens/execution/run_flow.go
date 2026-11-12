package execution

// run_flow.go — a workflow RUN as a vertical flow of its steps, with live status.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// runStepRow is one step RUN flattened for display.
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
	// forced is true when the step run was marked terminal by the MANUAL force-progress escape hatch
	// rather than by anything the engine did.
	forced bool
	// active is false for a SUPERSEDED iteration — it is shown (history matters) but it is not
	// what the cursor lands on and it cannot be jumped from.
	active bool
}

// stepRunForced reports whether a step run carries the force-progress marker, and the reason
// recorded with it.
func stepRunForced(result string) (bool, string) {
	if len(result) == 0 {
		return false, ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		return false, ""
	}
	f, _ := m["_forced"].(bool)
	if !f {
		return false, ""
	}
	reason, _ := m["_forced_reason"].(string)
	return true, reason
}

// runStepRows collapses a run's step runs into one row per STEP, in the workflow's AUTHORED order.
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

	// The authored order first, then anything the definition did not name — ordered by the server's own
	// arrival order, which is the best signal left for a step the version cannot account for.
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
			forced, _ := stepRunForced(s.GetResult())
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
				forced:      forced,
				active:      i == 0 && s.GetSupersededBy() == "",
			})
		}
	}
	return out
}

// authoredStepOrder reads a workflow version's steps in FLOW order and returns their ids.
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
	// LOCAL, WITH ITS ZONE.
	return screenkit.FmtClock(t)
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

		// The meta line: kind, attempts, timing, and WHERE it went.
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
		} else if r.forced {
			// The step did NOT run: force-progress marked it succeeded to unwedge the run, and there is no
			// execution because none was ever dispatched.
			meta = append(meta, "no execution — succeeded by manual force-progress")
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
