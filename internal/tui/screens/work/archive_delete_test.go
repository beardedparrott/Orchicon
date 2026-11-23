package work

// archive_delete_test.go — the archive view must be EMPTYABLE, not only restore-capable.
//
// The reported gap: "No delete or bulk delete options in TUI for archived work items."
//
// It was a real dead end rather than a missing convenience. An archived item is invisible to every
// active view (the list query filters `archived_at IS NULL`), so `restore` was the ONLY thing this
// pane offered and it puts the item back rather than away. An item archived by mistake and no longer
// wanted could only be removed by leaving the TUI for the GUI.
//
// The ordering assertions here are the substance, not decoration: the server REFUSES to hard-delete
// an item that still has children, so a fan-out in selection order would have a parent rejected while
// its descendants were deleted — a partially-dismantled hierarchy and a failed batch.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// archiveDeleteModel puts a two-level ARCHIVED subtree (feature → task) under a live epic, in the
// archive view, with the archived rows marked.
func archiveDeleteModel(t *testing.T) (*Model, *fakePlane) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "live", Title: "Live Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addArchive(archivedRow("af", "live", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))
	p.addArchive(archivedRow("at", "af", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_TASK))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v") // the archive view
	load(t, m, srcWorkItems)
	if m.ViewMode() != viewArchive {
		t.Fatalf("fixture: expected the archive view, got %q", m.ViewMode())
	}
	return m, p
}

// A MARKED ARCHIVE SELECTION OFFERS DELETE, and it deletes CHILDREN FIRST.
//
// The order is the whole point: deleting "af" (a parent) before "at" (its child) is REFUSED by the
// server, so the second assertion is what proves the batch can actually complete.
func TestBulkDeleteFromTheArchiveDeletesChildrenFirst(t *testing.T) {
	m, p := archiveDeleteModel(t)

	m.SelectItem(srcWorkItems, "af")
	press(t, m, " ")
	press(t, m, "down")
	press(t, m, " ")
	if n := m.Base.MarkCount(); n != 2 {
		t.Fatalf("fixture: expected 2 marked rows, got %d", n)
	}

	// THE OFFER: delete, on the shared chord, alongside restore.
	var labels []string
	for _, a := range m.actionsForSelection() {
		labels = append(labels, a.Key)
	}
	if _, ok := m.actionByKey(kit2.DeleteChord); !ok {
		t.Fatalf("a marked archive selection must offer delete, got keys %v", labels)
	}
	if _, ok := m.actionByKey("R"); !ok {
		t.Fatalf("delete must be ADDED to restore, not replace it — got keys %v", labels)
	}

	// THE HINT names it, so the chord is discoverable rather than secret.
	hint := ansi.Strip(m.HintLine())
	if !strings.Contains(hint, "delete") {
		t.Fatalf("the marked hint must name delete, got %q", hint)
	}

	// IT CONFIRMS FIRST — this is the one action in the pane that no keypress undoes.
	press(t, m, kit2.DeleteChord)
	if !m.DialogOpen() {
		t.Fatalf("`%s` on a marked selection must ask for confirmation first", kit2.DeleteChord)
	}
	run(t, m, press(t, m, "enter"))

	if len(p.hardDeleted) != 2 {
		t.Fatalf("HardDeleteWorkItem calls = %v, want both marked ids", p.hardDeleted)
	}
	if p.hardDeleted[0] != "at" || p.hardDeleted[1] != "af" {
		t.Fatalf("delete order = %v, want [at af] — CHILD FIRST. The server refuses to delete an "+
			"item that still has children, so deleting af before at would be rejected and the "+
			"batch would stop half-dismantled.", p.hardDeleted)
	}
}

// The PER-ROW delete: one archived item, no selection required.
//
// This is the case the operator hits most — a single item archived by mistake, no bulk workflow
// wanted. It must be reachable from the focused row exactly like restore is.
func TestPerRowDeleteFromTheArchive(t *testing.T) {
	m, p := archiveDeleteModel(t)

	m.SelectItem(srcWorkItems, "at") // the task — a leaf, so the server's children gate allows it

	var labels []string
	for _, a := range m.itemActions() {
		labels = append(labels, a.Key)
	}
	if _, ok := m.actionByKey(kit2.DeleteChord); !ok {
		t.Fatalf("the focused archive row must offer delete, got keys %v", labels)
	}

	press(t, m, kit2.DeleteChord)
	if !m.DialogOpen() {
		t.Fatalf("per-row delete must confirm first")
	}
	run(t, m, press(t, m, "enter"))

	if len(p.hardDeleted) != 1 || p.hardDeleted[0] != "at" {
		t.Fatalf("HardDeleteWorkItem calls = %v, want [at]", p.hardDeleted)
	}
}

// A GHOST ANCHOR STILL OFFERS NOTHING.
//
// The live epic is drawn in the archive view only to keep the hierarchy connected. It is NOT
// archived, so it must never gain a delete action from being visible here — the anchor rule predates
// this change and delete must not weaken it.
func TestGhostAnchorGainsNoDelete(t *testing.T) {
	m, _ := archiveDeleteModel(t)

	m.SelectItem(srcWorkItems, "live")
	if acts := m.itemActions(); len(acts) != 0 {
		var labels []string
		for _, a := range acts {
			labels = append(labels, a.Key)
		}
		t.Fatalf("a ghost anchor (an ACTIVE ancestor) must offer no archive actions at all, got %v", labels)
	}
}
