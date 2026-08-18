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
		// so each has 9 steps.
		const wantSteps = 9
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
			// Every task/approval step carries position_x/position_y.
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
		// The entry step (the only one with no dependencies — the Architect
		// step, since step-repo was removed) carries the edge_handles map.
		entryFound := false
		for _, s := range steps {
			deps, _ := s["depends_on"].([]any)
			if len(deps) == 0 {
				entryFound = true
				if s["edge_handles"] == nil {
					t.Errorf("%s: entry step %v missing edge_handles", label, s["id"])
				} else if eh, ok := s["edge_handles"].(map[string]any); !ok || len(eh) == 0 {
					t.Errorf("%s: entry step %v edge_handles empty", label, s["id"])
				}
			}
		}
		if !entryFound {
			t.Errorf("%s: no entry step found", label)
		}
	}
}

// TestConflictAwareSeedWorkflowJSON validates the conflict-aware canned
// template: it must include the merge gate (loop_decision), the Integrator
// worker step (w_se_integrator) wired into the gate's conflict_chain, and an
// exhausted_review escalation, while the Integrator stays off the first pass
// (its depends_on is empty and nothing on the normal path depends on it).
func TestConflictAwareSeedWorkflowJSON(t *testing.T) {
	var wf *cannedWorkflow
	for i := range cannedWorkflows {
		if cannedWorkflows[i].ID == "01KZB0CONFLICT000000000001" {
			wf = &cannedWorkflows[i]
		}
	}
	if wf == nil {
		t.Fatalf("conflict-aware canned workflow not found")
	}
	var steps []map[string]any
	if err := json.Unmarshal([]byte(wf.StepsJSON), &steps); err != nil {
		t.Fatalf("steps not valid JSON: %v", err)
	}

	var gate, integrator map[string]any
	hasIntegratorRef := false
	for _, s := range steps {
		switch s["id"] {
		case "step-loop-merge":
			gate = s
		case "step-integrator":
			integrator = s
		}
		if s["ref"] == "w_se_integrator" {
			hasIntegratorRef = true
		}
	}
	if gate == nil {
		t.Fatal("missing merge gate step (step-loop-merge)")
	}
	if gate["kind"] != "loop_decision" {
		t.Errorf("merge gate kind = %v, want loop_decision", gate["kind"])
	}
	if integrator == nil {
		t.Fatal("missing Integrator step (step-integrator)")
	}
	if !hasIntegratorRef {
		t.Errorf("Integrator step missing worker ref w_se_integrator")
	}
	if deps, ok := integrator["depends_on"].([]any); !ok || len(deps) != 0 {
		t.Errorf("Integrator must be free-floating (empty depends_on) so it never runs on the first clean pass")
	}

	// The gate config must route conflict through the Integrator and
	// escalate exhausted budgets to human review.
	var cfg map[string]any
	if err := json.Unmarshal([]byte(gate["config"].(string)), &cfg); err != nil {
		t.Fatalf("gate config not valid JSON: %v", err)
	}
	if cfg["conflict_value"] != "conflict" {
		t.Errorf("gate conflict_value = %v, want conflict", cfg["conflict_value"])
	}
	chain, _ := cfg["conflict_chain"].([]any)
	if len(chain) != 1 || chain[0] != "step-integrator" {
		t.Errorf("gate conflict_chain = %v, want [step-integrator]", cfg["conflict_chain"])
	}
	if cfg["exhausted_review"] != "human" {
		t.Errorf("gate exhausted_review = %v, want human", cfg["exhausted_review"])
	}
	// The gate must sit downstream of the merge step.
	gateDeps, _ := gate["depends_on"].([]any)
	if len(gateDeps) != 1 || gateDeps[0] != "step-devops-pr" {
		t.Errorf("gate depends_on = %v, want [step-devops-pr]", gate["depends_on"])
	}
}
