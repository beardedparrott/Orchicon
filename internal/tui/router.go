package tui

// router.go — the global key dispatch layer (plan §6 decision 7).

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
			// OPERATOR SPEC (Phase-3.5 finding 3): Tab is the focus ring — chat prompt FIRST, then the six area
			// tabs in order, then back to the prompt.
			Name: "focus ring", Keys: "tab", Scope: "global",
			Match: func(msg tea.Msg) bool {
				k, ok := msg.(tea.KeyMsg)
				return ok && k.String() == "tab"
			},
			Handle: func(m *App, _ tea.Msg) bool { m.tabRingNext(); return true },
		},
		{
			// Shift+Tab walks the ring BACKWARDS.
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
		// LEFT/RIGHT are NOT tab-chord routes.
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
			Name: "Ask transcript: find (search the transcript)", Keys: "ctrl+f", Scope: "global",
			// CTRL+F IS FIND, THE UNIVERSAL CHORD — not attach.
			Match: keyMatcher("ctrl+f"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.openFindBox()
				return true
			},
		},
		{
			Name: "paste from the clipboard", Keys: "ctrl+v", Scope: "global",
			// The chord that does NOT go through bracketed paste: a screenshot is bytes in the SYSTEM clipboard
			// with no path and no text form.
			Match: keyMatcher("ctrl+v"),
			Handle: func(m *App, _ tea.Msg) bool {
				m.pendingAttachCmd = attachOrPasteFromClipboard()
				return true
			},
		},
		{
			Name: "select all in the composer", Keys: "ctrl+a", Scope: "global",
			// THE WHOLE COMPOSER, IN ONE GESTURE.
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
			// QUIT IS CTRL+C ONLY.
			Name: "quit", Keys: "ctrl+c", Scope: "global",
			Match:  keyMatcher("ctrl+c"),
			Handle: func(m *App, _ tea.Msg) bool { m.quitting = true; return true },
		},
		{
			// Diff sidebar toggle: CTRL+D — the operator's binding ("change the diff panel to be ctrl+d").
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
		// THE DIFF RAIL'S RESIZE FLOOR.
		{
			Name: "widen the split (diff rail, or the screen's tree/detail)", Keys: "ctrl+right", Scope: "global",
			Match:  keyMatcher("ctrl+right"),
			Handle: func(m *App, _ tea.Msg) bool { return m.splitWidthStep(+1) },
		},
		{
			Name: "narrow the split (diff rail, or the screen's tree/detail)", Keys: "ctrl+left", Scope: "global",
			Match:  keyMatcher("ctrl+left"),
			Handle: func(m *App, _ tea.Msg) bool { return m.splitWidthStep(-1) },
		},
		{
			Name: "reset the split width", Keys: "ctrl+down", Scope: "global",
			Match:  keyMatcher("ctrl+down"),
			Handle: func(m *App, _ tea.Msg) bool { return m.splitWidthReset() },
		},
		// THE ASK TRANSCRIPT'S SEARCH + JUMP KEYS, registered here so the `?` overlay
		// names them (help renders FROM this registry; app_test asserts the parity).
		//
		// THE GATE IS THE WHOLE DESIGN, AND IT IS NOT ONE GATE:
		//
		//   - n/N need a SEARCH to step and a keyboard that can spare a bare LETTER, so they
		//     stay gated on Ask-transcript CONTENT focus. A letter in the composer is the
		//     operator's message — the rule composerBypassKeys exists for — so a search key
		//     must never reach it.
		//   - home/end are NOT letters and have no other meaning on this pane, so they follow
		//     transcriptJumpKeysActive: the transcript owns them from the content pane AND
		//     while the COMPOSER is empty — where Home/End are no-ops in the textarea and
		//     were being swallowed there. That is the operator's "the new home/end keys are
		//     not jumping to the top or bottom of a conversation".
		//
		// Every one of them acts through the same helpers (jumpTranscriptKey /
		// scrollTranscriptKey), so the chords, the find box and /find cannot drift. See
		// transcript_search.go.
		{
			Name: "Ask transcript: next search match", Keys: "n", Scope: "global",
			Match: keyMatcher("n"),
			Handle: func(m *App, _ tea.Msg) bool {
				if !m.askTranscriptContentFocus() || strings.TrimSpace(m.askSearch) == "" {
					return false
				}
				m.askSearchStep(1)
				return true
			},
		},
		{
			Name: "Ask transcript: previous search match", Keys: "N", Scope: "global",
			Match: keyMatcher("N"),
			Handle: func(m *App, _ tea.Msg) bool {
				if !m.askTranscriptContentFocus() || strings.TrimSpace(m.askSearch) == "" {
					return false
				}
				m.askSearchStep(-1)
				return true
			},
		},
		{
			Name: "Ask transcript: jump to the first message", Keys: "home", Scope: "global",
			Match: keyMatcher("home"),
			Handle: func(m *App, _ tea.Msg) bool {
				return m.jumpTranscriptKey("home")
			},
		},
		{
			Name: "Ask transcript: jump to the latest message", Keys: "end", Scope: "global",
			Match: keyMatcher("end"),
			Handle: func(m *App, _ tea.Msg) bool {
				return m.jumpTranscriptKey("end")
			},
		},
	}
	// One chord route per tab, in tab order: F1 … F7.
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

// EVERY KEY HERE MUST BE UNABLE TO BE TEXT — see the `q` note in the map.
var composerBypassKeys = map[string]bool{
	"ctrl+r": true, "ctrl+c": true, "ctrl+d": true,
	// NOT a bare `q`, AND ITS ABSENCE IS LOAD-BEARING.
	"tab": true, "shift+tab": true,
	// ctrl+y STOPS the in-flight reply.
	"ctrl+y": true,
	// THE ATTACHMENT/FIND CHORDS are here for exactly the same reason: they must work while the composer
	// is focused, that being where the operator is typing. The textarea binds ctrl+f to forward-char, so
	// without the bypass the find route would never receive the key.
	//
	//	ctrl+v — paste an image from the SYSTEM CLIPBOARD (a screenshot has no path to type)
	//	ctrl+f — FIND IN TRANSCRIPT (the universal chord; it opens a find box over the open
	//	         transcript — see the route above). A FILE is attached with the /attach <path>
	//	         slash command, or by pasting a path.
	"ctrl+v": true, "ctrl+f": true,
	// ctrl+a selects the whole composer (see its route). It has to bypass the textarea for the same reason:
	// the textarea binds ctrl+a to "line start", so without this the route would never see the key.
	"ctrl+a": true,
	// THE DIFF RAIL'S RESIZE CHORDS are here for the same structural reason: they must work while the
	// composer holds the focus, because that is where the operator is typing.
	"ctrl+left": true, "ctrl+right": true, "ctrl+down": true,
}

// The tab chords are ADDED from the SAME source the tab bar draws from.
func init() {
	for _, t := range Tabs {
		composerBypassKeys[t.Chord] = true
	}
}

// screenOwnsTab reports whether the active screen wants Tab for a focus move INSIDE its pane,
// rather than for the shell's tab ring.
func (m *App) screenOwnsTab() bool {
	s := m.screens[m.active]
	if s == nil {
		return false
	}
	ot, ok := s.(interface{ OwnsTab() bool })
	return ok && ot.OwnsTab()
}

// dispatch evaluates the route chain.
func (m *App) dispatch(msg tea.Msg) (*App, tea.Cmd) {
	// EVERY SCREEN IS RE-POINTED AT THIS APP, on every message, because THIS is the App that survives.
	m.rebindScreens()
	// AN ESC-LESS MOUSE REPORT IS NOT TEXT.
	if m.dropOrphanedMouseReport(msg) {
		return m, nil
	}
	if cmd, ok := m.pasteKey(msg); ok {
		return m, cmd
	}
	// THE TERMINAL-SIZE PROBE (resize_probe.go).
	if probe, ok := msg.(terminalSizeProbeMsg); ok {
		if wm := windowSizeFromProbe(probe, m.width, m.height); wm != nil {
			return m.dispatch(wm)
		}
		return m, nil
	}
	if _, ok := msg.(sizeProbeTickMsg); ok {
		return m.handleSizeProbeTick()
	}
	// PLANE STATE AND LIFECYCLE RESULTS land here, in the one message funnel, so a probe or a verb can
	// never be handled twice (or by a screen that does not own the plane).
	if pm, ok := msg.(planeStateMsg); ok {
		return m, m.applyPlaneState(pm.info)
	}
	if om, ok := msg.(planeOpMsg); ok {
		return m, m.applyPlaneOp(om)
	}
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
		// FIRST LAYOUT IS WHERE THE PLANE IS PROBED: the shell is about to start painting live data, and
		// the operator's first question on an empty screen is "is the plane even up".
		if !m.planeProbed {
			m.planeProbed = true
			return m, m.planeProbeCmd()
		}
		return m, nil
	}
	// THE LAUNCH CHECK'S RESULT lands here, and it may arrive BEFORE the prompt is up (m.launch is nil
	// until we decide to ask) — so it has to be handled outside the "prompt is showing" guard below.
	if lm, ok := msg.(launchPromptMsg); ok {
		if lm.need {
			m.beginLaunchPrompt(lm.dir, lm.visible, lm.images)
		}
		return m, nil
	}
	// THE LAUNCH PROMPT OWNS EVERY KEY while it is up — ABOVE the help overlay and above every route,
	// because it is the first screen of the session.
	if m.launch != nil {
		switch msg := msg.(type) {
		case launchCreatedMsg:
			// Done: continue into the app exactly as a normal launch would (the Ask "New" page is the launch
			// default and was never left).
			m.dismissLaunchPrompt()
			cmd := m.applyCreatedProjectScope(msg)
			return m, cmd
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
	// Results for a prompt that is no longer up (declined while the create was in flight, or a
	// duplicate) are SWALLOWED rather than falling through as unknown messages a screen might act on.
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
	// THE FIND BOX OWNS THE KEYBOARD AND THE MOUSE WHILE IT IS UP — AND NOTHING ELSE.
	if m.findBox != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			if handled, cmd := m.findBoxKey(msg); handled {
				return m, cmd
			}
			// The keys the box does NOT take are the shell's hard escapes (ctrl+c quits, ctrl+g returns
			// to the composer), left to the chain below on purpose — see findBoxKey.
		case tea.MouseMsg:
			return m, nil
		}
	}
	// The /models picker overlay owns EVERY key and the mouse while it is open: it is layered above the
	// composer, so a keystroke aimed at the picker can never reach the composer or a screen.
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
	// layered above the composer.
	if m.renameConv != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.renameConvKey(k)
		}
		return m, nil
	}
	// THE MESSAGE EDIT MENU OWNS EVERY KEY WHILE IT IS OPEN, and it is checked here, beside the other
	// App-level overlays, because it is opened by a CLICK rather than a chord.
	if m.msgMenuBlocksKeys() {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m, m.handleMsgMenuKey(k)
		}
		return m, nil
	}
	// A FORM'S OWN ANCILLARY MESSAGES OUTRANK THE FORM GUARD BELOW, and this is the ONE working example
	// of why: the guard swallows EVERY non-key message while a form is open.
	if pm, ok := msg.(versionSpecPrefillMsg); ok {
		m.openInlineSpecForm(&pm.spec, false)
		return m, nil
	}
	// A CREDENTIAL FOR AN INLINE SPEC ARRIVES THE SAME WAY AND FOR THE SAME REASON.
	if cm, ok := msg.(credentialAttachedMsg); ok {
		return m, m.onCredentialAttached(cm)
	}
	// The conversation-scope MCP modal (/mcp define) is the same shape again: it is layered above the
	// composer, so a save chord aimed at the form can never reach a message being typed.
	if m.convScopeForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.convScopeKey(k)
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
	// THE CONVERSATION SCOPE MODAL sits HERE, below the form it can open (convScopeForm) and below the
	// confirm dialog it can raise (bulkConfirm) — so those two, which are layered ON TOP of it.
	if m.scope != nil {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			return m.scopeKey(msg)
		case tea.MouseMsg:
			return m, nil
		case scopeDataMsg:
			m.onScopeData(msg)
			return m, nil
		case scopeSecretsMsg:
			// The tenant secrets store's names, for the credential picker (`r`, and the open path's own
			// read). Discarded unless it is still about THIS surface — see onScopeSecrets.
			m.onScopeSecrets(msg)
			return m, nil
		}
	}
	// The grouping-rename form, opened from a category row in any grouped pane.
	if m.catForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m.catFormKey(k)
		}
		return m, nil
	}
	// The /project picker owns every key while it is up, like the modals above and for the same reason:
	// it is layered over the rail it is about to re-scope.
	if m.projectPick != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			return m, m.projectPickerKeyCmd(k)
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
	// /connect in-place overlay owns ALL messages while open (never quits): keys drive the embedded
	// connection form; the async probe's start/done messages flow through connectTick.
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
	// The copy toast's timer.
	if tm, ok := msg.(clipToastMsg); ok {
		if m.clip != nil {
			m.clip.clearToast(tm.seq)
		}
		return m, nil
	}
	// The ROLLING REFRESH WINDOW's tick (refresh.go).
	if rt, ok := msg.(refreshTickMsg); ok {
		return m.handleRefreshTick(rt)
	}
	// The composer's caret animates from cursor.BlinkMsg ticks, which are NOT key messages — and the
	// dock is only ever handed keys (three call sites, all tea.KeyMsg).
	if bm, ok := msg.(cursor.BlinkMsg); ok {
		_, cmd := m.composerKey(bm)
		return m, cmd
	}
	k, isKey := msg.(tea.KeyMsg)
	// Focus chords come FIRST, as hard escapes alongside ctrl+c — and BEFORE the screen-claims check
	// below.
	if isKey {
		switch k.String() {
		case "ctrl+c":
			if m.chatFocus == focusComposer {
				// hard escape: quit always works, even mid-composition
				m.quitting = true
				return m, tea.Quit
			}
		case "tab":
			// TAB moves through a FORM'S FIELDS whenever a form is open — modal OR inline.
			if fs, ok := m.screens[m.active].(interface{ FormOpen() bool }); ok && fs.FormOpen() {
				return m.passToScreen(msg)
			}
			// A screen may also need Tab for a TWO-REGION toggle inside its own pane — the execution transcript
			// and its message box are two surfaces the operator moves between with Tab.
			if m.screenOwnsTab() {
				return m.passToScreen(msg)
			}
			// Otherwise Tab advances the top-level selection — and stops there.
			m.closeTabMenu()
			m.tabRingNext()
			m.refreshStreamStatus()
			return m, nil
		case "ctrl+g":
			// Focus the composer from ANY state, and LEAVE whatever holds the keys: an open form/modal and a
			// latched search box are both dismissed, so the next keystroke is typing rather than an action.
			if m.screens[m.active] != nil {
				if cl, ok := m.screens[m.active].(interface{ DropKeyClaim() }); ok {
					cl.DropKeyClaim()
				}
			}
			// A DROPDOWN IS ALSO "whatever holds the keys" and is dismissed for the same reason.
			m.closeTabMenu()
			m.setFocus(focusComposer)
			// DEFERRAL AND A LOSS.
			if m.chatStore != nil && m.chatConvID != "" && m.chatStore.hasPendingConsent(m.chatConvID) {
				m.dock.SetNotice("permission card still open — click it to decide, or /fullsend to approve it and stop asking")
			}
			m.refreshStreamStatus()
			return m, nil
		}
	}
	// ONLY while no submenu is open: with one up, Enter belongs to it (it SELECTS the highlighted
	// entry) and reopening here would reset the selection to the first row every press.
	if isKey && m.chatFocus == focusTabs && m.TabMenu() == nil {
		switch k.String() {
		case "enter", " ", "space":
			m.openTabMenu(m.active)
			m.refreshStreamStatus()
			return m, nil
		case "left", "right":
			// Left/right do NOT walk the tab ring — NOT EVEN HERE.
			m.setFocus(focusContent)
			m.refreshStreamStatus()
		case "esc":
			// ESC FROM THE BAR DISENGAGES INTO THE CONTENT — the same place esc from the COMPOSER already goes,
			// so "esc" means one thing at both places the operator starts from and never leaves them.
			if !m.diffOpen {
				m.setFocus(focusContent)
				m.refreshStreamStatus()
				return m, nil
			}
		}
	}
	// Tab dropdown submenu keys: the open menu owns arrows/enter/esc and its own tab chords (BEFORE
	// composer handling so esc closes the menu instead of falling through to focus toggling.
	if isKey && m.TabMenu() != nil {
		if handled, cmd := m.menuHandleKey(k); handled {
			m.refreshStreamStatus()
			return m, cmd
		}
	}
	// Screen-owned input mode: a screen with an open form/modal claims EVERY key (bar the hard chords
	// handled above), so typed characters are never intercepted by shell routes — 'q' would quit.
	if isKey {
		if ks, ok := m.screens[m.active].(interface{ ClaimsKeys() bool }); ok && ks.ClaimsKeys() {
			return m.passToScreen(msg)
		}
	}
	// Ask rail SELECTION: while the conversations rail is up and the composer is empty, Enter/Space
	// open the highlighted conversation (the operator's "I should be able to move up/down with arrow
	// keys.
	if isKey && m.TabMenu() == nil && m.active == TabAsk && m.railVisible() &&
		strings.TrimSpace(m.dock.Value()) == "" &&
		k.String() == "enter" {
		cmd := m.openSelectedRailConversation()
		m.refreshStreamStatus()
		return m, cmd
	}
	// Ask rail MARK: while the rail is up and the composer is empty, SPACE marks the highlighted
	// conversation and advances one row.
	if isKey && m.TabMenu() == nil && m.active == TabAsk && m.railVisible() &&
		strings.TrimSpace(m.dock.Value()) == "" && k.String() == " " {
		m.toggleConvMark()
		m.refreshStreamStatus()
		return m, nil
	}
	// Tab submenu ACTIVATION (Phase 3 finding 3): Enter or Space on the active tab opens its dropdown.
	if isKey && m.TabMenu() == nil && m.menuActivationKey(k) {
		m.openTabMenu(m.active)
		m.refreshStreamStatus()
		return m, nil
	}
	// Composer '/' palette: while open, palette keys own the message (filter/navigate/select); esc
	// hands the key back to the global routes.
	if m.palette.PaletteOpen() && isKey {
		handled, cmd := m.paletteHandleKey(k)
		if handled {
			m.refreshStreamStatus()
			return m, cmd
		}
	} else if isKey && m.chatFocus == focusComposer && k.String() == "/" && strings.TrimSpace(m.dock.Value()) == "" {
		// ONLY ON AN EMPTY COMPOSER, which is the operator's rule: "if you type a / in your prompt after
		// there is already text in the screen.
		_, cmd := m.composerKey(k)
		m.openPalette()
		m.refreshStreamStatus()
		return m, cmd
	}
	// Composer focus (the launch default): KEY messages go to the dock first (typing works
	// immediately), EXCEPT the shell's structural chords — tab switches, ctrl+r rail toggle, q quit,
	// ctrl+c.
	if m.chatFocus == focusComposer {
		if k, isKeyMsg := msg.(tea.KeyMsg); isKeyMsg {
			// Empty composer: the vertical keys drive the conversation list when the conversations rail is up
			// (the operator's "I can't go up/down with the arrow keys").
			if strings.TrimSpace(m.dock.Value()) == "" {
				// THE RAIL'S ITEM CHORDS: ctrl+n renames, ctrl+t categorizes (the whole MARKED SELECTION when there
				// is one), ctrl+x bulk-deletes.
				if handled, cmd := m.railItemKey(k.String()); handled {
					return m, cmd
				}
				// ESC CLEARS THE MULTI-SELECTION before it does anything else — the same order kit2 uses, and the
				// safe one: an operator mid-selection reaching for esc means "drop this selection".
				if k.String() == "esc" && m.clearConvMarks() {
					m.refreshStreamStatus()
					return m, nil
				}
				// THE RAIL OWNS THE VERTICAL KEYS whenever it is on screen with an empty composer — and it claims
				// them from the CONTENT too (the call is below, before the focusContent gate).
				if d := scrollKeyDelta(k.String()); d != 0 {
					if m.railOwnsVerticalKey(k.String()) {
						return m, nil
					}
					if !m.railVisible() || m.active != TabAsk {
						// Any other screen: the vertical keys belong to the SCREEN, which decides for itself — its list
						// moves the cursor, and its DETAIL scrolls once the detail holds focus.
						return m.passToScreen(msg)
					}
				}
				// THE TRANSCRIPT'S NAVIGATION KEYS, FROM THE COMPOSER.
				if m.transcriptNavKey(k.String()) {
					return m, nil
				}
			}
			if composerBypassKeys[k.String()] {
				// structural chord: skip the composer, the routes below
				// own it (switchTab routes, quit, rail toggle).
			} else {
				consumed, cmd := m.composerKey(msg)
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
					// Disengaging keys: they never SLIDE THE PANEL OUT.
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
					// Engagement: typing (or any composed key) on a screen other than Ask slides the conversation strip
					// out, so the send target is visible instead of the message landing off-screen.
					if consumed {
						m.openPanel()
					}
				}
				if consumed {
					// This site used to be the ONLY place that collected it, which is what made the other three callers
					// a message-eating hole — see composerKey.
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
				// The quit route only flips the flag (KeyRoute.Handle cannot carry a Cmd); emit tea.Quit here so
				// the program actually exits.
				return m, tea.Quit
			}
			// A route may have staged a diff-pane setup cmd (e.g. the D toggle's openDiffPane); re-emit it.
			if c := m.drainStaged(); c != nil {
				return m, c
			}
			return m, nil
		}
	}
	// THE RAIL'S KEYS FROM THE CONTENT, BEFORE THE SCREEN GETS THEM.
	if k, isKey := msg.(tea.KeyMsg); isKey && m.chatFocus == focusContent {
		// PANE SELECTION FIRST: left/right choose the rail or the conversation, and the vertical keys then
		// follow that choice (see askPaneKey).
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
		// ESC CLEARS AN ACTIVE TRANSCRIPT SEARCH before its other meanings: the search is a mode the
		// operator entered deliberately, and esc is how they leave it (acceptance.
		if k.String() == "esc" && m.clearAskSearch() {
			return m, nil
		}
		if m.railOwnsVerticalKey(k.String()) {
			return m, nil
		}
		// THE REASONING FOLD, before the transcript's scroll keys so it cannot be swallowed by them.
		if m.active == TabAsk && m.askPane == askPaneConversation && k.String() == "ctrl+o" {
			if cmd := m.toggleLastReasoningBlock(); cmd {
				m.onChatWake()
			}
			return m, nil
		}
		// THE CONVERSATION OWNS THE VERTICAL KEYS WHEN IT IS SELECTED.
		if m.scrollTranscriptKey(k.String()) {
			return m, nil
		}
	}
	// Defer to the active screen — but ONLY once the content actually holds focus.
	if _, isKey := msg.(tea.KeyMsg); isKey && m.chatFocus != focusContent {
		return m, nil
	}
	// Defer to the active screen (its input routes are inside its own
	// Update — chords already consumed above).
	return m.passToScreen(msg)
}

// dispatchMouse routes mouse events: the open dropdown first, then the tab bar (click = switch +
// open its menu), the Ask rail, then fall-through to the screen/pane.
func (m *App) dispatchMouse(mo tea.MouseMsg) (*App, tea.Cmd) {
	// DRAG-RESIZE THE DIFF RAIL — RESOLVED FIRST, ABOVE THE CLIPBOARD BLOCK BELOW.
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft &&
		!m.menuHit(mo.X, mo.Y) && m.diffDividerHit(mo.X, mo.Y) {
		m.diffResizing = true
		m.diffResizeMoved = false      // a press is not yet a drag — see the release below
		m.diffResizePrev = m.diffPaneW // the override to restore if it never becomes one
		m.setDiffPaneW(mo.X + 1)       // seed the override from the column the operator grabbed
		return m, nil                  // never forwarded to the pane: no tab switch, no file select, no ✕
	}
	if m.diffResizing {
		switch mo.Action {
		case tea.MouseActionMotion:
			// The button may arrive as MouseButtonNone (the shape clipState already tolerates, clipboard.go) —
			// the HELD flag lives on the App, not in the button, so the drag holds.
			cells := mo.X + 1
			if cells != m.diffPaneW {
				m.diffResizeMoved = true
			}
			m.setDiffPaneW(cells)
			return m, nil
		case tea.MouseActionRelease:
			m.diffResizing = false
			if !m.diffResizeMoved {
				// A CLICK, NOT A DRAG.
				m.diffPaneW = m.diffResizePrev
				m.refreshLayout()
				return m, nil
			}
			m.persistDiffRailWidth()
			return m, nil
		}
		// Any other action while resizing (e.g. a wheel) falls through to normal handling.
	}
	// DRAG THE DIFF RAIL'S SCROLLBAR — claimed here for the SAME reason the divider is, one block up.
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft &&
		!m.menuHit(mo.X, mo.Y) && m.diffScrollbarHit(mo.X, mo.Y) {
		m.diffScrollDragging = true
		// ON THE THUMB the content is picked up where it is; ON THE TRACK the viewport jumps to the pointer
		// first. Both are the pane's arithmetic — this only forwards the gesture.
		m.diffPane.ScrollbarDragStart(mo.Y)
		return m, nil // never forwarded: no selection region is set over the rail
	}
	if m.diffScrollDragging {
		if !m.diffOpen || m.diffPane == nil {
			// The rail was closed mid-drag (esc, or the ✕). Drop the gesture rather than jump a pane that is
			// no longer on screen.
			m.diffScrollDragging = false
			return m, nil
		}
		switch mo.Action {
		case tea.MouseActionMotion:
			// NO X TEST, deliberately: the gesture was claimed at the press, and the bar must keep following
			// the pointer even when a diagonal hand movement takes it out of the rail's columns.
			m.diffPane.ScrollbarDragTo(mo.Y)
			return m, nil
		case tea.MouseActionRelease:
			m.diffScrollDragging = false
			m.diffPane.ScrollbarDragEnd()
			return m, nil
		}
		// Any other action while dragging (e.g. a wheel) falls through to normal handling.
	}
	// SELECT AND COPY runs ahead of everything else, because it has to work over EVERYTHING: the tab
	// bar, the dropdown, the rails, a pane, the transcript.
	if m.clip != nil {
		// CONFINEMENT IS DECIDED AT THE PRESS, from the pane the drag STARTS in. THE LEFT BUTTON ONLY, the
		// same condition beginDrag uses.
		if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
			// AND IT FOCUSES THE COMPOSER, which is the half that was missing: placing the caret does NOT make
			// the dock focused, and the dock draws its caret ONLY when it is focused (unfocused.
			if m.composerClickAt(mo.X, mo.Y) {
				m.setFocus(focusComposer)
				m.footer.ComposerFocus = true
				return m, m.dock.TakeBlinkStart()
			}
			if r, ok := m.selectionRegionAt(mo.X, mo.Y); ok {
				m.clip.setRegion(r)
			} else {
				m.clip.clearRegion()
			}
		}
		if consumed, copyText := m.clip.handleMouse(mo); consumed {
			if copyText != "" {
				return m, m.clip.copyCmd(copyText)
			}
			return m, nil
		}
	}
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		// A TAB CLICK IS RESOLVED FIRST, ahead of the menu's outside-click close.
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
	// The wheel was handled ONLY for the conversations rail (above), so on the Ask tab the transcript
	// could not be scrolled at all.
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonRight &&
		m.active == TabAsk && m.chatConvID != "" && !m.msgMenuBlocksKeys() {
		if msgID, text, ok := m.transcriptUserMessageIDAtFrameRow(mo.Y); ok {
			m.openMsgMenu(m.chatConvID, msgID, text)
			return m, nil
		}
	}
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft && m.active == TabAsk && m.chatConvID != "" {
		// A CODE BLOCK IS CHECKED FIRST, because it sits INSIDE a message: the message rule below would
		// otherwise win and copy the whole message — band indent, prose and all.
		if src, ok := m.transcriptCodeBlockAtFrameRow(mo.Y); ok && m.clip != nil {
			return m, m.clip.copyCmd(src)
		}
		if text, ok := m.transcriptUserMessageAtFrameRow(mo.Y); ok && m.clip != nil {
			return m, m.clip.copyCmd(text)
		}
		// AN OPTION ON A CLARIFYING-QUESTION CARD IS A CHOICE, SO A CLICK ON IT SENDS THAT CHOICE.
		if kind, key, label, ok := m.transcriptCardOptionAtFrameRow(mo.Y); ok && m.chat != nil {
			if kind == chat.KindConsent {
				return m, m.consentDecideFromRow(label)
			}
			// THE RECORDED CARD'S FREE-TEXT ROW OPENS AN INPUT ON THE CARD, rather than sending the row's own
			// label as the answer.
			if label == chat.ConsentOther {
				if m.chatStore.beginAskDraft(m.chatConvID, key) {
					m.reArmAskDraftClaim()
					return m, m.onChatWake()
				}
				return m, nil
			}
			// A CLICK ON A ROW IS THE OPERATOR ACTING ON THE CARD, so the card takes the keyboard back and its
			// cursor lands on the row they clicked.
			m.chatStore.selectAskRow(m.chatConvID, key, label)
			m.reArmAskCardClaim()
			// THE ASK ID IS WHAT MAKES THIS A REPLY. See Controller.AnswerQuestion: without it the
			// answer is just a message and the awaiting marker — the card's state — is never cleared.
			return m, m.chat.AnswerQuestion(m.chatConvID, m.chatStore.askIDForKey(m.chatConvID, key), label)
		}
	}
	// THE DIFF RAIL OWNS THE WHEEL IN ITS OWN COLUMNS.
	if (mo.Button == tea.MouseButtonWheelUp || mo.Button == tea.MouseButtonWheelDown) && m.active == TabAsk && m.chatConvID != "" && !m.diffRailOwnsX(mo.X) {
		delta := -3
		if mo.Button == tea.MouseButtonWheelDown {
			delta = 3
		}
		m.ScrollTranscript(delta)
		m.onChatWake() // keep the scroll indicator honest
		return m, nil
	}
	// Composer row: a click anywhere in the dock block FOCUSES the composer (operator finding 7 — mouse
	// must genuinely focus the input, not only rely on the launch default).
	if mo.Action == tea.MouseActionPress && mo.Button == tea.MouseButtonLeft {
		inDiffRail := m.diffOpen && mo.X < m.diffPaneWidth()
		// The slide-out conversation strip owns its own rows: its header carries [minimize] and [continue
		// in conversations], and clicking the body just keeps the composer focus (it is part of the
		// compose.
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
			// The LAUNCH PAGE's composer is CENTERED in the viewport (centeredWelcomeView), not pinned in the
			// dock rows — so inDockRows does not recognise it and a click fell through to "focus the content".
			m.setFocus(focusComposer)
			m.footer.ComposerFocus = true
			return m, m.dock.TakeBlinkStart()
		}
		if !inDiffRail && m.inDockRows(mo.Y) {
			m.setFocus(focusComposer)
			// Focusing the composer must NOT open the conversation strip from the Ask LAUNCH PAGE.
			if m.active != TabAsk {
				m.openPanel()
			}
			m.footer.ComposerFocus = true
			// Dispatch the caret's blink starter so the caret animates as soon as the box holds focus — the
			// operator's "the cursor should blink when in the composer and focus is active so people know they.
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
		// operator was going to paste it anyway.
		m.dock.InsertText(msg.text)
		m.dock.SetError("")
		m.dock.SetNotice(fmt.Sprintf("pasted %d characters from the clipboard", len([]rune(msg.text))))
		m.refreshComposerHint()
		return nil
	case chatCmdMsg:
		return tea.Batch(msg.cmd, m.waitChat())
	case tea.KeyMsg:
		if msg.String() == "ctrl+g" {
			// Already handled as a hard chord in dispatch, BEFORE the screen-claims gate — an open form or a
			// latched search box must never be able to swallow the chord that returns focus to the composer.
			return nil
		}
		return nil
	case chat.ConversationsMsg:
		return tea.Batch(m.onConversations(msg), m.waitChat())
	case railProjectsMsg:
		// The project folder names, fetched alongside the conversations (see loadRailProjects).
		return m.onRailProjects(msg)
	case chat.ConversationProjectSetMsg:
		if msg.Err != "" {
			m.dock.SetError("project move failed: " + msg.Err)
			return m.waitChat()
		}
		// The rail re-fetches rather than patching the row locally: the move changes which FOLDER the chat
		// is in, and the folder list is a server-side fact.
		m.dock.SetNotice("moved to " + m.projectLabelFor(msg.ProjectID))
		return tea.Batch(m.reloadConversations(), m.waitChat())
	case chat.ConversationCreatedMsg:
		if msg.Err != "" {
			m.dock.SetError(msg.Err)
			return m.waitChat()
		}
		return tea.Batch(m.chat.LoadConversations(), m.waitChat())
	case chat.ConversationMutatedMsg:
		return tea.Batch(m.onConversationMutated(msg), m.waitChat())
	case workerVersionScopeMsg:
		// A worker's version landed: open the MCP + skills modal on it (see App.OpenWorkerMCPModal).
		return tea.Batch(m.onWorkerVersionScope(msg), m.waitChat())
	case convScopeMsg:
		// THE SCOPE MODAL RE-READS AFTER EVERY WRITE IT CAUSED, whatever the outcome: a definition added,
		// edited or deleted, a credential stored, an install started, a skill path set or removed.
		var cmd tea.Cmd
		if m.scope != nil {
			cmd = m.loadScope()
		}
		if msg.err != "" {
			m.dock.SetError(msg.op + " failed: " + msg.err)
			return cmd
		}
		if msg.detail != "" {
			m.dock.SetNotice(msg.op + ": " + msg.detail)
		}
		// A definitions write changes nothing the RAIL shows, but the skill-files write does (the list
		// carries the conversation's skill_files), so both re-read.
		if msg.op == "/skills" {
			return tea.Batch(cmd, m.reloadConversations())
		}
		return cmd
	case chat.TranscriptMsg:
		return tea.Batch(m.onTranscript(msg), m.waitChat())
	case chat.ErrMsg:
		m.setChatError(msg.Where, msg.Err)
		// AND THE FAILURE IS SURFACED ON THE TRANSCRIPT, not only on the composer strip.
		return tea.Batch(m.surfaceTurnFailure(msg.ConvID, msg.Err), m.waitChat())
	case chat.AbortTurnMsg:
		// The Stop outcome.
		if msg.Err != "" {
			m.dock.SetError("stop failed: " + msg.Err)
		} else {
			m.dock.SetNotice("reply stopped")
		}
		m.refreshComposerHint()
		m.onChatWake()
		return tea.Batch(m.chat.LoadConversations(), m.waitChat())
	case chat.TurnAckedMsg:
		// THE SEND RESOLVED: the server acked the turn, so the composer's "sending …" is no longer true and
		// is settled.
		m.dock.SettleSendingAck()
		// AND THE RAIL IS REFRESHED AT ONCE, so the row marks the conversation it just started.
		return tea.Batch(m.reloadConversations(), m.waitChat())
	case chat.TurnResolvedMsg:
		return m.waitChat()
	case chat.StreamDoneMsg:
		return tea.Batch(m.onStreamDone(msg), m.waitChat())
	// NO ConsentAskMsg / ConsentResolvedMsg CASES: an ask and its resolution now reach the store
	// DIRECTLY from the turn's goroutine (appEventStore.ShowConsentAsk / SettleConsentAsk) instead of
	// riding.
	case chat.PermissionGrantsMsg:
		// The SERVER's answer to a list or a revoke (see App.ConsentGrants / ConsentRevoke). It carries the
		// refreshed list in BOTH cases, so this client never keeps its own copy to drift.
		return tea.Batch(m.applyConsentGrants(msg), m.waitChat())
	case chat.ConsentRepliedMsg:
		// The SERVER's verdict on a decision we sent.
		switch {
		case msg.Err != "":
			m.dock.SetNotice("permission reply failed: " + msg.Err)
		case msg.Expired || !msg.Applied:
			// AND THE CARD STOPS CLAIMING A SCOPE IT NEVER GOT.
			m.settleConsentScope(msg.ConvID, msg.AskID, "")
			m.dock.SetNotice("that permission ask is no longer open — nothing was applied")
		default:
			// THE GRANT IS RECORDED HERE, on the verdict that says the server actually applied it.
			if dir := m.settleConsentScope(msg.ConvID, msg.AskID, "session · "); dir != "" {
				m.dock.SetNotice("allowed for this session · " + dir)
				// THE SERVER'S LIST JUST CHANGED, and this shell shows it (the grants header and /grants read the
				// server's answer), so it re-fetches rather than guessing that the grant it just watched being.
				return tea.Batch(m.loadConsentGrants(msg.ConvID), m.onChatWake(), m.waitChat())
			}
			m.dock.SetNotice("permission decision applied")
		}
		return tea.Batch(m.onChatWake(), m.waitChat())
	case askDefaultSettingsMsg:
		// Store the tenant default; if a conversation is already open its strip may now be able to resolve
		// a model (and therefore a context window) that it could not before, so re-read the metrics.
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
		// A SEND THAT CREATES A CONVERSATION OPENS IT — and this path sets chatConvID directly, without
		// going through OpenAskConversation.
		m.railSyncSelectionToOpenConversation()
		// The send landed: settle the composer's "sending …" ack (set by the dock when Enter fired) so it
		// cannot linger as a stale promise.
		m.dock.SettleSendingAck()
		m.chat.SetActive(msg.convID)
		// Optimistic echo of the operator's own message.
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
		cmds = append(cmds, m.chat.OpenConversation(msg.convID), m.refreshMetrics())
		// The new conversation has no detail open: without a RequestDetail the onChatWake
		// DetailID()==chatConvID guard never passes and the fresh turn's chunks land in chatStore but never
		// repaint (first.
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
