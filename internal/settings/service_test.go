package settings

import (
	"strings"
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

// --- validateModelRef (ADR-0003) ---

// settingsTestCLI mirrors the server's merged validation catalog: the builtin
// provider profiles, a tenant-custom provider (local-models), and a
// CLI-discovered provider id (deepseek) — the way the aigateway CLI registry
// composes over the static catalog.
func settingsTestCLI() adapter.ProviderRegistry {
	c := adapter.NewBuiltinProviderCatalog()
	c.AddAdapterKind(adapter.DefaultAdapterKind, "local-models", "deepseek")
	return c
}

// TestValidateModelRefThreeSegment pins AC1: 3-segment adapter/provider/model
// refs validate via the shared parser + CLI-aware registry at the Service seam
// (the gatekeeper of DefaultAskOrchiconModel). Empty = unset = valid.
func TestValidateModelRefThreeSegment(t *testing.T) {
	s := &Service{validationRegistry: settingsTestCLI()}
	for _, ref := range []string{"", "   ", "claude/anthropic/claude-sonnet-5", "opencode/opencode-go/deepseek-v4-flash", "orchicon/local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL", "orchicon/commandcode/deepseek/deepseek-v4-flash"} {
		if err := s.validateModelRef(ref); err != nil {
			t.Errorf("validateModelRef(%q) error = %v, want nil", ref, err)
		}
	}
}

// TestValidateModelRefLegacyTwoSegment pins AC2: legacy 2-segment provider/model
// refs still load/resolve via IsKnownProvider against the MERGED registry
// (built-in and tenant-custom first segments). A 2-seg first segment that is a
// KNOWN ADAPTER KIND is rejected as malformed.
func TestValidateModelRefLegacyTwoSegment(t *testing.T) {
	s := &Service{validationRegistry: settingsTestCLI()}
	for _, ref := range []string{"opencode-go/deepseek-v4-flash", "local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL"} {
		if err := s.validateModelRef(ref); err != nil {
			t.Errorf("validateModelRef(legacy 2-seg %q) error = %v, want nil", ref, err)
		}
	}
	err := s.validateModelRef("claude/anthropic")
	if err == nil {
		t.Fatal("validateModelRef(claude/anthropic) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "adapter kind") {
		t.Errorf("error %q does not explain the adapter-kind confusion", err.Error())
	}
}

// TestValidateModelRefSlashedModel pins AC3: slashed model ids stay a SINGLE
// model via the shared left-greedy parser — the Service seam reuses
// adapter.ParseModelRef (no forked splitter), so the slashed remainder is
// preserved intact.
func TestValidateModelRefSlashedModel(t *testing.T) {
	s := &Service{validationRegistry: settingsTestCLI()}
	ref := "orchicon/commandcode/deepseek/deepseek-v4-flash"
	if err := s.validateModelRef(ref); err != nil {
		t.Fatalf("validateModelRef(%q) error = %v, want nil", ref, err)
	}
	parsed, err := adapter.ParseModelRef(ref, settingsTestCLI())
	if err != nil {
		t.Fatalf("ParseModelRef(%q) error = %v", ref, err)
	}
	if parsed.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("ParseModelRef(%q).Model = %q, want deepseek/deepseek-v4-flash", ref, parsed.Model)
	}
}

// TestValidateModelRefUnknownAdapter pins AC4: an unknown adapter segment is
// rejected with a clear actionable error (register an adapter / use a known
// adapter/provider/model).
func TestValidateModelRefUnknownAdapter(t *testing.T) {
	s := &Service{validationRegistry: settingsTestCLI()}
	err := s.validateModelRef("foo/anthropic/claude-sonnet-5")
	if err == nil {
		t.Fatal("validateModelRef(unknown adapter) = nil error, want rejection")
	}
	for _, want := range []string{"foo", "register an adapter", "adapter/provider/model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestValidateModelRefUnknownProviderTwoSeg pins AC4's tenant-custom-first rule:
// an unknown 2-seg first segment errors and points at Settings → Adapters.
func TestValidateModelRefUnknownProviderTwoSeg(t *testing.T) {
	s := &Service{validationRegistry: settingsTestCLI()}
	err := s.validateModelRef("mystery-provider/claude-sonnet-5")
	if err == nil {
		t.Fatal("validateModelRef(unknown 2-seg provider) = nil error, want rejection")
	}
	for _, want := range []string{"mystery-provider", "Settings → Adapters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestValidateModelRefNilRegistryFallsBack pins the registry() nil fallback: a
// Service with no injected registry validates against the static builtin catalog
// (3-seg claude kind is built-in, so it still validates).
func TestValidateModelRefNilRegistryFallsBack(t *testing.T) {
	s := &Service{}
	if s.registry() == nil {
		t.Fatal("registry() returned nil for nil validationRegistry")
	}
	if err := s.validateModelRef("claude/anthropic/claude-sonnet-5"); err != nil {
		t.Errorf("validateModelRef(builtin claude kind) with nil registry = %v, want nil", err)
	}
}
