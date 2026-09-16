package tui

// slash.go — the slash command framework (plan §4): commands parse
// before send; unknown /word gives usage feedback instead of being sent
// as a chat message; `\/` escapes a literal slash. One registry, two
// frontdoors: the composer (any screen) and the help overlay (rendered
// FROM the registry so it cannot drift).

import (
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
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

// MatchesQuery reports whether a typed slash word (WITHOUT the leading slash) names this
// command — by its primary spelling OR any ALIAS.
//
// Aliases are not decoration: "/exit" resolves and RUNS, so a palette that cannot find it is
// telling the operator a working command does not exist. That was the reported inconsistency
// — "/exit works but doesn't get displayed in the slash command list as a valid slash
// command" — and it happened because the filter matched `c.Name` alone.
func (c SlashCommand) MatchesQuery(q string) bool {
	if strings.Contains(c.Name, q) {
		return true
	}
	for _, a := range c.Aliases {
		if strings.Contains(a, q) {
			return true
		}
	}
	return false
}

// AliasLabel renders the aliases for display, in one shared form so /help and the palette
// cannot drift apart — the exact drift that produced this report (helpLines showed the alias,
// the palette row did not).
func (c SlashCommand) AliasLabel() string {
	if len(c.Aliases) == 0 {
		return ""
	}
	alias := ""
	for _, a := range c.Aliases {
		alias += " " + a
	}
	return aliasSuffix(alias)
}

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
				// A VERB row (Ask's New / Conversations) runs its action instead of
				// focusing a source — otherwise /conversations would switch tabs
				// and focus a source the Ask screen never renders (its list is the
				// right rail).
				if eCopy.Action != nil {
					return eCopy.Action(m)
				}
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
		Name: "/new", Usage: "/new",
		Desc: "start a new Ask Orchicon conversation",
		Run: func(m *App, _ []string) tea.Cmd {
			m.newChat()
			m.dock.SetNotice("new conversation — type below to start")
			return nil
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
		Name: "/theme", Usage: "/theme [dark | light]",
		Desc: "switch the TUI theme (no arg = list); persisted to the profile",
		Run:  runThemeCmd,
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

	// New chat / conversation management / ask-model + mode (parity with the
	// GUI's Ask Orchicon surface). All writes go through the chat controller
	// (the shell's single Ask write path).
	add(SlashCommand{
		Name: "/new", Usage: "/new",
		Desc: "start a new conversation (the first send creates it)",
		Run: func(m *App, _ []string) tea.Cmd {
			m.newChat()
			m.dock.SetNotice("new chat — the next message starts a fresh conversation")
			return nil
		},
	})
	// /rename OPENS A PREFILLED FORM with no arguments, and writes directly with one.
	//
	// The direct form was the only way in, which is the operator's report — "We can't rename
	// conversations in the TUI" — because a command you must know, that cannot show you the current
	// title, is not a rename UI. With no arguments it now opens the same modal `ctrl+n` opens, so the
	// palette (the documented command surface) is a real route to it, and the current title is there
	// to edit rather than retype.
	add(SlashCommand{
		Name: "/rename", Usage: "/rename [title]",
		Desc: "rename the open conversation — with no title, opens a prefilled box (UpdateConversationTitle)",
		Run: func(m *App, args []string) tea.Cmd {
			id := m.chatConvID
			if id == "" {
				// The open conversation is the target by default, but if the operator has only the
				// RAIL loaded (a conversation selected, none "open"), that selection is what they are
				// looking at — use it rather than refusing.
				if m.railVisible() && m.active == TabAsk && m.convSel >= 0 && m.convSel < len(m.railRows()) {
					id = m.conversations[m.railConvIndexAt(m.convSel)].ID
				}
			}
			if id == "" {
				m.dock.SetError("no conversation open — /new or pick one from the rail")
				return nil
			}
			if len(args) == 0 {
				m.openRenameConversation(id, m.conversationTitle(id))
				return nil
			}
			title := strings.Join(args, " ")
			m.dock.SetNotice("renaming to " + title)
			return m.chat.RenameConversation(id, title)
		},
	})
	add(SlashCommand{
		Name: "/delete", Usage: "/delete",
		Desc: "delete the open conversation (DeleteConversation)",
		Run: func(m *App, _ []string) tea.Cmd {
			if m.chatConvID == "" {
				m.dock.SetError("no conversation open — /new or pick one from the rail")
				return nil
			}
			id := m.chatConvID
			m.dock.SetNotice("conversation deleted")
			return m.chat.DeleteConversation(id)
		},
	})
	add(SlashCommand{
		Name: "/model", Usage: "/model <model_ref>",
		Desc:    "set the Ask model new conversations are created with (persists to the conversation row's model_ref)",
		MinArgs: 1,
		Run: func(m *App, args []string) tea.Cmd {
			ref := strings.TrimSpace(strings.Join(args, " "))
			if ref == "" {
				m.dock.SetError("usage: /model <model_ref>")
				return nil
			}
			m.chat.SetPendingModel(ref)
			m.dock.SetNotice("ask model: " + ref + " (applies to the next new conversation; /new then send)")
			return nil
		},
	})
	add(SlashCommand{
		Name: "/models", Usage: "/models",
		Desc: "choose the Ask model (adapter → provider → model); sets it on the open conversation",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.openModelsPicker()
		},
	})
	add(SlashCommand{
		Name: "/mode", Usage: "/mode [brainstorm]",
		Desc:    "set the conversation mode/persona (SetConversationMode); no argument reports the current one",
		MinArgs: 0,
		Run: func(m *App, args []string) tea.Cmd {
			if len(args) == 0 {
				// The composer's stat row carries the mode pill (the TUI's
				// counterpart to the GUI's dropdown), so a bare /mode REPORTS
				// rather than acting blind.
				m.dock.SetNotice("mode: " + m.currentModeLabel() + "  (known: brainstorm — /mode brainstorm to set)")
				return nil
			}
			mode, ok := chat.ParseMode(args[0])
			if !ok {
				m.dock.SetError("unknown mode " + args[0] + " — known: brainstorm")
				return nil
			}
			m.chat.SetPendingMode(mode)
			if m.chatConvID == "" {
				m.dock.SetNotice("mode " + strings.ToLower(args[0]) + " applies to the next new conversation")
				return nil
			}
			m.dock.SetNotice("mode → " + strings.ToLower(args[0]))
			return m.chat.SetConversationMode(m.chatConvID, mode)
		},
	})
	add(SlashCommand{
		Name: "/compact", Usage: "/compact",
		Desc: "summarize this conversation's history to free context (compact the model's context window)",
		Run: func(m *App, _ []string) tea.Cmd {
			if reason := m.chat.CanCompact(m.chatConvID); reason != "" {
				m.dock.SetError(reason)
				return nil
			}
			m.dock.SetNotice("compacting conversation…")
			return m.chat.CompactConversation(m.chatConvID)
		},
	})
	add(SlashCommand{
		Name: "/attach", Usage: "/attach <path>",
		Desc:    "attach a file to the next message (not supported in the TUI)",
		MinArgs: 1,
		Run: func(m *App, args []string) tea.Cmd {
			// Explicit, user-visible refusal — never a silent drop. The
			// AttachConversationTurn surface is only reachable from the GUI
			// today, so the TUI says so instead of swallowing the path.
			m.dock.SetError("attachments are not supported in the TUI — attach " + args[0] + " in the web GUI (nothing was sent)")
			return nil
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
		lines = append(lines, fmt.Sprintf("  %-28s %s%s", usage, c.Desc, c.AliasLabel()))
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
// runThemeCmd switches the TUI theme. The palette set is TUI-OWNED (see
// internal/tui/theme) and validated for terminal contrast: dark (default),
// light, gruvbox-dark, gruvbox-light. "/theme" lists them; "/theme <name>"
// switches and persists the choice to the profile.
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
		where := ""
		if path, err := config.DefaultPath(); err == nil {
			where = "  ·  saved in " + path
		}
		if os.Getenv(config.EnvTheme) != "" {
			where += "  ·  pinned by " + config.EnvTheme + "=" + os.Getenv(config.EnvTheme)
		}
		m.dock.SetNotice("themes (TUI palettes): " + strings.Join(names, " · ") + "  (/theme <name> switches)" + where)
		return nil
	}
	name := args[0]
	if !m.SetTheme(name) {
		known := strings.Join(m.themes, ", ")
		m.dock.SetError(fmt.Sprintf("unknown theme %q — available: %s", name, known))
		return nil
	}
	m.dock.SetNotice("theme: " + name)
	return nil
}

// SetTheme applies a TUI palette, re-pins the styles captured at construction,
// and persists the choice to the profile. Reports false for an unknown name
// (nothing changes). This is the ONE path for switching themes, shared by the
// /theme command and the Control screen's Themes pane.
func (m *App) SetTheme(name string) bool {
	if !theme.Use(name) {
		return false
	}
	// The composer captures textarea/cursor styles at construction, so a switch
	// must re-pin them (otherwise the box keeps the old palette).
	m.dock.ApplyTheme()
	if m.profile != nil {
		m.profile.Theme = name
	}
	// Report WHERE the choice is persisted so a non-persisting environment is
	// visible instead of mysterious: the operator's "themes are not saving" is
	// unsolvable without knowing the config path (a launcher with an ephemeral
	// HOME writes somewhere that does not survive the run).
	path, pathErr := config.DefaultPath()
	if pathErr != nil {
		m.dock.SetNotice("theme: " + name + " (this run only — no config path: " + pathErr.Error() + ")")
		return true
	}
	cfg, err := config.Load(path)
	if err != nil {
		m.dock.SetNotice("theme: " + name + " (this run only — cannot read " + path + ")")
		return true
	}
	cfg.Theme = name
	if p := cfg.Profiles[cfg.Active]; p != nil {
		p.Theme = name
	}
	if err := config.Save(path, cfg); err != nil {
		m.dock.SetNotice("theme: " + name + " (this run only — cannot write " + path + ": " + err.Error() + ")")
		return true
	}
	m.dock.SetNotice("theme: " + name + " (saved to " + path + ")")
	// Reconcile any open Themes pane so its active marker moves.
	if s := m.screens[TabControl]; s != nil {
		if r, ok := s.(interface{ Refresh(string) tea.Cmd }); ok {
			m.pendingScreenCmd = tea.Batch(m.pendingScreenCmd, r.Refresh("themes"))
		}
	}
	return true
}
