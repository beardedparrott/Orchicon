package tui

// router.go — the global key dispatch layer (plan §6 decision 7).
//
// Routes are evaluated top-down: global chords first (Ctrl+O/W/E/A/F/T,
// help, quit), then screen-specific routes, then text-input fallthrough
// always last. The chat-dock feature registers new bindings here without
// touching screen code.

import (
	tea "github.com/charmbracelet/bubbletea"
)

// KeyRoute is one dispatch entry.
type KeyRoute struct {
	// Name is stable + human readable — the help overlay renders these.
	Name string
	// Keys describes the binding ("ctrl+o", "?", "enter").
	Keys string
	// Scope: "global", "screen", or "input".
	Scope string
	// Match decides whether this route handles the message.
	Match func(msg tea.Msg) bool
	// Handle applies the route. Return handled=false to fall through to
	// the next route layer.
	Handle func(m *App, msg tea.Msg) (handled bool)
}

// GlobalKeyRoutes returns the chord layer. Built from tabs so help and
// behavior can never drift.
func GlobalKeyRoutes(tabs []Tab) []KeyRoute {
	routes := []KeyRoute{
		{
			Name: "help overlay", Keys: "?", Scope: "global",
			Match: keyMatcher("?"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.help.open = !m.help.open
				return true
			},
		},
		{
			Name: "next tab", Keys: "right / tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && (k.String() == "right" || k.String() == "tab")
			},
			Handle: func(m *App, _ tea.Msg) bool { m.NextTab(); return true },
		},
		{
			Name: "previous tab", Keys: "left / shift+tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && (k.String() == "left" || k.String() == "shift+tab")
			},
			Handle: func(m *App, _ tea.Msg) bool { m.PrevTab(); return true },
		},
		{
			Name: "redraw / reconnect streams", Keys: "r", Scope: "global",
			Match: keyMatcher("r"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.reconnectStreams()
				return true
			},
		},
		{
			Name: "quit", Keys: "q / ctrl+c", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && (k.String() == "q" || k.String() == "ctrl+c")
			},
			Handle: func(m *App, _ tea.Msg) bool { m.quitting = true; return true },
		},
	}
	// One chord route per tab, in tab order: ctrl+o/w/e/a/f/t.
	for i := range tabs {
		tab := tabs[i]
		routes = append(routes, KeyRoute{
			Name:  "switch to " + tab.Title,
			Keys:  tab.Chord,
			Scope: "global",
			Match: keyMatcher(tab.Chord),
			Handle: func(m *App, _ tea.Msg) bool {
				m.SwitchTo(tab.ID)
				m.EnsureSubscriptions(tab.ID) // chord press re-arms streams
				return true
			},
		})
	}
	return routes
}

func keyMatcher(s string) func(tea.Msg) bool {
	return func(msg tea.Msg) bool {
		k, ok := msg.(tea.KeyMsg)
		return ok && k.String() == s
	}
}

// dispatch evaluates the route chain. Input routes registered by the
// active screen always sit last so chords fall through to text editing
// only when no global/screen route consumed the key.
func (m *App) dispatch(msg tea.Msg) (*App, tea.Cmd) {
	if wm, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = wm.Width, wm.Height
		if s := m.screens[m.active]; s != nil {
			s.SetSize(wm.Width, m.contentHeight())
		}
		m.footer.Width = wm.Width
		// First layout: start the active screen's live streams.
		m.EnsureSubscriptions(m.active)
		return m, nil
	}
	for _, r := range m.routes {
		if r.Match == nil || !r.Match(msg) {
			continue
		}
		if r.Handle(m, msg) {
			m.footer.StreamStatus = m.streamStatus()
			return m, nil
		}
	}
	// Defer to the active screen (its input routes are inside its own
	// Update — chords already consumed above).
	return m.passToScreen(msg)
}
