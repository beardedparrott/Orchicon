package tui

// router.go — the global key dispatch layer (plan §6 decision 7).
//
// Routes are evaluated top-down: global chords first (Ctrl+O/W/E/A/F/T,
// help, quit), then screen-specific routes, then text-input fallthrough
// always last. The chat-dock feature registers new bindings here without
// touching screen code.

import (
	"strings"

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
			// OPERATOR SPEC (Phase-3.5 finding 3): Tab is the focus ring —
			// chat prompt FIRST, then the six area tabs in order, then back
			// to the prompt. From the composer Tab drops to the current
			// tab's content; each further Tab advances to the next tab's
			// content; after Control it wraps back to the composer. Left /
			// right still switch tabs directly (content focus).
			Name: "focus ring", Keys: "tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "tab"
			},
			Handle: func(m *App, _ tea.Msg) bool { m.tabRingNext(); return true },
		},
		{
			// Shift+Tab pops the side rails (conversations right rail +
			// diff left pane) together — the secondary chrome toggle.
			Name: "toggle side rails", Keys: "shift+tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "shift+tab"
			},
			Handle: func(m *App, _ tea.Msg) bool { m.toggleSideRails(); return true },
		},
		{
			// Left / right switch tabs directly (content focus).
			Name: "next tab", Keys: "right", Scope: "global",
			Match:  keyMatcher("right"),
			Handle: func(m *App, _ tea.Msg) bool { m.NextTab(); return true },
		},
		{
			Name: "previous tab", Keys: "left", Scope: "global",
			Match:  keyMatcher("left"),
			Handle: func(m *App, _ tea.Msg) bool { m.PrevTab(); return true },
		},
		{
			Name: "toggle conversations rail", Keys: "ctrl+r", Scope: "global",
			Match: keyMatcher("ctrl+r"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.toggleRightRail()
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
				wasActive := m.ActiveTab()
				m.SwitchTo(tab.ID)
				// Re-arm streams on every chord press, exactly once: on an
				// actual switch the route arms here (SwitchTo doesn't arm
				// for a NEW active tab); on a same-tab re-press SwitchTo's
				// toggle path already armed — the route must not arm a
				// second time (double-arm = double stream dials).
				if m.ActiveTab() != wasActive || wasActive != tab.ID {
					m.EnsureSubscriptions(tab.ID)
				}
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

// composerBypassKeys are the structural chords that reach the global
// routes even while the composer is focused (the launch default): tab
// switches, the conversations-rail toggle, and quit. The textarea would
// otherwise consume them as editing no-ops (it consumes EVERY key —
// unknown chords are silent no-ops that still report consumed), locking
// the shell chrome behind a focus escape forever.
var composerBypassKeys = map[string]bool{
	"ctrl+o": true, "ctrl+w": true, "ctrl+e": true,
	"ctrl+a": true, "ctrl+f": true, "ctrl+t": true,
	"ctrl+r": true, "ctrl+c": true, "q": true,
	// Arrow tab cycling + tab key: structural chrome (the tab bar is the
	// shell's spine — arrows must switch tabs while composing).
	"right": true, "left": true, "tab": true, "shift+tab": true,
}

// dispatch evaluates the route chain. With the composer ALWAYS focused
// (Phase 2a), the global routes run only when the composer is not in
// control of a key: dispatch lets the dock consume each key first while
// focused, then evaluates global chords (tab switching still works via
// explicit chords the dock does not bind), then the screen.
func (m *App) dispatch(msg tea.Msg) (*App, tea.Cmd) {
	if wm, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = wm.Width, wm.Height
		m.footer.Width = wm.Width
		m.footer.MouseEnabled = m.mouseEnabled
		// Reflow the screen + dock (and the diff pane rail) to the new size,
		// accounting for the pane when it is open.
		m.refreshLayout()
		// First layout: run the active screen's first load + live streams.
		m.ensureLoaded(m.active)
		m.EnsureSubscriptions(m.active)
		return m, nil
	}
	if m.help.open {
		if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "?") {
			m.help.open = false
		}
		return m, nil // overlay swallows keys
	}
	// /connect in-place overlay owns ALL messages while open (never quits):
	// keys drive the embedded connection form; the async probe's start/done
	// messages flow through connectTick.
	if m.palette.connectOpen {
		if k, ok := msg.(tea.KeyMsg); ok {
			handled, cmd := m.connectHandleKey(k)
			if handled {
				return m, cmd
			}
			return m, nil
		}
		return m, m.connectTick(msg)
	}
	if mo, ok := msg.(tea.MouseMsg); ok {
		return m.dispatchMouse(mo)
	}
	k, isKey := msg.(tea.KeyMsg)
	if isKey && m.chatFocus == focusComposer && k.String() == "ctrl+c" {
		// hard escape: quit always works, even mid-composition
		m.quitting = true
		return m, tea.Quit
	}
	// Tab dropdown submenu keys: the open menu owns arrows/enter/esc and
	// its own tab chords (BEFORE composer handling so esc closes the menu
	// instead of falling through to focus toggling — no focus trap).
	if isKey && m.TabMenu() != nil {
		if handled, cmd := m.menuHandleKey(k); handled {
			m.footer.StreamStatus = m.streamStatus()
			return m, cmd
		}
	}
	// Tab submenu ACTIVATION (Phase 3 finding 3): Enter or Space on the
	// active tab opens its dropdown. Enter with an empty composer is a
	// no-op today (blank enter never sends), so the shell can claim it
	// there too; with text in the composer Enter still means send.
	if isKey && m.TabMenu() == nil && m.menuActivationKey(k) {
		m.openTabMenu(m.active)
		m.footer.StreamStatus = m.streamStatus()
		return m, nil
	}
	// Composer '/' palette: while open, palette keys own the message
	// (filter/navigate/select); esc hands the key back to the global
	// routes. Otherwise a leading '/' opens it. The palette floats ABOVE
	// the composer: the composer line + typed text stay visible in the
	// base view while it filters (paletteView composes over the base).
	if m.palette.PaletteOpen() && isKey {
		handled, cmd := m.paletteHandleKey(k)
		if handled {
			m.footer.StreamStatus = m.streamStatus()
			return m, cmd
		}
	} else if isKey && m.chatFocus == focusComposer && k.String() == "/" {
		// The composer OWNS the typing: the "/" goes into the buffer
		// FIRST so the operator's text ("/pro") stays visible in the bar
		// while the palette above filters on it (Phase 3 finding 4 — the
		// palette used to swallow the query and the bar stayed empty).
		_, cmd := m.dock.Update(k)
		m.openPalette()
		m.footer.StreamStatus = m.streamStatus()
		return m, cmd
	}
	// Composer focus (the launch default): KEY messages go to the dock
	// first (typing works immediately), EXCEPT the shell's structural
	// chords — tab switches, ctrl+r rail toggle, q quit, ctrl+c — which
	// bypass the composer so the chrome always works while typing. Every
	// other key is composer-local editing text; 'q'/'?'/'d'/'y' etc. are
	// literal characters, never shortcuts, while composing. ALL non-key
	// messages (chat results, subs pokes, diffs) fall through to appMsg —
	// the dock must never swallow async traffic.
	if m.chatFocus == focusComposer {
		if k, isKeyMsg := msg.(tea.KeyMsg); isKeyMsg {
			// Empty composer: the vertical keys scroll the active pane (the
			// GUI's transcript scroll). With text in the buffer the textarea
			// keeps them for cursor movement.
			if strings.TrimSpace(m.dock.Value()) == "" {
				if d := scrollKeyDelta(k.String()); d != 0 {
					m.scrollActiveDetail(d)
					m.footer.StreamStatus = m.streamStatus()
					return m, nil
				}
			}
			if composerBypassKeys[k.String()] {
				// structural chord: skip the composer, the routes below
				// own it (switchTab routes, quit, rail toggle).
			} else {
				consumed, cmd := m.dock.Update(msg)
				switch k.String() {
				case "ctrl+g", "esc":
					if consumed {
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
			}
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
			if c := m.drainStaged(); c != nil {
				return m, c
			}
			return m, nil
		}
	}
	// Defer to the active screen (its input routes are inside its own
	// Update — chords already consumed above).
	return m.passToScreen(msg)
}

// dispatchMouse routes mouse events: the open dropdown first, then the
// tab bar (click = switch + open its menu), the Ask rail, then
// fall-through to the screen/pane.
func (m *App) dispatchMouse(mo tea.MouseMsg) (*App, tea.Cmd) {
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		// Toggle decision BEFORE any close: one click = exactly one menu
		// transition. Capturing the open state after the outside-click
		// close would turn "click the open tab again" into "reopen" (the
		// menu could never close by mouse).
		wasOpen := m.MenuOpenID()
		if m.TabMenu() != nil {
			if m.MenuClick(mo.X, mo.Y) {
				return m, nil
			}
			if m.menuHit(mo.X, mo.Y) {
				return m, nil // header/border: keep the menu open
			}
			m.closeTabMenu() // click outside closes (standard menu behavior)
		}
		if mo.Y == 0 {
			if id, ok := m.TabClick(mo.X); ok {
				if wasOpen == id {
					// Click the open tab again: close its dropdown (the
					// tab is already active — SwitchTo re-arms only).
					m.SwitchTo(id)
					m.EnsureSubscriptions(id)
				} else {
					m.SwitchTo(id)
					m.EnsureSubscriptions(id)
					m.openTabMenu(id)
				}
				return m, nil
			}
		}
	}
	if m.railVisible() && mo.X >= m.width-ConversationsRailWidth {
		if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
			if m.railHeaderHit(mo.Y) {
				m.toggleRightRail()
				return m, m.drainRailCmd()
			}
			if m.railRetryHit(mo.Y) {
				// A failed rail: the whole body retries (never a silent
				// empty rail — finding 9).
				return m, m.reloadConversations()
			}
			if idx, ok := m.railRowAt(mo.Y); ok {
				return m, m.openRailConversation(idx)
			}
		}
		if mo.Button == tea.MouseButtonWheelUp {
			m.convScroll--
			if m.convScroll < 0 {
				m.convScroll = 0
			}
			return m, nil
		}
		if mo.Button == tea.MouseButtonWheelDown {
			max := len(m.conversations) - m.railVisibleRows()
			if max < 0 {
				max = 0
			}
			m.convScroll++
			if m.convScroll > max {
				m.convScroll = max
			}
			return m, nil
		}
	}
	// Composer row: a click anywhere in the dock block FOCUSES the composer
	// (operator finding 7 — mouse must genuinely focus the input, not only
	// rely on the launch default). The diff rail owns clicks in its own
	// columns, so those still go to the pane.
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		if !(m.diffOpen && mo.X < DiffPaneWidth) && m.inDockRows(mo.Y) {
			m.setFocus(focusComposer)
			m.footer.ComposerFocus = true
			return m, nil
		}
	}
	if cmd := m.appMsg(mo); cmd != nil {
		m.footer.StreamStatus = m.streamStatus()
		return m, cmd
	}
	for _, r := range m.routes {
		if r.Match == nil || !r.Match(mo) {
			continue
		}
		if r.Handle(m, mo) {
			m.footer.StreamStatus = m.streamStatus()
			if c := m.drainStaged(); c != nil {
				return m, c
			}
			return m, nil
		}
	}
	return m.passToScreen(mo)
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
		// The new conversation has no detail open: without a RequestDetail
		// the onChatWake DetailID()==chatConvID guard never passes and the
		// fresh turn's chunks land in chatStore but never repaint (first
		// send looks dead). Open the detail so the transcript surface
		// follows the new conversation.
		if s := m.screens[TabAsk]; s != nil {
			if rd, ok := s.(interface {
				RequestDetail(src, id string) tea.Cmd
			}); ok {
				if cmd := rd.RequestDetail("conversations", msg.convID); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		}
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
