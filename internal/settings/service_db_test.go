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
		ttx, err := pool.BeginTenantTx(context.Background(), probeTenant)
		if err != nil {
			return
		}
		_, _ = ttx.Exec(context.Background(), `DELETE FROM tenant_settings WHERE tenant_id=$1`, probeTenant)
		_ = ttx.Commit(context.Background())
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
