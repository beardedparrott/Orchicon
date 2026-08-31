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