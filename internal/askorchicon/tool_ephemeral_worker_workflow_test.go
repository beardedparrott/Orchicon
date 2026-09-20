package askorchicon

// tool_ephemeral_worker_workflow_test.go — the ephemeral layer for WORKERS and
// WORKFLOWS at the tool boundary.
//
// The operator added these two tables to the mechanism: "Yeah I would like
// ephemeral only on the workers and workflows Quick Work creates." Quick Work
// builds a throwaway worker pinned to its own model_ref and a throwaway workflow
// to run, and neither may appear in the Workers or Workflows screens.
//
// The DB-backed cases skip unless ORCHICON_TEST_DSN is set, like the rest of
// this package's pool tests.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// THE DELETE WORKFLOW TOOL EXISTS AND WARNS ABOUT THE CASCADE.
//
// The warning matters more here than for a work item: db.DeleteWorkflow also
// removes the workflow's runs, step runs and versions, so an unconsidered call
// destroys history rather than just a row.
func TestDeleteWorkflowToolIsRegisteredAndHonest(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, ok := r.Get("delete_workflow")
	if !ok {
		t.Fatal("delete_workflow is not registered — an agent that built a throwaway (Quick Work) workflow has " +
			"no way to remove it, so the transient it built is an invisible record that stays forever")
	}
	if !td.Mutating {
		t.Error("delete_workflow is not marked Mutating, so the confirm-before-mutate discipline would not " +
			"apply to a destructive, irreversible call")
	}
	desc := strings.ToUpper(td.Description)
	for _, want := range []string{"PERMANENTLY", "IRREVERSIBLE", "CANNOT BE RESTORED", "RUN HISTORY"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description does not state %q, which is the only warning an agent gets about the "+
				"cascade: %q", want, td.Description)
		}
	}
	if _, ok := r.Get(normalizeAskToolName(askToolNamePrefix + "delete_workflow")); !ok {
		t.Error("the advertised `orchicon_delete_workflow` form does not resolve")
	}
}

// THE FLAG AND THE OPT-IN ARE ADVERTISED ON EVERY AFFECTED TOOL, so the model can
// mark a transient and can still find the ones it marked.
func TestEphemeralParametersAreAdvertisedOnWorkerAndWorkflowTools(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)

	for _, tc := range []struct {
		tool   string
		prop   string
		expect string // a phrase the description must contain
	}{
		{"create_worker", "ephemeral", "hidden"},
		{"create_workflow", "ephemeral", "hidden"},
		{"list_workers", "include_ephemeral", "excluded by default"},
		{"list_workflows", "include_ephemeral", "excluded by default"},
	} {
		td, ok := r.Get(tc.tool)
		if !ok {
			t.Fatalf("%s is not registered", tc.tool)
		}
		prop, ok := td.Properties[tc.prop]
		if !ok {
			t.Errorf("%s has no %q property — Quick Work cannot manage its own transients through this tool",
				tc.tool, tc.prop)
			continue
		}
		if prop.Type != "boolean" {
			t.Errorf("%s.%s is %q, want boolean", tc.tool, tc.prop, prop.Type)
		}
		if !strings.Contains(strings.ToLower(td.Description), tc.expect) {
			t.Errorf("%s's description does not mention %q, so the model would not know the record is hidden: %q",
				tc.tool, tc.expect, td.Description)
		}
	}

	// The create tools must name the tool that removes the record: a transient
	// left behind by a cancel-style call IS the invisible record to avoid, and
	// the agent has to be told the disposal path.
	for _, pair := range [][2]string{
		{"create_worker", "delete_worker"},
		{"create_workflow", "delete_workflow"},
	} {
		td, _ := r.Get(pair[0])
		if !strings.Contains(td.Description, pair[1]) {
			t.Errorf("%s does not name %s, so an agent finishing a Quick Work job would not know how to dispose "+
				"of the record it created", pair[0], pair[1])
		}
	}

	// `ephemeral` must NOT be required: ordinary creates stay two-argument calls.
	for _, tool := range []string{"create_worker", "create_workflow"} {
		td, _ := r.Get(tool)
		for _, req := range td.Required {
			if req == "ephemeral" {
				t.Errorf("%s requires `ephemeral`, which would force every ordinary create to state it", tool)
			}
		}
	}
}

// everyEphemeralToolMustHaveAFunction guards against a registration whose Fn is
// nil — which would panic at call time rather than at boot.
func TestDeleteWorkflowToolHasAFunction(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, ok := r.Get("delete_workflow")
	if !ok {
		t.Fatal("delete_workflow is not registered")
	}
	if td.Fn == nil {
		t.Fatal("delete_workflow has no Fn — calling it would panic instead of returning an error")
	}
	// A malformed call must be rejected before any pool use.
	if _, err := td.Fn(context.Background(), nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("delete_workflow accepted a call with no id")
	}
}

// THE GUARD: A NON-EPHEMERAL WORKFLOW WITH RUNS IS REFUSED.
//
// Deleting a workflow takes its entire run history with it, and an agent cannot
// see how much history it is about to delete. So the destructive case has to be
// stated explicitly — and `confirm_delete_runs` must NOT be required for the
// case this tool exists for (an ephemeral workflow Quick Work just built), or the
// cleanup path would be unusable.
//
// This is asserted WITHOUT a database for the ephemeral case: a nil pool proves
// the confirmation is not consulted before the transaction opens, so an
// ephemeral delete is never blocked by it.
func TestDeleteWorkflowRefusesHistoryWithoutConfirmation(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, _ := r.Get("delete_workflow")
	props, ok := td.Properties["confirm_delete_runs"]
	if !ok {
		t.Fatal("delete_workflow has no `confirm_delete_runs` property, so the agent cannot acknowledge " +
			"destroying run history and would simply be refused")
	}
	if props.Type != "boolean" {
		t.Errorf("confirm_delete_runs is %q, want boolean", props.Type)
	}
	// confirm_delete_runs must not be REQUIRED — it would then be demanded for an
	// ephemeral workflow too, whose runs belong to the job being cleaned up.
	for _, req := range td.Required {
		if req == "confirm_delete_runs" {
			t.Error("confirm_delete_runs is required, which would make the ordinary cleanup path (an ephemeral " +
				"workflow with runs) need a confirmation that does not apply to it")
		}
	}
}

// THE DB-BACKED GUARD BEHAVIOUR: the real refusal, and the real allowance.
func TestDeleteWorkflowGuardAgainstRealRows(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed delete_workflow guard test")
	}
	ctx := tenant.WithID(context.Background(), "tnt_dwf_guard")
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatal(err)
	}
	tenantID := "tnt_dwf_guard"
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID, Name: "dwf", Slug: "dwf-" + db.NewID(),
		Status: domain.ProjectActive, Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	db.CleanupProject(t, pool, tenantID, proj.ID)

	mkWf := func(name string, ephemeral bool) db.WorkflowRow {
		wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
			ID: db.NewID(), TenantID: tenantID, ProjectID: proj.ID, Name: name,
			Type: domain.WorkflowTypeOneShot, Status: domain.WorkflowDraft, Ephemeral: ephemeral,
		})
		if err != nil {
			t.Fatal(err)
		}
		return wf
	}
	mkRun := func(wfID string) {
		if _, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: tenantID, WorkflowID: wfID, WorkflowVersion: 1,
			ProjectID: proj.ID, Status: domain.WorkflowRunPending, RunContext: []byte("{}"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	realWithRuns := mkWf("real-with-runs", false)
	mkRun(realWithRuns.ID)
	realNoRuns := mkWf("real-no-runs", false)
	ephWithRuns := mkWf("eph-with-runs", true)
	mkRun(ephWithRuns.ID)

	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	call := func(args string) (json.RawMessage, error) {
		return toolDeleteWorkflow(ctx, pool, json.RawMessage(args))
	}

	// 1. REFUSED: real workflow, real history, no confirmation.
	if _, err := call(`{"id":"` + realWithRuns.ID + `"}`); err == nil {
		t.Error("delete_workflow deleted a NON-ephemeral workflow's entire run history without confirmation — " +
			"an agent cannot see what it is destroying, so this has to be refused")
	} else if !strings.Contains(err.Error(), "confirm_delete_runs") {
		t.Errorf("the refusal does not tell the agent how to proceed deliberately: %v", err)
	}

	// 2. ALLOWED: the same workflow, with the confirmation. (Nothing to protect:
	//    the caller has said the history is expendable.)
	if _, err := call(`{"id":"` + realWithRuns.ID + `","confirm_delete_runs":true}`); err != nil {
		t.Errorf("delete_workflow refused a confirmed delete of a real workflow: %v", err)
	}

	// 3. ALLOWED WITHOUT CONFIRMATION: a real workflow with no runs — there is no
	//    history to lose, so demanding a confirmation would be noise.
	if _, err := call(`{"id":"` + realNoRuns.ID + `"}`); err != nil {
		t.Errorf("delete_workflow refused a workflow with no run history, which needs no confirmation: %v", err)
	}

	// 4. ALLOWED: the ephemeral workflow, runs and all. This is the path the tool
	//    exists for — requiring a confirmation here would make Quick Work's
	//    cleanup impossible.
	if _, err := call(`{"id":"` + ephWithRuns.ID + `"}`); err != nil {
		t.Errorf("delete_workflow refused an EPHEMERAL workflow that had runs — this is exactly the cleanup "+
			"Quick Work needs, and its runs belong to the job being removed: %v", err)
	}

	// And the history really went with it.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	var runs int
	if err := ttx2.Tx.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE tenant_id = $1`, tenantID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("%d run row(s) survived the workflow deletes", runs)
	}
}
