package scheduler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// --- the decision (pure) ---------------------------------------------------
//
// These exercise resolveStepRecoveryConfig directly, so they run everywhere.
// The DB-backed tests further down pin the two paths it feeds; they need
// ORCHICON_TEST_DSN like the rest of this package's DB tests.

// TestResolveStepRecoveryConfigAppliesEphemeralDefault pins the ephemeral
// default: when a step declares NO strategy of its own, an ephemeral run must
// resolve to the recovery engine and never to the blind "retry" fallback.
//
// Retry is not the recovery flow. It clones the ticket and re-dispatches it
// without ever creating a RecoveryExecution, so when its attempts run out the
// step fails with RecoveryID null: nothing captured, nothing summarized,
// nothing to resume from, and no recovery record for a human to review.
// That is the whole reason a machine-managed run — with no human watching the
// step between attempts — must not inherit it as a default.
func TestResolveStepRecoveryConfigAppliesEphemeralDefault(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{"no config at all", ""},
		{"empty object", "{}"},
		{"empty recovery block", `{"recovery":{}}`},
		{"recovery block carrying only max_attempts", `{"recovery":{"max_attempts":6}}`},
		{"config with unrelated keys only", `{"foo":"bar"}`},
		{"malformed config", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveStepRecoveryConfig(tc.config, true)
			if got.Strategy != strategySummarizeRestart {
				t.Fatalf("ephemeral run with step config %q resolved to strategy %q, want %q",
					tc.config, got.Strategy, strategySummarizeRestart)
			}
		})
	}
}

// TestResolveStepRecoveryConfigExplicitStrategyWins pins the other half of the
// contract: the default is a DEFAULT. A step that names a strategy keeps it —
// including an explicit "retry", because an operator who wrote that down is
// not second-guessed by the ephemeral rule.
func TestResolveStepRecoveryConfigExplicitStrategyWins(t *testing.T) {
	for _, want := range []string{strategyRetry, "stop", "human_escalation", strategySummarizeRestart} {
		t.Run(want, func(t *testing.T) {
			config := `{"recovery":{"strategy":"` + want + `"}}`
			if got := resolveStepRecoveryConfig(config, true); got.Strategy != want {
				t.Fatalf("explicit strategy was overridden: config %q resolved to %q, want %q",
					config, got.Strategy, want)
			}
		})
	}
}

// TestResolveStepRecoveryConfigNonEphemeralIsUnchanged pins that the change is
// scoped: every non-ephemeral run resolves exactly as readStepRecoveryConfig
// always did. A human-driven SDLC run must not silently change behaviour.
func TestResolveStepRecoveryConfigNonEphemeralIsUnchanged(t *testing.T) {
	configs := []string{
		"",
		"{}",
		`{"recovery":{}}`,
		`{"recovery":{"strategy":"retry"}}`,
		`{"recovery":{"strategy":"retry","max_attempts":1}}`,
		`{"recovery":{"strategy":"summarize_restart","max_attempts":6}}`,
		`{"recovery":{"strategy":"stop"}}`,
		`{not json`,
	}
	for _, config := range configs {
		want := readStepRecoveryConfig(config)
		got := resolveStepRecoveryConfig(config, false)
		if got.Strategy != want.Strategy || got.MaxAttempts != want.MaxAttempts {
			t.Fatalf("non-ephemeral config %q changed: got {%q, %d}, want {%q, %d}",
				config, got.Strategy, got.MaxAttempts, want.Strategy, want.MaxAttempts)
		}
	}
}

// TestResolveStepRecoveryConfigKeepsMaxAttempts pins that the ephemeral
// default swaps the STRATEGY only. The attempt bound is a separate knob with
// its own explicit value on every seeded step, and quietly rewriting it here
// would change how many times a step may re-enter recovery.
func TestResolveStepRecoveryConfigKeepsMaxAttempts(t *testing.T) {
	if got := resolveStepRecoveryConfig("", true); got.MaxAttempts != 3 {
		t.Fatalf("ephemeral default changed the generic attempt bound: got %d, want 3", got.MaxAttempts)
	}
	if got := resolveStepRecoveryConfig(`{"recovery":{"max_attempts":6}}`, true); got.MaxAttempts != 6 {
		t.Fatalf("explicit max_attempts was lost under the ephemeral default: got %d, want 6", got.MaxAttempts)
	}
}

// TestReadStepRecoveryConfigReportsExplicitness pins the mechanism the default
// rests on: the reader must distinguish "the step asked for retry" from "the
// step asked for nothing, so retry is the fallback". If that distinction ever
// collapses, the ephemeral default silently starts overriding explicit
// choices — which is the failure this test exists to catch.
func TestReadStepRecoveryConfigReportsExplicitness(t *testing.T) {
	if got := readStepRecoveryConfig(`{"recovery":{"strategy":"retry"}}`); !got.strategySet {
		t.Fatal("an explicit strategy must report as set")
	}
	if got := readStepRecoveryConfig(`{"recovery":{}}`); got.strategySet {
		t.Fatal("an absent strategy must not report as set")
	}
	if got := readStepRecoveryConfig(""); got.strategySet {
		t.Fatal("an empty config must not report a strategy as set")
	}
}

// --- the paths (DB-backed) -------------------------------------------------

// TestEphemeralStepFailureUsesTheRecoveryEngine is the acceptance criterion
// for the ephemeral default, driven through the real reconciler: an EPHEMERAL
// run whose task step carries NO recovery config must, on failure, enter the
// recovery flow — the step goes `recovering`, records the recovery strategy,
// and stages a recovery trigger — and must NOT leave a cloned work item
// behind.
func TestEphemeralStepFailureUsesTheRecoveryEngine(t *testing.T) {
	trigger := &recordingRecoveryTrigger{}
	// The step config is a bare "{}" — exactly what the Quick Work agent
	// created when it copied the seeded step SHAPE but dropped the recovery
	// block, the regression this change exists to absorb.
	steps := []workflow.StepWire{
		{ID: "st", Name: "Task Step", Kind: domain.StepKindTask, Config: "{}", Ref: "w_se_devops_engineer"},
	}
	pool, rc, run, ticket := newReconcileRun(t, trigger, steps,
		func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {
			createRunningFailedStepRun(t, ttx, run, ticket, "st")
		})
	markWorkItemEphemeral(t, pool, ticket.ID, "orchicon-runtime:ephemeral-test", `["/tmp/ctx"]`)

	before := countWorkItems(t, pool, run.ProjectID)

	if err := rc.reconcileRun(context.Background(), approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}

	if got := getActiveStepRunStatus(t, pool, approvalTestTenant, run.ID, "st"); got != domain.StepRunRecovering {
		t.Fatalf("ephemeral step did not enter recovery: status = %q, want %q", got, domain.StepRunRecovering)
	}

	res := decodeStepResult(t, getStepRunResult(t, pool, approvalTestTenant, run.ID, "st"))
	if got := res["_recovery_strategy"]; got != strategySummarizeRestart {
		t.Fatalf("ephemeral step recorded strategy %v, want %q — the blind retry strategy never creates a RecoveryExecution",
			got, strategySummarizeRestart)
	}
	if got := res["_work_item_id"]; got != ticket.ID {
		t.Fatalf("ephemeral step repointed at a different work item (%v, want %s) — a retry clone must not be created for a machine-managed run",
			got, ticket.ID)
	}
	if len(trigger.calls) != 1 || trigger.calls[0].reason != "step_recovery" {
		t.Fatalf("want exactly 1 step_recovery trigger, got %+v", trigger.calls)
	}

	if after := countWorkItems(t, pool, run.ProjectID); after != before {
		t.Fatalf("the failure created %d extra work item(s) — an ephemeral run must not leave artifacts behind", after-before)
	}
}

// TestEphemeralRetryCloneKeepsTheJobIdentity pins the retry-clone fix. An
// ephemeral run MAY still be configured for an explicit blind retry, and that
// path clones the ticket — so the clone must be the same job: still ephemeral
// (or a machine dispatch leaks into every human work-item view), still on the
// original runtime image, and still carrying the context files and secrets the
// original was dispatched with.
func TestEphemeralRetryCloneKeepsTheJobIdentity(t *testing.T) {
	const (
		wantImage = "orchicon-runtime:ephemeral-test"
		wantCtx   = `["/tmp/ctx"]`
		wantSecs  = `["sec-1"]`
	)
	trigger := &recordingRecoveryTrigger{}
	steps := []workflow.StepWire{
		{ID: "st", Name: "Task Step", Kind: domain.StepKindTask,
			Config: `{"recovery":{"strategy":"retry"}}`, Ref: "w_se_devops_engineer"},
	}
	pool, rc, run, ticket := newReconcileRun(t, trigger, steps,
		func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {
			createRunningFailedStepRun(t, ttx, run, ticket, "st")
		})
	markWorkItemEphemeral(t, pool, ticket.ID, wantImage, wantCtx)
	// Plant the secrets on the original before reconciling, so the clone has
	// something to carry.
	setWorkItemSecrets(t, pool, ticket.ID, wantSecs)

	if err := rc.reconcileRun(context.Background(), approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}

	recovering := decodeStepResult(t, getStepRunResult(t, pool, approvalTestTenant, run.ID, "st"))
	cloneID, _ := recovering["_work_item_id"].(string)
	if cloneID == "" || cloneID == ticket.ID {
		t.Fatalf("explicit retry must clone the ticket; _work_item_id = %q (original %s)", cloneID, ticket.ID)
	}
	if got := recovering["_recovery_strategy"]; got != strategyRetry {
		t.Fatalf("explicit retry must be honoured: strategy = %v, want %q", got, strategyRetry)
	}

	clone := getWorkItem(t, pool, cloneID)
	if !clone.Ephemeral {
		t.Fatal("the retry clone is NOT ephemeral — a machine-managed dispatch leaked into the human work-item views")
	}
	if clone.RuntimeImage != wantImage {
		t.Fatalf("retry clone runtime image = %q, want %q (the retry would run somewhere the original never did)",
			clone.RuntimeImage, wantImage)
	}
	if string(clone.ContextFiles) != wantCtx {
		t.Fatalf("retry clone context files = %s, want %s", clone.ContextFiles, wantCtx)
	}
	if string(clone.SecretIDs) != wantSecs {
		t.Fatalf("retry clone secret ids = %s, want %s", clone.SecretIDs, wantSecs)
	}
}

// --- helpers ---------------------------------------------------------------

func decodeStepResult(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode step result %s: %v", raw, err)
	}
	return m
}

// markWorkItemEphemeral flips a seeded ticket into the machine-managed
// ephemeral shape, carrying the input material a retry clone must preserve.
func markWorkItemEphemeral(t *testing.T, pool *db.Pool, id, runtimeImage, contextFiles string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx,
		`UPDATE work_items SET ephemeral = true, runtime_image = $1, context_files = $2::jsonb
		 WHERE tenant_id = $3 AND id = $4`,
		runtimeImage, contextFiles, approvalTestTenant, id); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func setWorkItemSecrets(t *testing.T, pool *db.Pool, id, secretIDs string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	if _, err := ttx.Exec(ctx,
		`UPDATE work_items SET secret_ids = $1::jsonb WHERE tenant_id = $2 AND id = $3`,
		secretIDs, approvalTestTenant, id); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func getWorkItem(t *testing.T, pool *db.Pool, id string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wi, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, id)
	if err != nil {
		t.Fatalf("get work item %s: %v", id, err)
	}
	return wi
}

func countWorkItems(t *testing.T, pool *db.Pool, projectID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	var n int
	if err := ttx.QueryRow(ctx,
		`SELECT count(*) FROM work_items WHERE tenant_id = $1 AND project_id = $2`,
		approvalTestTenant, projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
