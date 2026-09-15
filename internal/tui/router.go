package tui

// router.go — the global key dispatch layer (plan §6 decision 7).
//
// Routes are evaluated top-down: global chords first (Ctrl+O/W/E/A/F/T,
// help, quit), then screen-specific routes, then text-input fallthrough
// always last. The chat-dock feature registers new bindings here without
// touching screen code.

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
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
			// content; after Control it wraps back to the composer. Left/right
			// are NOT part of the ring: they move between the panes BELOW the
			// menu (kit2.Base), because rotating the screens with them made the
			// panes unreachable by arrow.
			//
			// NOTE: dispatch handles `tab` as a HARD CHORD before the
			// screen-claims gate, so this route is the fallback — it is kept so
			// the ring is still reachable if that gate is ever reordered. Do not
			// remove it without moving the chord deliberately.
			Name: "focus ring", Keys: "tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "tab"
			},
			Handle: func(m *App, _ tea.Msg) bool { m.tabRingNext(); return true },
		},
		{
			// Shift+Tab walks the ring BACKWARDS. (This route was named "toggle side
			// rails" and carried a comment about popping the conversations and diff
			// rails — it has called tabRingPrev since the ring was built, so the name
			// and the comment were both stale, and the name is what the help overlay
			// prints. The rails toggle is ctrl+r.)
			Name: "previous tab (reverse focus ring)", Keys: "shift+tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "shift+tab"
			},
			Handle: func(m *App, _ tea.Msg) bool { m.tabRingPrev(); return true },
		},
		// LEFT/RIGHT are NOT tab-chord routes. They used to rotate the whole tab bar,
		// which made it impossible to move focus between the two panes below it —
		// the operator's "left+right should move between the two panes". They now
		// reach the SCREEN (kit2.Base), which moves focus between its list and its
		// detail. The tab bar keeps Tab/Shift+Tab (and the ctrl chords).
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
			// Diff sidebar toggle: CTRL+D — the operator's binding ("change the diff
			// panel to be ctrl+d"). It used to be `d`/`D`, which stole a plain letter
			// from every screen and could only be bound there because this route ran
			// ahead of the screen. A ctrl chord is never text, so it works from the
			// composer too.
			Name: "toggle diff sidebar", Keys: "ctrl+d", Scope: "global",
			Match: keyMatcher("ctrl+d"),
			Handle: func(m *App, _ tea.Msg) bool {
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
	// One chord route per tab, in tab order: ctrl+o/v/w/e/a/f/t.
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
	"ctrl+o": true, "ctrl+v": true, "ctrl+w": true, "ctrl+e": true,
	"ctrl+a": true, "ctrl+f": true, "ctrl+t": true,
	"ctrl+r": true, "ctrl+c": true, "ctrl+d": true, "q": true,
	// TAB is structural chrome (the tab bar is the shell's spine). LEFT/RIGHT are
	// deliberately NOT here: in a text box they are cursor movement, which is what
	// the operator expects from a composer.
	"tab": true, "shift+tab": true,
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
		m.refreshComposerHint()
		return m, nil
	}
	if m.help.open {
		if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "esc" || k.String() == "?") {
			m.help.open = false
		}
		return m, nil // overlay swallows keys
	}
	// The /models picker overlay owns EVERY key and the mouse while it is open:
	// it is layered above the composer, so a keystroke aimed at the picker can
	// never reach the composer or a screen, and its load results route to it.
	if m.modelPicker != nil {
		switch msg := msg.(type) {
		case modelpick.KindsMsg:
			return m, m.applyModelKinds(msg)
		case modelpick.ProvidersMsg:
			return m, m.applyModelProviders(msg)
		case modelpick.ModelsMsg:
			return m, m.applyModelModels(msg)
		case tea.KeyMsg:
			_, cmd := m.modelPicker.HandleKey(msg)
			// The SHELL closes the modal (see finishAskModelPicker).
			return m, tea.Batch(cmd, m.finishAskModelPicker())
		case tea.MouseMsg:
			_, cmd := m.modelPicker.HandleMouse(msg)
			return m, tea.Batch(cmd, m.finishAskModelPicker())
		}
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
	// The composer's caret animates from cursor.BlinkMsg ticks, which are NOT key
	// messages — and the dock is only ever handed keys (three call sites, all
	// tea.KeyMsg). Without this the tick reached the shell and was dropped, so the
	// caret never blinked no matter what the dock did with it.
	if bm, ok := msg.(cursor.BlinkMsg); ok {
		_, cmd := m.dock.Update(bm)
		return m, cmd
	}
	k, isKey := msg.(tea.KeyMsg)
	// Focus chords come FIRST, as hard escapes alongside ctrl+c — and BEFORE the
	// screen-claims check below.
	//
	// They have to: a screen that claims every key (an open form, a latched search
	// box) is claiming TEXT INPUT, and that must never capture the chord that
	// returns the operator to the composer. The gate below used to swallow them,
	// which is exactly the operator's "hitting ctrl+g to get focus to the composer
	// is spotty and usually doesn't work. It will still try and recognize letters
	// being pushed to perform actions as opposed to truly dropping into the
	// composer to type" — with a claim latched, ctrl+g was handed to the screen
	// and the composer was never focused, so the next letters ran screen actions.
	if isKey {
		switch k.String() {
		case "ctrl+c":
			if m.chatFocus == focusComposer {
				// hard escape: quit always works, even mid-composition
				m.quitting = true
				return m, tea.Quit
			}
		case "tab":
			// TAB moves through a FORM'S FIELDS whenever a form is open — modal OR
			// inline. The operator: "when in an edit form, tab should move through
			// the fields of the form just like up/down keys. Tabbing currently
			// breaks out and moves to the next top tabmenu item. This is wrong.
			// Tabs should only move to the next menu item if you are NOT in edit
			// mode."
			//
			// So the test is FormOpen, not ModalFormOpen: the earlier rule yielded
			// only to a centred WINDOW, which let Tab ESCAPE an inline editor — the
			// host the worker, work-item and Control forms now use.
			if fs, ok := m.screens[m.active].(interface{ FormOpen() bool }); ok && fs.FormOpen() {
				return m.passToScreen(msg)
			}
			// Otherwise Tab advances the top-level selection — and stops there. No
			// submenu is popped open ("submenus should only pop up if you enter on
			// them"), because an open menu would then eat the arrows.
			m.closeTabMenu()
			m.tabRingNext()
			m.refreshStreamStatus()
			return m, nil
		case "ctrl+g":
			// Focus the composer from ANY state, and LEAVE whatever holds the keys:
			// an open form/modal and a latched search box are both dismissed, so
			// the next keystroke is typing rather than an action. The search QUERY
			// is kept (StopFilter, not ClearFilter) — the operator asked to go type,
			// not to throw their search away, and it is still there on return.
			if m.screens[m.active] != nil {
				if cl, ok := m.screens[m.active].(interface{ DropKeyClaim() }); ok {
					cl.DropKeyClaim()
				}
			}
			m.setFocus(focusComposer)
			m.refreshStreamStatus()
			return m, nil
		}
	}
	// THE TAB BAR'S OWN KEYS COME FIRST — ahead of every screen's key claim.
	//
	// The bar is the shell's chrome, and a screen claiming keys (an open form, a
	// latched search box, a form still being prepared — or a STUCK latch, which is
	// the real culprit behind "the projects screen still likes to steal focus") was
	// able to swallow the bar's Enter. With the keyboard ON THE BAR nothing below
	// should be able to take it: the operator is manipulating chrome, not content.
	//
	// Enter opens the selected tab's submenu; left/right walk the top-level tabs.
	//
	// ONLY while no submenu is open: with one up, Enter belongs to it (it SELECTS
	// the highlighted entry) and reopening here would reset the selection to the
	// first row every press — the menu could never be navigated at all.
	if isKey && m.chatFocus == focusTabs && m.TabMenu() == nil {
		switch k.String() {
		case "enter", " ", "space":
			m.openTabMenu(m.active)
			m.refreshStreamStatus()
			return m, nil
		case "left", "right":
			// Left/right do NOT walk the tab ring — NOT EVEN HERE.
			//
			// They used to, in this very branch. Removing only the GLOBAL routes
			// (27ad7c71) left this path alive, so with the bar focused — which is
			// where Tab and a tab click both land — the arrows still rotated the
			// screens. That is half of the operator's "left+right should not be moving
			// the tab menu at the top nor should it be rotating through the different
			// screens".
			//
			// They now hand the keyboard DOWN to the panes: "left+right should move
			// between the two panes below the menus so you can grab focus on those for
			// the specific submenu you have selected". There is deliberately NO return,
			// so the key keeps travelling through this dispatch — ONE press both grabs
			// a pane and moves to it (the screen's own left/right picks the source list
			// or the detail). Tab/Shift+Tab remain the way to walk the ring.
			m.setFocus(focusContent)
			m.refreshStreamStatus()
		}
	}
	// Screen-owned input mode: a screen with an open form/modal claims EVERY
	// key (bar the hard chords handled above), so typed characters are never
	// intercepted by shell routes — 'q' would quit, space opens the tab menu,
	// '/' opens the palette, 'd' the diff rail. Screens opt in through the
	// optional ClaimsKeys hook (automation's recurring-item form).
	if isKey {
		if ks, ok := m.screens[m.active].(interface{ ClaimsKeys() bool }); ok && ks.ClaimsKeys() {
			return m.passToScreen(msg)
		}
	}
	// Tab dropdown submenu keys: the open menu owns arrows/enter/esc and
	// its own tab chords (BEFORE composer handling so esc closes the menu
	// instead of falling through to focus toggling — no focus trap).
	if isKey && m.TabMenu() != nil {
		if handled, cmd := m.menuHandleKey(k); handled {
			m.refreshStreamStatus()
			return m, cmd
		}
	}
	// Ask rail SELECTION: while the conversations rail is up and the composer
	// is empty, Enter/Space open the highlighted conversation (the operator's
	// "I should be able to move up/down with arrow keys and space or enter
	// selects"). This is deliberately BEFORE the tab-menu activation below:
	// on Ask the rail is the list in focus, so Enter must select the
	// conversation rather than pop the New/Conversations menu. The menu is
	// still reachable from every other tab (and here whenever the rail is
	// hidden — the launch page shows no list).
	if isKey && m.TabMenu() == nil && m.active == TabAsk && m.railVisible() &&
		strings.TrimSpace(m.dock.Value()) == "" &&
		(k.String() == "enter" || k.String() == " " || k.String() == "space") {
		cmd := m.openSelectedRailConversation()
		m.refreshStreamStatus()
		return m, cmd
	}
	// Tab submenu ACTIVATION (Phase 3 finding 3): Enter or Space on the
	// active tab opens its dropdown. Enter with an empty composer is a
	// no-op today (blank enter never sends), so the shell can claim it
	// there too; with text in the composer Enter still means send.
	if isKey && m.TabMenu() == nil && m.menuActivationKey(k) {
		m.openTabMenu(m.active)
		m.refreshStreamStatus()
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
			m.refreshStreamStatus()
			return m, cmd
		}
	} else if isKey && m.chatFocus == focusComposer && k.String() == "/" {
		// The composer OWNS the typing: the "/" goes into the buffer
		// FIRST so the operator's text ("/pro") stays visible in the bar
		// while the palette above filters on it (Phase 3 finding 4 — the
		// palette used to swallow the query and the bar stayed empty).
		_, cmd := m.dock.Update(k)
		m.openPalette()
		m.refreshStreamStatus()
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
			// Empty composer: the vertical keys drive the conversation list
			// when the conversations rail is up (the operator's "I can't go
			// up/down with the arrow keys"), otherwise they scroll the active
			// pane's detail. With text in the buffer the textarea keeps them
			// for cursor movement.
			if strings.TrimSpace(m.dock.Value()) == "" {
				if d := scrollKeyDelta(k.String()); d != 0 {
					if m.railVisible() && m.active == TabAsk {
						switch k.String() {
						case "up":
							m.selectRailConversation(-1)
						case "down":
							m.selectRailConversation(1)
						case "pgup":
							m.selectRailConversation(-5)
						case "pgdown":
							m.selectRailConversation(5)
						}
					} else {
						// Any other screen: the vertical keys belong to the SCREEN, which
						// decides for itself — its list moves the cursor, and its DETAIL
						// scrolls once the detail holds focus. The shell scrolling the detail
						// here instead is why the arrow keys never moved a list from the
						// composer (the operator's "the default Projects view under Work
						// captures the down arrows").
						return m.passToScreen(msg)
					}
					m.refreshStreamStatus()
					return m, nil
				}
			}
			if composerBypassKeys[k.String()] {
				// structural chord: skip the composer, the routes below
				// own it (switchTab routes, quit, rail toggle).
			} else {
				consumed, cmd := m.dock.Update(msg)
				switch k.String() {
				case "ctrl+z":
					// Escalate to the full conversation view with this conversation
					// selected (the operator's ctrl+z).
					if m.panelVisible() {
						m.escalateToConversation()
						m.refreshStreamStatus()
						return m, nil
					}
				case "ctrl+g", "esc":
					// Disengaging keys: they never SLIDE THE PANEL OUT. Esc first
					// minimises an open strip back into the prompt (the operator's
					// ask); a second esc, or esc with no strip, moves focus to the
					// content. Opening here instead would make esc open-then-close
					// the strip in one dispatch and appear dead.
					if consumed {
						if m.panelVisible() {
							m.closePanel()
							m.refreshStreamStatus()
							return m, nil
						}
						m.setFocus(focusContent)
						m.refreshStreamStatus()
						return m, nil
					}
				default:
					// Engagement: typing (or any composed key) on a screen other
					// than Ask slides the conversation strip out, so the send
					// target is visible instead of the message landing off-screen.
					if consumed {
						m.openPanel()
					}
				}
				if consumed {
					if text := m.dock.SendRequest(); text != "" {
						cmd = m.sendFromComposer(text)
					}
					m.refreshStreamStatus()
					return m, cmd
				}
			}
		}
	}
	if cmd := m.appMsg(msg); cmd != nil {
		m.refreshStreamStatus()
		return m, cmd
	}
	for _, r := range m.routes {
		if r.Match == nil || !r.Match(msg) {
			continue
		}
		if r.Handle(m, msg) {
			m.refreshStreamStatus()
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
	// Defer to the active screen — but ONLY once the content actually holds focus.
	//
	// With the TAB BAR focused (focusTabs) nothing below it has been chosen yet, so
	// a key must not reach a pane: "the project page STILL captures the down/up
	// controls without actually selecting it yet ... No menus should grab up/down
	// until you actually select it." Selecting a submenu entry is what moves focus
	// into the content (MenuSelect), so the arrows arrive exactly when the operator
	// picked the thing they want to arrow through.
	if _, isKey := msg.(tea.KeyMsg); isKey && m.chatFocus != focusContent {
		return m, nil
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
	// CONTENT-REGION WHEEL: scroll the Ask transcript.
	//
	// The wheel was handled ONLY for the conversations rail (above), so on the
	// Ask tab the transcript could not be scrolled at all — the rail claims the
	// wheel (its own column) AND the keyboard (with an empty composer the
	// vertical keys move the rail SELECTION, and railVisible() is true whenever a
	// conversation is open). Nothing was left for the transcript.
	//
	// That is not cosmetic: the transcript follows the TAIL, so once a reply is
	// taller than the pane the operator's own message scrolls off the top with no
	// way back to it and no indication it exists — reported as "I still do not
	// see my initial test user message" and "any additional user messages show
	// up, but that initial message does not", because only the first message was
	// outside the visible window.
	if (mo.Button == tea.MouseButtonWheelUp || mo.Button == tea.MouseButtonWheelDown) && m.active == TabAsk && m.chatConvID != "" {
		delta := -3
		if mo.Button == tea.MouseButtonWheelDown {
			delta = 3
		}
		m.ScrollTranscript(delta)
		m.onChatWake() // keep the scroll indicator honest
		return m, nil
	}
	// Composer row: a click anywhere in the dock block FOCUSES the composer
	// (operator finding 7 — mouse must genuinely focus the input, not only
	// rely on the launch default). The diff rail owns clicks in its own
	// columns, so those still go to the pane.
	//
	// A click anywhere ELSE in the content region must move focus to the
	// CONTENT. Without this the composer kept focus after any pane click, so
	// the screen's own keys were typed into the chat box instead: "v" never
	// switched the work-item view, arrows never moved the list cursor, and
	// the per-screen create/edit/delete keys never fired (operator report:
	// "hitting v does nothing", "I can't move the cursor with the arrow key").
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		inDiffRail := m.diffOpen && mo.X < DiffPaneWidth
		// The slide-out conversation strip owns its own rows: its header carries
		// [minimize] and [continue in conversations], and clicking the body just
		// keeps the composer focus (it is part of the compose area).
		if !inDiffRail && m.panelVisible() {
			if top := m.panelTopRow(); top >= 0 && mo.Y >= top && mo.Y < top+m.panelRows() {
				if mo.Y == top {
					minX0, minX1, eX0, eX1 := m.panelButtonsX(m.width)
					switch {
					case mo.X >= eX0 && mo.X < eX1:
						m.escalateToConversation()
						return m, nil
					case mo.X >= minX0 && mo.X < minX1:
						m.closePanel()
						return m, nil
					}
				}
				return m, nil // consumed: never focus the composer through the strip
			}
		}
		if !inDiffRail && m.welcomeMode() && mo.Y > tabBarRows && mo.Y < m.height-1 {
			// The LAUNCH PAGE's composer is CENTERED in the viewport
			// (centeredWelcomeView), not pinned in the dock rows — so inDockRows
			// does not recognise it and a click fell through to "focus the
			// content", leaving the composer focused but unable to type (the
			// operator's "it doesn't actually allow me to type. Only hitting
			// ctrl+g does that. It is almost like there is a blocker there").
			//
			// On this page there is nothing else to click: no rail (railVisible is
			// false with no conversation and askMode == askNew), no source panes
			// (the Ask screen sets HideSources), no list. The brand and tagline are
			// inert text. So any click in the content region means "put me in the
			// composer", which is exactly what the operator asked for.
			m.setFocus(focusComposer)
			m.footer.ComposerFocus = true
			return m, m.dock.TakeBlinkStart()
		}
		if !inDiffRail && m.inDockRows(mo.Y) {
			m.setFocus(focusComposer)
			// Focusing the composer must NOT open the conversation strip from
			// the Ask LAUNCH PAGE. There, a click meant only to place the
			// CURSOR slid a full conversation on screen — "clicking into the
			// text box on first load opens up a full conversation. I don't
			// want that."
			//
			// The strip keeps its delivered behaviour everywhere else, which
			// is a different and deliberate intent: on a non-Ask screen it
			// shows WHERE a send will land instead of the message going
			// somewhere off-screen (TestSlideOutPanelOpensOnComposerClick) —
			// and on Ask with a session open it is how you continue it.
			// On the ASK TAB the transcript pane IS the destination, so the strip is
			// redundant — and on the launch page it put a whole conversation on
			// screen from a click meant only to place the cursor ("clicking into the
			// text box on first load opens up a full conversation. I don't want
			// that."). Every OTHER tab keeps the delivered behaviour, where the
			// strip shows WHERE a send will land
			// (TestSlideOutPanelOpensOnComposerClick).
			if m.active != TabAsk {
				m.openPanel()
			}
			m.footer.ComposerFocus = true
			// Dispatch the caret's blink starter so the caret animates as soon as
			// the box holds focus — the operator's "the cursor should blink when in
			// the composer and focus is active so people know they truly have
			// focus there."
			return m, m.dock.TakeBlinkStart()
		}
		if !inDiffRail && mo.Y > tabBarRows {
			m.setFocus(focusContent)
			m.footer.ComposerFocus = false
		}
	}
	if cmd := m.appMsg(mo); cmd != nil {
		m.refreshStreamStatus()
		return m, cmd
	}
	for _, r := range m.routes {
		if r.Match == nil || !r.Match(mo) {
			continue
		}
		if r.Handle(m, mo) {
			m.refreshStreamStatus()
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
		if msg.String() == "ctrl+g" {
			// Already handled as a hard chord in dispatch, BEFORE the screen-claims
			// gate — an open form or a latched search box must never be able to
			// swallow the chord that returns focus to the composer.
			return nil
		}
		return nil
	case chat.ConversationsMsg:
		return tea.Batch(m.onConversations(msg), m.waitChat())
	case chat.ConversationCreatedMsg:
		if msg.Err != "" {
			m.dock.SetError(msg.Err)
			return m.waitChat()
		}
		return tea.Batch(m.chat.LoadConversations(), m.waitChat())
	case chat.ConversationMutatedMsg:
		return tea.Batch(m.onConversationMutated(msg), m.waitChat())
	case chat.TranscriptMsg:
		return tea.Batch(m.onTranscript(msg), m.waitChat())
	case chat.ErrMsg:
		m.setChatError(msg.Where, msg.Err)
		return m.waitChat()
	case chat.TurnResolvedMsg:
		return m.waitChat()
	case chat.StreamDoneMsg:
		return tea.Batch(m.onStreamDone(msg), m.waitChat())
	case askDefaultSettingsMsg:
		// Store the tenant default; if a conversation is already open its strip may
		// now be able to resolve a model (and therefore a context window) that it
		// could not before, so re-read the metrics.
		if msg.model != "" && msg.model != m.askDefaultModel {
			m.askDefaultModel = msg.model
			m.ctxWindowFor = ""
			return m.refreshMetrics()
		}
		return nil
	case metricsMsg:
		return m.applyMetrics(msg)
	case chatConvCreatedMsg:
		m.askMode = askConversations // a session now exists: show it
		m.chatConvID = msg.convID
		// The send landed: clear the composer's "sending …" ack (set by the dock
		// when Enter fired) so it cannot linger as a stale promise.
		m.dock.SetNotice("")
		m.chat.SetActive(msg.convID)
		// Optimistic echo of the operator's own message. The existing-conversation
		// path appends this; the create path did not, so the FIRST send from any
		// screen left the transcript (and the slide-out strip) blank until the
		// reply landed — the message looked lost.
		m.chatStore.append(msg.convID, chat.ChatItem{
			Kind: chat.KindUser, Text: msg.text, At: time.Now().UnixMilli(),
			Key: fmt.Sprintf("draft-%d", time.Now().UnixNano()), Live: true,
		})
		cmds := []tea.Cmd{m.chat.Send(msg.convID, msg.text, msg.preamble), m.chat.LoadConversations()}
		// Load the durable transcript and the session metrics EXPLICITLY.
		//
		// This cannot go through OpenAskConversation: that helper early-returns
		// when the conversation is already active (`m.chatConvID == id`), and the
		// handler above just set it — so the durable transcript and
		// refreshMetrics were both skipped for a brand-new conversation. The
		// transcript stayed optimistic-only and the stat strip stayed empty until
		// the turn happened to end.
		cmds = append(cmds, m.chat.OpenConversation(msg.convID), m.refreshMetrics())
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
