package e2e

// launch.go — run the six pinned tasks ONE AT A TIME, each with a different adapter.
//
// THE POINT IS THE MODEL-REF CHANGE BETWEEN RUNS. A work item's adapter is decided by its assigned
// worker's model_ref, so exercising the matrix one task after another means re-pointing that binding
// before each run — which is exactly what a person would otherwise do by hand in the console.
//
// This is separate from the setup pass on purpose. Setup is cheap and idempotent; a launch spends real
// tokens and minutes, so it is its own command and never runs as a side effect of creating the project.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// launchPlan is one task's slot in the matrix: which adapter runs it, in which execution mode.
type launchPlan struct {
	index   int
	task    workItem
	adapter adapter
	mode    apiv1.ExecutionMode
}

// discoverLaunchPlan reads the epic's six children back from the plane, so launching works on a project
// that was set up by an EARLIER run — which is the whole reason the artifacts are left in place.
//
// The adapter and mode per task are re-derived from the flags rather than stored, because they are the
// thing being varied: the same six tasks can be re-run against a different set of adapters.
func discoverLaunchPlan(ctx context.Context, cs *client.Clients, projectID string, adapters []adapter,
	localOnly bool) ([]launchPlan, error) {

	resp, err := cs.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		ProjectId: projectID, PageSize: 200,
	}))
	if err != nil {
		return nil, fmt.Errorf("list work items: %w", err)
	}
	var epicID string
	children := map[string][]workItem{}
	for _, w := range resp.Msg.GetWorkItems() {
		if epicID == "" && w.GetKind() == apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC {
			epicID = w.GetId()
		}
		children[w.GetParentId()] = append(children[w.GetParentId()], workItem{
			id:     w.GetId(),
			title:  w.GetTitle(),
			status: w.GetStatus().String(),
		})
	}
	if epicID == "" {
		return nil, fmt.Errorf("no epic in project %s — run the setup pass first", projectID)
	}
	kids := children[epicID]
	if len(kids) == 0 {
		return nil, fmt.Errorf("the epic in project %s has no tasks — run the setup pass first", projectID)
	}

	plan := make([]launchPlan, 0, len(kids))
	for i, t := range kids {
		a := adapters[(i/2)%len(adapters)]
		mode := apiv1.ExecutionMode_EXECUTION_MODE_LOCAL
		if !localOnly && i%2 == 1 {
			mode = apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
		}
		plan = append(plan, launchPlan{index: i + 1, task: t, adapter: a, mode: mode})
	}
	return plan, nil
}

// launchOne re-points the task at the adapter's worker, then starts it and waits.
//
// A TASK THAT IS NOT RE-LAUNCHABLE IS SKIPPED, NOT RETRIED. An item that already succeeded, is running, or
// is still recovering cannot be started again — re-binding it and asking it to auto-start silently does
// nothing, and waiting on it reports a timeout on work that is not happening. Saying so is the useful
// behaviour: a matrix run reports six outcomes, and "already succeeded" is one of them.
//
// THE REBIND IS THE TEST. AssignWorker writes the worker_ref the dispatcher resolves, so changing it
// between runs is what makes the next run use a different adapter. The model_ref itself lives on the
// WORKER (one per adapter, created by the setup pass), not on the item.
func launchOne(ctx context.Context, cs *client.Clients, rep *report, p launchPlan,
	workflows map[string]string, image string, timeout time.Duration) error {

	if !launchable(p.task.status) {
		st := shortStatus(p.task.status)
		rep.tasks = append(rep.tasks, taskRow{n: p.index, title: p.task.title, status: "skipped (" + st + ")"})
		fmt.Printf("launch %d/6: %-8s %-9s SKIPPED — task is %s\n", p.index, p.adapter.kind,
			modeString(p.mode), st)
		return nil
	}

	wfID, ok := workflows[p.adapter.kind]
	if !ok {
		return fmt.Errorf("no workflow for the %s adapter — run the setup pass with -%s-model",
			p.adapter.kind, p.adapter.kind)
	}

	// BIND THE TASK TO THIS ADAPTER'S WORKFLOW. That is the lever: the dispatcher reads the WORKFLOW STEP's
	// worker ref, and that worker's published version carries the model_ref. Assigning a worker to the ITEM
	// does not affect a workflow-bound run at all.
	if err := bindTaskToWorkflow(ctx, cs, p.task.id, wfID, p.mode, image); err != nil {
		return fmt.Errorf("bind the task to the %s workflow: %w", p.adapter.kind, err)
	}

	// Start it. AutoStartWorkflow starts a bound item immediately.
	yes := true
	resp, err := cs.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id: p.task.id, AutoStartWorkflow: &yes,
	}))
	if err != nil {
		return fmt.Errorf("start the task: %w", err)
	}
	if warn := resp.Msg.GetWarning(); strings.TrimSpace(warn) != "" {
		// A declined auto-start is not an error to swallow: it names the status that blocked it.
		fmt.Printf("           (server warning: %s)\n", oneLine(warn))
	}

	fmt.Printf("launch %d/6: %-8s %-9s %s\n", p.index, p.adapter.kind, modeString(p.mode),
		truncate(p.task.title, 50))
	waitTask(ctx, cs, rep, p.task, p.index, timeout)
	return nil
}

// launchable reports whether a task can be started again. Only a task that has not run, or whose last run
// failed, can be dispatched; anything else is mid-flight or already decided.
func launchable(status string) bool {
	switch status {
	case "WORK_ITEM_STATUS_PENDING", "WORK_ITEM_STATUS_READY", "WORK_ITEM_STATUS_SCHEDULED",
		"WORK_ITEM_STATUS_FAILED", "WORK_ITEM_STATUS_CANCELLED":
		return true
	}
	return false
}
