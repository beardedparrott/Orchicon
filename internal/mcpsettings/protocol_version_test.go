package mcpsettings

// protocol_version_test.go — the operator's MCP protocol pin.
//
// The client handles a legacy server automatically now (internal/mcpclient/mcp_legacy_protocol.go), so this
// field is an OPTIMISATION and an escape hatch rather than the fix. What it must not be is a field that
// silently swallows a typo: a pin that cannot work should be rejected at write time, when the operator is
// looking at the form, rather than surfacing later as a connect timeout.

import "testing"

// "auto" and "" are the SAME stored value, so the read path never compares against two spellings.
func TestNormalizeProtocolVersionFoldsAuto(t *testing.T) {
	for _, in := range []string{"", "   ", "auto", "AUTO", " Auto "} {
		if got := normalizeProtocolVersion(in); got != "" {
			t.Errorf("normalizeProtocolVersion(%q) = %q, want \"\" (negotiate)", in, got)
		}
	}
	if got := normalizeProtocolVersion("  2025-11-25 "); got != "2025-11-25" {
		t.Errorf("normalizeProtocolVersion did not trim a real pin: %q", got)
	}
}

// A version-shaped value is accepted, including one this build has never heard of — new MCP versions appear
// faster than this platform releases, and rejecting them would make the field useless when it is needed.
func TestValidateProtocolVersionAcceptsAKnownShapeAndUnknownVersions(t *testing.T) {
	for _, ok := range []string{"", "2025-11-25", "2027-01-15", "2026-07-28", "1.0"} {
		if err := validateProtocolVersion(ok); err != nil {
			t.Errorf("validateProtocolVersion(%q) = %v, want nil", ok, err)
		}
	}
}

// ...but a value that cannot be a version is refused AT WRITE TIME.
func TestValidateProtocolVersionRefusesNonsense(t *testing.T) {
	for _, bad := range []string{"latest", "2025/11/25", "2025_11_25", "v2025-11-25", "twenty-five"} {
		if err := validateProtocolVersion(normalizeProtocolVersion(bad)); err == nil {
			t.Errorf("validateProtocolVersion(%q) = nil, want a rejection naming the expected shape", bad)
		}
	}
	if err := validateProtocolVersion("2025-11-25-and-a-very-long-suffix-beyond-any-version"); err == nil {
		t.Error("an over-long version was accepted")
	}
	// A trailing space is TRIMMED, not rejected: an operator pasting a version should not be fought by
	// whitespace, and the stored value must come out clean.
	if got := normalizeProtocolVersion("2025-11-25 "); got != "2025-11-25" {
		t.Errorf("a pinned version kept its trailing space: %q", got)
	}
}
