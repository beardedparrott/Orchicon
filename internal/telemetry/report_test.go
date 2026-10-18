package telemetry_test

// report_test.go — TelemetryService.GetTelemetryReport, the reports view of
// the native Telemetry page (token usage, cost, duration, failure rate per
// project). Reads Postgres only, so it must keep working when a telemetry
// store is down: that is what makes the UI's "partial" state truthful.

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestGetTelemetryReportProjectGrouping seeds two projects — one healthy, one
// with a failed execution — and asserts the report's tokens, cost, execution
// count and failure rate, plus the resolved display name and window.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed telemetry tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/telemetry/ -run TestGetTelemetryReportProjectGrouping -v
func TestGetTelemetryReportProjectGrouping(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed telemetry report test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const tenantID = "tnt_report_rpc_test"
	const goodProject = "prj_report_rpc_good"
	const badProject = "prj_report_rpc_bad"
	now := time.Now().UTC()
	base := now.Add(-1 * time.Hour).Truncate(time.Second)

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Tx.Exec(ctx, `INSERT INTO projects (id, tenant_id, name, slug)
		VALUES ($1,$2,'Good Project','good-project'), ($3,$2,'Bad Project','bad-project')`,
		goodProject, tenantID, badProject); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	if _, err := ttx.Tx.Exec(ctx, `INSERT INTO usage_records
		(id, tenant_id, project_id, task_id, execution_id, provider, model, prompt_tokens, total_tokens, cost_usd, occurred_at)
		VALUES ($1,$2,$3,'wi_1','exe_good','anthropic','claude',600,1000,0.10,$5),
		       ($4,$2,$6,'wi_1','exe_bad','anthropic','claude',300,500,0.05,$5)`,
		"ur_good", tenantID, goodProject, "ur_bad", base, badProject); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	if _, err := ttx.Tx.Exec(ctx, `INSERT INTO worker_executions
		(id, tenant_id, project_id, task_id, worker_id, worker_version, status, started_at, ended_at)
		VALUES ('exe_good',$1,$2,'wi_1','wrk_1',1,'succeeded',$4,$5),
		       ('exe_bad',$1,$3,'wi_1','wrk_1',1,'failed',$4,$5)`,
		tenantID, goodProject, badProject, base, base.Add(60*time.Second)); err != nil {
		t.Fatalf("seed executions: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		cleanup, err := pool.BeginTenantTx(cctx, tenantID)
		if err != nil {
			return
		}
		for _, q := range []string{
			`DELETE FROM usage_records WHERE tenant_id = $1`,
			`DELETE FROM worker_executions WHERE tenant_id = $1`,
			`DELETE FROM projects WHERE tenant_id = $1`,
		} {
			_, _ = cleanup.Tx.Exec(cctx, q, tenantID)
		}
		_ = cleanup.Commit(cctx)
	})

	svc := telemetry.NewService(pool, nil, nil)
	resp, err := svc.GetTelemetryReport(tenant.WithID(ctx, tenantID), connect.NewRequest(
		&apiv1.GetTelemetryReportRequest{
			GroupBy: apiv1.TelemetryReportGroupBy_TELEMETRY_REPORT_GROUP_BY_PROJECT,
		},
	))
	if err != nil {
		t.Fatalf("GetTelemetryReport: %v", err)
	}
	msg := resp.Msg
	if msg.GetWindowStart() == nil || msg.GetWindowEnd() == nil {
		t.Fatal("report must report its window")
	}
	byKey := map[string]*apiv1.TelemetryReportRow{}
	for _, r := range msg.GetRows() {
		byKey[r.GetGroupKey()] = r
	}
	good, ok := byKey[goodProject]
	if !ok {
		t.Fatalf("no row for %s (rows: %d)", goodProject, len(msg.GetRows()))
	}
	if good.GetGroupName() != "Good Project" {
		t.Errorf("project name not resolved: %q", good.GetGroupName())
	}
	if good.GetTotalTokens() != 1000 || good.GetCostUsd() != 0.10 {
		t.Errorf("good tokens/cost: got %d / %v", good.GetTotalTokens(), good.GetCostUsd())
	}
	if good.GetFailedRuns() != 0 {
		t.Errorf("good project should have no failures, got %d", good.GetFailedRuns())
	}
	bad, ok := byKey[badProject]
	if !ok {
		t.Fatalf("no row for %s", badProject)
	}
	if bad.GetFailedRuns() != 1 {
		t.Errorf("bad project failed count: want 1, got %d", bad.GetFailedRuns())
	}
	// 60 s execution → 60000 ms over 1 finished unit.
	if bad.GetDurationMsAvg() != 60000 {
		t.Errorf("bad project avg duration: want 60000 ms, got %d", bad.GetDurationMsAvg())
	}
	totals := msg.GetTotals()
	if totals.GetExecutions() != 2 || totals.GetFailedRuns() != 1 {
		t.Errorf("totals: want 2 executions / 1 failed, got %d / %d", totals.GetExecutions(), totals.GetFailedRuns())
	}
	if got, want := totals.GetFailureRate(), 0.5; got != want {
		t.Errorf("failure rate: want %v, got %v", want, got)
	}
}
