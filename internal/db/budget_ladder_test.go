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

// The typed compaction/memory columns (D4) serialize into the budget JSON
// as context_compaction / memory objects — the keys the native session's
// policyFromSettings and a worker's budget_overrides parse — so the
// Settings API and the dispatcher (mergeBudgets) both carry the policy.
func TestBudgetJSONEmitsCompactionAndMemoryPolicy(t *testing.T) {
	row := &TenantSettingsRow{
		ContextCompactionEnabled:      true,
		ContextCompactionPressureFrac: 0.85,
		ContextRecentTurns:            4,
		MemoryEnabled:                 false,
		MemoryDigestEntries:           3,
	}
	out := string(row.BudgetJSON())
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("BudgetJSON not valid JSON: %v\n%s", err, out)
	}
	cc, ok := m["context_compaction"].(map[string]any)
	if !ok {
		t.Fatalf("BudgetJSON missing context_compaction object: %s", out)
	}
	if cc["enabled"] != true || cc["pressure_frac"] != 0.85 || cc["recent_turns"] != float64(4) {
		t.Fatalf("context_compaction = %v, want {enabled:true pressure_frac:0.85 recent_turns:4}", cc)
	}
	mem, ok := m["memory"].(map[string]any)
	if !ok {
		t.Fatalf("BudgetJSON missing memory object: %s", out)
	}
	if mem["enabled"] != false || mem["digest_entries"] != float64(3) {
		t.Fatalf("memory = %v, want {enabled:false digest_entries:3}", mem)
	}
}

func TestApplyBudgetJSONIngestsCompactionAndMemoryPolicy(t *testing.T) {
	row := &TenantSettingsRow{}
	if err := row.ApplyBudgetJSON([]byte(`{
		"context_compaction": {"enabled": false, "pressure_frac": 0.9, "recent_turns": 3},
		"memory": {"enabled": true, "digest_entries": 7}
	}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if row.ContextCompactionEnabled != false || row.ContextCompactionPressureFrac != 0.9 || row.ContextRecentTurns != 3 {
		t.Fatalf("context_compaction not ingested: enabled=%v frac=%v turns=%v",
			row.ContextCompactionEnabled, row.ContextCompactionPressureFrac, row.ContextRecentTurns)
	}
	if row.MemoryEnabled != true || row.MemoryDigestEntries != 7 {
		t.Fatalf("memory not ingested: enabled=%v entries=%v", row.MemoryEnabled, row.MemoryDigestEntries)
	}
}

// A partial update that omits context_compaction/memory must preserve the
// current typed policy (partial update semantics, matching the gate fields).
func TestApplyBudgetJSONPolicyPartialKeepsCurrent(t *testing.T) {
	row := &TenantSettingsRow{
		ContextCompactionEnabled:      true,
		ContextCompactionPressureFrac: 0.8,
		ContextRecentTurns:            6,
		MemoryEnabled:                 true,
		MemoryDigestEntries:           5,
	}
	if err := row.ApplyBudgetJSON([]byte(`{"tokens":100}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if !row.ContextCompactionEnabled || row.ContextCompactionPressureFrac != 0.8 || row.ContextRecentTurns != 6 ||
		!row.MemoryEnabled || row.MemoryDigestEntries != 5 {
		t.Fatalf("absent policy keys must preserve current values, got %+v", row)
	}
	// An explicitly disabled feature still counts as "set": a client that
	// sends memory.enabled=false must land (never treated as absent).
	if err := row.ApplyBudgetJSON([]byte(`{"memory":{"enabled":false}}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if row.MemoryEnabled != false || row.MemoryDigestEntries != 5 {
		t.Fatalf("explicit memory.enabled=false not ingested: enabled=%v entries=%v", row.MemoryEnabled, row.MemoryDigestEntries)
	}
}

// A zero policy struct is not "set" — the DB write path uses this to
// preserve the stored policy on a partial settings update.
func TestPolicyIsSet(t *testing.T) {
	if (&TenantSettingsRow{}).policyIsSet() {
		t.Fatalf("all-zero policy must not be set")
	}
	if !(&TenantSettingsRow{MemoryEnabled: true}).policyIsSet() {
		t.Fatalf("memory.enabled=true must be set")
	}
	if !(&TenantSettingsRow{ContextCompactionEnabled: false, ContextCompactionPressureFrac: 0.8, ContextRecentTurns: 6}).policyIsSet() {
		t.Fatalf("explicit disable with valid numerics must be set")
	}
}
