package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/stream"
)

// streamStatusString aliases stream.Status — the footer consumes the same
// values useStream.ts emits (idle|connecting|open|reconnecting|closed|error).
type streamStatusString = stream.Status

const openStatus = stream.StatusOpen

// statusRank ranks statuses (higher = worse) for the footer's worst-wins
// aggregation.
func statusRank(s stream.Status) int {
	switch s {
	case stream.StatusIdle:
		return 0
	case stream.StatusOpen:
		return 1
	case stream.StatusConnecting:
		return 2
	case stream.StatusClosed:
		return 3
	case stream.StatusError:
		return 4
	case stream.StatusReconnecting:
		return 5
	default:
		return 0
	}
}

// lipglossPlace centers the help overlay in the terminal.
func lipglossPlace(w, h int, content string) string {
	if w < 1 || h < 1 {
		return content
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}
