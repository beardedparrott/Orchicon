// Package tui is orch's app shell: the six-tab shell (Ask, Work,
// Execution, Automation, Enforcement, Control) mirroring the GUI nav
// (frontend/src/lib/nav-config.ts + app-shell.tsx), with the global key
// router, status footer, and help overlay.
package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/automation"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/screens/control"
	"github.com/beardedparrott/orchicon/internal/tui/screens/enforcement"
	"github.com/beardedparrott/orchicon/internal/tui/screens/execution"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
	"github.com/beardedparrott/orchicon/internal/version"
)

// TabID is a stable screen identifier.
type TabID string

// The six GUI nav domains.
const (
	TabAsk         TabID = "ask"
	TabWork        TabID = "work"
	TabExecution   TabID = "execution"
	TabAutomation  TabID = "automation"
	TabEnforcement TabID = "enforcement"
	TabControl     TabID = "control"
)

// Tab is one shell tab (GUI nav domain).
type Tab struct {
	ID    TabID
	Title string // GUI nav label
	Chord string // ctrl+<key>
}

// Tabs is the top tab bar, in GUI nav order.
var Tabs = []Tab{
	{TabAsk, "Ask Orchicon", "ctrl+o"},
	{TabWork, "Work", "ctrl+w"},
	{TabExecution, "Execution", "ctrl+e"},
	{TabAutomation, "Automation", "ctrl+a"},
	{TabEnforcement, "Enforcement", "ctrl+f"},
	{TabControl, "Control", "ctrl+t"},
}

// Screen is the contract every area screen implements (alias of the
// screenkit interface so shell code stays short).
type Screen = screenkit.Screen

// App is the root model.
type App struct {
	clients  *client.Clients
	profile  *config.Profile
	reg      *subs.Registry
	screens  map[TabID]Screen
	factories map[TabID]func() Screen
	active   TabID
	width    int
	height   int
	footer   footerModel
	help     helpModel
	routes   []KeyRoute
	quitting bool
}

// NewApp builds the shell over an established client set.
func NewApp(cl *client.Clients, profile *config.Profile, serverVersion string) *App {
	m := &App{
		clients: cl,
		profile: profile,
		reg:     subs.NewRegistry(),
		screens: map[TabID]Screen{},
		footer: footerModel{
			URL:           profile.URL,
			ServerVersion: serverVersion,
			ClientVersion: version.Current().Tag,
		},
	}
	m.routes = GlobalKeyRoutes(Tabs)
	// One factory per tab — screens construct lazily on first visit so
	// stream subscriptions only exist while their tab is active.
	m.factories = map[TabID]func() Screen{
		TabAsk:         func() Screen { return ask.New(cl, m.reg) },
		TabWork:        func() Screen { return work.New(cl, m.reg) },
		TabExecution:   func() Screen { return execution.New(cl, m.reg) },
		TabAutomation:  func() Screen { return automation.New(cl, m.reg) },
		TabEnforcement: func() Screen { return enforcement.New(cl, m.reg) },
		TabControl:     func() Screen { return control.New(cl, m.reg) },
	}
	return m
}

// SetIdentity sets the footer identity (display name / subject).
func (m *App) SetIdentity(name string) { m.footer.Identity = name }

// RegisterScreen installs a screen for a tab (tests / custom wiring;
// overrides the factory).
func (m *App) RegisterScreen(id TabID, s Screen) {
	m.screens[id] = s
	if m.active == "" {
		m.active = id
	}
}

// SwitchTo activates a tab (lazily constructing its screen; the previous
// screen is closed = unsubscribed, useStream semantics).
func (m *App) SwitchTo(id TabID) {
	if m.active == id {
		return
	}
	if old, ok := m.screens[m.active]; ok && old != nil {
		old.Close()
	}
	m.active = id
	if _, ok := m.screens[id]; !ok {
		if f, ok := m.factories[id]; ok {
			s := f()
			if m.width > 0 {
				s.SetSize(m.width, m.contentHeight())
			}
			m.screens[id] = s
		}
	}
	m.updateContextChip()
}

// ActiveTab returns the active tab ID.
func (m *App) ActiveTab() TabID { return m.active }

// NextTab / PrevTab cycle the tab bar.
func (m *App) NextTab() { m.cycle(1) }
func (m *App) PrevTab() { m.cycle(-1) }

func (m *App) cycle(delta int) {
	idx := 0
	for i, t := range Tabs {
		if t.ID == m.active {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(Tabs)) % len(Tabs)
	m.SwitchTo(Tabs[idx].ID)
}

func (m *App) contentHeight() int {
	// tab bar (1) + blank (1) + footer (1)
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

// reconnectStreams forces every live subscription to redial now.
func (m *App) reconnectStreams() { m.reg.ReconnectAll() }

func (m *App) updateContextChip() {
	if s := m.screens[m.active]; s != nil {
		if c, ok := s.(interface{ ContextChip() string }); ok {
			m.footer.ContextChip = c.ContextChip()
			return
		}
	}
	m.footer.ContextChip = ""
}

// streamStatus derives the footer state from the active screen's
// reported subscription statuses (worst wins).
func (m *App) streamStatus() streamStatusString {
	s := m.screens[m.active]
	if s == nil {
		return openStatus
	}
	rp, ok := s.(screenkit.StatusReporter)
	if !ok {
		return openStatus
	}
	worst := openStatus
	for _, st := range rp.ReportStatus() {
		if statusRank(streamStatusString(st.Status)) > statusRank(worst) {
			worst = streamStatusString(st.Status)
		}
	}
	return worst
}

// Init implements tea.Model.
func (m *App) Init() tea.Cmd {
	var cmds []tea.Cmd
	if s := m.screens[m.active]; s != nil {
		cmds = append(cmds, s.Init())
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model (via the router dispatch).
func (m App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.dispatch(msg)
}

// passToScreen forwards to the active screen.
func (m *App) passToScreen(msg tea.Msg) (*App, tea.Cmd) {
	if m.help.open {
		if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "?") {
			m.help.open = false
			return m, nil
		}
		return m, nil // overlay swallows keys
	}
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		// handled in dispatch (App-level sizing)
		return m, nil
	}
	s := m.screens[m.active]
	if s == nil {
		return m, nil
	}
	ns, cmd := s.Update(msg)
	m.screens[m.active] = ns
	m.updateContextChip()
	m.footer.StreamStatus = m.streamStatus()
	m.footer.Width = m.width
	return m, cmd
}

// View implements tea.Model.
func (m App) View() string {
	if m.quitting {
		return ""
	}
	if m.help.open {
		overlay := m.help.view(m.routes)
		return lipglossPlace(m.width, m.height, overlay)
	}
	var b strings.Builder
	b.WriteString(m.tabBarView())
	b.WriteString("\n")
	if s := m.screens[m.active]; s != nil {
		b.WriteString(s.View())
	} else {
		b.WriteString(theme.HintText.Render("select an area"))
	}
	b.WriteString("\n")
	b.WriteString(m.footer.View())
	return b.String()
}

// tabBarView renders the top tab bar with active highlight.
func (m App) tabBarView() string {
	parts := make([]string, len(Tabs))
	for i, t := range Tabs {
		label := t.Title
		if t.ID == m.active {
			parts[i] = theme.TabActive.Render(label)
		} else {
			parts[i] = theme.TabInactive.Render(label)
		}
	}
	return theme.TabBar.Render(strings.Join(parts, " "))
}

// Footer returns footer internals for tests.
func (m App) Footer() footerModel { return m.footer }

// StreamStatus exposes the footer's stream state.
func (m App) StreamStatus() streamStatusString { return m.streamStatus() }
