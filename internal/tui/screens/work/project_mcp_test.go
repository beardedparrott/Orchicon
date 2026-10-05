package work

// project_mcp_test.go — THE MCP FIELD SEAM, AND THE PROJECT DELETE.
//
// Two operator reports:
//   "I guess let's create the MCP stuff." — the project forms were missing the MCP
//   server selection the GUI offers, because it is the one field whose data is not on
//   the Project message.
//   "There is no ctrl+x delete for single or bulk on projects." — the RPC existed and
//   the TUI never offered it; worse, the BULK path silently aimed at the wrong resource.
//
// CORRECTION (child 7): the "selection" above is GONE. A definition is OWNER-SCOPED
// (mcp_servers.project_id), so there is no tenant list to select from and the old
// ProjectMCPField (a KMultiSelect over the tenant's entries) was REMOVED. The create
// form now carries an OWNED-DEFINITION SEED (ProjectMCPDefinitionsField, a JSON array)
// and the project action bar defines rows one at a time. The tests below pin the NEW
// shape; the select-from-tenant shape is gone with the RPC that fed it.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
)

// THE CREATE FORM CARRIES AN OWNED-DEFINITION SEED, not a selection. It is a KJSON
// field, present ALWAYS (a create has no existing rows, so the array cannot delete
// anything, and an empty array is simply "no definitions yet").
func TestProjectMCPDefinitionsFieldShape(t *testing.T) {
	f := ProjectMCPDefinitionsField()
	if f.Kind != kit2.KJSON {
		t.Errorf("the MCP definitions field kind is %v, want KJSON — it is a JSON array of "+
			"inline specs, not a multi-select over a tenant list", f.Kind)
	}
	if f.Name != "mcp_servers" {
		t.Errorf("field name = %q, want mcp_servers", f.Name)
	}
	// A create form always offers it (unlike the old field, which vanished when the
	// tenant list failed to load). The seed is the form's own.
	m := newModel(t, newPlane())
	form := m.newProjectCreateForm()
	if !formHasField(form, "mcp_servers") {
		t.Error("the create form offers no mcp_servers field — the TUI cannot define a project MCP entry")
	}
	// The EDIT form deliberately does NOT: a blob save on an edit would silently
	// delete rows the operator never saw.
	edit := m.newProjectEditForm(&apiv1.Project{Id: "p1", Name: "P"})
	if formHasField(edit, "mcp_servers") {
		t.Error("the EDIT form offers an mcp_servers field — a blob editor on an edit can delete " +
			"rows the operator never saw, which is the failure the field exists to avoid")
	}
	// But the edit form DOES carry skill_files (the same path-list idiom as context_files).
	if !formHasField(edit, "skill_files") {
		t.Error("the edit form offers no skill_files field")
	}
}

// THE OWNERSHIP RULE THIS FILE USED TO PIN THROUGH THE DEFINE WRAPPER NOW LIVES WITH THE MODAL, which
// is what actually builds that request: see TestProjectModalCreateIsStampedWithTheProjectAndCarriesItsArgs
// in internal/tui/scope_modal_owners_test.go. The wrapper is gone (the modal calls mcpforms directly), so
// keeping the assertion here would have pinned a helper nothing calls.

// AN EMPTY DEFINITION ARRAY CREATES NOTHING — the create form's other fields still
// apply, and no stray owned row is written.
func TestEmptyProjectMCPDefinitionsCreateNothing(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Thing")
	if err := applyProjectPostCreate(context.Background(), p, p, "proj-1", 0, nil, nil, nil); err != nil {
		t.Fatalf("applyProjectPostCreate: %v", err)
	}
	if len(p.mcpCreated) != 0 {
		t.Errorf("an empty definitions array issued %d create(s); an empty array means no "+
			"definitions, not a null one", len(p.mcpCreated))
	}
}

// THE CREATE FORM'S SEED REACHES THE OWNED CREATE. A definition typed into the create
// form's JSON, submitted, lands as a project-owned row.
func TestCreateFormSeedBecomesOwnedRows(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Thing")
	specs := `{"mcp_servers":[{"id":"github","type":"stdio","command":["npx","-y","x"],"env":{"T":"${T}"}}]}`
	defs := mcpforms.ParseInline(specs)
	if len(defs) != 1 {
		t.Fatalf("parsed %d specs, want 1", len(defs))
	}
	if err := applyProjectPostCreate(context.Background(), p, p, "proj-1", 0, nil, nil, defs); err != nil {
		t.Fatalf("applyProjectPostCreate: %v", err)
	}
	if len(p.mcpCreated) != 1 {
		t.Fatalf("CreateMCPServer calls = %d, want 1", len(p.mcpCreated))
	}
	if p.mcpCreated[0].GetProjectId() != "proj-1" {
		t.Errorf("project_id = %q, want proj-1", p.mcpCreated[0].GetProjectId())
	}
	if p.mcpCreated[0].GetName() != "github" {
		t.Errorf("name = %q, want github", p.mcpCreated[0].GetName())
	}
}

// formHasField reports whether a form offers a named field — the kit2 form's Specs.
func formHasField(f *kit2.Form, name string) bool {
	for i := range f.Specs {
		if f.Specs[i].Name == name {
			return true
		}
	}
	return false
}

// runCmd executes a mutation cmd and asserts it succeeded.
func runCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a write cmd, got nil")
	}
	res, ok := cmd().(mutate.Result)
	if !ok {
		t.Fatal("the write cmd must produce a mutate.Result")
	}
	if res.Err != nil {
		t.Fatalf("write failed: %v", res.Err)
	}
}

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
