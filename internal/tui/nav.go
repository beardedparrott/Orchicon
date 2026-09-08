package tui

// nav.go — the nav slash-command model: generated from the screens' own
// Sources() (the no-drift source of truth) plus hand-listed GUI-mirror
// entries for panes that only exist in the web GUI (nav-config.ts).
// Every TUI screen and submenu gets a command; the acceptance test
// asserts parity between this set and the slash registry.

import (
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// NavEntry is one navigation command.
type NavEntry struct {
	Cmd      string // "work-items" (no slash)
	Label    string // human title
	Tab      TabID
	Source   string // screenkit source name ("" = tab-level only)
	ArgsHint string // " <id>" when the command takes an arg jump
}

// buildNavEntries constructs each area screen (constructors are pure
// wiring — no RPC in New()) and reads its Sources(), producing one
// command per list pane.
func buildNavEntries(m *App) []NavEntry {
	// Deterministic per-tab hand-ordering: the shell builds factories by
	// tab; Sources() order per screen is constructor order, which we
	// keep.
	var out []NavEntry
	seen := map[string]bool{}
	add := func(tab TabID, s Screen) {
		type sourcer interface{ Sources() []screenkit.SourceMeta }
		sr, ok := s.(sourcer)
		if !ok {
			return
		}
		for _, src := range sr.Sources() {
			cmd := slug(src.Name)
			if seen[cmd] {
				continue
			}
			seen[cmd] = true
			out = append(out, NavEntry{
				Cmd:    cmd,
				Label:  src.Title,
				Tab:    tab,
				Source: src.Name,
			})
		}
	}
	for _, tab := range Tabs {
		s := m.screenForNav(tab.ID)
		if s != nil {
			add(tab.ID, s)
		}
	}
	return out
}

// screenForNav returns (constructing if needed) the tab's screen for
// introspection. Screens built here are disposed of by SwitchTo when the
// user actually navigates.
func (m *App) screenForNav(tab TabID) Screen {
	if s, ok := m.screens[tab]; ok && s != nil {
		return s
	}
	f, ok := m.factories[tab]
	if !ok {
		return nil
	}
	s := f()
	m.screens[tab] = s // cache like a normal visit
	return s
}

// slug maps a source name to a command word ("workitems" →
// "work-items", "images" → "runtime-images").
var slugMap = map[string]string{
	"workitems":     "work-items",
	"images":        "runtime-images",
	"runs":          "runs",
	"executions":    "executions",
	"conversations": "conversations",
}

func slug(name string) string {
	if s, ok := slugMap[name]; ok {
		return s
	}
	return name
}

// guiMirrorEntries lists nav-config.ts panes with no TUI surface: the
// command switches to the owning tab and surfaces a "use the GUI"
// notice (plan fact 7). /settings maps to Control (its sources are the
// settings-adjacent surfaces).
func guiMirrorEntries() []NavEntry {
	return []NavEntry{
		{Cmd: "settings", Label: "Settings", Tab: TabControl},
		{Cmd: "providers", Label: "Providers", Tab: TabControl},
		{Cmd: "webhooks", Label: "Webhooks", Tab: TabControl},
		{Cmd: "adapters", Label: "Adapters", Tab: TabControl},
		{Cmd: "admin", Label: "Admin", Tab: TabControl},
		{Cmd: "telemetry", Label: "Telemetry", Tab: TabControl},
		{Cmd: "cost-explorer", Label: "Cost Explorer", Tab: TabControl},
		{Cmd: "dashboard", Label: "Dashboard", Tab: TabControl},
		{Cmd: "recurring-items", Label: "Recurring Items", Tab: TabAutomation},
		{Cmd: "idea-cloud", Label: "Idea Cloud", Tab: TabAutomation},
		{Cmd: "recovery", Label: "Recovery", Tab: TabEnforcement},
	}
}
