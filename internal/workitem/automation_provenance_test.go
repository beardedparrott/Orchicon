package workitem

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// baseStatus is any non-Idea status a plain create starts as; it must be left
// untouched by provenance stamping unless outputs_mode is idea.
const baseStatus = domain.WorkItemBlocked

// TestApplyAutomationProvenanceAC2 pins the feature 4.1 / AC2 contract: a work
// item created from inside a recurring fire's run is stamped with the fire's
// provenance block, and (only when the fire's outputs_mode is idea) lands in
// IDEA state. Plain creates (empty run_context) are untouched — backward
// compatible.
func TestApplyAutomationProvenanceAC2(t *testing.T) {
	ctx := context.Background()

	t.Run("empty run_context is a no-op", func(t *testing.T) {
		for _, rc := range [][]byte{nil, []byte{}, []byte("{}"), []byte("not json")} {
			row := &db.WorkItemRow{Status: baseStatus}
			ApplyAutomationProvenance(row, rc)
			if row.SpawnedByWorkItemID != nil || row.SpawnedByRunID != nil {
				t.Fatalf("empty/malformed run_context (%q) must not stamp provenance", rc)
			}
			if row.Status != baseStatus {
				t.Fatalf("empty run_context must not change status: got %q", row.Status)
			}
		}
	})

	t.Run("provenance stamps spawned_by and run id, keeps status", func(t *testing.T) {
		row := &db.WorkItemRow{Status: baseStatus}
		rc := provenanceCtx(t, "recur_1", "run_1", domain.RecurringOutputsStandard)
		ApplyAutomationProvenance(row, rc)
		if row.SpawnedByWorkItemID == nil || *row.SpawnedByWorkItemID != "recur_1" {
			t.Fatalf("spawned_by not stamped: %v", row.SpawnedByWorkItemID)
		}
		if row.SpawnedByRunID == nil || *row.SpawnedByRunID != "run_1" {
			t.Fatalf("spawned_by_run_id not stamped: %v", row.SpawnedByRunID)
		}
		if row.Status != baseStatus {
			t.Fatalf("non-idea outputs_mode must not change status: %q", row.Status)
		}
	})

	t.Run("idea outputs_mode lands the item in IDEA state", func(t *testing.T) {
		row := &db.WorkItemRow{Status: baseStatus}
		rc := provenanceCtx(t, "recur_2", "run_2", domain.RecurringOutputsIdea)
		ApplyAutomationProvenance(row, rc)
		if row.SpawnedByWorkItemID == nil || *row.SpawnedByWorkItemID != "recur_2" {
			t.Fatalf("spawned_by not stamped")
		}
		if row.Status != domain.WorkItemIdea {
			t.Fatalf("idea outputs_mode must set IDEA status: got %q", row.Status)
		}
	})

	t.Run("context injection round-trips", func(t *testing.T) {
		rc := provenanceCtx(t, "recur_3", "run_3", domain.RecurringOutputsIdea)
		cctx := WithAutomationRunContext(ctx, rc)
		if got := RunContextFrom(cctx); string(got) != string(rc) {
			t.Fatalf("RunContextFrom(WithAutomationRunContext) mismatch")
		}
		if got := RunContextFrom(ctx); got != nil {
			t.Fatalf("plain context must yield nil run_context")
		}
	})
}

// provenanceCtx builds a run_context JSONB carrying the automation provenance
// block, exactly as the workflow start path writes it.
func provenanceCtx(t *testing.T, spawnedBy, runID, outputsMode string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"spawned_by":        spawnedBy,
		"spawned_by_run_id": runID,
		"outputs_mode":      outputsMode,
	})
	if err != nil {
		t.Fatalf("marshal provenance: %v", err)
	}
	return b
}
