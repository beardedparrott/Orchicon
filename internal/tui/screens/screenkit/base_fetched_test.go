package screenkit

import (
	"context"
	"testing"
)

// Phase-3.5 finding 4 support: the fetchedMsg → List population path.
// The Control screen renders "nothing here" for every pane when this
// wiring breaks, with NO error state (the operator's screenshot) — so
// this pins that a successful fetch lands its items in the right
// source's list and clears Loading.
func TestBaseFetchedMsgPopulatesList(t *testing.T) {
	b := &Base{}
	b.AddSource("workers", "Workers", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "w1", Title: "Quick Software Engineer", Meta: "published"}}, "", nil
	})
	b.AddSource("settings", "Settings", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "tenant-settings", Title: "Tenant Settings"}}, "", nil
	})
	b.SetSize(100, 30)
	for _, c := range b.loadCmdsForTest() {
		msg := c()
		if handled, _ := b.Update(msg); !handled {
			t.Fatalf("fetchedMsg not handled: %T", msg)
		}
	}
	w := b.SourceListForTest("workers")
	if w == nil || len(w.Items) != 1 || w.Items[0].Title != "Quick Software Engineer" {
		t.Fatalf("workers list did not populate: %+v", w)
	}
	if w.Loading {
		t.Fatal("workers list still Loading after fetch")
	}
	s := b.SourceListForTest("settings")
	if s == nil || len(s.Items) != 1 {
		t.Fatalf("settings list did not populate: %+v", s)
	}
}
