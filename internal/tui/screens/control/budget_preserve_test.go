package control

import (
	"encoding/json"
	"strings"
	"testing"
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

// TestContextEditingIsModeledNow: context_editing moved from "preserved" to "rendered". This pins that it
// is never BOTH unrendered and unpreserved — the state that silently loses the setting on save.
func TestContextEditingIsModeledNow(t *testing.T) {
	for _, k := range unmodeledBudgetKeys {
		if k == "context_editing" {
			t.Fatal("context_editing is rendered by this form now; listing it as unmodeled would keep a stale copy")
		}
	}
	blob := `{"context_editing":{"enabled":true,"trigger_frac":0.6,"keep_recent":12,"min_bytes":4096,"band_width":0.2}}`
	initials := budgetInitials(blob)
	if initials["clear_enabled"] != "true" {
		t.Errorf("clear_enabled not seeded from the document: %q", initials["clear_enabled"])
	}
	if initials["clear_keep_recent"] != "12" {
		t.Errorf("clear_keep_recent = %q, want 12", initials["clear_keep_recent"])
	}
	out := buildBudgetJSON(initials)
	if !strings.Contains(out, `"context_editing"`) {
		t.Fatalf("the modeled block was not written back: %s", out)
	}
	if !strings.Contains(out, `"keep_recent":12`) {
		t.Errorf("round-trip lost keep_recent: %s", out)
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
