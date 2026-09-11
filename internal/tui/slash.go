package tui

// slash.go — the slash command framework (plan §4): commands parse
// before send; unknown /word gives usage feedback instead of being sent
// as a chat message; `\/` escapes a literal slash. One registry, two
// frontdoors: the composer (any screen) and the help overlay (rendered
// FROM the registry so it cannot drift).

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// SlashCommand is one registered command.
type SlashCommand struct {
	Name    string // "/help" (with slash)
	Usage   string // "/wi <id>" — shown in /help
	Desc    string
	Aliases []string
	MinArgs int
	// Run executes the command; returned cmds are batched by the caller.
	Run func(m *App, args []string) tea.Cmd
}

// NoticeOnly marks commands that only surface a notice (GUI-mirror
// panes with no TUI surface).
func (c SlashCommand) NoticeOnly() bool { return c.Usage == "" }

// slashRegistry is the ordered command set (built once per App).
type slashRegistry struct {
	byName map[string]*SlashCommand
	names  []string // primary names, sorted for /help
}

// ParseSlash parses composer input: "/word ..." at buffer start →
// (name, args). Returns ok=false when the text is not a command (plain
// chat). A leading backslash before the slash (`\/…`) escapes the
// slash — the text is sent literally with the backslash stripped.
func ParseSlash(text string) (name string, args []string, escaped bool, ok bool) {
	if strings.HasPrefix(text, "\\/") {
		return "", nil, true, false // literal slash message, backslash stripped by caller
	}
	if !strings.HasPrefix(text, "/") {
		return "", nil, false, false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 || fields[0] == "/" {
		return "", nil, false, false // bare "/" is not a command
	}
	return fields[0], fields[1:], false, true
}

// buildSlashRegistry assembles the full command set: nav-generated
// entries (from the screens' Sources() — no drift from real screens),
// hand-listed GUI-mirror entries, arg jumps, aliases, and system
// commands.
func buildSlashRegistry(m *App) *slashRegistry {
	reg := &slashRegistry{byName: map[string]*SlashCommand{}}

	add := func(c SlashCommand) {
		cp := c
		reg.byName[c.Name] = &cp
		for _, a := range c.Aliases {
			reg.byName[a] = &cp
		}
		reg.names = append(reg.names, c.Name)
	}

	// 1. Tab commands (the seven top-level areas).
	for _, tab := range Tabs {
		add(SlashCommand{
			Name:  "/" + string(tab.ID),
			Usage: "/" + string(tab.ID),
			Desc:  "switch to the " + tab.Title + " area",
			Run: func(m *App, _ []string) tea.Cmd {
				m.SwitchTo(tab.ID)
				m.EnsureSubscriptions(tab.ID)
				return nil
			},
		})
	}

	// 2. Entity commands generated from the screens' Sources() — every
	// TUI list pane gets one, so the set cannot drift from real screens.
	nav := buildNavEntries(m)
	for _, e := range nav {
		eCopy := e
		add(SlashCommand{
			Name:  "/" + e.Cmd,
			Usage: "/" + e.Cmd + e.ArgsHint,
			Desc:  "open " + e.Label,
			Run: func(m *App, args []string) tea.Cmd {
				m.SwitchTo(eCopy.Tab)
				m.EnsureSubscriptions(eCopy.Tab)
				if eCopy.Source != "" {
					m.selectScreenSource(eCopy.Tab, eCopy.Source)
				}
				if len(args) > 0 && eCopy.Source != "" {
					m.selectScreenItem(eCopy.Tab, eCopy.Source, args[0])
				}
				return nil
			},
		})
	}

	// 3. GUI-mirror notice-only panes were removed (QA finding 4). The
	// registry now builds commands only from real screens + system commands;
	// no command surfaces a "use the web GUI" notice. GUI panes without a
	// TUI source are documented in docs/tui-parity.md as child work items.

	// 4. Arg-jump aliases (navigate straight to the detail view).
	jump := func(name, alias, usage string) {
		base := reg.byName[name]
		if base == nil {
			return
		}
		add(SlashCommand{
			Name: alias, Usage: usage, Desc: "jump to " + strings.TrimPrefix(name, "/") + " detail",
			Run: base.Run,
		})
	}
	jump("/work-items", "/wi", "/wi <id>")
	jump("/executions", "/exec", "/exec <id>")
	jump("/runs", "/run", "/run <id>")
	jump("/workers", "/worker", "/worker <id>")

	// 5. System commands.
	add(SlashCommand{
		Name: "/help", Usage: "/help",
		Desc: "list all commands with usage",
		Run: func(m *App, _ []string) tea.Cmd {
			m.dock.SetNotice(m.slashHelpText())
			return nil
		},
	})
	add(SlashCommand{
		Name: "/connect", Usage: "/connect",
		Desc: "reopen the connection/auth screen",
		Run: func(m *App, _ []string) tea.Cmd {
			// In-place overlay: NEVER exits the process or tears down
			// alt-screen. The main.go reconnect loop remains only the first-run
			// fallback (no profile yet).
			m.reconnectRequested = true
			return m.runConnectCommand()
		},
	})
	add(SlashCommand{
		Name: "/diff", Usage: "/diff",
		Desc: "toggle the left diff rail for the active execution/conversation",
		Run: func(m *App, _ []string) tea.Cmd {
			if m.diffOpen {
				m.closeDiffPane()
				m.dock.SetNotice("diff rail closed (d or /diff toggles)")
				return nil
			}
			if kind, id := m.diffOwner(); kind == diffs.NoneOwner || id == diffs.NoneOwner {
				m.dock.SetNotice("no diff session here — open an execution or conversation first")
				return nil
			}
			cmd := m.openDiffPane()
			m.dock.SetNotice("diff rail open (d or /diff toggles; ctrl+d is never bound)")
			return cmd
		},
	})
	add(SlashCommand{
		Name: "/reconnect", Usage: "/reconnect",
		Desc: "redial every live stream",
		Run: func(m *App, _ []string) tea.Cmd {
			m.reconnectStreams()
			m.dock.SetNotice("live streams redialed")
			return nil
		},
	})
	add(SlashCommand{
		Name: "/context", Usage: "/context [pin <description> | pin clear]",
		Desc:    "show the context injected with each message; pin to override",
		MinArgs: 0,
		Run:     runContextCmd,
	})
	add(SlashCommand{
		Name:  "/theme", Usage: "/theme [dark | light]",
		Desc:  "switch the TUI theme (no arg = list); persisted to the profile",
		Run:   runThemeCmd,
	})
	add(SlashCommand{
		Name: "/quit", Usage: "/quit",
		Desc:    "exit orch (terminal state restored)",
		Aliases: []string{"/exit"},
		Run: func(m *App, _ []string) tea.Cmd {
			m.quitting = true
			return tea.Quit
		},
	})

	sort.Strings(reg.names)
	reg.names = dedupe(reg.names)
	return reg
}

func dedupe(in []string) []string {
	out := in[:0]
	var prev string
	for _, s := range in {
		if s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}

// resolve looks a command name (with slash) or alias up.
func (r *slashRegistry) resolve(name string) *SlashCommand { return r.byName[name] }

// helpLines renders /help FROM the registry (no-drift with behavior).
func (r *slashRegistry) helpLines() []string {
	lines := []string{theme.ListTitle.Render("commands")}
	for _, n := range r.names {
		c := r.byName[n]
		if n != c.Name {
			continue // alias: rendered with its primary
		}
		usage := c.Usage
		if usage == "" {
			usage = c.Name
		}
		alias := ""
		for _, a := range c.Aliases {
			alias += " " + a
		}
		lines = append(lines, fmt.Sprintf("  %-28s %s%s", usage, c.Desc, aliasSuffix(alias)))
	}
	return lines
}

func aliasSuffix(alias string) string {
	if alias == "" {
		return ""
	}
	return "  (alias:" + alias + ")"
}

// slashHelpText joins help lines for the dock notice strip.
func (m *App) slashHelpText() string {
	return strings.Join(m.slash.helpLines(), " · ")
}

// dispatchSlash parses and runs a slash command from the composer.
// Returns (handled, cmd): handled=false means the text is a chat
// message (or an escaped literal) and should be sent.
func (m *App) dispatchSlash(text string) (bool, tea.Cmd) {
	name, args, escaped, ok := ParseSlash(text)
	if escaped {
		// literal slash: strip the escape backslash, send as chat
		return false, nil
	}
	if !ok {
		return false, nil
	}
	cmd := m.slash.resolve(name)
	if cmd == nil {
		m.dock.SetError(fmt.Sprintf("unknown command %s — /help lists commands (escape a literal slash as \\/)", name))
		return true, nil
	}
	if len(args) < cmd.MinArgs {
		m.dock.SetError(fmt.Sprintf("usage: %s", cmd.Usage))
		return true, nil
	}
	m.dock.SetError("")
	m.dock.SetNotice("✓ " + strings.TrimPrefix(name, "/"))
	return true, cmd.Run(m, args)
}

// runContextCmd shows the current injected context, or pins an override
// (`/context pin <desc>` / `/context pin clear`).
func runContextCmd(m *App, args []string) tea.Cmd {
	if len(args) == 0 {
		line := "context: " + m.contextPreambleLabel()
		if m.contextOverride != "" {
			line += " (pinned)"
		}
		m.dock.SetNotice(line)
		return nil
	}
	if args[0] == "pin" && len(args) >= 2 {
		desc := strings.Join(args[1:], " ")
		if desc == "clear" {
			m.contextOverride = ""
			m.dock.SetNotice("context override cleared — auto context resumes")
			return nil
		}
		m.contextOverride = desc
		m.dock.SetNotice("context pinned to: " + desc)
		return nil
	}
	m.dock.SetError("usage: /context [pin <description> | pin clear]")
	return nil
}

// runThemeCmd switches the TUI theme ("/theme" lists, "/theme <name>"
// switches + persists to the profile). Persisting keeps the selection
// across launches; the profile is saved best-effort at the config path.
func runThemeCmd(m *App, args []string) tea.Cmd {
	if len(args) == 0 {
		names := make([]string, 0, len(m.themes))
		for _, n := range m.themes {
			marker := " "
			if n == theme.Active().Name {
				marker = "*"
			}
			names = append(names, marker+" "+n)
		}
		m.dock.SetNotice("themes: " + strings.Join(names, " · ") + "  (/theme <name> switches)")
		return nil
	}
	name := args[0]
	if !theme.Use(name) {
		known := strings.Join(m.themes, ", ")
		m.dock.SetError(fmt.Sprintf("unknown theme %q — available: %s", name, known))
		return nil
	}
	if m.profile != nil {
		m.profile.Theme = name
		if path, err := config.DefaultPath(); err == nil {
			if cfg, err := config.Load(path); err == nil {
				if p := cfg.Profiles[cfg.Active]; p != nil {
					p.Theme = name
					_ = config.Save(path, cfg)
				} else if cfg.Profiles != nil {
					_ = cfg
				}
			}
		}
	}
	m.dock.SetNotice("theme: " + name)
	return nil
}
