// keyprobe is a diagnostic: it prints the EXACT bubbletea key events a terminal
// delivers, so a key that "does nothing" in the real TUI can be identified instead
// of guessed at.
//
// Why it exists: the composer's Enter appeared dead on one terminal while
// Shift+Enter inserted the literal text "0M" — the signature of an escape sequence
// the input parser does not understand, with its tail leaking into the buffer as
// runes. bubbletea has several spellings for Enter (CR -> KeyEnter, LF -> KeyCtrlJ)
// and terminals may additionally send CSI-u / modifyOtherKeys encodings that
// bubbletea v1.3.10 does not decode. Reading a KeyMsg is the only way to know which
// one a given terminal produces.
//
// Run it and press the keys that misbehave:
//
//	export PATH="$PWD/.dev/tools/go/bin:$PATH" GOPATH="$PWD/.dev/tools/gopath" \
//	       GOCACHE="$PWD/.dev/tools/gocache" GOTMPDIR="$PWD/.dev/tools/gotmp"
//	go run ./tools/keyprobe
//
// Every event is listed newest-first as `type=<n> string=<quoted> runes=[...]`, so a
// screenshot of the screen is enough to report back. It opens with the SAME program
// options the real TUI uses (alt screen, mouse cell motion, focus reporting),
// because those options are part of what the terminal chooses an encoding for.
package main

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const keep = 14

type probe struct {
	lines []string
	wide  int
}

func (p probe) Init() tea.Cmd { return nil }

func (p probe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		// Never let the probe trap the operator: ctrl+c always exits.
		if m.String() == "ctrl+c" {
			return p, tea.Quit
		}
		p.push(fmt.Sprintf("KEY   type=%-4d string=%-14q alt=%-5v paste=%-5v runes=%v",
			int(m.Type), m.String(), m.Alt, m.Paste, m.Runes))
	case tea.MouseMsg:
		p.push(fmt.Sprintf("MOUSE x=%-4d y=%-4d button=%v action=%v", m.X, m.Y, m.Button, m.Action))
	case tea.WindowSizeMsg:
		p.wide = m.Width
	default:
		p.push(fmt.Sprintf("%T", msg))
	}
	return p, nil
}

func (p *probe) push(s string) {
	p.lines = append([]string{s}, p.lines...)
	if len(p.lines) > keep {
		p.lines = p.lines[:keep]
	}
}

func (p probe) View() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("keyprobe — press the key that misbehaves") + "\n\n")
	b.WriteString("newest first. ctrl+c quits.\n\n")
	if len(p.lines) == 0 {
		b.WriteString("(nothing yet)\n")
	}
	for _, l := range p.lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func main() {
	// The SAME options the real client uses: these decide which keyboard protocol
	// the terminal engages, so a probe with different options could report a
	// different encoding than the TUI sees.
	p := tea.NewProgram(&probe{},
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "keyprobe:", err)
		os.Exit(1)
	}
}
