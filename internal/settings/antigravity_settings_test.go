package settings

// antigravity_settings_test.go — the Antigravity settings must survive the RPC mapping and be
// validated where the operator writes them.
//
// WHY THE MAPPING IS TESTED SEPARATELY FROM THE TOOL. The RPC is the path the web app, the TUI and
// the desktop app all use; the MCP tool is a second, independent write path. A field added to one
// and forgotten in the other is exactly the drift this file prevents (the row struct is positional,
// so a missing field would compile and silently blank the setting).
//
// The ToS notice is READ-ONLY: it is filled from the exported constant on every read, so a client
// that sends its own text cannot change what other clients display.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/db"
)

func TestAntigravitySettingsRoundTripThroughProto(t *testing.T) {
	row := db.TenantSettingsRow{
		AntigravityAuthMethod:     authmethod.MethodGeminiAPIKey,
		AntigravityShareTelemetry: true,
	}
	ts := settingsRowToProto(&row)
	if ts.GetAntigravityAuthMethod() != authmethod.MethodGeminiAPIKey {
		t.Errorf("proto antigravity_auth_method = %q", ts.GetAntigravityAuthMethod())
	}
	if !ts.GetAntigravityShareTelemetry() {
		t.Error("proto antigravity_share_telemetry = false, want true")
	}
	if ts.GetAntigravityTosNotice() != authmethod.ToSNotice {
		t.Errorf("proto antigravity_tos_notice does not come from the exported constant")
	}

	back := settingsProtoToRow(ts)
	if back.AntigravityAuthMethod != row.AntigravityAuthMethod {
		t.Errorf("row antigravity_auth_method = %q, want %q", back.AntigravityAuthMethod, row.AntigravityAuthMethod)
	}
	if back.AntigravityShareTelemetry != row.AntigravityShareTelemetry {
		t.Errorf("row antigravity_share_telemetry = %v, want %v", back.AntigravityShareTelemetry, row.AntigravityShareTelemetry)
	}
}

// An unset column must READ as the default rather than as an empty select, and an empty value on
// WRITE must mean "leave unchanged" (the DB layer's CASE guard), so Validate accepts "".
func TestAntigravitySettingsDefaultsAndValidation(t *testing.T) {
	ts := settingsRowToProto(&db.TenantSettingsRow{})
	if ts.GetAntigravityAuthMethod() != authmethod.MethodOAuthPersonal {
		t.Errorf("empty column reads as %q, want the oauth-personal default", ts.GetAntigravityAuthMethod())
	}
	if ts.GetAntigravityShareTelemetry() {
		t.Error("telemetry must read as off for a never-configured tenant")
	}
	if err := validateAntigravitySettings(&apiv1.TenantSettings{AntigravityAuthMethod: ""}); err != nil {
		t.Errorf("empty method rejected on write: %v", err)
	}
	if err := validateAntigravitySettings(&apiv1.TenantSettings{AntigravityAuthMethod: "oauth-personal"}); err != nil {
		t.Errorf("valid method rejected: %v", err)
	}
	if err := validateAntigravitySettings(&apiv1.TenantSettings{AntigravityAuthMethod: "oauth"}); err == nil {
		t.Error("invalid method accepted; it must be InvalidArgument")
	}
	if err := validateAntigravitySettings(nil); err != nil {
		t.Errorf("nil settings rejected: %v", err)
	}
}
