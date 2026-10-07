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
		name       string
		accessTTL  int64
		refreshTTL int64
		wantErr    bool
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

// TEST-MERGE-OURS-BEGIN
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

	builtin := New(nil, nil, "") // no registry injected → static catalog
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

// TEST-MERGE-OURS-END

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

// THE OPERATOR'S SETTING MUST SURVIVE A SAVE THAT DOES NOT MENTION IT.
//
// "I have it set to 600seconds" — and still a two-minute stall error. The cause: the Settings page's
// Backup panel has its OWN Save button that sends only {backup_schedule, backup_retention_days,
// backup_directory}. db.UpdateTenantSettings writes the stall columns FROM THE ROW IT IS HANDED, so the
// absent fields arrived as nil, landed as NULL, and the schema reads NULL as "use the built-in default"
// — reverting 600s to 120s. Saving a backup schedule quietly reconfigured the stall detector.
func TestStallSettingsSurviveASaveThatDoesNotMentionThem(t *testing.T) {
	cur := db.TenantSettingsRow{
		StallNoProgressWindowSeconds: i64p(600),
		StallNoFileDiffWindowSeconds: i64p(900),
		StallTextLoopWindowSeconds:   i64p(150),
		StallRepetitionCount:         i32p(7),
		StallRepetitionWindowSeconds: i64p(45),
		StallNudgeMax:                i32p(3),
		StallNudgeReplyWindowSeconds: i64p(75),
		StallNudgeCooldownSeconds:    i64p(30),
		StallToolHangSeconds:         i64p(240),
	}
	// The Backup panel's payload: it names no stall field at all.
	inRow := db.TenantSettingsRow{BackupSchedule: "0 3 * * *"}
	mergeStallSettingsFromCurrent(&inRow, &apiv1.TenantSettings{}, cur)

	if inRow.StallNoProgressWindowSeconds == nil || *inRow.StallNoProgressWindowSeconds != 600 {
		t.Fatalf("the 600s no-progress window did not survive a backup save: %v", inRow.StallNoProgressWindowSeconds)
	}
	if inRow.StallNoFileDiffWindowSeconds == nil || *inRow.StallNoFileDiffWindowSeconds != 900 {
		t.Errorf("no-diff window = %v, want 900 preserved", inRow.StallNoFileDiffWindowSeconds)
	}
	if inRow.StallRepetitionCount == nil || *inRow.StallRepetitionCount != 7 {
		t.Errorf("repetition count = %v, want 7 preserved", inRow.StallRepetitionCount)
	}
	if inRow.StallToolHangSeconds == nil || *inRow.StallToolHangSeconds != 240 {
		t.Errorf("tool-hang = %v, want 240 preserved", inRow.StallToolHangSeconds)
	}
	if inRow.BackupSchedule != "0 3 * * *" {
		t.Errorf("the field the client DID send was lost: %q", inRow.BackupSchedule)
	}

	// A field the client DOES name is applied — including an explicit 0, which means DISABLED here and
	// must not be confused with "absent, so keep what is stored".
	//
	// The row is built the way production builds it (settingsProtoToRow carries what the client sent,
	// and the merge only fills the gaps), so this also pins that a present 0 survives the round trip.
	want := &apiv1.TenantSettings{
		StallNoProgressWindowSeconds: i64p(0),
		StallNudgeMax:                i32p(9),
	}
	explicit := settingsProtoToRow(want)
	mergeStallSettingsFromCurrent(&explicit, want, cur)
	if explicit.StallNoProgressWindowSeconds == nil || *explicit.StallNoProgressWindowSeconds != 0 {
		t.Fatalf("an explicit 0 (DISABLED) was rewritten to %v — the one value that must be applied verbatim", explicit.StallNoProgressWindowSeconds)
	}
	if got := explicit.StallNudgeMax; got == nil || *got != 9 {
		t.Errorf("nudge max = %v, want the sent 9", got)
	}
	// ...while the fields it did NOT name still come from the stored row.
	if explicit.StallTextLoopWindowSeconds == nil || *explicit.StallTextLoopWindowSeconds != 150 {
		t.Errorf("text-loop = %v, want 150 preserved beside the two fields that were sent", explicit.StallTextLoopWindowSeconds)
	}

	// A request with NO settings object asserts nothing, so it blanks nothing either.
	none := db.TenantSettingsRow{}
	mergeStallSettingsFromCurrent(&none, nil, cur)
	if none.StallNoProgressWindowSeconds == nil || *none.StallNoProgressWindowSeconds != 600 {
		t.Errorf("a nil settings object blanked the stall windows: %v", none.StallNoProgressWindowSeconds)
	}
}

func i64p(v int64) *int64 { return &v }
func i32p(v int32) *int32 { return &v }
