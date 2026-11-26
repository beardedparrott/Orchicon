package askorchicon

// tool_settings_antigravity_test.go — the Antigravity auth method and telemetry preference must
// be settable, readable, validated and DEFAULTED through the agent's own settings surface.
//
// WHY: the method decides which Google credential `agy acp` authenticates with, and the personal
// one is a per-operator acceptance of Google's Antigravity terms. A default that is not what a
// fresh tenant gets, or an invalid value that stores anyway, is a silent policy change for every
// tenant. Invalid values are rejected by BOTH layers (the tool/Go validation and the column
// CHECK constraint).
//
// Runs against a SYNTHETIC tenant and deletes what it creates.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon_test?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestSettingsToolAntigravity -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

const antigravitySettingsProbeTenant = "tnt_settings_tool_antigravity_probe"

func TestSettingsToolAntigravity(t *testing.T) {
	pool := chatDBTestPool(t)
	ctx := context.Background()

	seed, err := pool.BeginTenantTx(ctx, antigravitySettingsProbeTenant)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if _, err := db.UpdateTenantSettings(ctx, seed.Tx, antigravitySettingsProbeTenant, db.TenantSettingsRow{}); err != nil {
		_ = seed.Rollback(ctx)
		t.Fatalf("seed settings: %v", err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	t.Cleanup(func() {
		tctx := context.Background()
		ptx, err := pool.BeginTenantTx(tctx, antigravitySettingsProbeTenant)
		if err != nil {
			return
		}
		_, _ = ptx.Exec(tctx, `DELETE FROM audit_events WHERE tenant_id = $1`, antigravitySettingsProbeTenant)
		_, _ = ptx.Exec(tctx, `DELETE FROM tenant_settings WHERE tenant_id = $1`, antigravitySettingsProbeTenant)
		_ = ptx.Commit(tctx)
	})

	tctx := tenant.WithID(ctx, antigravitySettingsProbeTenant)

	readSettings := func() map[string]any {
		t.Helper()
		raw, err := toolGetSettings(tctx, pool, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("toolGetSettings: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal get_settings: %v", err)
		}
		return out
	}

	// AC1: a never-configured tenant reads the shipped defaults, under the literal keys the
	// acceptance criteria name.
	settings := readSettings()
	if got := settings["antigravity_auth_method"]; got != authmethod.MethodOAuthPersonal {
		t.Fatalf("fresh antigravity_auth_method = %v, want oauth-personal", got)
	}
	if got := settings["antigravity_share_telemetry"]; got != false {
		t.Fatalf("fresh antigravity_share_telemetry = %v, want false", got)
	}
	if got := settings["antigravity_tos_notice"]; got != authmethod.ToSNotice {
		t.Fatalf("antigravity_tos_notice = %v, want the exported ToSNotice constant", got)
	}

	// Both fields are writable, and the write is visible on the next read.
	if _, err := toolUpdateSettings(tctx, pool, json.RawMessage(`{"antigravity_auth_method":"gemini-api-key","antigravity_share_telemetry":true}`)); err != nil {
		t.Fatalf("toolUpdateSettings(gemini-api-key, telemetry=true): %v", err)
	}
	settings = readSettings()
	if got := settings["antigravity_auth_method"]; got != authmethod.MethodGeminiAPIKey {
		t.Fatalf("antigravity_auth_method = %v, want gemini-api-key", got)
	}
	if got := settings["antigravity_share_telemetry"]; got != true {
		t.Fatalf("antigravity_share_telemetry = %v, want true", got)
	}

	// A PARTIAL update must not disturb the other field: the MCP tool omits telemetry here, and
	// the column is written unconditionally, so a missing carry-over would silently reset it.
	if _, err := toolUpdateSettings(tctx, pool, json.RawMessage(`{"antigravity_auth_method":"oauth-business"}`)); err != nil {
		t.Fatalf("toolUpdateSettings(oauth-business): %v", err)
	}
	settings = readSettings()
	if got := settings["antigravity_auth_method"]; got != authmethod.MethodOAuthBusiness {
		t.Fatalf("antigravity_auth_method = %v, want oauth-business", got)
	}
	if got := settings["antigravity_share_telemetry"]; got != true {
		t.Fatalf("partial update reset antigravity_share_telemetry to %v; it must stay true", got)
	}

	// AC1: an invalid method is rejected and changes nothing.
	_, err = toolUpdateSettings(tctx, pool, json.RawMessage(`{"antigravity_auth_method":"personal-oauth"}`))
	if err == nil {
		t.Fatal("toolUpdateSettings accepted antigravity_auth_method \"personal-oauth\"; invalid methods must be rejected")
	}
	if !strings.Contains(err.Error(), "not valid") || !strings.Contains(err.Error(), authmethod.MethodGeminiAPIKey) {
		t.Errorf("rejection error %q should name the allowed values", err)
	}
	settings = readSettings()
	if got := settings["antigravity_auth_method"]; got != authmethod.MethodOAuthBusiness {
		t.Fatalf("after a rejected update antigravity_auth_method = %v, want the unchanged oauth-business", got)
	}
}
