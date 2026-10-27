//go:build windows

package tui

// resize_windows.go — keep the TUI resizing on Windows when bubbletea is fed the console as a BYTE stream.
//
// WHY THIS IS NEEDED AT ALL, i.e. what reader_windows.go trades away.
//
// bubbletea's Windows resize detection is a Windows CONSOLE INPUT EVENT (WINDOW_BUFFER_SIZE_EVENT), read by
// the coninput reader (key_windows.go: `case coninput.WindowBufferSizeEventRecord`), and Windows has no
// SIGWINCH (signals_windows.go: listenForResize "is not available on windows"). So the moment input is routed
// to the ANSI byte path — which is what makes bracketed paste and Shift+Enter work — a later resize stops
// being reported.
//
// The INITIAL size still lands, and it is worth being precise about why: handleResize runs a one-shot
// checkResize (tea.go) against the OUTPUT file, which the input switch does not touch. So the shell starts
// correctly sized and simply never learns about a drag. Losing live resize silently would be trading a loud
// bug for a quiet one, so the same information is re-derived here — from the same source bubbletea consults,
// term.GetSize on stdout, polled rather than pushed.
//
// IT REPORTS THE RAW SIZE AND LETS THE SHELL DECIDE. The probe emits terminalSizeProbeMsg (app.go) rather
// than a tea.WindowSizeMsg, because the shell has to DROP an unchanged size: the renderer repaints on every
// WindowSizeMsg, so re-emitting one per tick would repaint an idle chat forever.
//
// THE TICK IS FASTER ON WINDOWS for that reason (refresh.go: refreshPeriodFor), since the tick now doubles as
// the resize poll.

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
