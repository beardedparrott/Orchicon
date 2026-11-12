//go:build windows

package tui

// resize_windows.go — keep the TUI resizing on Windows when bubbletea is fed the console as a BYTE
// stream.

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// resizeProbeCmd reads the terminal size from stdout. It is nil where the platform needs no poll, which is
// what keeps this a one-line seam in the shared refresh tick.
func resizeProbeCmd() tea.Cmd {
	fd := os.Stdout.Fd()
	return func() tea.Msg {
		w, h, err := term.GetSize(fd)
		if err != nil || w <= 0 || h <= 0 {
			return terminalSizeProbeMsg{}
		}
		return terminalSizeProbeMsg{width: w, height: h}
	}
}
