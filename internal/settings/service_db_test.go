package settings

// service_db_test.go — the WIRING, not just the helper.
//
// The unit test beside this file proves mergeStallSettingsFromCurrent does the right thing; it cannot
// prove UpdateSettings CALLS it, because it never goes through the service. That gap is exactly how the
// original defect shipped, so the call site gets its own test, driving the real Service over a real pool.
//
// It runs against a SYNTHETIC TENANT, not the operator's. tenant_settings has no foreign key to tenants
// and its RLS policy is tenant-scoped, so a row under a probe id is invisible to every other tenant and
// the test cannot touch the operator's own configuration. The row is deleted on cleanup.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable'
//	go test ./internal/settings/ -run TestUpdateSettingsPartialSave -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"

	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func TestUpdateSettingsPartialSaveKeepsStallWindows(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed settings-merge test")
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

	// A synthetic tenant, so the operator's own settings are never in play.
	const probeTenant = "tnt_settings_merge_probe"
	t.Cleanup(func() {
		cctx := context.Background()
		ttx, err := pool.BeginTenantTx(cctx, probeTenant)
		if err != nil {
			return
		}
		// THE AUDIT ROWS GO TOO.
		_, _ = ttx.Exec(cctx, `DELETE FROM audit_events WHERE tenant_id=$1`, probeTenant)
		_, _ = ttx.Exec(cctx, `DELETE FROM tenant_settings WHERE tenant_id=$1`, probeTenant)
		_ = ttx.Commit(cctx)
	})

	// Seed the window the operator configured.
	ttx, err := pool.BeginTenantTx(ctx, probeTenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := db.UpdateTenantSettings(ctx, ttx.Tx, probeTenant, db.TenantSettingsRow{
		StallNoProgressWindowSeconds: i64p(600),
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("seed settings: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	svc := New(pool, slog.Default(), dsn)
	tctx := tenant.WithID(ctx, probeTenant)

	// THE BACKUP PANEL'S PAYLOAD: it names no stall field at all. Before the merge this blanked every
	// stall window in the row, reverting 600s to the 120s built-in default.
	if _, err := svc.UpdateSettings(tctx, connect.NewRequest(&apiv1.UpdateSettingsRequest{
		Settings: &apiv1.TenantSettings{BackupSchedule: "0 3 * * *"},
	})); err != nil {
		t.Fatalf("partial save: %v", err)
	}

	read, err := pool.BeginTenantTx(ctx, probeTenant)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer read.Rollback(ctx)
	got, err := db.GetTenantSettings(ctx, read.Tx, probeTenant)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if got.StallNoProgressWindowSeconds == nil || *got.StallNoProgressWindowSeconds != 600 {
		t.Fatalf("the 600s no-progress window was lost by a save that did not mention it (got %v) — this is the wiring bug: UpdateSettings must call mergeStallSettingsFromCurrent", got.StallNoProgressWindowSeconds)
	}
	if got.BackupSchedule != "0 3 * * *" {
		t.Errorf("the field the client DID name was not written: %q", got.BackupSchedule)
	}

	// AND A FIELD THE CLIENT NAMES IS STILL APPLIED — an explicit 0 means DISABLED and must survive.
	if _, err := svc.UpdateSettings(tctx, connect.NewRequest(&apiv1.UpdateSettingsRequest{
		Settings: &apiv1.TenantSettings{StallNoProgressWindowSeconds: i64p(0)},
	})); err != nil {
		t.Fatalf("explicit-zero save: %v", err)
	}
	read2, err := pool.BeginTenantTx(ctx, probeTenant)
	if err != nil {
		t.Fatalf("begin read2: %v", err)
	}
	defer read2.Rollback(ctx)
	after, err := db.GetTenantSettings(ctx, read2.Tx, probeTenant)
	if err != nil {
		t.Fatalf("read settings again: %v", err)
	}
	if after.StallNoProgressWindowSeconds == nil || *after.StallNoProgressWindowSeconds != 0 {
		t.Fatalf("an explicit 0 (DISABLED) did not take effect (got %v) — the merge must not swallow a value the client sent", after.StallNoProgressWindowSeconds)
	}
}

// TestUpdateSettingsRoundTripsContextEditing is the reported bug, end to end, through the real Service:
//
//	"set Keep last N tool results to 20, hit Save, and it shows blank immediately / after a reload."
//
// It failed because UpdateSettings reset the ladder to a zero value before overlaying the client's
// document — and BudgetJSON() is an allowlist, so every key it could not carry was dropped from the row.
// The setting was accepted, stored without itself, and read back missing.
//
// This asserts the FULL round trip: save, read the row, and re-derive the document the UI is seeded from.
func TestUpdateSettingsRoundTripsContextEditing(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed settings round-trip test")
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

	const probeTenant = "tnt_settings_context_editing_probe"
	t.Cleanup(func() {
		cctx := context.Background()
		ttx, err := pool.BeginTenantTx(cctx, probeTenant)
		if err != nil {
			return
		}
		_, _ = ttx.Exec(cctx, `DELETE FROM audit_events WHERE tenant_id=$1`, probeTenant)
		_, _ = ttx.Exec(cctx, `DELETE FROM tenant_settings WHERE tenant_id=$1`, probeTenant)
		_ = ttx.Commit(cctx)
	})

	svc := New(pool, slog.Default(), dsn)
	tctx := tenant.WithID(ctx, probeTenant)

	// Exactly what the GUI sends: the whole budget document with the knob set.
	const sent = `{"max_tool_output_bytes":32768,"context_editing":{"keep_tool_calls":20}}`
	if _, err := svc.UpdateSettings(tctx, connect.NewRequest(&apiv1.UpdateSettingsRequest{
		Settings: &apiv1.TenantSettings{DefaultBudgetOverrides: sent},
	})); err != nil {
		t.Fatalf("save: %v", err)
	}

	read, err := pool.BeginTenantTx(ctx, probeTenant)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer read.Rollback(ctx)
	got, err := db.GetTenantSettings(ctx, read.Tx, probeTenant)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	// The document the UI is SEEDED FROM is BudgetJSON(), not the stored blob — assert on that, because
	// that is the thing the page reads.
	var doc map[string]any
	if err := json.Unmarshal(got.BudgetJSON(), &doc); err != nil {
		t.Fatalf("BudgetJSON: %v", err)
	}
	ce, ok := doc["context_editing"].(map[string]any)
	if !ok {
		t.Fatalf("context_editing is MISSING from the document the UI reloads (blob=%s) — a save appears to do nothing", got.DefaultBudgetOverrides)
	}
	if n, _ := ce["keep_tool_calls"].(float64); n != 20 {
		t.Errorf("keep_tool_calls = %v, want 20", ce["keep_tool_calls"])
	}
	if n, _ := doc["max_tool_output_bytes"].(float64); n != 32768 {
		t.Errorf("max_tool_output_bytes = %v, want 32768", doc["max_tool_output_bytes"])
	}
}
