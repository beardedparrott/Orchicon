package settings

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
)

// TestSettingsProtoMaxConcurrentRunsRoundTrip verifies the optional
// max_concurrent_runs field (proto) survives the row conversion with its
// "explicitly set vs. absent" distinction intact: a nil proto pointer must
// NOT clobber the persisted value (MaxConcurrentRunsSet=false), while a
// present pointer — including 0, which means "clear the cap" — must.
func TestSettingsProtoMaxConcurrentRunsRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		protoValue *int32
		wantVal    int
		wantSet    bool
	}{
		{"absent", nil, 0, false},
		{"explicit zero clears cap", int32Ptr(0), 0, true},
		{"positive cap", int32Ptr(4), 4, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := settingsProtoToRow(&apiv1.TenantSettings{MaxConcurrentRuns: c.protoValue})
			if row.MaxConcurrentRuns != c.wantVal {
				t.Errorf("MaxConcurrentRuns = %d, want %d", row.MaxConcurrentRuns, c.wantVal)
			}
			if row.MaxConcurrentRunsSet != c.wantSet {
				t.Errorf("MaxConcurrentRunsSet = %v, want %v", row.MaxConcurrentRunsSet, c.wantSet)
			}
		})
	}
}

// TestSettingsRowToProtoMaxConcurrentRuns verifies the row→proto direction
// always fills the pointer with the persisted value (0 means "no cap").
func TestSettingsRowToProtoMaxConcurrentRuns(t *testing.T) {
	proto := settingsRowToProto(&db.TenantSettingsRow{MaxConcurrentRuns: 3})
	if proto.MaxConcurrentRuns == nil {
		t.Fatal("MaxConcurrentRuns nil, want non-nil pointer")
	}
	if *proto.MaxConcurrentRuns != 3 {
		t.Fatalf("MaxConcurrentRuns = %d, want 3", *proto.MaxConcurrentRuns)
	}
	zero := settingsRowToProto(&db.TenantSettingsRow{MaxConcurrentRuns: 0})
	if zero.MaxConcurrentRuns == nil || *zero.MaxConcurrentRuns != 0 {
		t.Fatalf("MaxConcurrentRuns for 0 = %v, want pointer to 0", zero.MaxConcurrentRuns)
	}
}

func int32Ptr(v int32) *int32 { return &v }

// TestValidateSessionTTLs exercises the session TTL validation constants
// and boundary conditions enforced by validateSessionTTLs.
func TestValidateSessionTTLs(t *testing.T) {
	cases := []struct {
		name      string
		accessTTL int64
		refreshTTL int64
		wantErr   bool
	}{
		{"both zero — leave unchanged", 0, 0, false},
		{"access zero, refresh set", 0, 86400, false},
		{"access set, refresh zero", 900, 0, false},
		{"both set — valid", 900, 86400, false},
		{"access below minimum (29)", 29, 0, true},
		{"access at minimum (30)", 30, 0, false},
		{"access above maximum (86401)", 86401, 0, true},
		{"access at maximum (86400)", 86400, 0, false},
		{"refresh below minimum (299)", 900, 299, true},
		{"refresh at minimum (300)", 900, 300, false},
		{"refresh at minimum above access — allowed", 300, 300, false},
		{"refresh above access — accepted", 300, 900, false},
		{"refresh above maximum (31536001)", 900, 31536001, true},
		{"refresh at maximum (31536000)", 900, 31536000, false},
		{"refresh equal to access — rejected", 900, 900, true},
		{"refresh less than access — rejected", 900, 800, true},
		{"refresh just above access — accepted", 900, 901, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSessionTTLs(c.accessTTL, c.refreshTTL)
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Errorf("validateSessionTTLs(%d, %d) error = %v, wantErr = %v",
					c.accessTTL, c.refreshTTL, err, c.wantErr)
			}
		})
	}
}

// TestValidateModelRef_CLIRegistry pins that tenant-default model refs are
// validated against the CLI-aware registry injected via SetValidationRegistry
// (builtin catalog ∪ CLI-discovered provider ids), never the bare builtin
// catalog. This is the settings-service half of the "CLI-aware validation"
// acceptance: a CLI-namespace provider (e.g. "deepseek") that the picker
// happily offers must not be rejected at save-time. Both tenant defaults
// (DefaultAskOrchiconModel, DefaultWorkerModel) flow through the same
// validateModelRef, so this exercises the shared contract.
//
// The observable CLI-aware distinction lives on the LEGACY 2-SEGMENT form
// (provider/model), where the head is validated as a provider: the builtin
// catalog does not know "deepseek", so a 2-seg "deepseek/..." ref is
// rejected; the CLI-aware registry accepts it. A fully-qualified 3-segment
// "opencode/deepseek/model" ref validates its ADAPTER segment only, so it
// passes under either registry (and confirms the left-greedy grammar keeps
// a slashed model id like "deepseek/deepseek-v4-flash" as one model segment).
func TestValidateModelRef_CLIRegistry(t *testing.T) {
	cliRegistry := adapter.NewBuiltinProviderCatalog().Clone()
	cliRegistry.AddAdapterKind(adapter.DefaultAdapterKind, "deepseek")

	builtin := New(nil, nil, "")          // no registry injected → static catalog
	cliAware := New(nil, nil, "")
	cliAware.SetValidationRegistry(cliRegistry)

	cases := []struct {
		name    string
		svc     *Service
		ref     string
		wantErr bool
	}{
		// Empty = unset → valid under every registry.
		{"empty builtin", builtin, "", false},
		{"empty cli-aware", cliAware, "", false},

		// 3-seg with a known adapter passes under both (adapter-only check).
		{"3-seg opencode/openai builtin", builtin, "opencode/openai/gpt-4o", false},
		{"3-seg opencode/deepseek slashed cli-aware", cliAware, "opencode/deepseek/deepseek-v4-flash", false},

		// Legacy 2-seg CLI-namespace provider: rejected by builtin, accepted
		// by the CLI-aware registry.
		{"2-seg deepseek builtin rejected", builtin, "deepseek/deepseek-v4-flash", true},
		{"2-seg deepseek cli-aware accepted", cliAware, "deepseek/deepseek-v4-flash", false},

		// Unknown provider (2-seg) always rejected.
		{"2-seg mystery cli-aware rejected", cliAware, "mystery-provider/some-model", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.svc.validateModelRef(c.ref)
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Errorf("validateModelRef(%q) error = %v, wantErr = %v", c.ref, err, c.wantErr)
			}
		})
	}
}
