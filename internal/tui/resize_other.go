//go:build !windows

package tui

// resize_other.go — the non-Windows half of the resize seam.
//
// There is NOTHING TO POLL here: bubbletea reports resize through SIGWINCH on these platforms
// (signals_unix.go -> checkResize), so the probe is nil. The seam still exists so the shared refresh tick has
// ONE call site on every platform rather than a build-tagged branch in the middle of it.

import tea "github.com/charmbracelet/bubbletea"

// resizeProbeCmd is nil: the platform delivers its own resize events.
func resizeProbeCmd() tea.Cmd { return nil }
