package tui

// slash.go — the slash command framework (plan §4): commands parse before send; unknown /word gives
// usage feedback instead of being sent as a chat message; `\/` escapes a literal slash.

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

// MatchesQuery reports whether a typed slash word (WITHOUT the leading slash) names this command —
// by its primary spelling OR any ALIAS.
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

// AliasLabel renders the aliases for display, in one shared form so /help and the palette cannot
// drift apart — the exact drift that produced this report (helpLines showed the alias.
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

// ParseSlash parses composer input: "/word ..." at buffer start → (name, args).
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

// buildSlashRegistry assembles the full command set: nav-generated entries (from the screens'
// Sources() — no drift from real screens), hand-listed GUI-mirror entries, arg jumps, aliases.
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

	// 2.
	add(SlashCommand{
		Name:  "/undo",
		Usage: "/undo",
		Desc:  "remove your most recent message (its reply stays) — confirms before acting",
		Run: func(m *App, _ []string) tea.Cmd {
			id, text, ok := m.lastEditableUserMessage()
			if !ok {
				m.dock.SetNotice("undo: no message of yours to remove in this conversation")
				return nil
			}
			m.openMsgMenuArmed(m.chatConvID, id, text, chat.MessageDeleteOnly)
			return nil
		},
	})
	add(SlashCommand{
		Name:    "/revert",
		Aliases: []string{"/truncate"},
		Usage:   "/revert",
		Desc:    "remove your most recent message and everything after it — confirms before acting",
		Run: func(m *App, _ []string) tea.Cmd {
			id, text, ok := m.lastEditableUserMessage()
			if !ok {
				m.dock.SetNotice("revert: no message of yours to revert to in this conversation")
				return nil
			}
			m.openMsgMenuArmed(m.chatConvID, id, text, chat.MessageDeleteAndFollowing)
			return nil
		},
	})
	add(SlashCommand{
		Name:  "/grants",
		Usage: "/grants",
		Desc:  "the directories allowed for this session, and revoke them",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.runAskOverlay("grants")
		},
	})
	add(SlashCommand{
		Name:    "/permissions",
		Usage:   "/permissions",
		Aliases: []string{"/perm"},
		Desc:    "the persistent allow/deny list (the file is the source of truth)",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.runAskOverlay("permissions")
		},
	})

	nav := buildNavEntries(m)
	for _, e := range nav {
		eCopy := e
		add(SlashCommand{
			Name:  "/" + e.Cmd,
			Usage: "/" + e.Cmd + e.ArgsHint,
			Desc:  "open " + e.Label,
			Run: func(m *App, args []string) tea.Cmd {
				// A VERB row (Ask's New / Conversations) runs its action instead of focusing a source — otherwise
				// /conversations would switch tabs and focus a source the Ask screen never renders (its list.
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

	// 3.

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
			// In-place overlay: NEVER exits the process or tears down alt-screen.
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

	// THE TUI IS ONE OF THE TWO USER-FACING CONTROL SURFACES (with the desktop app; the web app carries
	// none by design) — so for a terminal-first operator this is the ONLY place a plane they just.
	add(SlashCommand{
		Name: "/plane", Usage: "/plane [start | stop | restart]",
		Desc: "plane state for the connected local instance; a verb runs the same plane control the CLI and desktop app call (not the plane's API)",
		Run: func(m *App, args []string) tea.Cmd {
			verb := ""
			if len(args) > 0 {
				verb = strings.ToLower(strings.TrimSpace(args[0]))
			}
			return m.planeCommand(verb)
		},
	})
	add(SlashCommand{
		Name: "/plane-start", Usage: "/plane-start",
		Desc: "start the connected instance's plane (internal/planecontrol, not the API)",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.planeCommand("start")
		},
	})
	add(SlashCommand{
		Name: "/plane-stop", Usage: "/plane-stop",
		Desc: "stop the connected instance's plane (graceful, idempotent)",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.planeCommand("stop")
		},
	})
	add(SlashCommand{
		Name: "/plane-restart", Usage: "/plane-restart",
		Desc: "restart the plane so it runs the installed binary (also the remedy for drift)",
		Run: func(m *App, _ []string) tea.Cmd {
			return m.planeCommand("restart")
		},
	})

	// New chat / conversation management / ask-model + mode (parity with the GUI's Ask Orchicon
	// surface).
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
	add(SlashCommand{
		Name: "/rename", Usage: "/rename [title]",
		Desc: "rename the open conversation — with no title, opens a prefilled box (UpdateConversationTitle)",
		Run: func(m *App, args []string) tea.Cmd {
			id := m.chatConvID
			if id == "" {
				// The open conversation is the target by default, but if the operator has only the RAIL loaded (a
				// conversation selected, none "open"), that selection is what they are looking.
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
		Name: "/find", Usage: "/find <text>",
		Desc: "search the open transcript — filters matching messages (case-insensitive); n/N step, esc clears (also ctrl+f)",
		Run: func(m *App, args []string) tea.Cmd {
			m.findInTranscript(strings.Join(args, " "))
			return nil
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
		// THE NAME IS THE SINGULAR, AND THAT IS A DELIBERATE LINE: /project SWITCHES the workspace,
		// /projects NAVIGATES to the Projects pane.
		Name: "/project", Usage: "/project [<filter>]",
		Desc:    "switch the PROJECT WORKSPACE the conversations rail shows (type text to filter the list)",
		MinArgs: 0,
		Run: func(m *App, args []string) tea.Cmd {
			// BARE, OR WITH A FILTER, IT OPENS THE LIST.
			return m.openProjectPickerFiltered("", strings.TrimSpace(strings.Join(args, " ")))
		},
	})
	add(SlashCommand{
		Name: "/project-move", Usage: "/project-move",
		Desc:    "move the OPEN conversation into another project workspace",
		MinArgs: 0,
		Run: func(m *App, args []string) tea.Cmd {
			// A SEPARATE NAME RATHER THAN AN ARGUMENT ON /project.
			if m.chatConvID == "" {
				m.dock.SetError("no conversation is open — /project chooses the workspace")
				return nil
			}
			return m.openProjectPickerFiltered(m.chatConvID, "")
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
		Name: "/mode", Usage: "/mode [" + strings.Join(chat.ModeNames(), "|") + "]",
		Desc:    "set the conversation mode/persona (SetConversationMode); no argument reports the current one and lists what is available",
		MinArgs: 0,
		Run: func(m *App, args []string) tea.Cmd {
			if len(args) == 0 {
				// BARE /mode REPORTS AND LISTS.
				m.dock.SetNotice("mode: " + m.currentModeLabel() + "  (available: " +
					strings.Join(chat.ModeNames(), ", ") + " — /mode <name> to switch)")
				return nil
			}
			// THE MODE NAME MAY CONTAIN A SPACE, so the arguments are JOINED rather than taken one at a time:
			// the slash layer splits on whitespace.
			mode, ok := chat.ParseMode(strings.Join(args, " "))
			if !ok {
				m.dock.SetError("unknown mode " + args[0] + " — available: " + strings.Join(chat.ModeNames(), ", "))
				return nil
			}
			// THE PENDING MODE IS ONLY SET WHEN THERE IS NOTHING OPEN TO SET IT ON.
			if m.chatConvID == "" {
				m.chat.SetPendingMode(mode)
				m.dock.SetNotice("mode " + m.currentModeLabel() + " applies to the next new conversation")
				return nil
			}
			// The name the operator TYPED, not the raw arg: "quick work" should be
			// confirmed as a mode, not echoed back as a guess.
			m.dock.SetNotice("mode → " + m.modeSwitchLabel(mode))
			return m.chat.SetConversationMode(m.chatConvID, mode)
		},
	})
	// --- THE CONVERSATION SCOPE: ONE MODAL, THREE NAMES -------------------------------
	//
	// The operator: "The MCP tools that were added for the TUI is abysmal. There are too many commands and
	// the shortcuts go off screen. I think simply type /mcp should pop up a modal that mimics what the gui
	// has for scope. Same with /skills. It should pop up the same modal. Or /scope."
	//
	// So the subcommand grammar is GONE — /mcp [define | edit <name> | delete <name> | secret <name> |
	// install <name>] was 76 cells, and the palette (capped at 70) had to clip its last verbs, which is
	// the "shortcuts go off screen" half of the report. The capabilities did not change; they moved into
	// the modal (scope_modal.go), where the hint bar names every one of them and the conversation's
	// definitions are visible instead of being recallable only by someone who already knew the grammar.
	//
	// THREE NAMES, ONE SURFACE, on purpose: /scope is the honest name for what it edits, /mcp and /skills
	// are the words an operator will actually type, and a name that opens the thing you want is not
	// duplication — it is the difference between a command that has to be learned and one that is found.
	scopeCmd := func(m *App, args []string) tea.Cmd {
		// NO ARGUMENTS, AND THAT IS THE POINT: the grammar is gone.
		if len(args) > 0 {
			m.dock.SetError("/" + strings.TrimPrefix(args[0], "/") + " is not a subcommand any more — " +
				"the scope modal has every one of them as a key: a add · c catalog · enter/e edit · " +
				"i install · k credential · d delete · s skill files · esc close")
			return nil
		}
		return m.openScopeModal()
	}
	add(SlashCommand{
		Name: "/scope", Usage: "/scope",
		Desc: "this conversation's scope: its MCP servers and skill files, with the project's shown read-only",
		Run:  scopeCmd,
	})
	add(SlashCommand{
		// /mcp KEEPS THE NAME THE OPERATOR ALREADY KNOWS.
		Name: "/mcp", Usage: "/mcp",
		Desc: "open the scope modal at this conversation's MCP servers (same modal as /scope)",
		Run:  scopeCmd,
	})
	add(SlashCommand{
		// /skills WITH NO ARGUMENT opens the same modal; WITH a path list it writes directly, because a
		// typed list is a complete statement and does not need a modal in between.
		Name: "/skills", Usage: "/skills [<path>[, <path>…] | clear]",
		Desc:    "this conversation's skill FILES — no argument opens the scope modal; paths (or clear) write directly",
		MinArgs: 0,
		Run: func(m *App, args []string) tea.Cmd {
			if m.chatConvID == "" {
				m.dock.SetError("no conversation open — /skills applies to one conversation; /project chooses the workspace")
				return nil
			}
			if len(args) == 0 {
				return m.openScopeModal()
			}
			return m.applyConversationSkills(args)
		},
	})
	add(SlashCommand{
		Name: "/fullsend", Usage: "/fullsend",
		Desc: "toggle FULLSEND for this conversation: stop asking for permission (a deny entry and the never-allow class still refuse)",
		Run: func(m *App, _ []string) tea.Cmd {
			// NO PENDING FORM, deliberately.
			if m.chatConvID == "" {
				m.dock.SetError("no conversation open — /fullsend applies to one conversation, and there is no pending form: a bypass must not be armed for a chat you have not opened")
				return nil
			}
			// THE OPERATOR'S INTENT IS A TOGGLE; THE WIRE CARRIES A VALUE.
			on := !m.fullsendOn()
			switch {
			case !on:
				m.dock.SetNotice("FULLSEND off — this conversation asks for permission again")
			case m.chatStore.hasPendingConsent(m.chatConvID):
				// A CARD ALREADY ON SCREEN IS CLEARED TOO — the operator's call: "Clear it too — fullsend means
				// stop asking." It is also the consistent behaviour.
				m.dock.SetNotice("⚠ FULLSEND ON — open permission cards are approved and asks from here on are skipped. A question still needs your answer. Your deny list and sudo-class binaries still refuse.")
			default:
				m.dock.SetNotice("⚠ FULLSEND ON — Orchicon stops asking for permission in this conversation. Your deny list and sudo-class binaries still refuse. /fullsend again turns it off.")
			}
			return m.chat.SetConversationFullsend(m.chatConvID, on)
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
		Desc:    "attach a file to the next message",
		MinArgs: 1,
		Run: func(m *App, args []string) tea.Cmd {
			// THE TUI CAN ATTACH.
			return attachFileFromPrompt(strings.Join(args, " "))
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

// findInTranscript is the SHARED transcript-search entry point for the TERM-AT-HAND case: /find
// <text> and the find box both land here, so the two surfaces can never drift.
func (m *App) findInTranscript(q string) {
	if m.chatConvID == "" {
		m.dock.SetError("no conversation open — nothing to search")
		return
	}
	q = strings.TrimSpace(q)
	if q == "" {
		if m.clearAskSearch() {
			m.dock.SetNotice("transcript search cleared")
		} else {
			m.dock.SetNotice("usage: /find <text> (or press ctrl+f to search from a find box)")
		}
		return
	}
	m.setAskSearch(q)
	matches, total := transcriptSearchCount(m.chatStore.snapshot(m.chatConvID), q)
	if matches == 0 {
		m.dock.SetNotice(fmt.Sprintf("search %q — no matches", q))
	} else {
		m.dock.SetNotice(fmt.Sprintf("search %q — %d/%d matches (n/N step, esc clears)", q, matches, total))
	}
}

// dispatchSlash parses and runs a slash command from the composer.
func (m *App) dispatchSlash(text string) (bool, tea.Cmd) {
	name, args, escaped, ok := ParseSlash(text)
	if escaped {
		// literal slash: strip the escape backslash, send as chat
		return false, nil
	}
	if !ok {
		return false, nil
	}
	// A COMMAND DISMISSES THE CHOOSER. The palette's `enter` case closed itself ONLY when it had a
	// selected candidate (palette.go: `if c == nil { return false, nil }`).
	m.closePalette()
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

// runThemeCmd switches the TUI theme ("/theme" lists, "/theme <name>" switches + persists to the
// profile).
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
	// NO NOTICE HERE: SetTheme writes a complete one that says WHERE the choice was kept — bound to the
	// active project, or saved as the shared default. A second, vaguer line here would only race it.
	return nil
}

// SetTheme applies a TUI palette and PERSISTS the choice — the COMMIT. Reports false for an unknown name
// (nothing changes).
//
// IT WRITES TO ONE OF TWO PLACES, AND WHICH ONE IS THE WHOLE POINT:
//
//   - A PROJECT IS ACTIVE → the palette is BOUND to that project (project_themes), which is what "different
//     colours to distinguish different sessions" asked for. The config's DEFAULT is deliberately left alone:
//     it is the value every project with no palette of its own INHERITS, so writing it here would make one
//     project's choice follow the operator into every other project. That is the report this fixes — "I
//     opened orch in the ai-tools directory, changed the theme, then opened orch in the Orchicon directory
//     and the theme changed. It didn't stay the same."
//   - NO PROJECT IS ACTIVE (All projects / No project) → the persisted DEFAULT is written, which is the only
//     palette a scope with no workspace of its own can honestly set. It is also the way to change what every
//     UNBOUND project starts from.
//
// THE LIVE PREVIEW IS PreviewTheme, NOT THIS (see below). Arrowing through the Themes pane must show a
// palette without committing it: routing the preview through here is what rewrote a project's binding — or
// the shared default — on every keystroke.
//
// This and PreviewTheme are the only two ways a palette changes, shared by /theme and the Control screen's
// Themes pane, so the two gestures cannot disagree about what committing means.
//
// PRECEDENCE AGAINST A PROJECT BINDING (criterion 5, see projecttheme.go): this call PINS the palette for
// the rest of the current scope — themePinned marks it so. The pin is released the next time the workspace
// changes (applyScopeTheme), at which point the SCOPE's own palette takes back over, with a notice saying
// so. An explicit choice never gets silently overwritten mid-scope, and it never silently survives past a
// workspace switch either.
func (m *App) SetTheme(name string) bool {
	if !m.applyThemeAndRefresh(name) {
		return false
	}
	m.themePinned = true

	// A PROJECT IS ACTIVE: this is THAT PROJECT's palette. The binding IS the write; the shared default is
	// not touched, because it belongs to every project that has none.
	if label, ok := m.BindProjectTheme(name); ok {
		m.dock.SetNotice("theme: " + name + " — this project's palette (" + label +
			"; kept for it, and the default is left alone — set that from All projects)")
		m.refreshThemesPane()
		return true
	}

	// NO PROJECT: the persisted default, which is what a scope with no workspace of its own can set.
	if m.profile != nil {
		m.profile.Theme = name
	}
	// Report WHERE the choice is persisted so a non-persisting environment is visible instead of
	// mysterious: the operator's "themes are not saving" is unsolvable without knowing the config path
	// (a.
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
	m.dock.SetNotice("theme: " + name + " (the default, saved to " + path +
		"; projects with their own palette keep theirs)")
	m.refreshThemesPane()
	return true
}

// PreviewTheme applies a palette WITHOUT persisting or binding anything: the Themes pane's live
// preview as the cursor moves (control/screen.go's OnHighlight -> previewTheme -> applyTheme).
func (m *App) PreviewTheme(name string) bool {
	return m.applyThemeAndRefresh(name)
}

// refreshThemesPane reconciles any open Themes pane so its active/bound markers move after a COMMIT.
func (m *App) refreshThemesPane() {
	s := m.screens[TabControl]
	if s == nil {
		return
	}
	if r, ok := s.(interface{ Refresh(string) tea.Cmd }); ok {
		m.pendingScreenCmd = tea.Batch(m.pendingScreenCmd, r.Refresh("themes"))
	}
}
