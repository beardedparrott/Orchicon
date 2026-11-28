package db

import (
	"encoding/json"
	"testing"
)

// The reported bug: set "Keep last N tool results" to 20, save, and it shows blank immediately. The write
// was accepted and then THROWN AWAY on the read, because BudgetJSON() rebuilds the document from an
// allowlist of known fields — so a key with no field never comes back out.
func TestBudgetJSONRoundTripsContextEditing(t *testing.T) {
	var row TenantSettingsRow
	if err := row.ApplyBudgetJSON([]byte(`{"context_editing":{"keep_tool_calls":20}}`)); err != nil {
		t.Fatalf("ApplyBudgetJSON: %v", err)
	}
	if row.Budget.ContextEditingKeepToolCalls == nil || *row.Budget.ContextEditingKeepToolCalls != 20 {
		t.Fatalf("keep_tool_calls did not survive the parse: %v", row.Budget.ContextEditingKeepToolCalls)
	}

	// The half that was missing: it must come back OUT.
	var out map[string]any
	if err := json.Unmarshal(row.BudgetJSON(), &out); err != nil {
		t.Fatalf("BudgetJSON produced invalid JSON: %v", err)
	}
	ce, ok := out["context_editing"].(map[string]any)
	if !ok {
		t.Fatalf("context_editing was DROPPED from the document — a save would appear to do nothing: %s", row.BudgetJSON())
	}
	if got, _ := ce["keep_tool_calls"].(float64); got != 20 {
		t.Errorf("keep_tool_calls = %v, want 20", ce["keep_tool_calls"])
	}
}

// An unset knob must not be invented into the document.
func TestBudgetJSONOmitsUnsetContextEditing(t *testing.T) {
	var row TenantSettingsRow
	if err := row.ApplyBudgetJSON([]byte(`{"tokens":100}`)); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(row.BudgetJSON(), &out)
	if _, ok := out["context_editing"]; ok {
		t.Errorf("context_editing must not be emitted when unset: %s", row.BudgetJSON())
	}
}

// 0 means off, and must not be written as a live setting.
func TestBudgetJSONOmitsZeroContextEditing(t *testing.T) {
	var row TenantSettingsRow
	if err := row.ApplyBudgetJSON([]byte(`{"context_editing":{"keep_tool_calls":0}}`)); err != nil {
		t.Fatal(err)
	}
	if row.Budget.ContextEditingKeepToolCalls != nil {
		t.Errorf("keep_tool_calls=0 must read as unset, got %v", *row.Budget.ContextEditingKeepToolCalls)
	}
}

// max_tool_output_bytes had the SAME allowlist gap: no ladder field, so BudgetJSON() dropped it and a
// saved cap would vanish on the next read.
func TestBudgetJSONRoundTripsMaxToolOutputBytes(t *testing.T) {
	var row TenantSettingsRow
	if err := row.ApplyBudgetJSON([]byte(`{"max_tool_output_bytes":32768}`)); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(row.BudgetJSON(), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := out["max_tool_output_bytes"].(float64); got != 32768 {
		t.Fatalf("max_tool_output_bytes = %v, want 32768 (it was dropped from the document)", out["max_tool_output_bytes"])
	}
}

// The end-to-end shape of the reported bug: save a document, read it back, and every knob the operator can
// set in the UI must still be there. This is the regression guard for "I set it and it shows blank".
func TestBudgetJSONSurvivesASecondSave(t *testing.T) {
	const first = `{"max_tool_output_bytes":32768,"context_editing":{"keep_tool_calls":20},"tokens":500000}`
	var row TenantSettingsRow
	if err := row.ApplyBudgetJSON([]byte(first)); err != nil {
		t.Fatal(err)
	}
	// The read-back document IS what the next save is built from.
	readBack := row.BudgetJSON()

	var row2 TenantSettingsRow
	if err := row2.ApplyBudgetJSON(readBack); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(row2.BudgetJSON(), &out); err != nil {
		t.Fatal(err)
	}
	if out["max_tool_output_bytes"] == nil {
		t.Error("max_tool_output_bytes lost across a second save")
	}
	if out["context_editing"] == nil {
		t.Error("context_editing lost across a second save")
	}
	if out["tokens"] == nil {
		t.Error("tokens lost across a second save")
	}
}
