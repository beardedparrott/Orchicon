package screenkit

import (
	"context"
	"strings"
	"testing"
)

// Phase-3.5 finding 4: the Control screen's 7 sources must not push the
// detail off-screen. The single-pane layout (strip + active pane +
// detail) guarantees the detail keeps >= 20 columns at 60/90/120 wide,
// and strip clicks switch the list pane.
func TestStripLayoutShowsDetail(t *testing.T) {
	b := &Base{}
	for _, s := range []struct{ name, title string }{
		{"workers", "Workers"}, {"images", "Runtime Images"}, {"secrets", "Secrets"},
		{"mcp", "MCP Servers"}, {"providers", "Providers"}, {"webhooks", "Webhooks"},
		{"settings", "Settings"},
	} {
		name := s.name
		b.AddSource(s.name, s.title, func(ctx context.Context, pageToken string) ([]Item, string, error) {
			return []Item{{ID: name + "-1", Title: name + " item"}}, "", nil
		})
	}
	for _, w := range []int{60, 90, 120} {
		b.SetSize(w, 30)
		if b.stripH != 1 {
			t.Fatalf("w=%d: stripH = %d, want 1 (7 sources)", w, b.stripH)
		}
		if got := b.detail.Width; got < 20 {
			t.Fatalf("w=%d: detail width = %d, want >= 20 (detail pushed off-screen)", w, got)
		}
		for _, c := range b.loadCmdsForTest() {
			msg := c()
			if handled, _ := b.Update(msg); !handled {
				t.Fatalf("w=%d: fetchedMsg not handled", w)
			}
		}
		found := false
		for i := range b.sources {
			if b.sources[i].name == "settings" {
				b.active = i
				found = true
			}
		}
		if !found {
			t.Fatal("settings source missing")
		}
		v := b.View()
		if !strings.Contains(v, "Settings") {
			t.Fatalf("w=%d: strip missing Settings title", w)
		}
		if idx := b.stripSourceAt(2); idx != 0 {
			t.Fatalf("w=%d: strip click x=2 = %d, want 0 (Workers)", w, idx)
		}
	}
	b2 := &Base{}
	b2.AddSource("conversations", "Conversations", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return nil, "", nil
	})
	b2.SetSize(90, 30)
	if b2.stripH != 0 {
		t.Fatalf("single source: stripH = %d, want 0", b2.stripH)
	}
}
