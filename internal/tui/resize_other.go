//go:build !windows

package tui

// resize_other.go — the non-Windows half of the resize seam.

import tea "github.com/charmbracelet/bubbletea"

// resizeProbeCmd is nil: the platform delivers its own resize events.
func resizeProbeCmd() tea.Cmd { return nil }
