package db_test

// settings_roundtrip_test.go — the settings row must survive a write-and-read-back.
//
// THIS TEST EXISTS BECAUSE THE BUG IT CATCHES SHIPPED. The tenant_settings row is read by a single
// ~70-column SELECT scanned positionally into a struct, and written by a ~69-parameter INSERT whose
// column list, VALUES placeholders, SET clause and Go argument order must all agree. Nothing checked
// that they did, and Go's type system cannot: every field is a scalar, so a column inserted in the wrong
// position compiles, unit-tests green, and fails only at runtime against a real database.
//
// It failed exactly that way. Two retention columns landed in the wrong slot in the SELECT list, and the
// TUI died with:
//
//	internal: db: scan tenant settings: can't scan into dest[54]
//	(col: execution_retention_enabled): cannot scan bool (OID 16) in binary format into *int
//
// The misalignment also reached the INSERT, where session_access_token_ttl_seconds was being written
// into execution_retention_enabled's placeholders — a data-corruption bug nothing would have surfaced
// until an operator noticed a token TTL had turned into a boolean.
//
// SO: write DIFFERENT, DISTINGUISHABLE values into every field, read the row back, and compare. A
// column-order mistake anywhere in the chain shows up as a field that came back holding another
// field's value, in one test, with no database expertise required to read the failure.

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestTenantSettingsRoundTrip writes a distinctive value into every tenant_settings column and reads it
// back, so any reordering of the SELECT/INSERT/scan chain fails loudly here rather than in production.
func TestTenantSettingsRoundTrip(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping tenant settings round-trip test")
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

	tenant := "tnt_roundtrip_" + db.NewID()

	keep := int64(42)
	want := db.TenantSettingsRow{
		TenantID:                tenant,
		DefaultWorkerModel:      "adapter/prov/model-a",
		DefaultAskOrchiconModel: "adapter/prov/model-b",

		// The retention knobs — the columns this test was written for. DISTINCT, non-default values so
		// a swap with a neighbouring column cannot coincidentally match.
		ExecutionRetentionEnabled:        true,
		ExecutionRetentionKeep:           &keep,
		ExecutionRetentionVacuum:         true,
		ExecutionRetentionTimeoutSeconds: 137,
		OutboxRetentionDays:              11,

		// Neighbours the retention columns were originally placed next to, and therefore the most likely
		// victims of a positional mistake. Each gets a value no retention field could hold.
		SessionAccessTokenTtlSeconds:  1900,
		SessionRefreshTokenTtlSeconds: 91000,
		LogRetentionDays:              3,
		LogMaxFiles:                   4,
		LogMaxSizeMB:                  55,
		LogRollIntervalHours:          13,
		BackupRetentionDays:           9,
		MaxConcurrentRuns:             7,
		MaxConcurrentRunsSet:          true,

		// The Antigravity adapter knobs. Both are NON-default here so a swapped column (or a
		// dropped one) cannot pass by coincidence: the default is oauth-personal / telemetry off.
		AntigravityAuthMethod:     "gemini-api-key",
		AntigravityShareTelemetry: true,
	}

	tx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := db.UpdateTenantSettings(ctx, tx.Tx, tenant, want); err != nil {
		t.Fatalf("update tenant settings: %v", err)
	}

	got, err := db.GetTenantSettings(ctx, tx.Tx, tenant)
	if err != nil {
		// This is the exact call whose scan failed in the TUI. Report it as the primary failure.
		t.Fatalf("get tenant settings (the scan the TUI performs): %v", err)
	}

	// --- the retention fields themselves ---
	if got.ExecutionRetentionEnabled != want.ExecutionRetentionEnabled {
		t.Errorf("execution_retention_enabled = %v, want %v (a column-order bug reads a NEIGHBOUR here)",
			got.ExecutionRetentionEnabled, want.ExecutionRetentionEnabled)
	}
	if got.ExecutionRetentionKeep == nil {
		t.Errorf("execution_retention_keep came back NULL, want %d", keep)
	} else if *got.ExecutionRetentionKeep != keep {
		t.Errorf("execution_retention_keep = %d, want %d", *got.ExecutionRetentionKeep, keep)
	}
	if got.ExecutionRetentionVacuum != want.ExecutionRetentionVacuum {
		t.Errorf("execution_retention_vacuum = %v, want %v", got.ExecutionRetentionVacuum, want.ExecutionRetentionVacuum)
	}
	if got.ExecutionRetentionTimeoutSeconds != want.ExecutionRetentionTimeoutSeconds {
		t.Errorf("execution_retention_timeout_seconds = %d, want %d",
			got.ExecutionRetentionTimeoutSeconds, want.ExecutionRetentionTimeoutSeconds)
	}
	if got.OutboxRetentionDays != want.OutboxRetentionDays {
		t.Errorf("outbox_retention_days = %d, want %d", got.OutboxRetentionDays, want.OutboxRetentionDays)
	}

	// --- the Antigravity knobs ---
	if got.AntigravityAuthMethod != want.AntigravityAuthMethod {
		t.Errorf("antigravity_auth_method = %q, want %q", got.AntigravityAuthMethod, want.AntigravityAuthMethod)
	}
	if got.AntigravityShareTelemetry != want.AntigravityShareTelemetry {
		t.Errorf("antigravity_share_telemetry = %v, want %v", got.AntigravityShareTelemetry, want.AntigravityShareTelemetry)
	}

	// A NEVER-CONFIGURED tenant must get the shipped defaults: personal OAuth (the method the
	// operator most likely already has working) with telemetry OFF (ToS section 5 is an explicit
	// grant, so nothing is shared without an opt-in).
	fresh := "tnt_fresh_" + db.NewID()
	freshTx, err := pool.BeginTenantTx(ctx, fresh)
	if err != nil {
		t.Fatalf("begin fresh tx: %v", err)
	}
	defer freshTx.Rollback(ctx)
	freshRow, err := db.GetTenantSettings(ctx, freshTx.Tx, fresh)
	if err != nil {
		t.Fatalf("get fresh tenant settings: %v", err)
	}
	if freshRow.AntigravityAuthMethod != "oauth-personal" {
		t.Errorf("fresh tenant antigravity_auth_method = %q, want oauth-personal", freshRow.AntigravityAuthMethod)
	}
	if freshRow.AntigravityShareTelemetry {
		t.Error("fresh tenant antigravity_share_telemetry = true, want false (telemetry ships OFF)")
	}

	// --- and the neighbours, which is where a positional mistake actually shows up ---
	if got.SessionAccessTokenTtlSeconds != want.SessionAccessTokenTtlSeconds {
		t.Errorf("session_access_token_ttl_seconds = %d, want %d (was written into a retention column)",
			got.SessionAccessTokenTtlSeconds, want.SessionAccessTokenTtlSeconds)
	}
	if got.SessionRefreshTokenTtlSeconds != want.SessionRefreshTokenTtlSeconds {
		t.Errorf("session_refresh_token_ttl_seconds = %d, want %d",
			got.SessionRefreshTokenTtlSeconds, want.SessionRefreshTokenTtlSeconds)
	}
	if got.LogRetentionDays != want.LogRetentionDays {
		t.Errorf("log_retention_days = %d, want %d", got.LogRetentionDays, want.LogRetentionDays)
	}
	if got.LogMaxFiles != want.LogMaxFiles {
		t.Errorf("log_max_files = %d, want %d", got.LogMaxFiles, want.LogMaxFiles)
	}
	if got.LogMaxSizeMB != want.LogMaxSizeMB {
		t.Errorf("log_max_size_mb = %d, want %d", got.LogMaxSizeMB, want.LogMaxSizeMB)
	}
	if got.BackupRetentionDays != want.BackupRetentionDays {
		t.Errorf("backup_retention_days = %d, want %d", got.BackupRetentionDays, want.BackupRetentionDays)
	}
	if got.MaxConcurrentRuns != want.MaxConcurrentRuns {
		t.Errorf("max_concurrent_runs = %d, want %d", got.MaxConcurrentRuns, want.MaxConcurrentRuns)
	}

	// --- the three-state keep column, exercised directly ---
	// An explicit 0 must survive as 0 rather than arriving as NULL; the retention pass treats the
	// difference as "keep nothing" vs "use the built-in default".
	zero := int64(0)
	want.ExecutionRetentionKeep = &zero
	if _, err := db.UpdateTenantSettings(ctx, tx.Tx, tenant, want); err != nil {
		t.Fatalf("update with keep=0: %v", err)
	}
	got, err = db.GetTenantSettings(ctx, tx.Tx, tenant)
	if err != nil {
		t.Fatalf("get after keep=0: %v", err)
	}
	if got.ExecutionRetentionKeep == nil {
		t.Fatal("execution_retention_keep came back NULL after being set to 0 — 'keep nothing' became 'unset'")
	}
	if *got.ExecutionRetentionKeep != 0 {
		t.Fatalf("execution_retention_keep = %d, want 0", *got.ExecutionRetentionKeep)
	}
}
