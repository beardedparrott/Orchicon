package tui

// mouse_leak.go — AN ORPHANED MOUSE REPORT MUST NOT BE TYPED INTO THE COMPOSER.

import (
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// sgrMouseReport matches an SGR mouse report EXACTLY, without its ESC: the `<` marks the SGR form,
// the three numbers are button+modifiers, column and row.
var sgrMouseReport = regexp.MustCompile(`^\[<\d+;\d+;\d+[Mm]$`)

// orphanedMouseReport reports whether a key message is the ESC-less tail of a mouse report.
func orphanedMouseReport(k tea.KeyMsg) bool {
	if k.Type != tea.KeyRunes || len(k.Runes) < 2 {
		return false
	}
	s := string(k.Runes)
	if !strings.HasPrefix(s, "[<") {
		return false
	}
	if sgrMouseReport.MatchString(s) {
		return true
	}
	// A TRUNCATED report: only the characters a report can be made of, so a textarea's `[<` followed by a
	// letter or space is untouched.
	if len(s) < 3 {
		return false
	}
	for _, r := range s[2:] {
		if (r < '0' || r > '9') && r != ';' && r != 'M' && r != 'm' {
			return false
		}
	}
	return true
}

// dropOrphanedMouseReport consumes the tail of a split mouse report, reporting whether the message
// was one.
func (m *App) dropOrphanedMouseReport(msg tea.Msg) bool {
	k, ok := msg.(tea.KeyMsg)
	return ok && orphanedMouseReport(k)
}
