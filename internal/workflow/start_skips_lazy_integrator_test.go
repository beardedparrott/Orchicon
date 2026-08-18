package workflow_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestStartWorkflowSkipsLazyIntegrator verifies the laziness contract (AC#1
// regression): StartRun must NOT seed a PENDING step run for a lazy
// conflict-chain step (the Integrator). The Integrator is wired only through
// the merge gate's conflict_chain with no static depends_on edge, so seeding
// one would let the reconciler dispatch it on the first clean pass — running
// the Integrator even with no conflict. It gains a run only when the gate
// detects a conflict and re-enters the chain (conflictReenter →
// createChainRuns). The normal-path steps (merge + gate) ARE seeded.
func TestStartWorkflowSkipsLazyIntegrator(t *testing.T) {
	pool := forceTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ttx, err := pool.BeginTenantTx(ctx, forceTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: forceTestTenant,
		Name: "Lazy Chain", Slug: "lazy-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: forceTestTenant, ProjectID: proj.ID,
		Name: "Conflict-Aware Template", CurrentVersion: 1,
		Status: domain.WorkflowPublished, Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	steps := []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", Ref: "w_se_devops_engineer", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", Ref: "w_se_integrator", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: forceTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionPublished,
		Steps: stepsJSON, Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	svc := workflow.New(pool, logger, nil)
	resp, err := svc.StartWorkflow(tenant.WithID(ctx, forceTestTenant),
		connect.NewRequest(&apiv1.StartWorkflowRequest{
			WorkflowId: wf.ID, ProjectId: proj.ID, RunContext: "{}",
		}))
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, forceTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx2.Tx, forceTestTenant, resp.Msg.Run.Id)
	if err != nil {
		t.Fatalf("list step runs: %v", err)
	}
	seeded := map[string]bool{}
	for _, sr := range stepRuns {
		seeded[sr.StepID] = true
	}
	if !seeded["merge"] {
		t.Error("merge step not seeded at run start")
	}
	if !seeded["gate"] {
		t.Error("merge gate not seeded at run start")
	}
	if seeded["integrator"] {
		t.Error("Integrator was seeded at run start — it must be lazy (only a conflict re-entry creates its run)")
	}
}
