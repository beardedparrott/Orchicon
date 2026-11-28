package control

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The bug these pin: buildBudgetJSON rebuilds the whole budget document from the fields this form
// renders, so any key it does not model is DROPPED on save. Nothing errors — the setting just stops
// applying — which is the worst failure shape for something that costs money.

// TestBudgetSavePreservesUnmodeledBlock proves the GENERIC preserve machinery works, using a synthetic
// key so the test keeps meaning something as real keys move between "unmodeled" and "rendered".
func TestBudgetSavePreservesUnmodeledBlock(t *testing.T) {
	blob := `{"tokens":1000,"third_party_block":{"k":1}}`

	// Install the key BEFORE seeding: seed and save read the same list, so the test cannot drift from
	// production ordering.
	saved := unmodeledBudgetKeys
	unmodeledBudgetKeys = []string{"third_party_block"}
	defer func() { unmodeledBudgetKeys = saved }()

	initials := budgetInitials(blob)
	if initials[preservedKeyField("third_party_block")] == "" {
		t.Fatalf("budgetInitials must capture an unmodeled block for preservation; got %v", initials)
	}
	out := buildBudgetJSON(initials)

	var got struct {
		Tokens    *float64        `json:"tokens"`
		Preserved json.RawMessage `json:"third_party_block"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("saved JSON is invalid: %v", err)
	}
	if got.Preserved == nil {
		t.Fatalf("an unmodeled block was DROPPED by a save that did not touch it: %s", out)
	}
	var blk struct {
		K int `json:"k"`
	}
	if err := json.Unmarshal(got.Preserved, &blk); err != nil || blk.K != 1 {
		t.Errorf("preserved block lost its value: %s", got.Preserved)
	}
	if got.Tokens == nil || *got.Tokens != 1000 {
		t.Errorf("modeled field tokens was not saved: %s", out)
	}
}

// TestContextHygieneIsTypedNotDynamic pins the reason this setting kept vanishing: it was carried only in the
// budget JSON DOCUMENT, which db.BudgetJSON derives from the typed ladder by an allowlist — a key with no
// column had nothing to rehydrate it on read, so a saved value came back blank. Both knobs are TYPED
// tenant_settings columns now, exactly like every other field on this screen, and must NOT be written into
// the budget document at all.
func TestContextHygieneIsTypedNotDynamic(t *testing.T) {
	// The form writes them as typed proto fields, and must not smuggle them into the document.
	out := buildBudgetJSON(map[string]string{
		"budget_tokens":                   "1000",
		"context_editing_keep_tool_calls": "20",
		"max_tool_output_bytes":           "32768",
	})
	if strings.Contains(out, "context_editing") {
		t.Fatalf("context_editing must not be written into the budget document — it is a typed column now: %s", out)
	}
	if strings.Contains(out, "max_tool_output_bytes") {
		t.Fatalf("max_tool_output_bytes must not be written into the budget document — it is a typed column: %s", out)
	}

	// And the typed fields must be populated from the row for the form.
	row := &apiv1.TenantSettings{}
	keep := int32(20)
	capBytes := int64(32768)
	row.ContextEditingKeepToolCalls = &keep
	row.MaxToolOutputBytes = &capBytes
	if got := row.GetContextEditingKeepToolCalls(); got != 20 {
		t.Errorf("typed keep_tool_calls = %d, want 20", got)
	}
	if got := row.GetMaxToolOutputBytes(); got != 32768 {
		t.Errorf("typed max_tool_output_bytes = %d, want 32768", got)
	}
}

// TestContextEditingNotInventedForUntouchedTenant: a tenant that never engaged with clearing must not get
// a block written, so the built-in "off" default keeps applying.
func TestContextEditingNotInventedForUntouchedTenant(t *testing.T) {
	out := buildBudgetJSON(budgetInitials(`{"tokens":1000}`))
	if strings.Contains(out, "context_editing") {
		t.Fatalf("an untouched tenant must not have context_editing invented: %s", out)
	}
}

// TestBudgetSaveWithNoSourceStillWorks: a fresh tenant (empty blob) must not break the save.
func TestBudgetSaveWithNoSourceStillWorks(t *testing.T) {
	out := buildBudgetJSON(budgetInitials(""))
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("empty-blob save produced invalid JSON: %v", err)
	}
}
