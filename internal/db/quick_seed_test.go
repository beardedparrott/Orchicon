package db

import (
	"encoding/json"
	"strings"
	"testing"
)

// quick_seed_test.go: pure (DB-less) pins for the Quick Software Engineer +
// Quick Work seeder promotion. The DB-backed seeder tests
// (seed_workers_test.go, seed_workflows_test.go) cover boot idempotency;
// these assert the seeded DEFINITIONS are valid and carry the corrected
// deny-by-default stance.

func quickCannedWorker(t *testing.T) *cannedWorker {
	t.Helper()
	for i := range cannedWorkers {
		if cannedWorkers[i].ID == "01M1ERH9921YS9S1QWGZV8D1VM" {
			return &cannedWorkers[i]
		}
	}
	t.Fatal("cannedWorkers must contain the quick-software-engineer entry (id 01M1ERH9921YS9S1QWGZV8D1VM)")
	return nil
}

func quickCannedWorkflow(t *testing.T) *cannedWorkflow {
	t.Helper()
	for i := range cannedWorkflows {
		if cannedWorkflows[i].ID == "01M1ERHNCNF38MP3SEV1GTH26G" {
			return &cannedWorkflows[i]
		}
	}
	t.Fatal("cannedWorkflows must contain the Quick Work entry (id 01M1ERHNCNF38MP3SEV1GTH26G)")
	return nil
}

func TestQuickWorkerSeedDefinition(t *testing.T) {
	w := quickCannedWorker(t)
	if w.Slug != "quick-software-engineer" {
		t.Errorf("slug = %q, want quick-software-engineer", w.Slug)
	}
	if w.Name != "Quick Software Engineer" {
		t.Errorf("name = %q", w.Name)
	}
	// Deny-by-default: no role binding, no plane channel.
	if w.RoleRef != "" {
		t.Errorf("RoleRef = %q, want empty (deny-by-default: no plane channel)", w.RoleRef)
	}
	if w.RecreateSlugOwner {
		t.Error("RecreateSlugOwner must be false (the live id already owns the slug)")
	}
	if len(w.BudgetOverrides) > 0 {
		var v map[string]any
		if err := json.Unmarshal(w.BudgetOverrides, &v); err != nil {
			t.Errorf("BudgetOverrides invalid JSON: %v", err)
		}
	}
	if w.RollMarker == "" {
		t.Error("Quick worker must carry a per-worker RollMarker")
	}
	if !strings.Contains(w.AgentsMD, w.RollMarker) && !strings.Contains(w.Behavior, w.RollMarker) {
		t.Errorf("RollMarker %q must appear in the seed content", w.RollMarker)
	}
	// Corrected stance in the persisted content.
	persisted := seedAgentsMD(*w)
	for _, want := range []string{
		"Sandbox vs plane",
		seedSafetyMarker,
		sandboxPlaneMarker, // deny-by-default roll-forward fragment
		"deny-by-default",
		"Verify, don't assume",
		"PR_URL:",
		"PR_STATE:",
		"architecture-notes/",
	} {
		if !strings.Contains(persisted, want) {
			t.Errorf("seeded Quick AgentsMD missing %q", want)
		}
	}
	// The rationalizing fallback is gone from the default path.
	for _, gone := range []string{"shipping manifests for the UI", "not image-gated", "role-scoped through your worker identity"} {
		if strings.Contains(persisted, gone) {
			t.Errorf("seeded Quick AgentsMD must not contain %q", gone)
		}
	}
	if !strings.Contains(persisted, "never use plane tools") {
		t.Errorf("seeded Quick AgentsMD must keep the sandbox/plane hard rules")
	}
}

func TestQuickWorkflowSeedDefinition(t *testing.T) {
	w := quickCannedWorkflow(t)
	if w.Name != "Quick Work" {
		t.Errorf("name = %q, want Quick Work", w.Name)
	}
	if w.VersionID != "wfv_quick_work_v1" {
		t.Errorf("VersionID = %q, want wfv_quick_work_v1", w.VersionID)
	}
	if w.GitStrategy != "pr" {
		t.Errorf("GitStrategy = %q, want pr (single-step harness pushes + PRs)", w.GitStrategy)
	}
	var steps []map[string]any
	if err := json.Unmarshal([]byte(w.StepsJSON), &steps); err != nil {
		t.Fatalf("StepsJSON invalid: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("Quick Work steps = %d, want 2 (step-quick + step-end)", len(steps))
	}
	byID := map[string]map[string]any{}
	for _, s := range steps {
		id, _ := s["id"].(string)
		byID[id] = s
		if s["kind"] == nil || s["name"] == nil || s["config"] == nil {
			t.Errorf("step %v missing id/kind/name/config", s["id"])
		}
	}
	quick, ok := byID["step-quick"]
	if !ok {
		t.Fatal("Quick Work steps missing step-quick")
	}
	if quick["ref"] != "01M1ERH9921YS9S1QWGZV8D1VM" {
		t.Errorf("step-quick ref = %v, want the Quick worker id", quick["ref"])
	}
	if quick["kind"] != "task" {
		t.Errorf("step-quick kind = %v, want task", quick["kind"])
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(quick["config"].(string)), &cfg); err != nil {
		t.Fatalf("step-quick config invalid: %v", err)
	}
	end, ok := byID["step-end"]
	if !ok {
		t.Fatal("Quick Work steps missing step-end")
	}
	if end["kind"] != "end" {
		t.Errorf("step-end kind = %v, want end", end["kind"])
	}
	deps, _ := end["depends_on"].([]any)
	if len(deps) != 1 || deps[0] != "step-quick" {
		t.Errorf("step-end depends_on = %v, want [step-quick]", end["depends_on"])
	}
}

func TestQuickWorkerWorkflowRefAgreement(t *testing.T) {
	// The workflow's task step must point at the seeded Quick worker.
	w := quickCannedWorker(t)
	wf := quickCannedWorkflow(t)
	if !strings.Contains(wf.StepsJSON, w.ID) {
		t.Errorf("Quick Work steps must reference the Quick worker id %s", w.ID)
	}
}

func TestSeededWorkersCarryDenyByDefaultStance(t *testing.T) {
	// Every canned worker's persisted seed content carries the corrected
	// stance (via the shared sandboxPlaneBlock) and none carries the old
	// rationalizing fallback.
	for _, w := range cannedWorkers {
		persisted := seedAgentsMD(w)
		if !strings.Contains(persisted, "deny-by-default") {
			t.Errorf("%s (%s) missing the deny-by-default stance", w.Slug, w.ID)
		}
		if strings.Contains(persisted, "shipping manifests for the UI") {
			t.Errorf("%s (%s) still carries the ship-manifests fallback", w.Slug, w.ID)
		}
		if strings.Contains(persisted, "not image-gated") {
			t.Errorf("%s (%s) still carries the not-image-gated wording", w.Slug, w.ID)
		}
	}
}
