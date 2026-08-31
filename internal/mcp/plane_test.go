package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func TestCompactPlaneWorkItems(t *testing.T) {
	items := make([]*apiv1.WorkItem, 0, planeListCap+5)
	for i := 0; i < planeListCap+5; i++ {
		items = append(items, &apiv1.WorkItem{
			Id:       fmt.Sprintf("wi_%d", i),
			Title:    "Backlog item",
			Kind:     apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			Status:   apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
			Priority: 2,
		})
	}
	raw, err := compactPlaneWorkItems(items)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Count         int              `json:"count"`
		Truncated     bool             `json:"truncated"`
		Note          string           `json:"note"`
		Items         []map[string]any `json:"items"`
		NextPageToken string           `json:"next_page_token"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Count != planeListCap || !env.Truncated || len(env.Items) != planeListCap {
		t.Fatalf("env = count %d truncated %v items %d", env.Count, env.Truncated, len(env.Items))
	}
	if env.NextPageToken != "wi_24" {
		t.Fatalf("next_page_token = %q, want wi_24 (the 25th/last shown item's id)", env.NextPageToken)
	}
	if !strings.Contains(env.Note, "5 more item(s)") {
		t.Fatalf("note = %q", env.Note)
	}
	if !strings.Contains(env.Note, "next_page_token") {
		t.Fatalf("note should mention paging: %q", env.Note)
	}
	first := env.Items[0]
	if first["kind"] != "task" || first["status"] != "pending" {
		t.Fatalf("labels = %v", first)
	}
}

func TestWorkItemLabelHelpers(t *testing.T) {
	if workItemKindLabel(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE) != "feature" {
		t.Fatalf("kind label = %q", workItemKindLabel(apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))
	}
	if workItemStatusLabel(apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED) != "succeeded" {
		t.Fatalf("status label = %q", workItemStatusLabel(apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED))
	}
}

// TestCompactPlaneWorkItemCreated regression-tests the two 2026-08-31
// plane-channel defects: (1) the create relay sent a bare run ID where the
// run_context JSONB belongs — ProvenanceFromRunContext unmarshal-fails on
// it, stamping silently no-ops, and idea spawns land as plain pending
// items invisible to the Idea Cloud; (2) the raw proto response showed
// `status: 1` (PENDING) with no labels, which the worker misread as IDEA
// confirmation. The compact envelope must carry labeled status + explicit
// provenance/idea_state fields so a spawn's landing state is verifiable
// from the tool response alone.
func TestCompactPlaneWorkItemCreated(t *testing.T) {
	t.Run("pending create is labeled plainly — never misreadable as idea", func(t *testing.T) {
		raw, err := compactPlaneWorkItemCreated(&apiv1.CreateWorkItemResponse{
			WorkItem: &apiv1.WorkItem{
				Id:     "wi_1",
				Title:  "Some proposal",
				Kind:   apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE,
				Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Status    string `json:"status"`
			IdeaState bool   `json:"idea_state"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.Status != "pending" {
			t.Fatalf("status label = %q, want the readable string \"pending\" — a bare enum number (1) got misread as IDEA by a spawning worker", env.Status)
		}
		if env.IdeaState {
			t.Fatal("idea_state must be false for a pending item")
		}
	})

	t.Run("idea-state create reports landed idea state + provenance", func(t *testing.T) {
		raw, err := compactPlaneWorkItemCreated(&apiv1.CreateWorkItemResponse{
			WorkItem: &apiv1.WorkItem{
				Id:     "wi_2",
				Title:  "Spawned idea",
				Kind:   apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE,
				Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA,
				// The server stamps these from the relayed run_context.
				SpawnedBy:      "01M17F7E4S340KEXQYFZA0PZZ4",
				SpawnedByRunId: "01M1AW8G6MSBSKZVEAA9XTDVQN",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Status       string `json:"status"`
			LandedStatus string `json:"landed_status"`
			IdeaState    bool   `json:"idea_state"`
			SpawnedBy    string `json:"spawned_by"`
			SpawnedRun   string `json:"spawned_run"`
			Kind         string `json:"kind"`
			ParentID     string `json:"parent_id"`
			Priority     int32  `json:"priority"`
			ID           string `json:"id"`
			Title        string `json:"title"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.Status != "idea" {
			t.Fatalf("status label = %q, want \"idea\"", env.Status)
		}
		if !env.IdeaState || env.SpawnedBy == "" || env.SpawnedRun == "" {
			t.Fatalf("idea landing not verifiable: idea_state=%v spawned_by=%q spawned_run=%q", env.IdeaState, env.SpawnedBy, env.SpawnedRun)
		}
		if env.Kind != "feature" || env.ID != "wi_2" || env.Title != "Spawned idea" {
			t.Fatalf("labels = %q/%q/%q", env.Kind, env.ID, env.Title)
		}
		if env.Status != env.LandedStatus {
			t.Fatalf("status %q and landed_status %q must agree", env.Status, env.LandedStatus)
		}
	})
}

// TestRefuseIdeaSpawn pins the loud-no-op gate on orchicon_plane_create_idea_item:
// the tool must REFUSE (not silently land plain-pending) when the run's
// run_context carries no provenance block (the bare-run-ID relay bug class)
// or when the recurrence's outputs_mode is not idea. Only a full idea block
// passes.
func TestRefuseIdeaSpawn(t *testing.T) {
	t.Run("no provenance block refuses", func(t *testing.T) {
		err := refuseIdeaSpawn(nil)
		if err == nil {
			t.Fatal("nil run_context must refuse")
		}
		if !strings.Contains(err.Error(), "no automation provenance") {
			t.Fatalf("err = %v, want the provenance-missing reason", err)
		}
		if !strings.Contains(err.Error(), "FACTS LEARNED") {
			t.Fatalf("err must name the facts-learned fallback: %v", err)
		}
	})
	t.Run("bare run id refuses (unparsable as run_context)", func(t *testing.T) {
		err := refuseIdeaSpawn([]byte("01M1AZPBTGGBZSX22BWQMZPZAF"))
		if err == nil {
			t.Fatal("a bare run ID is not a run_context and must refuse")
		}
	})
	t.Run("standard outputs_mode refuses", func(t *testing.T) {
		rc := []byte(`{"spawned_by": "01M17F7E4S340KEXQYFZA0PZZ4", "spawned_by_run_id": "01M1AZPBTGGBZSX22BWQMZPZAF", "outputs_mode": "standard"}`)
		err := refuseIdeaSpawn(rc)
		if err == nil {
			t.Fatal("a standard-mode recurrence must not spawn idea items")
		}
		if !strings.Contains(err.Error(), "outputs_mode") {
			t.Fatalf("err = %v, want the outputs_mode mismatch reason", err)
		}
	})
	t.Run("idea outputs_mode with provenance passes", func(t *testing.T) {
		rc := []byte(`{"spawned_by": "01M17F7E4S340KEXQYFZA0PZZ4", "spawned_by_run_id": "01M1AZPBTGGBZSX22BWQMZPZAF", "outputs_mode": "idea"}`)
		if err := refuseIdeaSpawn(rc); err != nil {
			t.Fatalf("an idea-mode spawn must pass the gate, got: %v", err)
		}
	})
}

// TestCompactIdeaEnvelopeError: a create whose landed state is NOT idea
// (despite the gate passing) must return an error envelope — never a
// success-shaped one — so a spawning worker cannot misread the landing.
func TestCompactIdeaEnvelopeError(t *testing.T) {
	raw, err := compactIdeaEnvelopeError(&apiv1.WorkItem{
		Id:     "wi_3",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Error      string `json:"error"`
		IdeaState  bool   `json:"idea_state"`
		LandedStat string `json:"landed_status"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == "" {
		t.Fatal("error envelope must carry the explanation")
	}
	if !strings.Contains(env.Error, "NOT confirmed") || !strings.Contains(env.Error, "pending") {
		t.Fatalf("error = %q, want the landed-state mismatch (pending surfaced)", env.Error)
	}
	if env.IdeaState {
		t.Fatal("idea_state must be false in the error envelope")
	}
	if env.LandedStat != "pending" {
		t.Fatalf("landed_status = %q, want pending", env.LandedStat)
	}
}