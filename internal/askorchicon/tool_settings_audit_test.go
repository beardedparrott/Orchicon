package askorchicon

// tool_settings_audit_test.go — the agent's settings write must leave a trail.
//
// WHY, and it is not a formality. `update_settings` writes tenant_settings DIRECTLY rather than through the
// SettingsService RPC — and the `settings.updated` audit row lived in that RPC. So an agent-driven change
// left NO trail at all while a human's did, which is the worse half of an audit gap: the trail looked
// complete. The operator hit the consequence of the sibling gap in the same area ("I set 600 seconds … yet
// you said I had to set it"), and there was no record to settle it either way.
//
// It also pins the SHAPE: the row records the stall knobs (db.TenantSettingsRow.AuditFields), so a stall
// window that a save blanked is visible as null rather than absent. That distinction is the whole reason
// the question was unanswerable.
//
// Runs against a SYNTHETIC tenant, so the operator's own settings are never in play, and deletes what it
// creates — a DB-backed test must not leave rows in the tenant it ran against.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestUpdateSettingsToolAudits -v

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

const settingsToolProbeTenant = "tnt_settings_tool_audit_probe"

func TestUpdateSettingsToolAuditsItsWrite(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := context.Background()

	// Seed a settings row under the probe tenant so the tool's read-modify-write has something to read.
	seed, err := pool.BeginTenantTx(ctx, settingsToolProbeTenant)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if _, err := db.UpdateTenantSettings(ctx, seed.Tx, settingsToolProbeTenant, db.TenantSettingsRow{}); err != nil {
		_ = seed.Rollback(ctx)
		t.Fatalf("seed settings: %v", err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	t.Cleanup(func() {
		// Both rows go: the settings row AND the audit row this test causes to be written.
		tctx := context.Background()
		ptx, err := pool.BeginTenantTx(tctx, settingsToolProbeTenant)
		if err != nil {
			return
		}
		_, _ = ptx.Exec(tctx, `DELETE FROM audit_events WHERE tenant_id = $1`, settingsToolProbeTenant)
		_, _ = ptx.Exec(tctx, `DELETE FROM tenant_settings WHERE tenant_id = $1`, settingsToolProbeTenant)
		_ = ptx.Commit(tctx)
	})

	// The agent changes ONE stall window and nothing else — the partial-update shape this tool exists for.
	tctx := tenant.WithID(ctx, settingsToolProbeTenant)
	if _, err := toolUpdateSettings(tctx, pool, json.RawMessage(`{"stall_no_progress_window_seconds":600}`)); err != nil {
		t.Fatalf("toolUpdateSettings: %v", err)
	}

	// THE TRAIL MUST EXIST. Before this change there was no row at all, so the assertion is first that one
	// was written — not merely that it has the right shape.
	read, err := pool.BeginTenantTx(ctx, settingsToolProbeTenant)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer read.Rollback(ctx)

	var after string
	if err := read.QueryRow(ctx,
		`SELECT after::text FROM audit_events WHERE tenant_id = $1 AND action = 'settings.updated' ORDER BY occurred_at DESC LIMIT 1`,
		settingsToolProbeTenant).Scan(&after); err != nil {
		t.Fatalf("no settings.updated audit row was written by the tool (%v) — an agent-driven change would be invisible", err)
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(after), &fields); err != nil {
		t.Fatalf("the audit snapshot is not JSON: %v", err)
	}
	if got, ok := fields["stall_no_progress_window_seconds"]; !ok {
		t.Fatalf("the snapshot does not record stall_no_progress_window_seconds, so a change to it would "+
			"still leave no trace: %s", after)
	} else if got != float64(600) {
		t.Errorf("the snapshot records stall_no_progress_window_seconds as %v, want 600", got)
	}
	// AND THE NULLABLE ONES ARE PRESENT AS NULL. A key that is absent reads as "this build does not record
	// that field"; a null reads as "recorded, and blank". Only the second answers the operator's question.
	for _, key := range []string{"stall_no_file_diff_window_seconds", "stall_nudge_reply_window_seconds", "stall_nudge_cooldown_seconds"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the snapshot omits %q entirely — a blank window and an unrecorded one would look the same", key)
		}
	}
}
