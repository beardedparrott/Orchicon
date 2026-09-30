package work

// project_mcp_test.go — THE MCP FIELD SEAM, AND THE PROJECT DELETE.
//
// Two operator reports:
//   "I guess let's create the MCP stuff." — the project forms were missing the MCP
//   server selection the GUI offers, because it is the one field whose data is not on
//   the Project message.
//   "There is no ctrl+x delete for single or bulk on projects." — the RPC existed and
//   the TUI never offered it; worse, the BULK path silently aimed at the wrong resource.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// THE FIELD APPEARS ONLY WHEN THERE IS SOMETHING TO CHOOSE, and its options are the
// servers with a visible label.
func TestProjectMCPFieldShape(t *testing.T) {
	if f := ProjectMCPField(nil, nil); f != nil {
		t.Errorf("ProjectMCPField with no servers returned a field (%v) — an empty multi-select is a control "+
			"that cannot do anything", f)
	}
	servers := []*apiv1.MCPServer{
		{Id: "m1", Name: "filesystem", Enabled: true},
		{Id: "m2", Name: "github", Enabled: false},
	}
	f := ProjectMCPField(servers, []string{"m1"})
	if f == nil {
		t.Fatal("ProjectMCPField returned nil with two servers")
	}
	if f.Kind != kit2.KMultiSelect {
		t.Errorf("the MCP field kind is %v, want a multi-select", f.Kind)
	}
	if len(f.Options) != 2 || f.Options[0].Value != "m1" || f.Options[0].Label != "filesystem" {
		t.Errorf("options = %+v, want m1/filesystem first", f.Options)
	}
	// A DISABLED server is still selectable (a project may reference one that is off),
	// but its row says so.
	if !strings.Contains(f.Options[1].Label, "disabled") {
		t.Errorf("a disabled server's option label is %q — it should say so, or the operator is surprised "+
			"when nothing happens at run time", f.Options[1].Label)
	}
	// The initial value is the COMMA-JOINED selection: that is what seeds a multi-select,
	// and it is why a project with servers selected cannot open with none ticked.
	if f.Initial != "m1" {
		t.Errorf("Initial = %q, want the project's selection joined", f.Initial)
	}
}

// NOTE: the project↔server SELECTION tests are GONE with the selection itself.
// A definition is OWNER-SCOPED (mcp_servers.project_id) and the
// Set/GetProjectMCPServers RPC pair was removed; the project forms no longer
// read or write a reference set. ProjectMCPField stays as the seam child 7
// re-homes onto the owner-scoped create payload.

// DELETE IS OFFERED, SINGLE.
func TestAProjectOffersDelete(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Thing")
	m := newModel(t, p)
	m.SelectSource(srcProjects)
	load(t, m, srcProjects)

	act, ok := findProjectAction(m, "delete")
	if !ok {
		t.Fatal("a project offers no delete action — the operator's report: \"There is no ctrl+x delete for " +
			"single or bulk on projects\"")
	}
	if act.Key != kit2.DeleteChord {
		t.Errorf("delete's key is %q, want %q — the operator asked for ctrl+x across the board, and every "+
			"other pane answers it", act.Key, kit2.DeleteChord)
	}
	if act.Confirm == "" {
		t.Error("delete has no confirmation, and DeleteProject is permanent")
	}
	if err := act.Do(context.Background()); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if len(p.projDeleted) != 1 || p.projDeleted[0] != "proj-1" {
		t.Fatalf("DeleteProject was called with %v, want [proj-1]", p.projDeleted)
	}
}

// DELETE IS OFFERED, BULK — and it calls the PROJECT RPC with PROJECT ids.
//
// This is the defect the report exposed: the bulk path was source-blind, so a projects
// selection was offered a "delete" that called DeleteWorkItem with project ids. The ids
// are distinct ULIDs, so nothing was destroyed — every call failed with not-found, and
// the operator saw "deleted 0 of 2 — 2 failed" for an operation aimed at the wrong thing.
func TestBulkDeleteOnProjectsCallsTheProjectRPC(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "One")
	p.seedProject("proj-2", "Two")
	m := newModel(t, p)
	m.Base.SelectSource(srcProjects)
	// LoadItems + marks, the way the bulk tests drive a real selection: BulkIDs() is what
	// makes actionsForSelection take the BULK branch at all, and a fixture that skipped the
	// marking would silently exercise the single-row path instead.
	m.Base.LoadItems(srcProjects, []kit2.Item{{ID: "proj-1", Title: "One"}, {ID: "proj-2", Title: "Two"}}, "")
	mark(t, m, 2)
	if got := m.Base.BulkIDs(); len(got) != 2 {
		t.Fatalf("fixture: marked %v, want both projects", got)
	}

	acts := m.actionsForSelection()
	var del *kit2.Action
	for i := range acts {
		if strings.Contains(strings.ToLower(acts[i].Label), "delete") {
			del = &acts[i]
		}
	}
	if del == nil {
		t.Fatalf("a projects bulk selection offers no delete; actions: %v", labelsOf(acts))
	}
	if !strings.Contains(del.Label, "2 selected") {
		t.Errorf("the bulk delete label is %q — it must name the COUNT, or a two-row operation reads like a "+
			"one-row one", del.Label)
	}
	if !strings.Contains(del.Confirm, "2") {
		t.Errorf("the confirm does not name the count: %q", del.Confirm)
	}
	if err := del.Do(context.Background()); err != nil {
		t.Fatalf("bulk delete failed: %v", err)
	}
	// THE PROJECT RPC, with project ids.
	if len(p.projDeleted) != 2 {
		t.Fatalf("DeleteProject calls = %d, want 2 (got %v)", len(p.projDeleted), p.projDeleted)
	}
	if len(p.deleted) != 0 {
		t.Errorf("the bulk delete called DeleteWorkItem %d time(s) with these ids (%v) — the bulk path is "+
			"aiming at the wrong resource", len(p.deleted), p.deleted)
	}
}

// THE WORK-ITEM BULK PATH IS UNCHANGED, so routing by source did not take the item
// actions away from the items.
func TestBulkDeleteOnWorkItemsStillCallsTheWorkItemRPC(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "One")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "A", ProjectId: "proj-1",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "wi-2", Title: "B", ProjectId: "proj-1",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	m := newModel(t, p)
	m.Base.SelectSource(srcWorkItems)
	m.Base.LoadItems(srcWorkItems, []kit2.Item{{ID: "wi-1", Title: "A"}, {ID: "wi-2", Title: "B"}}, "")
	mark(t, m, 2)
	if got := m.Base.BulkIDs(); len(got) != 2 {
		t.Fatalf("fixture: marked %v, want both items", got)
	}

	for _, a := range m.actionsForSelection() {
		if strings.Contains(strings.ToLower(a.Label), "delete") && a.Key == kit2.DeleteChord {
			if err := a.Do(context.Background()); err != nil {
				t.Fatalf("work-item bulk delete failed: %v", err)
			}
			if len(p.deleted) != 2 {
				t.Fatalf("DeleteWorkItem calls = %d, want 2", len(p.deleted))
			}
			if len(p.projDeleted) != 0 {
				t.Errorf("the WORK ITEM bulk delete called DeleteProject %d time(s)", len(p.projDeleted))
			}
			return
		}
	}
	t.Fatalf("no bulk delete on a work-items selection; actions: %v", labelsOf(m.actionsForSelection()))
}

func labelsOf(acts []kit2.Action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Label)
	}
	return out
}
