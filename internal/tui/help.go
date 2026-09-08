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
	out = append(out, "", theme.HintText.Render("Mouse: click tabs/lists/panes, wheel scrolls. Shift+drag stays free for native select-copy."))
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
