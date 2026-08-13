package db

import (
	"encoding/json"
	"testing"
)

func TestArchitectSeedWorkflowsJSON(t *testing.T) {
	// The Architect workflows added as canned seeds must carry valid,
	// parseable steps with positions, edge_handles, and per-step config.
	type arch struct {
		id  string
		wf  *cannedWorkflow
		ref string
	}
	wants := []arch{
		{id: "01KZ43VD5CGFXHK1SWPDJKEGPT", ref: "w_se_principal_architect"},
		{id: "01KZ1W513F25ASPZM1XW4ZJ2MB", ref: "w_se_principal_architect"},
		{id: "01KZA9H7935CRTAHVE3EHVC1NZ", ref: "w_se_architect_vision"},
		{id: "01KZA9M2PMVNZG3QPHQ7AS3GA1", ref: "w_se_architect_vision"},
	}
	for i := range wants {
		for j := range cannedWorkflows {
			if cannedWorkflows[j].ID == wants[i].id {
				wants[i].wf = &cannedWorkflows[j]
			}
		}
		if wants[i].wf == nil {
			t.Fatalf("%s: Architect canned workflow not found", wants[i].id)
		}
	}

	for _, w := range wants {
		label := w.id
		var steps []map[string]any
		if err := json.Unmarshal([]byte(w.wf.StepsJSON), &steps); err != nil {
			t.Fatalf("%s: steps not valid JSON: %v", label, err)
		}
		// All Architect templates parallelize PR review + QA under an
		// explicit Parallel step feeding a single fan-in loop gate,
		// so each has 10 steps.
		const wantSteps = 10
		if len(steps) != wantSteps {
			t.Errorf("%s: expected %d steps, got %d", label, wantSteps, len(steps))
		}
		for _, s := range steps {
			if s["id"] == nil || s["name"] == nil || s["kind"] == nil {
				t.Errorf("%s: step missing id/name/kind: %v", label, s)
			}
			if s["config"] == "" {
				t.Errorf("%s: step %v has no config (settings)", label, s["id"])
			}
			// Positions and edge_handles live on the shared repo step; every
			// task/approval step carries position_x/position_y.
			_, hasX := s["position_x"]
			_, hasY := s["position_y"]
			if !hasX || !hasY {
				t.Errorf("%s: step %v missing position", label, s["id"])
			}
		}
		// The Architect step (Principal Architect or Vision variant) must be
		// present with the expected worker ref.
		hasArch := false
		for _, s := range steps {
			if s["ref"] == w.ref {
				hasArch = true
			}
		}
		if !hasArch {
			t.Errorf("%s: missing Architect step (ref %s)", label, w.ref)
		}
		// The shared repo step carries the edge_handles map.
		for _, s := range steps {
			if s["id"] == "step-repo" {
				if s["edge_handles"] == nil {
					t.Errorf("%s: step-repo missing edge_handles", label)
				} else if eh, ok := s["edge_handles"].(map[string]any); !ok || len(eh) == 0 {
					t.Errorf("%s: step-repo edge_handles empty", label)
				}
			}
		}
	}
}
