package tui

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// helpModel renders the '?' overlay FROM the KeyRoute registry, so the
// overlay cannot drift from actual behavior (plan §6; app_test asserts
// parity).
type helpModel struct {
	open bool
}

// lines renders the overlay content from the routes.
func helpLines(routes []KeyRoute) []string {
	var global, screen []string
	for _, r := range routes {
		line := padKeys(r.Keys) + "  " + r.Name
		switch r.Scope {
		case "global":
			global = append(global, line)
		default:
			screen = append(screen, line)
		}
	}
	out := []string{theme.ListTitle.Render("orch — keys")}
	out = append(out, "")
	out = append(out, theme.DetailKey.Render("Global"))
	out = append(out, global...)
	if len(screen) > 0 {
		out = append(out, "", theme.DetailKey.Render("Screen"))
		out = append(out, screen...)
	}
	out = append(out, "", theme.HintText.Render("Mouse: click tabs/lists/panes, wheel scrolls. DRAG to select text anywhere on screen — release copies it to the clipboard and a \"copied to clipboard\" confirmation appears (the copy uses the terminal's OSC 52 escape, so the terminal must allow clipboard writes; under tmux set-clipboard must be on)."))
	out = append(out, theme.HintText.Render("Chat dock: the composer is focused at launch — type immediately; ctrl+g/esc toggle content↔composer. / opens the command palette (above the composer, input stays visible). Enter sends; alt+enter (or a trailing \\ then Enter) inserts a newline; pasted text never sends until Enter. /help lists slash commands."))
	out = append(out, theme.HintText.Render("Tabs: enter/space on the active tab opens its submenu of sub-screens (from content focus, or from an empty composer) — up/down (or j/k) move, enter selects, esc closes; the rows are mouse click targets. F1 … F7 switch areas (the key printed in the tab bar, underlined), click a tab to switch + open its menu."))
	out = append(out, theme.HintText.Render("Rails: ctrl+r toggles the Ask conversations rail (open by default, loaded from the API; a failed load shows a retry row, never a silent empty rail); with the rail up and the box EMPTY the arrows move it FROM THE COMPOSER OR THE CONTENT, SPACE marks the highlighted conversation and steps down, ctrl+n RENAMES it, ctrl+t CATEGORIZES it (the whole marked selection when there is one), ctrl+x BULK-DELETES the marked selection after a confirm, and esc CLEARS the marks before doing anything else. There is no bulk action below two marks — one marked row is the row the cursor is on. /rename with no title opens the same prefilled box. GROUPINGS ARE MANAGED IN PLACE, like the GUI: an item's pane is where you put it in a group (ctrl+t here, C on Workers and Workflows), and a group's own row is where you rename or delete it — `e` renames the highlighted grouping, and the shared delete chord (`ctrl+x`, the same one that deletes the marked items on Workers and Workflows) deletes it after a confirm (its items move to Uncategorized). The folder arrow collapses and expands a group. On WORKERS, space marks rows and `ctrl+x` deletes the marked selection while `M` (shift+m) sets the model for all of them at once, in one modal. The LEFT diff rail is the d / shift+d toggle FROM CONTENT FOCUS (esc first — while composing, d is a literal character), or type /diff from the composer. Ctrl+d is never bound. Click the composer to focus it. /connect shows the in-place re-auth overlay (never exits)."))
	out = append(out, theme.HintText.Render("Windows: chords + mouse work in Windows Terminal; legacy conhost may degrade mouse."))
	return out
}

func padKeys(k string) string {
	const w = 16
	if len(k) >= w {
		return k
	}
	return k + strings.Repeat(" ", w-len(k))
}

// view renders the overlay box.
func (h helpModel) view(routes []KeyRoute) string {
	if !h.open {
		return ""
	}
	return theme.HelpOverlay.Render(strings.Join(helpLines(routes), "\n"))
}
