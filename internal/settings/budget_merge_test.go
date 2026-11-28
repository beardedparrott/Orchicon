package settings

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// This is the bug that shipped twice: a settings save that appears to do NOTHING, because the value is
// accepted, stored without itself, and read back missing.
//
//	db.BudgetJSON() is an ALLOWLIST — it rebuilds default_budget_overrides from the ladder's fields — so any
//	request key the ladder cannot represent is dropped from the document written to the row. Seeding the
//	ladder from a ZERO value on every save therefore deleted every unmodelled key. Seeding from the CURRENT
//	ladder makes the overlay additive, which is the property these tests pin.

func iptr(v int) *int { return &v }

// TestBudgetSaveKeepsContextEditing is the reported scenario in miniature: the operator's knob must survive
// the merge and be present in the document the UI is re-seeded from.
func TestBudgetSaveKeepsContextEditing(t *testing.T) {
	cur := db.TenantSettingsRow{}
	cur.Budget.ContextEditingKeepToolCalls = iptr(20)

	inRow := db.TenantSettingsRow{}
	inRow.DefaultBudgetOverrides = []byte(`{"context_editing":{"keep_tool_calls":20}}`)
	if err := applyBudgetOverridesFromCurrent(&inRow, cur); err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(inRow.BudgetJSON(), &doc); err != nil {
		t.Fatalf("BudgetJSON: %v", err)
	}
	ce, ok := doc["context_editing"].(map[string]any)
	if !ok {
		t.Fatalf("context_editing missing from the document after a save — the field would render blank: %s", inRow.BudgetJSON())
	}
	if n, _ := ce["keep_tool_calls"].(float64); n != 20 {
		t.Errorf("keep_tool_calls = %v, want 20", ce["keep_tool_calls"])
	}
}

// TestBudgetSaveKeepsUnmentionedKeys is the general property: a save that does NOT mention a knob must not
// delete it. This is the regression that would have caught both shipped failures.
func TestBudgetSaveKeepsUnmentionedKeys(t *testing.T) {
	// The operator previously saved a cap and a keep count.
	cur := db.TenantSettingsRow{}
	cur.Budget.ContextEditingKeepToolCalls = iptr(20)
	capBytes := 32768.0
	cur.Budget.MaxToolOutputBytes = &capBytes

	// Now they save an unrelated field (e.g. toggling something else), with a document that names neither.
	inRow := db.TenantSettingsRow{}
	inRow.DefaultBudgetOverrides = []byte(`{}`)
	if err := applyBudgetOverridesFromCurrent(&inRow, cur); err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(inRow.BudgetJSON(), &doc); err != nil {
		t.Fatalf("BudgetJSON: %v", err)
	}
	if doc["context_editing"] == nil {
		t.Error("a save that did not mention context_editing DELETED it")
	}
	if doc["max_tool_output_bytes"] == nil {
		t.Error("a save that did not mention max_tool_output_bytes DELETED it")
	}
}

// TestBudgetSaveOverlaysRatherThanReplaces: a value the client DOES name must win.
func TestBudgetSaveOverlaysRatherThanReplaces(t *testing.T) {
	cur := db.TenantSettingsRow{}
	cur.Budget.ContextEditingKeepToolCalls = iptr(20)

	inRow := db.TenantSettingsRow{}
	inRow.DefaultBudgetOverrides = []byte(`{"context_editing":{"keep_tool_calls":5}}`)
	if err := applyBudgetOverridesFromCurrent(&inRow, cur); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if inRow.Budget.ContextEditingKeepToolCalls == nil || *inRow.Budget.ContextEditingKeepToolCalls != 5 {
		t.Fatalf("the client's value did not win: got %v", inRow.Budget.ContextEditingKeepToolCalls)
	}
}

// TestBudgetSavePreservesExistingGates: the same property for an ordinary gate, which is what makes this a
// general fix rather than a two-key patch.
func TestBudgetSavePreservesExistingGates(t *testing.T) {
	tokens := 500000.0
	cur := db.TenantSettingsRow{}
	cur.Budget.Tokens = &tokens

	inRow := db.TenantSettingsRow{}
	inRow.DefaultBudgetOverrides = []byte(`{"cost_usd":5}`)
	if err := applyBudgetOverridesFromCurrent(&inRow, cur); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(inRow.BudgetJSON(), &doc)
	if doc["tokens"] == nil {
		t.Error("an unrelated save deleted the token gate")
	}
	if n, _ := doc["cost_usd"].(float64); n != 5 {
		t.Errorf("the named gate was not applied: %v", doc["cost_usd"])
	}
}
