// Package theme is the single source of lipgloss styling for the orch
// TUI. Every later TUI feature (chat dock, diff sidebar, …) imports this
// package; nothing else constructs lipgloss styles.
//
// The palette mirrors the GUI's dark glass aesthetic: deep slate/indigo
// surfaces with cyan→indigo accents for active elements (the GUI's
// ACTIVE_ITEM gradient is `from-cyan-500 to-indigo-500` — nav-config.ts).
// Colors are declared as AdaptiveColor so lipgloss/termenv degrade
// automatically: truecolor terminals get the exact values, 256-color
// terminals the nearest map, 16-color terminals plain emphasis.
package theme

import "github.com/charmbracelet/lipgloss"

// Adaptive palette. "Dark" values target the dark-glass look; the light
// variants keep the TUI legible for users on light terminal themes.
var (
	// Surface colors: layered panes over a deep slate base.
	Bg         = lipgloss.AdaptiveColor{Dark: "#0f1420", Light: "#f4f6fb"}
	Surface    = lipgloss.AdaptiveColor{Dark: "#171e2e", Light: "#e9edf6"}
	SurfaceAlt = lipgloss.AdaptiveColor{Dark: "#1f2940", Light: "#dde4f0"}
	Border     = lipgloss.AdaptiveColor{Dark: "#2c3a57", Light: "#c3cde0"}
	BorderFaint = lipgloss.AdaptiveColor{Dark: "#222c44", Light: "#d3dbea"}

	// Text.
	Text       = lipgloss.AdaptiveColor{Dark: "#dbe4f5", Light: "#1c2434"}
	TextDim    = lipgloss.AdaptiveColor{Dark: "#8391ad", Light: "#5a6478"}
	TextFaint  = lipgloss.AdaptiveColor{Dark: "#5b6880", Light: "#8792a6"}

	// Accents: cyan→indigo is the GUI's active-item gradient.
	AccentCyan   = lipgloss.AdaptiveColor{Dark: "#06b6d4", Light: "#0891b2"}
	AccentIndigo = lipgloss.AdaptiveColor{Dark: "#6366f1", Light: "#4f46e5"}

	// Status.
	OK    = lipgloss.AdaptiveColor{Dark: "#34d399", Light: "#059669"}  // emerald
	Warn  = lipgloss.AdaptiveColor{Dark: "#fbbf24", Light: "#d97706"}  // amber
	Err   = lipgloss.AdaptiveColor{Dark: "#fb7185", Light: "#e11d48"}  // rose
	Busy  = lipgloss.AdaptiveColor{Dark: "#22d3ee", Light: "#0e7490"}  // cyan for in-progress
)

// Named styles. All later TUI features consume these instead of building
// their own — theming drift is contained to this file.
var (
	// Tab bar: inactive tabs are dim glass, active tab carries the
	// cyan→indigo accent (approximated with a filled indigo pill).
	TabInactive = lipgloss.NewStyle().Foreground(TextDim).Background(Surface).Padding(0, 1)
	TabActive   = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Bold(true).Background(AccentIndigo).Padding(0, 1)
	TabBar      = lipgloss.NewStyle().Background(Bg).Padding(0, 1).Border(lipgloss.NormalBorder(), false, false, true, false).BorderForeground(BorderFaint)

	// Footer: single dim glass strip.
	Footer = lipgloss.NewStyle().Foreground(TextDim).Background(Surface).Padding(0, 1)
	FooterVersionDrift = lipgloss.NewStyle().Foreground(Warn).Bold(true)

	// Lists / detail panes.
	ListTitle   = lipgloss.NewStyle().Foreground(Text).Bold(true)
	ListItem    = lipgloss.NewStyle().Foreground(Text)
	ListItemSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Bold(true).Background(AccentCyan)
	ListMeta    = lipgloss.NewStyle().Foreground(TextDim)
	DetailKey   = lipgloss.NewStyle().Foreground(TextDim)
	DetailValue = lipgloss.NewStyle().Foreground(Text)
	PaneBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Border)

	// Status badges.
	StatusOK   = lipgloss.NewStyle().Foreground(OK)
	StatusWarn = lipgloss.NewStyle().Foreground(Warn)
	StatusErr  = lipgloss.NewStyle().Foreground(Err)
	StatusBusy = lipgloss.NewStyle().Foreground(Busy)

	// Overlays.
	HelpOverlay = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(AccentIndigo).
			Background(Surface).
			Foreground(Text).
			Padding(1, 2)
	ErrorText = lipgloss.NewStyle().Foreground(Err)
	HintText  = lipgloss.NewStyle().Foreground(TextDim)
	SpinnerStyle = lipgloss.NewStyle().Foreground(AccentCyan)
)
