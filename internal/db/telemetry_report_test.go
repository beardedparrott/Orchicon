package db

// telemetry_report_test.go — the duration / failure aggregation behind the
// native Telemetry page's Reports view.
//
// Two things are pinned here because both were cheap to get wrong:
//
//  1. durations use EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000 —
//     EXTRACT(MILLISECOND …) returns only the milliseconds FIELD of the
//     interval, so a 90 s execution reported 30000 ms.
//  2. an execution that has not ended (ended_at IS NULL) counts toward the
//     execution total but contributes no duration and is never "failed".
//
// Guarded by ORCHICON_TEST_DSN like the rest of the package's DB tests; it
// deletes the rows it inserts.

import (
	"context"
	"os"
	"testing"
	"time"
)

func telemetryReportTestPool(t *testing.T) *Pool {
	t.Helper()
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed telemetry report test")
	}
	pool, err := Open(context.Background(), os.Getenv("ORCHICON_TEST_DSN"))
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestGetProjectDurationStatsCountsFailuresAndMillis(t *testing.T) {
	pool := telemetryReportTestPool(t)
	ctx := context.Background()
	const tenantID = "tnt_report_test"
	const projectID = "prj_report_test"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	rows := []struct {
		id      string
		status  string
		started time.Time
		ended   *time.Time
	}{
		{"exe_report_ok", "succeeded", base, durPtr(base, 90*time.Second)},
		{"exe_report_fail", "failed", base.Add(time.Minute), durPtr(base.Add(time.Minute), 30*time.Second)},
		{"exe_report_running", "running", base.Add(2 * time.Minute), nil},
	}
	for _, r := range rows {
		if _, err := ttx.Tx.Exec(ctx, `INSERT INTO worker_executions
			(id, tenant_id, project_id, task_id, worker_id, worker_version, status, started_at, ended_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.id, tenantID, projectID, "wi_report_test", "wrk_report_test", 1, r.status, r.started, r.ended,
		); err != nil {
			t.Fatalf("insert execution %s: %v", r.id, err)
		}
	}
	// The whole test runs inside one rolled-back tenant tx, so the shared
	// instance is left exactly as found (no cleanup DELETE needed).
	stats, err := GetProjectDurationStats(ctx, ttx.Tx, tenantID, projectID, "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetProjectDurationStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("want 1 group, got %d", len(stats))
	}
	got := stats[0]
	if got.Executions != 3 {
		t.Errorf("executions: want 3, got %d", got.Executions)
	}
	if got.Failed != 1 {
		t.Errorf("failed: want 1 (running is not failed), got %d", got.Failed)
	}
	// 90 s + 30 s = 120000 ms. The EXTRACT(MILLISECOND …) bug would have
	// produced 0 + 0 here because both intervals have a 0 ms field.
	if got.DurationMsTotal != 120000 {
		t.Errorf("duration total: want 120000 ms, got %d", got.DurationMsTotal)
	}
}

func durPtr(start time.Time, d time.Duration) *time.Time {
	end := start.Add(d)
	return &end
}
