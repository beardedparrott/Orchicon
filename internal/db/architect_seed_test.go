package db

import (
	"encoding/json"
	"testing"
)

func TestArchitectSeedWorkflowsJSON(t *testing.T) {
	// The two Architect workflows added as canned seeds must carry valid,
	// parseable steps with positions, edge_handles, and per-step config.
	var ai, human *cannedWorkflow
	for i := range cannedWorkflows {
		switch cannedWorkflows[i].ID {
		case "01KZ43VD5CGFXHK1SWPDJKEGPT":
			ai = &cannedWorkflows[i]
		case "01KZ1W513F25ASPZM1XW4ZJ2MB":
			human = &cannedWorkflows[i]
		}
	}
	if ai == nil {
		t.Fatal("AI-Architect canned workflow not found")
	}
	if human == nil {
		t.Fatal("Human-Architect canned workflow not found")
	}

	for name, wf := range map[string]*cannedWorkflow{"ai": ai, "human": human} {
		var steps []map[string]any
		if err := json.Unmarshal([]byte(wf.StepsJSON), &steps); err != nil {
			t.Fatalf("%s: steps not valid JSON: %v", name, err)
		}
		// Both Architect templates parallelize PR review + QA under a
		// single fan-in gate (loop-2 removed), so each has 9 steps.
		const wantSteps = 9
		if len(steps) != wantSteps {
			t.Errorf("%s: expected %d steps, got %d", name, wantSteps, len(steps))
		}
		for _, s := range steps {
			if s["id"] == nil || s["name"] == nil || s["kind"] == nil {
				t.Errorf("%s: step missing id/name/kind: %v", name, s)
			}
			if s["config"] == "" {
				t.Errorf("%s: step %v has no config (settings)", name, s["id"])
			}
			// Positions and edge_handles live on the shared repo step; every
			// task/approval step carries position_x/position_y.
			_, hasX := s["position_x"]
			_, hasY := s["position_y"]
			if !hasX || !hasY {
				t.Errorf("%s: step %v missing position", name, s["id"])
			}
		}
		// The Principal Architect step must be present.
		hasArch := false
		for _, s := range steps {
			if s["ref"] == "w_se_principal_architect" {
				hasArch = true
			}
		}
		if !hasArch {
			t.Errorf("%s: missing Principal Architect step", name)
		}
		// The shared repo step carries the edge_handles map.
		for _, s := range steps {
			if s["id"] == "step-repo" {
				if s["edge_handles"] == nil {
					t.Errorf("%s: step-repo missing edge_handles", name)
				} else if eh, ok := s["edge_handles"].(map[string]any); !ok || len(eh) == 0 {
					t.Errorf("%s: step-repo edge_handles empty", name)
				}
			}
		}
	}
}
