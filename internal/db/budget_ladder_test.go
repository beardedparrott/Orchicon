package db

import (
	"encoding/json"
	"testing"
)

// TenantSettingsRow's BudgetJSON/ApplyBudgetJSON round-trip the per-tier
// compaction toggles (`compact_tiers`) so the Settings API and the dispatcher
// (mergeBudgets → parseBudgetSpec) both see the operator's compaction policy.
func TestBudgetJSONEmitsCompactTiers(t *testing.T) {
	row := &TenantSettingsRow{}
	row.Budget.CompactWarnTier = true
	row.Budget.CompactEscalTier = false
	row.Budget.CompactFinalTier = true

	out := string(row.BudgetJSON())
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("BudgetJSON not valid JSON: %v\n%s", err, out)
	}
	ct, ok := m["compact_tiers"].([]any)
	if !ok {
		t.Fatalf("BudgetJSON missing compact_tiers: %s", out)
	}
	if len(ct) != 3 || ct[0] != true || ct[1] != false || ct[2] != true {
		t.Fatalf("compact_tiers = %v, want [true false true]", ct)
	}
}

func TestApplyBudgetJSONIngestsCompactTiers(t *testing.T) {
	row := &TenantSettingsRow{}
	if err := row.ApplyBudgetJSON([]byte(`{"tokens":100,"compact_tiers":[true,true,false]}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if !row.Budget.CompactWarnTier || !row.Budget.CompactEscalTier || row.Budget.CompactFinalTier {
		t.Fatalf("compact tiers not ingested: warn=%v escalate=%v final=%v",
			row.Budget.CompactWarnTier, row.Budget.CompactEscalTier, row.Budget.CompactFinalTier)
	}
}

func TestApplyBudgetJSONCompactTiersPartialKeepsCurrent(t *testing.T) {
	row := &TenantSettingsRow{}
	row.Budget.CompactWarnTier = true
	row.Budget.CompactEscalTier = true
	// An update WITHOUT compact_tiers must leave the current policy intact
	// (partial update semantics, matching the gate fields).
	if err := row.ApplyBudgetJSON([]byte(`{"tokens":100}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if !row.Budget.CompactWarnTier || !row.Budget.CompactEscalTier {
		t.Fatalf("absent compact_tiers must preserve current values, got warn=%v escalate=%v",
			row.Budget.CompactWarnTier, row.Budget.CompactEscalTier)
	}
	// A non-3-element array is ignored too.
	if err := row.ApplyBudgetJSON([]byte(`{"compact_tiers":[true]}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if !row.Budget.CompactWarnTier {
		t.Fatalf("a partial compact_tiers array must be ignored, warn=%v", row.Budget.CompactWarnTier)
	}
}
