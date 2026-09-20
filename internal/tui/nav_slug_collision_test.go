package tui

// nav_slug_collision_test.go — no screen's list pane may be SILENTLY dropped from the tabs.
//
// THE BUG THIS PINS. buildNavEntries dedupes by slug:
//
//	cmd := slug(src.Name)
//	if seen[cmd] { continue }
//
// so the FIRST screen whose source slugs to a given word wins and every later screen's source
// is dropped WITHOUT A WORD. Tabs are iterated in order (… Work, Execution, Automation …), so a
// new source on an EARLIER tab silently removes an entry from a LATER tab's menu.
//
// That is exactly what happened: the Schedules pane registered its source as "schedules" on the
// Execution tab, and Automation's Recurring Items — also named "schedules" internally — vanished
// from the Automation menu. The operator found it ("What happened to recurring items in the
// TUI? It's gone."); nothing in the suite did.
//
// The fix for that instance was to give the Automation source the slug the GUI already uses for
// it ("recurring-items"). These tests are the general guard: every source must reach the menu,
// and the slugs must be unique across screens.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// collectSources walks every tab's screen and reports each source with the tab that owns it.
func collectSources(t *testing.T, m *App) map[string][]string {
	t.Helper()
	type sourcer interface{ Sources() []screenkit.SourceMeta }
	out := map[string][]string{}
	for _, tab := range Tabs {
		if tab.ID == TabAsk {
			continue // Ask's dropdown is verbs, not sources
		}
		s := m.screenForNav(tab.ID)
		if s == nil {
			continue
		}
		sr, ok := s.(sourcer)
		if !ok {
			continue
		}
		for _, src := range sr.Sources() {
			out[src.Name] = append(out[src.Name], string(tab.ID))
		}
	}
	return out
}

// NO TWO SCREENS MAY SHARE A SOURCE NAME.
//
// A duplicate is not merely confusing: the nav dedupe turns it into a missing menu entry on the
// later tab, which is invisible from the code and from the earlier tab.
func TestSourceNamesAreGloballyUnique(t *testing.T) {
	m := newTestApp()
	for name, tabs := range collectSources(t, m) {
		if len(tabs) > 1 {
			t.Errorf("source %q is registered by %v — the nav dedupe keys on the slug, so the "+
				"LATER tab's entry is silently dropped from its menu (this is how Recurring Items "+
				"disappeared when Execution added a \"schedules\" source)", name, tabs)
		}
	}
}

// EVERY SOURCE REACHES THE MENU, ON ITS OWN TAB.
//
// The dedupe is silent by construction, so this asserts the OUTCOME: the nav entry for a
// source must exist AND belong to the tab that declares it. Checking only that the slug exists
// somewhere would pass for a collision — the slug IS present, just owned by the other tab,
// which is precisely how Recurring Items vanished from Automation while "/schedules" still
// resolved.
func TestEveryScreenSourceHasANavEntry(t *testing.T) {
	m := newTestApp()
	entries := buildNavEntries(m)

	// Slug → the tab that actually claims it.
	slugTab := map[string]string{}
	for _, e := range entries {
		slugTab[e.Cmd] = string(e.Tab)
	}

	type sourcer interface{ Sources() []screenkit.SourceMeta }
	for _, tab := range Tabs {
		if tab.ID == TabAsk {
			continue
		}
		s := m.screenForNav(tab.ID)
		if s == nil {
			continue
		}
		sr, ok := s.(sourcer)
		if !ok {
			continue
		}
		for _, src := range sr.Sources() {
			sl := slug(src.Name)
			owner, ok := slugTab[sl]
			if !ok {
				t.Errorf("tab %q declares the source %q (%q) but it has NO nav entry — the "+
					"operator cannot reach that pane from the tab menu or by /%s",
					tab.ID, src.Name, src.Title, sl)
				continue
			}
			if owner != string(tab.ID) {
				t.Errorf("tab %q declares the source %q but /%s routes to tab %q — the slug was "+
					"claimed by an earlier tab, so THIS tab's menu entry was quietly dropped",
					tab.ID, src.Name, sl, owner)
			}
		}
	}
}

// The TWO sources this work touched, pinned by their GUI slugs.
//
// The GUI routes are /schedules (the schedules page) and /recurring-items (the recurring items
// page) — different subjects, different pages. The TUI's slugs must say the same thing, or the
// two clients cannot be reasoned about together.
func TestSchedulesAndRecurringItemsHaveDistinctSlugs(t *testing.T) {
	m := newTestApp()
	entries := buildNavEntries(m)

	want := map[string]string{
		"schedules":       string(TabExecution),
		"recurring-items": string(TabAutomation),
	}
	got := map[string]string{}
	for _, e := range entries {
		if _, ok := want[e.Cmd]; ok {
			got[e.Cmd] = string(e.Tab)
		}
	}
	for cmd, tab := range want {
		if got[cmd] != tab {
			t.Errorf("/%s routes to tab %q, want %q — the GUI's /%s lives under that area",
				cmd, got[cmd], tab, cmd)
		}
	}
}
