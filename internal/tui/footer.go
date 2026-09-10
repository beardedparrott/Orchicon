package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// footerModel renders the status strip: connection state, URL, server
// version (+drift warning), identity, context chip.
type footerModel struct {
	URL           string
	ServerVersion string
	ClientVersion string
	Identity      string
	ConnState     string // "connected" | "connecting" | "disconnected, retrying" | …
	StreamStatus  stream.Status
	ContextChip   string // current project / work item
	Width         int
	MouseEnabled  bool // set by the shell; renders "Mouse Enabled" chip
	ComposerFocus bool // set by the shell; renders the focus hint
}

// versionDrift reports whether client and server versions disagree
// (dev builds never warn — plan §3).
func versionDrift(server, clientV string) bool {
	if server == "" || clientV == "" || clientV == "dev" {
		return false
	}
	return server != clientV
}

func connLabel(st stream.Status) (string, string) { // label, themed state
	switch st {
	case stream.StatusOpen:
		return "connected", "ok"
	case stream.StatusConnecting:
		return "connecting…", "busy"
	case stream.StatusReconnecting, stream.StatusError:
		// graceful degradation: prior state preserved, banner in footer
		return "disconnected, retrying", "err"
	case stream.StatusClosed:
		return "disconnected, retrying", "err"
	default:
		return "idle", "dim"
	}
}

// View renders the one-line footer.
func (f footerModel) View() string {
	label, kind := connLabel(f.StreamStatus)
	var state string
	switch kind {
	case "ok":
		state = theme.StatusOK.Render("● " + label)
	case "busy":
		state = theme.StatusBusy.Render("◐ " + label)
	case "err":
		state = theme.StatusErr.Render("○ " + label)
	default:
		state = theme.HintText.Render("· " + label)
	}

	ver := "server " + orDash(f.ServerVersion)
	if versionDrift(f.ServerVersion, f.ClientVersion) {
		ver += " " + theme.FooterVersionDrift.Render("⚠ drift: client "+f.ClientVersion)
	}

	parts := []string{state, orDash(f.URL), ver}
	if f.Identity != "" {
		parts = append(parts, "identity: "+f.Identity)
	}
	if f.ContextChip != "" {
		parts = append(parts, theme.ListMeta.Render(f.ContextChip))
	}
	if f.ComposerFocus {
		parts = append(parts, theme.StatusOK.Render("— esc/ctrl+g content"))
	} else {
		parts = append(parts, theme.HintText.Render("ctrl+g composer"))
	}
	if f.MouseEnabled {
		parts = append(parts, theme.StatusOK.Render("Mouse Enabled"))
	}
	line := strings.Join(parts, theme.HintText.Render(" · "))
	// Phase 2a (operator finding 5): the footer is ONE row on every screen.
	// It truncates (ANSI-aware) to the terminal width instead of wrapping —
	// a wrapping footer steals a budget row and breaks the composer-flush
	// layout. Priority order: connection state · URL · version · drift ·
	// identity · context chip · mouse · focus hint (the hint is the first
	// casualty of a narrow terminal, never the connection state).
	if f.Width > 0 {
		line = ansi.Truncate(line, f.Width-2, "")
	}
	return theme.Footer.Render(line)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
