package control

// budget_fields_test.go — the budget blob expanded into individual fields.
//
// The important property is that the two directions are INVERSES: the form seeds
// its fields from the transport blob and composes the blob back on submit, so a
// save that changes nothing must produce the same document. A one-way conversion
// would silently drop gates whenever the operator pressed save.

import (
	"encoding/json"
	"testing"
)

// The form's fields and the blob agree in both directions for every key covered.
func TestBudgetFieldsBlobRoundTrip(t *testing.T) {
	blob := `{
	  "tokens": 500000, "cost_usd": 0.5, "wall_clock_seconds": 7200,
	  "tool_call_count": 100, "compact_max_turns": 12,
	  "compact_tiers": [false, true, true],
	  "context_compaction": {"enabled": true, "pressure_frac": 0.9, "recent_turns": 8},
	  "memory": {"enabled": true, "digest_entries": 5},
	  "warnings": {"fractions": {"tokens": [0.25, 0.5, 0.75], "cost_usd": [0.5, 0.75, 0.9]}}
	}`
	initials := budgetInitials(blob)

	// Every field the form edits is seeded.
	for _, k := range []string{
		"budget_tokens", "budget_cost_usd", "budget_wall_clock_seconds",
		"budget_tool_call_count", "budget_compact_max_turns",
		"warn_frac_tokens", "warn_frac_cost",
		"compact_tier_warn", "compact_tier_escalate", "compact_tier_final",
		"compact_enabled", "compact_pressure_frac", "compact_recent_turns",
		"memory_enabled", "memory_digest_entries",
	} {
		if _, ok := initials[k]; !ok {
			t.Errorf("field %q was not seeded from the blob", k)
		}
	}
	if initials["budget_tokens"] != "500000" {
		t.Errorf("budget_tokens = %q, want 500000", initials["budget_tokens"])
	}
	if initials["warn_frac_tokens"] != "0.25,0.5,0.75" {
		t.Errorf("warn_frac_tokens = %q, want the triple", initials["warn_frac_tokens"])
	}
	if initials["compact_tier_warn"] != "false" {
		t.Errorf("compact_tier_warn = %q, want false (the built-in policy never compacts at the first warning)", initials["compact_tier_warn"])
	}

	// Compose it back and compare the keys the form covers.
	out := buildBudgetJSON(initials)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("buildBudgetJSON produced invalid JSON: %v (%s)", err, out)
	}
	for _, k := range []string{"tokens", "cost_usd", "wall_clock_seconds", "tool_call_count",
		"compact_max_turns", "compact_tiers", "context_compaction", "memory", "warnings"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q was lost on the way back: %s", k, out)
		}
	}
	if got["tokens"].(float64) != 500000 {
		t.Errorf("tokens = %v, want 500000", got["tokens"])
	}
	cc := got["context_compaction"].(map[string]any)
	if cc["pressure_frac"].(float64) != 0.9 || cc["recent_turns"].(float64) != 8 {
		t.Errorf("context_compaction lost values: %v", cc)
	}
	mem := got["memory"].(map[string]any)
	if mem["digest_entries"].(float64) != 5 {
		t.Errorf("memory lost values: %v", mem)
	}
}

// An EMPTY field must be OMITTED, not written as zero: the server reads an absent
// gate as "built-in default" and an explicit 0 as "disable this gate", so blanking
// a field would turn gates OFF on an untouched save.
func TestBlankBudgetFieldsAreOmittedNotZeroed(t *testing.T) {
	out := buildBudgetJSON(map[string]string{})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, gate := range []string{"tokens", "cost_usd", "wall_clock_seconds", "tool_call_count", "compact_max_turns"} {
		if _, present := got[gate]; present {
			t.Errorf("blank field wrote gate %q=%v — an absent gate means \"built-in default\", 0 means \"disabled\"", gate, got[gate])
		}
	}
	if _, present := got["warnings"]; present {
		t.Errorf("a blank ladder wrote a warnings block: %v", got["warnings"])
	}
	// The tier toggles ARE always meaningful (NOT NULL DEFAULT columns), so they
	// are always written — and they carry the built-in policy when nothing set them.
	tiers, ok := got["compact_tiers"].([]any)
	if !ok || len(tiers) != 3 {
		t.Fatalf("compact_tiers = %v, want a 3-element array", got["compact_tiers"])
	}
}

// A partial ladder writes only the rows that were filled in.
func TestPartialLadderWritesOnlyFilledRows(t *testing.T) {
	out := buildBudgetJSON(map[string]string{"warn_frac_tokens": "0.5,0.6,0.7"})
	var got struct {
		Warnings struct {
			Fractions map[string][3]float64 `json:"fractions"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(got.Warnings.Fractions) != 1 {
		t.Fatalf("fractions = %v, want only the tokens row", got.Warnings.Fractions)
	}
	if got.Warnings.Fractions["tokens"] != [3]float64{0.5, 0.6, 0.7} {
		t.Errorf("tokens triple = %v", got.Warnings.Fractions["tokens"])
	}
}

// A malformed ladder row is refused at the field rather than half-written.
func TestLadderValidatorRejectsMalformedRows(t *testing.T) {
	for _, bad := range []string{"0.5", "0.5,0.6", "a,b,c", "0.5,0.6,0.7,0.8"} {
		if err := validFractionTriple(bad); err == nil {
			t.Errorf("validFractionTriple(%q) accepted a malformed row", bad)
		}
	}
	for _, ok := range []string{"", "0.25,0.5,0.75", " 0.1 , 0.2 , 0.3 "} {
		if err := validFractionTriple(ok); err != nil {
			t.Errorf("validFractionTriple(%q) rejected a valid row: %v", ok, err)
		}
	}
}
