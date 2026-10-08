package work

// skipped_status_test.go — "skipped" is a user-settable status in the TUI,
// sanctioned rather than accidental (this item's AC8/17).
//
// statusOptions() already listed "skipped" and the form specs already wired
// it through statusForEdit/statusFromName — both pre-dating this item. What
// this pins is the OBSERVABLE behaviour that makes the offer real: a
// work item that is ALREADY skipped must open its status form SEEDED to
// "skipped" (not silently fall back to the "pending" default statusForEdit
// uses for a non-user-assignable status), and submitting that form must
// send WORK_ITEM_STATUS_SKIPPED on the wire — proving the "s" quick-status
// gesture and the full detail editor both reach the same status the
// sequence engine consumes as terminal-success.

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func TestSkippedItemStatusFormSeedsAndSubmitsSkipped(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Skipped item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// The quick status gesture ("s").
	run(t, m, press(t, m, "s"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("'s' must open the status form")
	}
	if got := f.Values["status"]; got != "skipped" {
		t.Fatalf("status form Initial for a skipped item = %q, want \"skipped\" (not the pending fallback)", got)
	}

	run(t, m, submit(t, m, "priority"))
	if len(p.updated) == 0 {
		t.Fatal("submitting the status form must send an UpdateWorkItem")
	}
	req := p.updated[len(p.updated)-1]
	if req.GetStatus() != apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED {
		t.Fatalf("submitted status = %v, want WORK_ITEM_STATUS_SKIPPED", req.GetStatus())
	}

	// The same seed must hold in the full detail editor ("e"), not just the
	// quick-status shortcut.
	run(t, m, press(t, m, "e"))
	ef := m.ActiveForm()
	if ef == nil {
		t.Fatal("'e' must open the detail editor")
	}
	if got := ef.Values["status"]; got != "skipped" {
		t.Fatalf("detail editor status Initial for a skipped item = %q, want \"skipped\"", got)
	}
}
