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
			Handle: func(m *App, _ tea.Msg) bool {
				if m.screenOwnsTab() {
					// The pane owns Tab, so the ring must NOT also act on it: the key falls through to the
					// screen, which toggles between its own two regions.
					return false
				}
				m.tabRingPrev()
				return true
			},
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
			// STOP THE IN-FLIGHT REPLY — the GUI's Stop button, on a key.
			//
			// The operator: "I realized there is no stop button like in the GUI. We need a stop
			// button/control key + hint in composer that can interrupt the agent mid flight."
			//
			// WHY ctrl+y. Every other candidate is taken or is an EDITING key, and stealing an
			// editing key would break typing to add a feature:
			//   - the textarea's own keymap claims ctrl+k (delete after cursor), ctrl+w (delete word
			//     backward), ctrl+u (delete before cursor), ctrl+b/ctrl+f (character movement),
			//     ctrl+n/ctrl+p (line movement), ctrl+a/ctrl+e (line start/end), ctrl+v (paste) and
			//     ctrl+t (transpose) — see bubbles textarea's DefaultKeyMap;
			//   - ctrl+i IS tab and ctrl+j IS enter (control bytes 0x09 / 0x0a, measured — the composer
			//     treats ctrl+j as Enter for exactly this reason), and ctrl+q is what ctrl+1 arrives as
			//     on many emulators (the tab-chord note in app.go);
			//   - ctrl+x (rail bulk-delete), ctrl+n (rename), ctrl+t (categorize), ctrl+s (form save),
			//     ctrl+z (escalate), ctrl+d (diff), ctrl+r (rail), ctrl+g (focus) and ctrl+c (quit) are
			//     already bound here.
			// ctrl+y is free, is not an editing key in this textarea, and reads as "yield" — hand
			// control back to the operator.
			//
			// IT IS A GLOBAL ROUTE so it works from the content pane as well as from the composer (the
			// GUI's Stop button is on screen whatever the operator is doing), and it sits BELOW the
			// screen-claims gate on purpose: a screen with an open form claims every key, so ctrl+y can
			// never silently stop a reply while the operator is filling in a form.
			//
			// A KeyRoute.Handle cannot return a Cmd, so the abort is STAGED and re-emitted by
			// drainStaged — the same pattern the diff-pane and rail routes use.
			Name: "stop the in-flight reply (interrupt the agent)", Keys: "ctrl+y", Scope: "global",
			Match: keyMatcher("ctrl+y"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.pendingStopCmd = m.stopReply()
				return true
			},
		},
		{
			Name: "attach a file by path", Keys: "ctrl+f", Scope: "global",
			// Reads the file OFF THE DISK in a command (it can be megabytes), so the tea loop is never
			// blocked by I/O — the same reason every fetch in this shell is a command.
			//
			// The PATH comes from the composer's own text, because a terminal cannot browse a filesystem:
			// the operator pastes or types the path (which is how a path reaches a terminal anyway), and a
			// successful attach CLEARS the box so the path is not also sent as prose.
			Match: keyMatcher("ctrl+f"),
			Handle: func(m *App, _ tea.Msg) bool {
				path := m.dock.Value()
				m.pendingAttachCmd = attachFileFromPrompt(path)
				if strings.TrimSpace(path) != "" {
					m.pendingAttachClear = true
				}
				return true
			},
		},
		{
			Name: "paste from the clipboard", Keys: "ctrl+v", Scope: "global",
			// The chord that does NOT go through bracketed paste: a screenshot is bytes in the SYSTEM clipboard
			// with no path and no text form, so it has to be read out with a platform helper (see
			// readClipboardImage for why OSC 52 cannot do it). TEXT on the clipboard is pasted into the
			// composer instead — see attachOrPasteFromClipboard for why one gesture serves both.
			Match: keyMatcher("ctrl+v"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.pendingAttachCmd = attachOrPasteFromClipboard()
				return true
			},
		},
		{
			Name: "select all in the composer", Keys: "ctrl+a", Scope: "global",
			// THE WHOLE COMPOSER, IN ONE GESTURE. The operator: "It would be nice to allow a ctrl+a to select
			// all text in the composer so a user can easily copy or delete it all if possible."
			//
			// "if possible" is load-bearing: bubbles' textarea has NO selection model (v1.0.0 has no Select,
			// SelectAll or selection state), so there is nothing to highlight and no ctrl+c to drive. What the
			// operator actually wants is the two OUTCOMES — copy it, or delete it — so this copies the buffer
			// to the clipboard on the way in and puts the composer into a state where the next replacing key
			// throws it away (dock.SelectAll). The alternative was a chord that looked like a selection and
			// did nothing.
			//
			// It takes over ctrl+a, which the textarea bound to "line start". Home still does that, and the
			// trade is the standard one every text field makes.
			Match: keyMatcher("ctrl+a"),
			Handle: func(m *App, _ tea.Msg) bool {
				text := m.dock.Value()
				if text == "" {
					m.dock.SetNotice("composer is empty — nothing to select")
					return true
				}
				m.dock.SelectAll()
				if m.clip != nil {
					m.pendingClipCmd = m.clip.copyCmd(text)
				}
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
	// One chord route per tab, in tab order: F1 … F7.
	//
	// The chord is taken from the Tab itself, so the key the operator presses and the key printed in the
	// tab bar are the same fact. It used to be a hand-listed ctrl+letter — and one of those (ctrl+e)
	// was ALSO the composer's end-of-line binding, which is the kind of collision a derived key cannot
	// have.
	//
	// The HANDLER is shared with the tab click and with the chord-pressed-while-a-menu-is-open case
	// (openTabWithMenu): go to the tab AND drop its submenu down with the keyboard in it. This route
	// used to do SwitchTo ALONE, which is why the chord left the menu shut and Enter then went wherever
	// the screen underneath wanted — the operator's "when hitting the F#, it automatically gains focus
	// on the first submenu in the list and hitting enter doesn't bring down the submenu".
	for i := range tabs {
		tab := tabs[i]
		routes = append(routes, KeyRoute{
			Name:  "switch to " + tab.Title,
			Keys:  tab.Chord,
			Scope: "global",
			Match: keyMatcher(tab.Chord),
			Handle: func(m *App, _ tea.Msg) bool {
				m.openTabWithMenu(tab.ID)
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

// composerBypassKeys are the structural chords that reach the global routes even while the composer is
// focused (the launch default): tab switches, the conversations-rail toggle, and quit. The textarea
// would otherwise consume them as editing no-ops (it consumes EVERY key — unknown chords are silent
// no-ops that still report consumed), locking the shell chrome behind a focus escape forever.
//
// EVERY KEY HERE MUST BE UNABLE TO BE TEXT — see the `q` note in the map. Tab and the ctrl chords
// qualify; a printable character never does, because while the composer holds the focus a printable
// character is the operator's message.
var composerBypassKeys = map[string]bool{
	"ctrl+r": true, "ctrl+c": true, "ctrl+d": true,
	// NOT a bare `q`, AND ITS ABSENCE IS LOAD-BEARING. It used to be here, which made the LETTER q skip
	// the composer and fall through to the global routes — one of which is `quit` on `q`/`ctrl+c`. So
	// TYPING THE LETTER q ANYWHERE IN A CHAT MESSAGE EXITED ORCH AND DROPPED TO THE COMMAND LINE, without
	// even inserting the character first. The operator: "The last two times I was typing a message into
	// the chat, orch just exited and dropped to the command line. Are you able to see the reason why?" —
	// no message containing q (question, request, quote) could be typed at all, and it had been that way
	// since Phase 2a with nothing asserting otherwise.
	//
	// The rule this map exists for is that the textarea consumes EVERY key it is handed, so a global chord
	// the composer is allowed to eat is unreachable. Every OTHER entry is a ctrl chord or tab: keys that
	// can only be a command. `q` is TEXT, and a key that is text while typing must never be a bypass —
	// stated generally, so the class is impossible rather than this instance fixed.
	//
	// Quitting from the composer is still reachable by ctrl+c, which the router documents as the hard
	// escape that "always works, even mid-composition". `q` remains the quit when the composer does NOT
	// hold the focus (content focus, the rails, a list) — conventional, and unchanged, because the
	// composer branch is simply not reached there.
	"tab": true, "shift+tab": true,
	// ctrl+y STOPS the in-flight reply. It is here for the same structural reason as the others, and it
	// is the reason the stop chord works at all: the textarea CONSUMES every key it is handed (unknown
	// chords are silent no-ops that still report consumed), and the composer branch RETURNS the moment a
	// key is consumed — so a global route can never see a chord the composer was allowed to eat. This
	// map is the documented way to keep such a chord reachable while the composer holds the focus.
	"ctrl+y": true,
	// THE ATTACHMENT CHORDS are here for exactly the same reason: they must work while the composer is
	// focused, because that is where the operator is typing when they paste.
	//
	//	ctrl+v — paste an image from the SYSTEM CLIPBOARD (a screenshot has no path to type)
	//	ctrl+f — attach a FILE BY PATH (the operator pastes the path into the prompt; a file may never
	//	         have been on the clipboard, and a terminal cannot browse a filesystem)
	"ctrl+v": true, "ctrl+f": true,
	// ctrl+a selects the whole composer (see its route). It has to bypass the textarea for the same reason:
	// the textarea binds ctrl+a to "line start", so without this the route would never see the key.
	"ctrl+a": true,
}

// The tab chords are ADDED from the SAME source the tab bar draws from.
//
// They were previously spelled out as literals in this map (under a comment claiming they were
// derived, which is how a claim like that rots): that is the FOURTH place the old ctrl+letter chords
// were written down, and it is exactly how a chord change can leave the composer swallowing the key
// instead of switching tabs. Deriving them makes a chord change a one-line change in `Tabs`.
//
// An F-key is safe to bypass: a textarea has no meaning for F1–F7 (it binds no F-key at all), so
// consuming one here cannot take a keystroke away from typing. (The alternative is not academic —
// this list once held ctrl+e, which IS a textarea's end-of-line, so the composer lost that binding
// to a tab switch.)
func init() {
	for _, t := range Tabs {
		composerBypassKeys[t.Chord] = true
	}
}

// screenOwnsTab reports whether the active screen wants Tab for a focus move INSIDE its pane,
// rather than for the shell's tab ring.
//
// It is an OPTIONAL hook (tui screens implement it when they have two regions to move between), and
// it is deliberately distinct from FormOpen, which the Tab chord also consults: FormOpen additionally
// makes the composer advertise a FORM's keys, and a transcript is not a form.
func (m *App) screenOwnsTab() bool {
	s := m.screens[m.active]
	if s == nil {
		return false
	}
	ot, ok := s.(interface{ OwnsTab() bool })
	return ok && ot.OwnsTab()
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
	// THE LAUNCH CHECK'S RESULT lands here, and it may arrive BEFORE the prompt is up
	// (m.launch is nil until we decide to ask) — so it has to be handled outside the
	// "prompt is showing" guard below, or the very message that RAISES the prompt
	// would fall through to the screens and be lost.
	if lm, ok := msg.(launchPromptMsg); ok {
		if lm.need {
			m.beginLaunchPrompt(lm.dir, lm.visible, lm.mcpServers, lm.images)
		}
		return m, nil
	}
	// THE LAUNCH PROMPT OWNS EVERY KEY while it is up — ABOVE the help overlay and
	// above every route, because it is the first screen of the session: a keystroke
	// answering its question must not also open help, switch tabs, or reach the
	// composer behind it. Same discipline as the rename and category modals.
	if m.launch != nil {
		switch msg := msg.(type) {
		case launchCreatedMsg:
			// Done: continue into the app exactly as a normal launch would (the Ask
			// "New" page is the launch default and was never left).
			m.dismissLaunchPrompt()
			return m, nil
		case launchFailedMsg:
			// KEEP THE FORM OPEN with the reason on it, so the operator can correct
			// and retry rather than losing everything they typed.
			if m.launch.Form != nil {
				m.launch.Form.SubmitErr = msg.err.Error()
			}
			return m, nil
		case tea.KeyMsg:
			return m.launchKey(msg)
		}
		return m, nil
	}
	// Results for a prompt that is no longer up (declined while the create was in
	// flight, or a duplicate) are SWALLOWED rather than falling through as unknown
	// messages a screen might act on.
	switch msg.(type) {
	case launchCreatedMsg, launchFailedMsg:
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
	// The rename modal owns EVERY key while it is up, for the same reason the model picker does: it is
	// layered above the composer, so a keystroke aimed at the form can never reach the composer or a
	// screen behind it. It is checked HERE, next to the other App-level overlays, so no route below can
	// claim a key first (notably ctrl+s, which is the form's save chord).
	if m.renameConv != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.renameConvKey(k)
		}
		return m, nil
	}
	// The assign-or-create modal is the same shape and the same discipline: every key, above every
	// route and every screen claim, so ctrl+s can never be swallowed by the screen behind it.
	if m.assignForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.assignCategoryKey(k)
		}
		return m, nil
	}
	// The bulk confirm dialog (a destructive rail operation) is the same shape again: it owns every key
	// while it is open, so the confirm cannot be answered by a keystroke that also did something else.
	if m.bulkConfirm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.bulkConfirmKey(k)
		}
		return m, nil
	}
	// The grouping-rename form, opened from a category row in any grouped pane.
	if m.catForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.catFormKey(k)
		}
		return m, nil
	}
	// Category write results and the category list land here rather than in a screen: the modal and the
	// cache are the shell's, so the shell reconciles them.
	switch msg := msg.(type) {
	case categoriesLoadedMsg:
		return m, m.onCategoriesLoaded(msg)
	case categoriesMutatedMsg:
		return m, m.onCategoriesMutated(msg)
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
	// The copy toast's timer. It is the TUI's own message (never produced by a screen or the
	// wire), so it is safe to consume here — and it must be, or the confirmation would stay on
	// screen until something else happened to repaint.
	if tm, ok := msg.(clipToastMsg); ok {
		if m.clip != nil {
			m.clip.clearToast(tm.seq)
		}
		return m, nil
	}
	// The ROLLING REFRESH WINDOW's tick (refresh.go). It is the shell's own message — no screen
	// produces it — so it is consumed here, ahead of the screens, and it re-arms itself. Handled
	// before the screens so a tick can never be mistaken for a screen's business.
	if rt, ok := msg.(refreshTickMsg); ok {
		return m.handleRefreshTick(rt)
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
			// A screen may also need Tab for a TWO-REGION toggle inside its own pane — the execution
			// transcript and its message box are two surfaces the operator moves between with Tab, and
			// the operator's report is what a missing hook costs: "once you go into a chat in an
			// execution you are locked in it and can't get out." Either way the shell was moving the top
			// menu while the caret stayed in the chat, which reads as being stuck.
			//
			// This is a SEPARATE hook from FormOpen on purpose: FormOpen also decides that the composer
			// advertises the FORM's keys (ctrl+s / esc), which would be a lie over a transcript.
			if m.screenOwnsTab() {
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
			// A DROPDOWN IS ALSO "whatever holds the keys" and is dismissed for the same reason. It
			// outranks every screen claim (menuHandleKey runs ahead of the gate below), so leaving it
			// up made ctrl+g a HALF-escape: the composer had the caret and the menu still owned Enter
			// and the arrows — one screen state with the keyboard split in two, which is the exact
			// confusion this key exists to end. ctrl+g is the documented way back to typing (the
			// footer and the help overlay both name it), so it must land somewhere a keystroke means
			// "type".
			m.closeTabMenu()
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
		case "esc":
			// ESC FROM THE BAR DISENGAGES INTO THE CONTENT — the same place esc from the COMPOSER
			// already goes, so "esc" means one thing at both places the operator starts from and
			// never leaves them somewhere their typing is silently discarded.
			//
			// It was DEAD here, and that became load-bearing with this change: an F-key now lands
			// on the bar with the submenu down, so its first esc closes the MENU (menuHandleKey,
			// standard) — and the second esc had nowhere to go. With the keyboard on the bar and
			// nothing selected below it, typing went to the screen, so the operator's next letters
			// ran screen actions instead of filling the box: the trap TestFocusChordAndFallthrough
			// pins by asserting "esc must return focus to content" after a chord.
			//
			// The DIFF PANE keeps precedence, exactly as delivered: it is a global route further
			// down, so when the pane is open esc still closes it first and the content is reached on
			// the next press.
			if !m.diffOpen {
				m.setFocus(focusContent)
				m.refreshStreamStatus()
				return m, nil
			}
		}
	}
	// Tab dropdown submenu keys: the open menu owns arrows/enter/esc and
	// its own tab chords (BEFORE composer handling so esc closes the menu
	// instead of falling through to focus toggling — no focus trap).
	//
	// THIS RUNS BEFORE THE SCREEN-CLAIMS GATE BELOW, and the order is load-bearing.
	// The menu is drawn ON TOP of every pane and is the thing the operator just opened
	// and is looking at, so it must outrank a screen's claim on the keyboard. With the
	// gate first, any latched claim made the open menu completely inert — the exact
	// defect where a stray `formLoading = true` inside the Projects pane's action builder
	// killed the Work submenu's arrows/Enter AND the shell's shift+tab.
	//
	// It also has to be above the gate because a MOUSE click can open the menu while a
	// screen claims keys (dispatchMouse handles the menu at the very top of dispatch,
	// before the gate), so gate-first left a mouse-opened menu dead on the same path.
	//
	// This steals nothing: menuHandleKey returns handled=false for every key it does not
	// own, so typing still reaches the screen (and the rest of the chain) normally.
	if isKey && m.TabMenu() != nil {
		if handled, cmd := m.menuHandleKey(k); handled {
			m.refreshStreamStatus()
			return m, cmd
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
	// Ask rail SELECTION: while the conversations rail is up and the composer
	// is empty, Enter/Space open the highlighted conversation (the operator's
	// "I should be able to move up/down with arrow keys and space or enter
	// selects"). This is deliberately BEFORE the tab-menu activation below:
	// on Ask the rail is the list in focus, so Enter must select the
	// conversation rather than pop the New/Conversations menu. The menu is
	// still reachable from every other tab (and here whenever the rail is
	// hidden — the launch page shows no list).
	// SPACE IS NO LONGER AN OPEN KEY. It MARKS now (railbulk.go) — the operator's "Spacebar selects" —
	if isKey && m.TabMenu() == nil && m.active == TabAsk && m.railVisible() &&
		strings.TrimSpace(m.dock.Value()) == "" &&
		k.String() == "enter" {
		cmd := m.openSelectedRailConversation()
		m.refreshStreamStatus()
		return m, cmd
	}
	// Ask rail MARK: while the rail is up and the composer is empty, SPACE marks the highlighted
	// conversation and advances one row — the gesture every other list in this client uses
	// (kit2.ToggleMark + Move(1); see railbulk.go).
	//
	// IT IS CLAIMED HERE, AHEAD OF THE TAB-SUBMENU ACTIVATION BELOW, because that activation also fires
	// on SPACE with an empty composer. Without this branch, removing space from the open gesture above
	// would have made it pop the tab menu instead — worse than either behaviour it replaced.
	if isKey && m.TabMenu() == nil && m.active == TabAsk && m.railVisible() &&
		strings.TrimSpace(m.dock.Value()) == "" && k.String() == " " {
		m.toggleConvMark()
		m.refreshStreamStatus()
		return m, nil
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
	} else if isKey && m.chatFocus == focusComposer && k.String() == "/" && strings.TrimSpace(m.dock.Value()) == "" {
		// The composer OWNS the typing: the "/" goes into the buffer
		// FIRST so the operator's text ("/pro") stays visible in the bar
		// while the palette above filters on it (Phase 3 finding 4 — the
		// palette used to swallow the query and the bar stayed empty).
		//
		// ONLY ON AN EMPTY COMPOSER, which is the operator's rule: "if you type a / in your
		// prompt after there is already text in the screen, we should assume the user is NOT
		// trying to run a slash command and should NOT pop up the slash command reference
		// box." A slash mid-sentence is punctuation — a path, a date, "and/or" — and opening a
		// command list over what someone is writing is the modal-steals-your-keystrokes class
		// of annoyance. With the condition unmet the key falls through to the dock below and is
		// typed literally.
		//
		// The empty test is TrimSpace, matching the rule the rail's chords use: a buffer of
		// only whitespace is empty for this purpose, so leading spaces do not block a command.
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
				// THE RAIL'S ITEM CHORDS: ctrl+n renames, ctrl+t categorizes (the whole MARKED
				// SELECTION when there is one), ctrl+x bulk-deletes. One implementation for the
				// composer and the content, so the two cannot disagree about what the rail's
				// selection is — see railbulk.go.
				if handled, cmd := m.railItemKey(k.String()); handled {
					return m, cmd
				}
				// ESC CLEARS THE MULTI-SELECTION before it does anything else — the same order kit2
				// uses, and the safe one: an operator mid-selection reaching for esc means "drop this
				// selection", not "unfocus the pane". With nothing marked it keeps its meaning.
				if k.String() == "esc" && m.clearConvMarks() {
					m.refreshStreamStatus()
					return m, nil
				}
				// THE RAIL OWNS THE VERTICAL KEYS whenever it is on screen with an empty composer —
				// and it claims them from the CONTENT too (the call is below, before the
				// focusContent gate). Both sites share railOwnsVerticalKey, because they did not
				// share anything before and that is exactly what broke: see the helper for the bug
				// the composer-only wiring caused.
				if d := scrollKeyDelta(k.String()); d != 0 {
					if m.railOwnsVerticalKey(k.String()) {
						return m, nil
					}
					if !m.railVisible() || m.active != TabAsk {
						// Any other screen: the vertical keys belong to the SCREEN, which
						// decides for itself — its list moves the cursor, and its DETAIL
						// scrolls once the detail holds focus. The shell scrolling the detail
						// here instead is why the arrow keys never moved a list from the
						// composer (the operator's "the default Projects view under Work
						// captures the down arrows").
						return m.passToScreen(msg)
					}
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
	// THE RAIL'S KEYS FROM THE CONTENT, BEFORE THE SCREEN GETS THEM.
	//
	// This is the fix for the operator's report. The rail's arrows were wired into the COMPOSER branch
	// only, so with the keyboard in the content — which is where they are when reading the transcript,
	// and what the footer's "ctrl+g composer" tells them — the key fell through to the Ask screen. That
	// screen has a HIDDEN "conversations" source (HideSources), so its cursor moved and the pane
	// re-requested that conversation's detail: the transcript changed while the rail's highlight never
	// moved. "it moves through the conversations but the currently selected conversation selector is
	// not moving."
	//
	// The empty-composer rule is deliberately NOT applied here: it exists only because the composer needs
	// the arrows for text navigation, and from content focus the composer does not have the keys at all.
	// A draft is left untouched by this path.
	if k, isKey := msg.(tea.KeyMsg); isKey && m.chatFocus == focusContent {
		// PANE SELECTION FIRST: left/right choose the rail or the conversation, and the
		// vertical keys then follow that choice (see askPaneKey). It is ahead of the rail's
		// item chords because left/right are not among them.
		if handled, cmd := m.askPaneKey(k.String()); handled {
			return m, cmd
		}
		if handled, cmd := m.railItemKey(k.String()); handled {
			return m, cmd
		}
		if k.String() == "esc" && m.clearConvMarks() {
			m.refreshStreamStatus()
			return m, nil
		}
		if m.railOwnsVerticalKey(k.String()) {
			return m, nil
		}
		// THE REASONING FOLD, before the transcript's scroll keys so it cannot be swallowed by them.
		//
		// ctrl+o rather than a bare letter: every letter on this pane is a rail chord (e: rename,
		// ctrl+n: new, space: mark), and the transcript itself is not a text input, so a ctrl chord is the
		// only shape guaranteed free. It toggles the LAST reasoning block — the newest one, which is the
		// one the operator is looking at while a reply streams — because the Ask transcript has no
		// per-block cursor and adding one would make every arrow key ambiguous between scrolling and
		// selecting.
		if m.active == TabAsk && m.askPane == askPaneConversation && k.String() == "ctrl+o" {
			if cmd := m.toggleLastReasoningBlock(); cmd {
				m.onChatWake()
			}
			return m, nil
		}
		// THE CONVERSATION OWNS THE VERTICAL KEYS WHEN IT IS SELECTED. Without this the
		// transcript had no keyboard scroll at all: the rail claimed these keys whenever it
		// was visible — which is whenever a conversation is open — so the operator's only way
		// to read back through a long reply was the mouse wheel.
		if m.active == TabAsk && m.askPane == askPaneConversation && m.chatConvID != "" {
			if d := scrollKeyDelta(k.String()); d != 0 {
				m.ScrollTranscript(d)
				m.onChatWake() // keep the scroll indicator honest
				return m, nil
			}
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
	// SELECT AND COPY runs ahead of everything else, because it has to work over EVERYTHING: the
	// tab bar, the dropdown, the rails, a pane, the transcript. The shell owns the frame, so it
	// is the only layer that can select across all of them (clipboard.go).
	//
	// It is deliberately ahead of the menu handling: while a drag is live the operator is
	// selecting text, and the press that started it has already been delivered (a press is never
	// consumed — that is what keeps clicking intact), so nothing here can swallow a click that a
	// click should have got.
	if m.clip != nil {
		if consumed, copyText := m.clip.handleMouse(mo); consumed {
			if copyText != "" {
				return m, m.clip.copyCmd(copyText)
			}
			return m, nil
		}
	}
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		// A TAB CLICK IS RESOLVED FIRST, ahead of the menu's outside-click close.
		//
		// The two are in the same gesture — row 0 is the bar, which is always outside the panel —
		// and the close would erase the state the tab's own toggle depends on: with the menu
		// closed first, "click the open tab again" would look like "no menu is open", i.e. a
		// REOPEN, and the menu could never be closed by mouse. That is why this used to capture
		// MenuOpenID() into a `wasOpen` and re-derive the toggle inline; resolving the click first
		// lets the toggle live in ONE place (openTabWithMenu) instead of a fourth copy of it.
		//
		// Nothing is lost by the reorder: the dropdown renders BELOW the bar, so a press at y == 0
		// can never be inside the panel or on its header/border — the two cases handled just below.
		if mo.Y == 0 {
			if id, ok := m.TabClick(mo.X); ok {
				// The click and the chord are the SAME behaviour: go to the tab and drop its
				// submenu. It used to be spelled out again here, which is how they came to disagree.
				m.openTabWithMenu(id)
				return m, nil
			}
		}
		if m.TabMenu() != nil {
			if m.MenuClick(mo.X, mo.Y) {
				return m, nil
			}
			if m.menuHit(mo.X, mo.Y) {
				return m, nil // header/border: keep the menu open
			}
			m.closeTabMenu() // click outside closes (standard menu behavior)
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
			max := len(m.railRows()) - m.railVisibleRows()
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
		inDiffRail := m.diffOpen && mo.X < m.diffPaneWidth()
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
	case attachResultMsg:
		// An attachment acquisition landed. It is an App-level message (the pending set is the shell's), and
		// it reports its own outcome — success says WHAT was attached, failure says WHY not.
		m.applyAttachResult(msg)
		return nil
	case clipboardTextMsg:
		// ctrl+v found TEXT on the clipboard rather than an image: paste it into the composer, where the
		// operator was going to paste it anyway. It says how much, so a chord that inserts an invisible
		// number of characters — or none, from a whitespace clipboard — is not mistaken for a dead key.
		m.dock.InsertText(msg.text)
		m.dock.SetError("")
		m.dock.SetNotice(fmt.Sprintf("pasted %d characters from the clipboard", len([]rune(msg.text))))
		m.refreshComposerHint()
		return nil
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
	case chat.AbortTurnMsg:
		// The Stop outcome. The controller has ALREADY cleared the turn slot (that is what makes Stop
		// instant), so this repaint is what makes the thinking indicator and the composer's stop
		// affordance disappear TOGETHER — and the rail reload clears the conversation's "running"
		// marker once the server finalizes the abort.
		if msg.Err != "" {
			m.dock.SetError("stop failed: " + msg.Err)
		} else {
			m.dock.SetNotice("reply stopped")
		}
		m.refreshComposerHint()
		m.onChatWake()
		return tea.Batch(m.chat.LoadConversations(), m.waitChat())
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
			// The first message of a NEW conversation carries its attachment markers too, so a screenshot
			// sent as the opening turn is visible on the transcript rather than only in the server's copy.
			Attachments: m.pendingAttachMarkers(),
		})
		cmds := []tea.Cmd{m.sendChat(msg.convID, msg.text, msg.preamble), m.chat.LoadConversations()}
		// The turn is in flight as of the line above (chat.Send flips the slot synchronously), so the
		// composer's stop affordance has to appear with it.
		m.refreshComposerHint()
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
