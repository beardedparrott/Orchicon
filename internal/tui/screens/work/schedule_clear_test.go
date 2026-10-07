package work

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestClearingTheScheduleSendsAnExplicitClear is the operator's item 1:
//
//	"There is no way to clear a schedule on a work item in the TUI currently in edit mode. There
//	 should be a clear key to easily empty out a field."
//
// scheduled_start_at is optional, so an EMPTY field meant "unchanged" and the form seeded it from the
// item — the stored schedule round-tripped on every save and could never be removed. Emptying the
// field must now send clear_scheduled_start_at; leaving it alone must round-trip the VALUE.
func TestClearingTheScheduleSendsAnExplicitClear(t *testing.T) {
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	// (a) A scheduled item, field EMPTIED → explicit clear, no value.
	m, f, p := openItemEditorP(t, &start)
	f.Set("scheduled_start", "")
	run(t, m, submit(t, m, "scheduled_start"))
	if len(p.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p.updated), f.SubmitErr)
	}
	req := p.updated[0]
	if !req.GetClearScheduledStartAt() {
		t.Fatal("emptying Scheduled start must send clear_scheduled_start_at — without it the field " +
			"is indistinguishable from 'unchanged' and the schedule can never be removed")
	}
	if req.GetScheduledStartAt() != nil {
		t.Fatalf("a cleared schedule must not also carry a value, got %v", req.GetScheduledStartAt())
	}

	// (b) Field LEFT ALONE → the seeded value round-trips, no clear.
	m2, f2, p2 := openItemEditorP(t, &start)
	if got := f2.Values["scheduled_start"]; got == "" {
		t.Fatal("fixture: the editor must SEED the stored schedule, or saving would clear it unwatched")
	}
	run(t, m2, submit(t, m2, "scheduled_start"))
	if len(p2.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p2.updated), f2.SubmitErr)
	}
	req2 := p2.updated[0]
	if req2.GetClearScheduledStartAt() {
		t.Fatal("an untouched schedule must NOT be cleared — the seeded value must round-trip as-is")
	}
	if req2.GetScheduledStartAt() == nil {
		t.Fatal("an untouched schedule must round-trip its VALUE")
	}
}

// TestAutoStartBoxOpensUncheckedEvenWhenTheStoredFlagIsSet is the CLIENT half of the operator's safety
// rule:
//
//	"We have to be careful though to not fire anything off that already has auto or schedule set. It
//	 must be done as an action when saving the record only."
//
// The server refuses to fire on the stored flag alone. If the form SEEDED the box from
// auto_start_workflow, opening a legacy/armed item and saving an unrelated edit (a rename) would send
// auto_start=true — manufacturing the explicit gesture the operator did not make, and firing the run
// the server was careful not to.
func TestAutoStartBoxOpensUncheckedEvenWhenTheStoredFlagIsSet(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	truthy := true
	p.addItem(&apiv1.WorkItem{
		Id:                "wi-armed",
		Title:             "Legacy armed item",
		Kind:              apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId:         "proj-1",
		Status:            apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		AutoStartWorkflow: &truthy, // the stale stored flag
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}

	if got := f.Values["auto_start"]; got != "false" {
		t.Fatalf("the auto-start box opened as %q on an item whose STORED flag is true — saving any "+
			"unrelated edit would then fire a run the operator never asked for", got)
	}

	// Saving an unrelated field must therefore send auto_start=false, not true.
	f.Set("title", "renamed only")
	run(t, m, submit(t, m, "title"))
	if len(p.updated) != 1 {
		t.Fatalf("expected one update, got %d (SubmitErr=%q)", len(p.updated), f.SubmitErr)
	}
	if p.updated[0].GetAutoStartWorkflow() {
		t.Fatal("a title-only save sent auto_start=true — the stored flag leaked into an explicit " +
			"gesture, which is exactly the 'don't fire what already has auto set' hazard")
	}
}

// TestCtrlUClearsTheScheduledStartField is the kit2 half of item 1, driven through the real form: the
// clear KEY must actually work on a reference field. ctrl+u required `editable`, which is false for
// KDateTime, so it was a silent no-op on the very field the operator could not clear — while the
// footer advertised "ctrl+u: clear" the whole time.
func TestCtrlUClearsTheScheduledStartField(t *testing.T) {
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	_, f, _ := openItemEditorP(t, &start)
	if f.Values["scheduled_start"] == "" {
		t.Fatal("fixture: the field must be seeded")
	}
	if !f.FocusName("scheduled_start") {
		t.Fatal("fixture: could not focus the scheduled-start field")
	}
	_, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if !handled {
		t.Fatal("ctrl+u must be consumed by the form")
	}
	if got := f.Values["scheduled_start"]; got != "" {
		t.Fatalf("ctrl+u left the scheduled-start value as %q — the clear key is a no-op on a reference "+
			"field, which is the operator's 'there is no way to clear a schedule'", got)
	}
}

// openItemEditorP is openItemEditor plus the fake plane, so a test can inspect the request the form
// built (the existing helper drops it).
func openItemEditorP(t *testing.T, start *time.Time) (*Model, *kit2.Form, *fakePlane) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	item := &apiv1.WorkItem{
		Id:        "wi-1",
		Title:     "Item",
		Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1",
		Status:    apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	}
	if start != nil {
		item.ScheduledStartAt = timestamppb.New(*start)
	}
	p.addItem(item)
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	return m, f, p
}
