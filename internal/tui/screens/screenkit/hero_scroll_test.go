package screenkit

import (
	"context"
	"strings"
	"testing"
)

// Regression for the Ask launch state: noAutoDetail must keep the hero on
// screen — the fetch landing must NOT auto-select (and therefore
// auto-open) the first conversation, which is what made Ask default to an
// established chat instead of a fresh one.
func TestNoAutoDetailKeepsHero(t *testing.T) {
	b := &Base{}
	b.AddSource("conversations", "Conversations", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "c1", Title: "existing chat"}}, "", nil
	})
	b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
		return "detail of " + id, nil, "transcript body", nil
	})
	b.SetNoAutoDetail(true)
	b.SetHero("Ask Orchicon anything.", "Plan, execute, and govern with real-time clarity and thin control.")
	b.SetSize(100, 30)

	for _, c := range b.loadCmdsForTest() {
		b.Update(c())
	}
	if got := b.DetailID(); got != "" {
		t.Fatalf("noAutoDetail must not auto-select a conversation, got %q", got)
	}
	v := b.View()
	if !strings.Contains(v, "Ask Orchicon anything.") {
		t.Fatal("hero title missing from the detail pane")
	}
	if !strings.Contains(v, "Plan, execute") {
		t.Fatal("hero body missing from the detail pane")
	}

	// A deliberate open replaces the hero with the real detail.
	b.Update(b.RequestDetail("conversations", "c1")())
	if got := b.DetailID(); got != "c1" {
		t.Fatalf("DetailID = %q, want c1 after an explicit open", got)
	}
	if strings.Contains(b.View(), "Ask Orchicon anything.") {
		t.Fatal("hero must be replaced once real content lands")
	}

	// ClearDetail brings the hero back (the New chat affordance).
	b.ClearDetail()
	if got := b.DetailID(); got != "" {
		t.Fatalf("DetailID = %q after ClearDetail, want empty", got)
	}
	if !strings.Contains(b.View(), "Ask Orchicon anything.") {
		t.Fatal("ClearDetail must restore the hero")
	}
}

// Scrolling the detail must survive content updates: a live chunk appends
// to the body on every repaint, and the operator's scroll offset must not
// be yanked back to the top (mouse/arrow scrolling in a live conversation
// was effectively dead).
func TestDetailScrollSurvivesContentUpdate(t *testing.T) {
	d := &Detail{Width: 60, Height: 10}
	body := strings.Repeat("line\n", 200)
	d.SetContent("t", nil, body)
	_ = d.View() // first paint loads the viewport

	d.Wheel(-30) // scroll up into history
	before := d.vp.YOffset
	if before == 0 {
		t.Fatal("wheel up did not move the viewport offset")
	}
	// A live chunk arrives (body grows) — the offset must be preserved.
	d.SetContent("t", nil, body+"new chunk\n")
	_ = d.View()
	if got := d.vp.YOffset; got != before {
		t.Fatalf("scroll offset moved on content update: %d → %d", before, got)
	}
}
