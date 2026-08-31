package askorchicon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// dbWorkItemRowFat is a work-item row carrying the fat columns that must
// never reach a list output (description, acceptance criteria, budgets,
// context files, worker ref, prompt context).
func dbWorkItemRowFat() db.WorkItemRow {
	return db.WorkItemRow{
		ID: "wi_1", TenantID: "tnt", ProjectID: "p", Title: "t", Kind: "task",
		Description: "long", AcceptanceCriteria: "long", Budgets: []byte("{}"),
		ContextFiles: []byte("[]"), Status: "pending", Priority: 1,
		AssignedWorkerRef: []byte("{}"), PromptContext: []byte("{}"),
	}
}

func TestNewCompactListUnderCap(t *testing.T) {
	rows := make([]any, 3)
	for i := range rows {
		rows[i] = map[string]any{"ID": i}
	}
	env := newCompactList(rows, "get_work_item")
	if env.Count != 3 || env.Truncated || env.Note != "" || len(env.Items) != 3 {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestNewCompactListTruncates(t *testing.T) {
	rows := make([]any, listCap+7)
	env := newCompactList(rows, "get_work_item")
	if env.Count != listCap || !env.Truncated || len(env.Items) != listCap {
		t.Fatalf("expected cap+truncation, got count=%d truncated=%v items=%d", env.Count, env.Truncated, len(env.Items))
	}
	if !strings.Contains(env.Note, "7 more item(s)") || !strings.Contains(env.Note, "get_work_item") {
		t.Fatalf("note = %q", env.Note)
	}
	// The wire shape must be {count, truncated, note, items} — the model
	// reads the envelope to decide whether to narrow or branch.
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Count     int              `json:"count"`
		Truncated bool             `json:"truncated"`
		Note      string           `json:"note"`
		Items     []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != listCap || !parsed.Truncated || len(parsed.Items) != listCap {
		t.Fatalf("wire shape wrong: %s", b)
	}
}

func TestNewCompactListNoDetailTool(t *testing.T) {
	rows := make([]any, listCap+1)
	env := newCompactList(rows, "")
	if !strings.Contains(env.Note, "filters") {
		t.Fatalf("note = %q", env.Note)
	}
}

func TestCompactListSetNextPage(t *testing.T) {
	// A truncated list with a next-page cursor gets the token AND a paging
	// hint so the model knows the tail is reachable.
	rows := make([]any, listCap+1)
	env := newCompactList(rows, "get_work_item")
	env.setNextPage("wi_last_25")
	if env.NextPageToken != "wi_last_25" {
		t.Fatalf("next_page_token = %q", env.NextPageToken)
	}
	if !strings.Contains(env.Note, "next_page_token") {
		t.Fatalf("note should mention paging: %q", env.Note)
	}
	// A non-truncated list never carries a token.
	small := newCompactList(rows[:3], "get_work_item")
	small.setNextPage("x")
	if small.NextPageToken != "" || small.Note != "" {
		t.Fatalf("non-truncated list must not page: %+v", small)
	}
}

func TestCompactWorkItemDropsFatColumns(t *testing.T) {
	// The compact row must carry only the identify-and-branch fields — the
	// bloat (description, acceptance criteria, budgets, context files,
	// worker refs, prompt context) never reaches the conversation.
	r := compactWorkItem(dbWorkItemRowFat())
	if len(r) != 6 {
		t.Fatalf("compact work item has %d fields, want 6: %v", len(r), r)
	}
	if _, ok := r["Description"]; ok {
		t.Fatal("compact row must not carry Description")
	}
	if r["Title"] != "t" || r["Status"] != "pending" {
		t.Fatalf("compact row = %v", r)
	}
}