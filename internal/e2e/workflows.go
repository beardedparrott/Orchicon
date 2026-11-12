package e2e

// workflows.go — ONE WORKFLOW PER ADAPTER, which is where the model is actually decided.
//
// A work item does not carry the model. Launching is: item → bound workflow → the workflow STEP's worker ref
// → that worker's published version → its model_ref (workflow_reconciler.go:1899). The item's
// assigned_worker_ref is not consulted on this path at all, so re-assigning it between runs changes nothing —
// which is what an earlier version of this harness did, and why every task ran the same adapter.
//
// So the rotation is expressed where the platform reads it: one workflow per adapter, identical to Quick Work
// except that its step points at that adapter's worker. Launching a task is then binding it to the matching
// workflow.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// quickWorkTemplate is the step graph a per-adapter workflow copies: the single task step Quick Work uses,
// with its version pin cleared so the dispatcher takes the worker's LATEST PUBLISHED version (its
// model_ref is what we are rotating).
type stepWire struct {
	ID            string   `json:"id"`
	Ref           string   `json:"ref"`
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	Config        string   `json:"config,omitempty"`
	DependsOn     []string `json:"depends_on"`
	PositionX     float64  `json:"position_x"`
	PositionY     float64  `json:"position_y"`
	WorkerVersion int32    `json:"worker_version"`
	GatePolicyRef string   `json:"gate_policy_ref,omitempty"`
}

// adapterWorkflowName is the workflow created for one adapter. Prefixed so it is obvious in the console that
// it belongs to the harness and can be deleted with the project.
func adapterWorkflowName(kind string) string { return "e2e-quick-" + kind }

// ensureAdapterWorkflows creates (or reuses) one Quick Work copy per adapter, each pinned to that adapter's
// worker, and returns kind -> workflow id.
//
// ONE SHOT, NOT TEMPLATE: each workflow is bound to this project and fires once per task, which is what a
// per-task launch needs.
func ensureAdapterWorkflows(ctx context.Context, cs *client.Clients, projectID string,
	adapters []adapter, workers map[string]worker) (map[string]string, error) {

	existing, err := cs.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	byName := map[string]string{}
	for _, w := range existing.Msg.GetWorkflows() {
		byName[w.GetName()] = w.GetId()
	}

	out := map[string]string{}
	for _, a := range adapters {
		name := adapterWorkflowName(a.kind)
		w, ok := workers[a.kind]
		if !ok {
			return nil, fmt.Errorf("no worker for the %s adapter — cannot build its workflow", a.kind)
		}
		// A STALE REUSE IS WORSE THAN A DUPLICATE. Each setup pass creates FRESH workers, so a workflow left
		// from an earlier pass is pinned to a worker that still exists — and dispatches that worker's model
		// while the matrix claims it is testing this run's adapter. So an existing workflow is re-pointed
		// (a new published version) rather than adopted, and verifyWorkflowPins below is what proves it.
		if id, ok := byName[name]; ok {
			if rerr := repointWorkflow(ctx, cs, id, w.id, a.kind); rerr != nil {
				return nil, rerr
			}
			out[a.kind] = id
			continue
		}
		if !ok {
			return nil, fmt.Errorf("no worker for the %s adapter — cannot build its workflow", a.kind)
		}
		steps := []stepWire{
			{
				ID: "step-quick", Ref: w.id, Kind: "task",
				Name: "Quick Software Engineer",
				Config: `{"recovery":{"strategy":"summarize_restart","max_attempts":6}}`,
				// ZERO: dispatch takes the worker's latest published version, which is where model_ref lives.
				WorkerVersion: 0,
				DependsOn:     []string{},
			},
			{
				ID: "step-end", Kind: "end", Name: "End",
				DependsOn: []string{"step-quick"}, PositionY: 220,
			},
		}
		raw, merr := json.Marshal(steps)
		if merr != nil {
			return nil, fmt.Errorf("encode steps for %s: %w", a.kind, merr)
		}

		resp, cerr := cs.Workflows.CreateWorkflow(ctx, connect.NewRequest(&apiv1.CreateWorkflowRequest{
			ProjectId:   projectID,
			Name:        name,
			Steps:       string(raw),
			Type:        "one_shot",
			VersionNote: "Created by the e2e harness: Quick Work pinned to the " + a.kind + " adapter.",
		}))
		if cerr != nil {
			return nil, fmt.Errorf("create the %s workflow: %w", a.kind, cerr)
		}
		id := resp.Msg.GetWorkflow().GetId()

		// A CREATED WORKFLOW IS A DRAFT. It cannot be bound or run until published, so publish before the
		// launch pass can possibly need it.
		if _, perr := cs.Workflows.PublishWorkflow(ctx, connect.NewRequest(
			&apiv1.PublishWorkflowRequest{WorkflowId: id, VersionNote: "e2e harness"})); perr != nil {
			return nil, fmt.Errorf("publish the %s workflow: %w", a.kind, perr)
		}
		out[a.kind] = id
		fmt.Printf("workflow : %-22s → %s worker\n", name, a.kind)
	}
	return out, nil
}

// verifyWorkflowPins asserts that each per-adapter workflow's task step really points at the adapter's worker.
//
// THE ROTATION ONLY WORKS IF THE STEP REF IS RIGHT, and a wrong ref is silent: the run dispatches under
// whatever worker the step names, so the matrix would look like it ran three adapters and run one three
// times. This reads the published version back and checks.
func verifyWorkflowPins(ctx context.Context, cs *client.Clients, workflows map[string]string,
	workers map[string]worker) error {

	for kind, wfID := range workflows {
		want, ok := workers[kind]
		if !ok {
			return fmt.Errorf("no worker recorded for the %s adapter", kind)
		}
		resp, err := cs.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: wfID}))
		if err != nil {
			return fmt.Errorf("read the %s workflow: %w", kind, err)
		}
		latest := resp.Msg.GetLatestVersion()
		if latest == nil {
			return fmt.Errorf("the %s workflow has no version to verify", kind)
		}
		var steps []stepWire
		if uerr := json.Unmarshal([]byte(latest.GetSteps()), &steps); uerr != nil {
			return fmt.Errorf("decode the %s workflow's steps: %w", kind, uerr)
		}
		found := false
		for _, s := range steps {
			if s.Kind == "task" && s.Ref == want.id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("the %s workflow's task step does not point at its worker %s — the rotation would "+
				"dispatch the wrong adapter in silence", kind, want.id)
		}
	}
	return nil
}

// bindTaskToWorkflow points a work item at the workflow that carries the right adapter.
//
// This is the step the earlier version was missing: it edited the item's worker assignment, which the
// bound-workflow path never reads.
func bindTaskToWorkflow(ctx context.Context, cs *client.Clients, taskID, workflowID string,
	mode apiv1.ExecutionMode, image string) error {

	upd := &apiv1.UpdateWorkItemRequest{
		Id:            taskID,
		WorkflowId:    &workflowID,
		ExecutionMode: &mode,
	}
	if mode == apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME && strings.TrimSpace(image) != "" {
		upd.RuntimeImage = &image
	}
	_, err := cs.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(upd))
	return err
}

// findAdapterWorkflows resolves the per-adapter workflows an EARLIER setup pass created, so -launch-only
// needs no stored state — the names are derived from the adapter kind.
func findAdapterWorkflows(ctx context.Context, cs *client.Clients, adapters []adapter) (map[string]string, error) {
	resp, err := cs.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	byName := map[string]string{}
	for _, w := range resp.Msg.GetWorkflows() {
		byName[w.GetName()] = w.GetId()
	}
	out := map[string]string{}
	for _, a := range adapters {
		id, ok := byName[adapterWorkflowName(a.kind)]
		if !ok {
			return nil, fmt.Errorf("no %s workflow found — run a setup pass with -%s-model first (it creates one "+
				"Quick Work copy per adapter)", adapterWorkflowName(a.kind), a.kind)
		}
		out[a.kind] = id
	}
	return out, nil
}

// repointWorkflow publishes a NEW version of an existing per-adapter workflow whose step points at this
// run's worker. Published versions are immutable, so this is a new version rather than an edit — the same
// mechanism the console uses when a workflow is changed.
func repointWorkflow(ctx context.Context, cs *client.Clients, workflowID, workerID, kind string) error {
	steps := []stepWire{
		{
			ID: "step-quick", Ref: workerID, Kind: "task",
			Name: "Quick Software Engineer",
			Config: `{"recovery":{"strategy":"summarize_restart","max_attempts":6}}`,
			WorkerVersion: 0,
			DependsOn:     []string{},
		},
		{
			ID: "step-end", Kind: "end", Name: "End",
			DependsOn: []string{"step-quick"}, PositionY: 220,
		},
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		return fmt.Errorf("encode steps for %s: %w", kind, err)
	}
	// A NEW VERSION IS CREATED EMPTY AND THEN FILLED: CreateWorkflowVersion takes no steps, so the draft's
	// steps are set with UpdateWorkflowVersion before it is published.
	if _, err := cs.Workflows.CreateWorkflowVersion(ctx, connect.NewRequest(
		&apiv1.CreateWorkflowVersionRequest{
			WorkflowId:  workflowID,
			VersionNote: "e2e harness: re-pointed at this run's " + kind + " worker",
		})); err != nil {
		return fmt.Errorf("create a new %s workflow version: %w", kind, err)
	}
	// UpdateWorkflowVersion edits the DRAFT the create just made — it takes no version number.
	if _, uerr := cs.Workflows.UpdateWorkflowVersion(ctx, connect.NewRequest(
		&apiv1.UpdateWorkflowVersionRequest{WorkflowId: workflowID, Steps: string(raw)})); uerr != nil {
		return fmt.Errorf("set the %s workflow's steps: %w", kind, uerr)
	}
	if _, err := cs.Workflows.PublishWorkflow(ctx, connect.NewRequest(
		&apiv1.PublishWorkflowRequest{WorkflowId: workflowID, VersionNote: "e2e harness"})); err != nil {
		return fmt.Errorf("publish the re-pointed %s workflow: %w", kind, err)
	}
	fmt.Printf("workflow : %-22s re-pointed at this run's %s worker\n", adapterWorkflowName(kind), kind)
	return nil
}
