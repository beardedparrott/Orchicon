package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// chordApp builds a shell over stub screens, one per tab, so a chord test touches no plane.
func chordApp(t *testing.T) *App {
	t.Helper()
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	for _, tab := range Tabs {
		m.RegisterScreen(tab.ID, &stubScreen{id: string(tab.ID)})
	}
	return m
}

// TestTabChordDropsSubmenuAndTakesFocus is the operator's report, pinned:
//
//	"when hitting the F#, it automatically gains focus on the first submenu in the list and hitting
//	 enter doesn't bring down the submenu. Maybe we should make the action when you hit the F# key, it
//	 goes to the menu, but also automatically drops the submenu down and gains focus to that."
//
// Driven from each tab's OWN chord, so the test follows the bindings rather than restating them.
func TestTabChordDropsSubmenuAndTakesFocus(t *testing.T) {
	m := chordApp(t)
	for _, tab := range Tabs {
		m := m
		nm, _ := m.Update(keyFor(tab.Chord))
		got := nm.(*App)
		if got.ActiveTab() != tab.ID {
			t.Fatalf("%s: active = %q, want %q", tab.Chord, got.ActiveTab(), tab.ID)
		}
		// THE SUBMENU IS DOWN. This is the half that was missing: the chord switched the tab and
		// opened nothing, so the menu the operator could see in their head was not on screen.
		if got.MenuOpenID() != tab.ID {
			t.Fatalf("%s: submenu not dropped (menuOpen=%q, want %q)", tab.Chord, got.MenuOpenID(), tab.ID)
		}
		// AND THE KEYBOARD IS IN IT, which is what makes the next key unambiguous.
		if got.chatFocus != focusTabs {
			t.Fatalf("%s: the keyboard must be in the submenu it opened, got focus %d", tab.Chord, got.chatFocus)
		}
		// The selection starts at the TOP of the menu.
		if tm := got.TabMenu(); tm == nil || tm.Sel != 0 {
			t.Fatalf("%s: the new submenu must start on its first entry, got %+v", tab.Chord, tm)
		}
		// ENTER BELONGS TO THE MENU: it SELECTS the highlighted entry and hands the keyboard to the
		// content — it does not fall through to whatever the screen underneath wanted, which is the
		// failure the operator described.
		nm, _ = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
		got = nm.(*App)
		if got.MenuOpenID() != "" {
			t.Fatalf("%s: enter must select from the submenu and close it, menuOpen=%q", tab.Chord, got.MenuOpenID())
		}
		if got.chatFocus != focusContent {
			t.Fatalf("%s: selecting an entry must hand the keyboard to the content, got focus %d", tab.Chord, got.chatFocus)
		}
	}
}

// TestChordEnterBeatsAKeyClaimingScreen is the "on CERTAIN screens" half of the report.
//
// The chord used to leave the menu shut, so Enter went to the screen — and a screen with an open form
// or a latched search box claims EVERY key, so the submenu could never drop. With the menu open the
// shell's menu handling runs ahead of that claim, so Enter is the menu's on every screen.
func TestChordEnterBeatsAKeyClaimingScreen(t *testing.T) {
	m := chordApp(t)
	claim := &claimStub{id: TabWork, claim: true}
	m.RegisterScreen(TabWork, claim)

	nm, _ := m.Update(keyFor(tabChord(TabWork)))
	m = nm.(*App)
	if m.MenuOpenID() != TabWork {
		t.Fatalf("the chord must drop the submenu even when the screen claims keys, menuOpen=%q", m.MenuOpenID())
	}

	claim.keys = nil
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	if m.MenuOpenID() != "" {
		t.Fatalf("enter must operate the open submenu, menuOpen=%q", m.MenuOpenID())
	}
	if len(claim.keys) != 0 {
		t.Fatalf("the claiming screen must NOT receive the enter that selected a submenu entry, got %v", claim.keys)
	}
}

// TestChordEnterBeatsTheAskRailSelection is the concrete "certain screen" the operator hit: on Ask,
// with the conversations rail up, an empty composer's Enter opens the HIGHLIGHTED CONVERSATION — which
// is delivered behaviour, and which is precisely why Enter could not drop the submenu there. With a
// submenu open the rail must not get the key at all (its branch requires the menu to be shut).
func TestChordEnterBeatsTheAskRailSelection(t *testing.T) {
	m := chordApp(t)
	m.askMode = askConversations
	m.chatConvID = "conv-1"
	m.convRailOpen = true

	nm, _ := m.Update(keyFor(tabChord(TabAsk)))
	m = nm.(*App)
	if m.MenuOpenID() != TabAsk {
		t.Fatalf("the chord must drop the Ask submenu, menuOpen=%q", m.MenuOpenID())
	}
	before := m.chatConvID
	_ = before
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	// DECISIVE, and it does not depend on which entry the Ask menu happens to highlight: the rail's
	// Enter leaves the menu OPEN (its branch is skipped, not served), so a closed menu proves the MENU
	// took the key. Asserting on the conversation instead would be ambiguous — the Ask menu's own
	// first entry is "New chat", which legitimately clears it.
	if m.MenuOpenID() != "" {
		t.Fatalf("enter must operate the open submenu, menuOpen=%q (the rail's Enter leaves it open)", m.MenuOpenID())
	}
	if m.chatFocus != focusContent {
		t.Fatalf("selecting a menu entry must hand the keyboard to the content, got focus %d", m.chatFocus)
	}
}

// TestRepeatedChordTogglesTheSubmenu: re-selecting the tab whose submenu is already down CLOSES it.
// The click route, SwitchTo's same-tab path and menuHandleKey all already honoured this, so the chord
// must too — otherwise the same key would close the menu in one route and reopen it in another.
func TestRepeatedChordTogglesTheSubmenu(t *testing.T) {
	m := chordApp(t)
	nm, _ := m.Update(keyFor(tabChord(TabWork)))
	m = nm.(*App)
	if m.MenuOpenID() != TabWork {
		t.Fatalf("first press must open the submenu, got %q", m.MenuOpenID())
	}
	nm, _ = m.Update(keyFor(tabChord(TabWork)))
	m = nm.(*App)
	if m.MenuOpenID() != "" {
		t.Fatalf("re-pressing the same chord must close the submenu, got %q", m.MenuOpenID())
	}
	if m.ActiveTab() != TabWork {
		t.Fatalf("the re-press must not leave the tab, got %q", m.ActiveTab())
	}
}

// TestChordAndClickSelectATabTheSameWay pins the reason the chord was wrong: this behaviour existed
// THREE times (the chord route, menuHandleKey, and the tab click) and the chord route was the copy
// that had drifted. One gesture must not depend on the input device.
func TestChordAndClickSelectATabTheSameWay(t *testing.T) {
	byKey := chordApp(t)
	nm, _ := byKey.Update(keyFor(tabChord(TabExecution)))
	byKey = nm.(*App)

	byClick := chordApp(t)
	nm, _ = byClick.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: byClick.tabStartCol(Tabs[3]), Y: 0,
	})
	byClick = nm.(*App)

	if byKey.ActiveTab() != byClick.ActiveTab() {
		t.Fatalf("chord and click selected different tabs: %q vs %q", byKey.ActiveTab(), byClick.ActiveTab())
	}
	if byKey.MenuOpenID() != byClick.MenuOpenID() {
		t.Fatalf("chord and click left different menus: %q vs %q", byKey.MenuOpenID(), byClick.MenuOpenID())
	}
	if byKey.chatFocus != byClick.chatFocus {
		t.Fatalf("chord and click left the keyboard in different places: %d vs %d", byKey.chatFocus, byClick.chatFocus)
	}
}

// TestCtrlGDismissesTheSubmenuAndFocusesTheComposer: ctrl+g is the DOCUMENTED way back to typing (the
// footer and the help overlay both name it), and it promises to "leave whatever holds the keys". A
// dropdown holds the keys — it outranks every screen claim — so leaving it up made ctrl+g a
// half-escape: the caret was in the composer while Enter and the arrows still belonged to the menu.
func TestCtrlGDismissesTheSubmenuAndFocusesTheComposer(t *testing.T) {
	m := chordApp(t)
	nm, _ := m.Update(keyFor(tabChord(TabControl)))
	m = nm.(*App)
	if m.MenuOpenID() == "" {
		t.Fatal("precondition: the chord must drop a submenu")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = nm.(*App)
	if m.MenuOpenID() != "" {
		t.Fatalf("ctrl+g must dismiss the dropdown it is leaving, menuOpen=%q", m.MenuOpenID())
	}
	if m.chatFocus != focusComposer || !m.dock.Focused {
		t.Fatal("ctrl+g must put the keyboard in the composer")
	}
	// And the next keystroke is TYPING, not a menu action — the point of dismissing it.
	nm, _ = m.Update(keyRunes("ok"))
	m = nm.(*App)
	if m.dock.Value() != "ok" {
		t.Fatalf("the keystroke after ctrl+g must reach the composer, got %q", m.dock.Value())
	}
}

// TestFooterFocusHintFollowsTheKeyboard: the footer's composer indicator is the ONLY on-screen
// evidence of where the keyboard is, so a stale value is worse than none. It was maintained in three
// places in the MOUSE handler and recomputed on the pass-to-screen path — neither covers the keyboard,
// so Tab or a chord left the footer claiming "composer" while the composer was blurred and typing went
// nowhere. It is derived in setFocus now, and this asserts the derivation, not the call sites.
func TestFooterFocusHintFollowsTheKeyboard(t *testing.T) {
	m := chordApp(t)
	if !m.footer.ComposerFocus {
		t.Fatal("the composer is focused at launch, so the footer must say so")
	}
	nm, _ := m.Update(keyFor(tabChord(TabExecution)))
	m = nm.(*App)
	if m.footer.ComposerFocus {
		t.Fatal("the footer still claims the composer has the keyboard while a chord has put it on the bar")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = nm.(*App)
	if !m.footer.ComposerFocus {
		t.Fatal("the footer must follow the keyboard back into the composer")
	}
	// Tab to the bar, the other keyboard route there.
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(*App)
	if m.footer.ComposerFocus {
		t.Fatal("tab moved the keyboard to the bar, so the footer must not claim the composer")
	}
}

// TestEscapeFromTheBarDisengagesIntoContent: with a chord now landing on the bar with the submenu
// down, its first esc closes the menu (standard) and the SECOND must not be dead — it disengages into
// the content, exactly where esc from the composer goes, so the operator is never parked in a state
// where typing is silently discarded.
func TestEscapeFromTheBarDisengagesIntoContent(t *testing.T) {
	m := chordApp(t)
	nm, _ := m.Update(keyFor(tabChord(TabAutomation)))
	m = nm.(*App)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(*App)
	if m.MenuOpenID() != "" {
		t.Fatalf("the first esc must close the submenu, menuOpen=%q", m.MenuOpenID())
	}
	if m.chatFocus != focusTabs {
		t.Fatalf("closing the submenu must leave the keyboard on the bar, got %d", m.chatFocus)
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(*App)
	if m.chatFocus != focusContent {
		t.Fatalf("the second esc must disengage into the content, got focus %d", m.chatFocus)
	}
}
