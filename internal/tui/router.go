package tui

// router.go — the global key dispatch layer (plan §6 decision 7).
//
// Routes are evaluated top-down: global chords first (Ctrl+O/W/E/A/F/T,
// help, quit), then screen-specific routes, then text-input fallthrough
// always last. The chat-dock feature registers new bindings here without
// touching screen code.

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// chatWakeMsg is declared in app.go (package-level message type).

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
		{
			// Diff sidebar toggle: D / Shift+D OUTSIDE text input. Ctrl+D is
			// never used (EOF muscle memory). Both keys toggle the pane; when
			// the pane is already open, re-toggling closes it.
			Name: "toggle diff sidebar", Keys: "d / shift+d", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && (k.String() == "d" || k.String() == "D")
			},
			Handle: func(m *App, _ tea.Msg) bool {
				if m.chatFocus != focusContent {
					// While composing, `d` inserts a literal 'd' into the
					// composer — the router never consumes it here.
					return false
				}
				if m.diffOpen {
					m.closeDiffPane()
				} else {
					m.pendingDiffCmd = m.openDiffPane()
				}
				return true
			},
		},
		{
			Name: "close diff sidebar", Keys: "esc", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "esc"
			},
			Handle: func(m *App, _ tea.Msg) bool {
				if !m.diffOpen {
					return false // let the screen handle its own esc
				}
				m.closeDiffPane()
				return true // consume esc so the screen's base esc is skipped
			},
		},
		{
			Name: "copy diff (OSC 52)", Keys: "y", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "y"
			},
			Handle: func(m *App, _ tea.Msg) bool {
				if m.diffOpen && m.diffPane != nil {
					m.diffPane.CopySelectedDiff()
					return true
				}
				return false
			},
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
		m.footer.Width = wm.Width
		// Reflow the screen + dock (and the diff pane rail) to the new size,
		// accounting for the pane when it is open.
		m.reflowForDiff()
		// First layout: start the active screen's live streams.
		m.EnsureSubscriptions(m.active)
		return m, nil
	}
	if m.help.open {
		if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "?") {
			m.help.open = false
		}
		return m, nil // overlay swallows keys
	}
	// Composer focus: composer keys first; tab chords fall through to
	// the input (Ctrl+W/A/E/F/T remain delete-word / line-start / …).
	if m.chatFocus == focusComposer {
		if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
			// hard escape: quit always works, even mid-composition
			m.quitting = true
			return m, tea.Quit
		}
		consumed, cmd := m.dock.Update(msg)
		if k, ok := msg.(tea.KeyMsg); ok && consumed {
			switch k.String() {
			case "ctrl+g", "esc":
				m.setFocus(focusContent)
				m.footer.StreamStatus = m.streamStatus()
				return m, nil
			}
		}
		if consumed {
			if text := m.dock.SendRequest(); text != "" {
				cmd = m.sendFromComposer(text)
			}
			m.footer.StreamStatus = m.streamStatus()
			return m, cmd
		}
		if _, isKey := msg.(tea.KeyMsg); isKey {
			// a key the dock did not take while focused: keep it out of
			// the router's tab chords (focus stays in the composer).
			m.footer.StreamStatus = m.streamStatus()
			return m, nil
		}
	}
	if cmd := m.appMsg(msg); cmd != nil {
		m.footer.StreamStatus = m.streamStatus()
		return m, cmd
	}
	for _, r := range m.routes {
		if r.Match == nil || !r.Match(msg) {
			continue
		}
		if r.Handle(m, msg) {
			m.footer.StreamStatus = m.streamStatus()
			if m.quitting {
				// The quit route only flips the flag (KeyRoute.Handle
				// cannot carry a Cmd); emit tea.Quit here so the program
				// actually exits — regression-tested by
				// TestQuitRouteIssuesQuitCmd (QA pass on the TUI
				// foundation found q/ctrl+c hanging the TUI).
				return m, tea.Quit
			}
			// A route may have staged a diff-pane setup cmd (e.g. the D
			// toggle's openDiffPane); re-emit it. The route already consumed
			// the message, so there is no screen fall-through here.
			if m.pendingDiffCmd != nil {
				cmd := m.pendingDiffCmd
				m.pendingDiffCmd = nil
				return m, cmd
			}
			return m, nil
		}
	}
	// Defer to the active screen (its input routes are inside its own
	// Update — chords already consumed above).
	return m.passToScreen(msg)
}

// appMsg handles App-level messages: chat controller results, pending
// detail loads, and the ctrl+g focus chord (content → composer).
func (m *App) appMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case chatWakeMsg:
		return tea.Batch(m.onChatWake(), m.waitChat())
	case chatCmdMsg:
		return tea.Batch(msg.cmd, m.waitChat())
	case tea.KeyMsg:
		if msg.String() == "ctrl+g" && m.chatFocus == focusContent {
			m.setFocus(focusComposer)
			return nil
		}
		return nil
	case chat.ConversationsMsg:
		return tea.Batch(m.onConversations(msg), m.waitChat())
	case chat.TranscriptMsg:
		return tea.Batch(m.onTranscript(msg), m.waitChat())
	case chat.ErrMsg:
		m.setChatError(msg.Where, msg.Err)
		return m.waitChat()
	case chat.TurnResolvedMsg:
		return m.waitChat()
	case chat.StreamDoneMsg:
		return tea.Batch(m.onStreamDone(msg), m.waitChat())
	case chatConvCreatedMsg:
		m.chatConvID = msg.convID
		m.chat.SetActive(msg.convID)
		cmds := []tea.Cmd{m.chat.Send(msg.convID, msg.text, msg.preamble), m.chat.LoadConversations()}
		if m.diffOpen {
			cmds = append(cmds, m.refreshDiffOwner())
		}
		return tea.Batch(cmds...)
	case execSessionMsg:
		return m.onExecutionSessionLoaded(msg.execID, msg.parts, msg.err)
	case interjectOKMsg:
		m.dock.SetNotice("interjection delivered to " + msg.execID + " — reply streams below")
		return nil
	default:
		if m.pendingDetail != nil {
			cmd := m.pendingDetail
			m.pendingDetail = nil
			// re-dispatch into the screen after the detail cmd lands
			return tea.Batch(cmd, m.passToScreenLater())
		}
		return nil
	}
}

// passToScreenLater keeps the pending-detail cmd's msg flowing to the
// screen (detailMsg / detailErrMsg are consumed by Base.Update).
func (m *App) passToScreenLater() tea.Cmd {
	return nil
}
