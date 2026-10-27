package tui

// resize_probe.go — the shell-side half of the terminal-size probe, on every platform.
//
// WHAT IT IS FOR. On Windows, bubbletea's resize detection is a console INPUT event, and the input path that
// makes bracketed paste and Shift+Enter work is the one that bypasses it (reader_windows.go). So the shell
// re-derives the size itself, on a chain of its own.
//
// ITS OWN CHAIN, NOT THE REFRESH TICK. The shell already has a rolling refresh window (refresh.go), and at
// first glance this belongs on it. It does not: that tick re-reads the ACTIVE VIEW — real queries against the
// plane — and it is deliberately polite. Running it four times a second to serve a resize probe would
// multiply backend load for a cosmetic need. A size probe is one ioctl, so it gets its own cheap timer and
// leaves the refresh cadence alone.
//
// ONLY A CHANGE IS FORWARDED. The standard renderer repaints on every WindowSizeMsg, so emitting one per tick
// would repaint an idle chat several times a second for no reason.
//
// THE TYPE AND THE COMPARISON LIVE HERE, UNTAGGED, because the shell handles the message on every platform;
// only the POLLING differs (resize_windows.go polls term.GetSize, resize_other.go returns nil because
// SIGWINCH already reaches the shell).

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// terminalSizeProbeInterval is how often the size is re-derived where a poll is needed — the same order as a
// human noticing a window drag. A VARIABLE so a test can shorten it rather than waiting.
var terminalSizeProbeInterval = 250 * time.Millisecond

// terminalSizeProbeMsg carries a freshly observed terminal size.
//
// width/height are 0 when the size could not be read — "no information", which is not the same as "size 0"
// and must not be acted on.
type terminalSizeProbeMsg struct {
	width, height int
}

// sizeProbeTickMsg re-arms the size probe chain.
type sizeProbeTickMsg struct{}

// sizeProbeTickCmd arms the next size probe. Nil where the platform needs no poll, which is what keeps the
// chain from ever starting on Linux/macOS.
func sizeProbeTickCmd() tea.Cmd {
	if resizeProbeCmd() == nil {
		return nil // platform delivers its own resize events
	}
	return tea.Tick(terminalSizeProbeInterval, func(time.Time) tea.Msg { return sizeProbeTickMsg{} })
}

// handleSizeProbeTick reads the size once and re-arms, so the probe is a window rather than a one-shot.
func (m *App) handleSizeProbeTick() (*App, tea.Cmd) {
	probe := resizeProbeCmd()
	if probe == nil {
		return m, nil
	}
	return m, tea.Batch(probe, tea.Tick(terminalSizeProbeInterval, func(time.Time) tea.Msg { return sizeProbeTickMsg{} }))
}

// windowSizeFromProbe turns a probe result into a tea.WindowSizeMsg, or nil when the shell should IGNORE it.
//
// TWO REASONS TO IGNORE, and both are load-bearing:
//
//   - the size could not be read (0,0): acting on it would resize the shell to nothing;
//   - the size has NOT CHANGED: see the header — the renderer repaints on every WindowSizeMsg, so forwarding
//     an identical size on every tick would repaint an idle chat several times a second.
//
// Pure, so both rules are pinned without a terminal.
func windowSizeFromProbe(msg terminalSizeProbeMsg, currentWidth, currentHeight int) tea.Msg {
	if msg.width <= 0 || msg.height <= 0 {
		return nil
	}
	if msg.width == currentWidth && msg.height == currentHeight {
		return nil
	}
	return tea.WindowSizeMsg{Width: msg.width, Height: msg.height}
}
