package scheduler

import (
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// verdict_routing.go — WHAT HAPPENS WHEN A STEP FAILS BECAUSE A WORKER DELIVERED A VERDICT.
//
// A worker that ran to completion and wrote `ORCHICON WORKER SUMMARY: failure` has DELIVERED ITS
// JUDGEMENT. It is not a crash, a stall or an environment fault, and there is exactly one thing the
// workflow can usefully do with it: route it. `upstreamFaultedByInfra` (workflow_reconciler.go)
// already encodes that distinction, and two call sites already consult it — the loop's hold gate
// (`loopDecisionUpstreamFailed`) and the loop chain (`:anyFailed`). The two that did NOT are the ones
// this file exists for:
//
//  1. the point where RECOVERY IS CHOSEN (pollTaskStep) — which charged a delivered verdict against
//     `max_attempts` and, with `summarize_restart`, resumed the SAME step on the SAME work item until
//     the budget was gone. Measured on run 01M8B2A33SB9WDKDB98GQQKV0G: six QA executions, 15 seconds
//     apart, `CheckpointRef: null`, no worktree, an `Output` byte-identical to the first — ~1.13M
//     tokens to re-reach a conclusion already reached at attempt one. The earlier run
//     01M8AYA1V9RMJ35MK7XZ04HAB8 did it fourteen times.
//
//  2. the RUN SETTLE — `anyFailed` treated that failed step as a run failure and skipped every
//     not-yet-dispatched step, INCLUDING the loop_decision that was supposed to route it. So even
//     after the hold gate was relaxed (see the comment on loopDecisionUpstreamFailed), the loop was
//     killed before it could fire.
//
// THE SCOPE IS DELIBERATELY NARROW. Both rules apply only where a loop_decision is genuinely waiting
// to route on that failed step: below it in the graph, and not yet terminal. A failed step with no
// such loop keeps today's behaviour exactly — recovery, attempts and run failure all unchanged. That
// keeps the blast radius on workflows that already opted into looping.

// stepRunsByStepIDFunc lists every step run of ONE step id within a run, including superseded
// iterations. Injected so these rules are unit-testable without a database.
type stepRunsByStepIDFunc func(stepID string) ([]db.WorkflowStepRunRow, error)

// loopStillRouting reports whether ld is a loop_decision step that has not decided yet, and is
// therefore still owed its chance to route on an upstream failure.
//
// "Not decided yet" is read from the loop step's own run: while it is pending, ready, running or
// recovering it still has work to do. Once it is stored (succeeded/failed/skipped) it has decided,
// and it must stop holding the run open — otherwise a routed-then-failed run could never fail.
func loopStillRouting(ld workflow.StepWire, runsOf stepRunsByStepIDFunc) bool {
	if ld.Kind != domain.StepKindLoopDecision || runsOf == nil {
		return false
	}
	rows, err := runsOf(ld.ID)
	if err != nil {
		// A listing failure must not silently disable the rule in either direction. Treat it as "not
		// routing": the conservative behaviour is today's, i.e. recover and fail as before.
		return false
	}
	if len(rows) == 0 {
		// Never dispatched. It is owed a turn.
		return true
	}
	for _, sr := range rows {
		if sr.SupersededBy != "" {
			continue
		}
		switch sr.Status {
		case domain.StepRunSucceeded, domain.StepRunFailed, domain.StepRunSkipped:
			// Decided.
		default:
			return true
		}
	}
	return false
}

// verdictRoutedLoop returns the id of a loop_decision step that is still waiting to route on
// failedStepID's verdict, and whether one exists.
//
// "A LOOP STEP IT WILL REACH" — the walk is TRANSITIVE, because the seeded SDLC graph puts a PR
// Reviewer between a failure and the loop that consumes it: step-qa fails, step-b2bmqv2p
// (loop_branch: step-sse) routes it, and step-9gfybwnv sits one hop further on still waiting for that
// routing to happen. A direct-dependency-only rule would miss the very workflow this was measured on
// and fix nothing.
func verdictRoutedLoop(failedStepID string, allSteps []workflow.StepWire, runsOf stepRunsByStepIDFunc) (string, bool) {
	for _, ld := range loopStepsDownstreamOf(allSteps, failedStepID) {
		if loopStillRouting(ld, runsOf) {
			return ld.ID, true
		}
	}
	return "", false
}

// loopDesiredDownstreams returns every step id that a still-routing loop_decision is owed, so the run
// settle can set those aside instead of skipping them — the half that let the loop die unrun.
//
// It deliberately does NOT include the loop step's upstream: those already ran. It stops expanding at
// a nested loop_decision (it is included, its own downstream is that loop's business).
func loopDesiredDownstreams(allSteps []workflow.StepWire, runsOf stepRunsByStepIDFunc) map[string]bool {
	out := make(map[string]bool)
	children := childrenByStep(allSteps)
	for _, ld := range allSteps {
		if !loopStillRouting(ld, runsOf) {
			continue
		}
		queue := append([]string(nil), children[ld.ID]...)
		seen := map[string]bool{ld.ID: true}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			out[id] = true
			queue = append(queue, children[id]...)
		}
		out[ld.ID] = true
	}
	return out
}

// loopStepsDownstreamOf returns every loop_decision step reachable from stepID, breadth-first.
func loopStepsDownstreamOf(allSteps []workflow.StepWire, stepID string) []workflow.StepWire {
	children := childrenByStep(allSteps)
	byID := make(map[string]workflow.StepWire, len(allSteps))
	for _, s := range allSteps {
		byID[s.ID] = s
	}
	var found []workflow.StepWire
	seen := map[string]bool{stepID: true}
	queue := append([]string(nil), children[stepID]...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if byID[id].Kind == domain.StepKindLoopDecision {
			found = append(found, byID[id])
			// A nested loop's own downstream is its business, not this search's.
			continue
		}
		queue = append(queue, children[id]...)
	}
	return found
}

// childrenByStep indexes every step by the steps that depend on it.
func childrenByStep(allSteps []workflow.StepWire) map[string][]string {
	children := make(map[string][]string, len(allSteps))
	for _, s := range allSteps {
		for _, dep := range s.DependsOn {
			children[dep] = append(children[dep], s.ID)
		}
	}
	return children
}
