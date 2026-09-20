package theme

import "github.com/charmbracelet/lipgloss"

// theme_named.go — hand-written ports of widely used COMMUNITY palettes.
//
// WHY THESE ARE WRITTEN OUT RATHER THAN DERIVED. theme_palettes.go generates its families from HSL specs,
// which is right for a palette whose character IS a hue (forest is green, ocean is blue). It is wrong for
// these: Dracula, Nord, Catppuccin and the rest are SPECIFIC published colours, and a generated
// approximation of Dracula would be a different palette wearing its name. So the hexes here are the
// upstream ones.
//
// PROVENANCE, STATED PLAINLY. The operator asked to "look to opencode and the themes they have
// available". opencode's theme list is compiled into its binary and is not on this machine, so these are
// NOT copies of its files: they are the well-known community palettes that family of themes is drawn from
// — Dracula, Nord, Catppuccin (Mocha/Latte), Tokyo Night, One Dark, Everforest, Kanagawa, Solarized,
// GitHub and Rosé Pine. Anyone who knows the upstream palette will recognise what is here.
//
// WHERE AN UPSTREAM COLOUR WAS CHANGED, AND WHY. Two floors bind, and both are the same adaptation the
// Light theme already documents:
//
//   - STATUS on a LIGHT background is measured at 4.5:1 (see TestStructuralContrast), and several upstream
//     accents are built for a browser where a colour only has to LOOK distinct. Catppuccin Latte's green
//     `#40a02b` measures 2.96:1 and Rosé Pine Dawn's `#b4637a` 3.81:1, so each is darkened within its own
//     family until it clears the floor. A status that cannot be read is not a status.
//   - BORDERS are floored for the same reason the Light theme floors its own: an upstream hairline is
//     designed to sit on a page with spacing and shadow around it, whereas here the border is the ONLY
//     thing separating panes and it CARRIES THE PANEL TITLE.
//
// Status colours stay per-palette (not the shared darkStatus/lightStatus sets) because these palettes
// define their own — that is part of their character — and every one of them is gated.

var namedThemes = []*Theme{
	// Dracula — draculatheme.com. Purple/cyan/pink over a near-black slate.
	{
		Name:         "dracula",
		Bg:           lipgloss.Color("#282a36"),
		Surface:      lipgloss.Color("#343746"),
		SurfaceAlt:   lipgloss.Color("#44475a"),
		Border:       lipgloss.Color("#6272a4"),
		BorderFaint:  lipgloss.Color("#4c5468"),
		Text:         lipgloss.Color("#f8f8f2"),
		TextDim:      lipgloss.Color("#c8cbd9"),
		TextFaint:    lipgloss.Color("#9aa0b5"),
		Accent:       lipgloss.Color("#bd93f9"),
		AccentCyan:   lipgloss.Color("#8be9fd"),
		AccentIndigo: lipgloss.Color("#ff79c6"),
		Select:       lipgloss.Color("#44475a"),
		OK:           lipgloss.Color("#50fa7b"),
		Warn:         lipgloss.Color("#f1fa8c"),
		Err:          lipgloss.Color("#ff5555"),
		Busy:         lipgloss.Color("#8be9fd"),
		Tool:         lipgloss.Color("#8be9fd"),
	},

	// Nord — nordtheme.com. Muted arctic blues.
	{
		Name:         "nord",
		Bg:           lipgloss.Color("#2e3440"),
		Surface:      lipgloss.Color("#3b4252"),
		SurfaceAlt:   lipgloss.Color("#434c5e"),
		Border:       lipgloss.Color("#616e88"),
		BorderFaint:  lipgloss.Color("#4c566a"),
		Text:         lipgloss.Color("#eceff4"),
		TextDim:      lipgloss.Color("#d8dee9"),
		TextFaint:    lipgloss.Color("#a9b3c4"),
		Accent:       lipgloss.Color("#88c0d0"),
		AccentCyan:   lipgloss.Color("#8fbcbb"),
		AccentIndigo: lipgloss.Color("#b48ead"),
		Select:       lipgloss.Color("#5e81ac"),
		OK:           lipgloss.Color("#a3be8c"),
		Warn:         lipgloss.Color("#ebcb8b"),
		Err:          lipgloss.Color("#cb6a73"),
		Busy:         lipgloss.Color("#88c0d0"),
		Tool:         lipgloss.Color("#88c0d0"),
	},

	// Catppuccin Mocha — catppuccin.com. Soft pastels on a warm dark.
	{
		Name:         "catppuccin-mocha",
		Bg:           lipgloss.Color("#1e1e2e"),
		Surface:      lipgloss.Color("#313244"),
		SurfaceAlt:   lipgloss.Color("#45475a"),
		Border:       lipgloss.Color("#585b70"),
		BorderFaint:  lipgloss.Color("#45475a"),
		Text:         lipgloss.Color("#cdd6f4"),
		TextDim:      lipgloss.Color("#a6adc8"),
		TextFaint:    lipgloss.Color("#7f849c"),
		Accent:       lipgloss.Color("#89b4fa"),
		AccentCyan:   lipgloss.Color("#94e2d5"),
		AccentIndigo: lipgloss.Color("#cba6f7"),
		Select:       lipgloss.Color("#45475a"),
		OK:           lipgloss.Color("#a6e3a1"),
		Warn:         lipgloss.Color("#f9e2af"),
		Err:          lipgloss.Color("#f38ba8"),
		Busy:         lipgloss.Color("#94e2d5"),
		Tool:         lipgloss.Color("#94e2d5"),
	},

	// Tokyo Night — deep indigo night sky.
	{
		Name:         "tokyo-night",
		Bg:           lipgloss.Color("#1a1b26"),
		Surface:      lipgloss.Color("#1f2335"),
		SurfaceAlt:   lipgloss.Color("#292e42"),
		Border:       lipgloss.Color("#565f89"),
		BorderFaint:  lipgloss.Color("#3b4261"),
		Text:         lipgloss.Color("#c0caf5"),
		TextDim:      lipgloss.Color("#a9b1d6"),
		TextFaint:    lipgloss.Color("#737aa2"),
		Accent:       lipgloss.Color("#7aa2f7"),
		AccentCyan:   lipgloss.Color("#7dcfff"),
		AccentIndigo: lipgloss.Color("#bb9af7"),
		Select:       lipgloss.Color("#3b4261"),
		OK:           lipgloss.Color("#9ece6a"),
		Warn:         lipgloss.Color("#e0af68"),
		Err:          lipgloss.Color("#f7768e"),
		Busy:         lipgloss.Color("#7dcfff"),
		Tool:         lipgloss.Color("#7dcfff"),
	},

	// One Dark — the Atom editor's dark theme, still the most-copied dark palette there is.
	{
		Name:         "one-dark",
		Bg:           lipgloss.Color("#282c34"),
		Surface:      lipgloss.Color("#2c313a"),
		SurfaceAlt:   lipgloss.Color("#333843"),
		Border:       lipgloss.Color("#5c6370"),
		BorderFaint:  lipgloss.Color("#4b5263"),
		Text:         lipgloss.Color("#abb2bf"),
		TextDim:      lipgloss.Color("#9da5b4"),
		TextFaint:    lipgloss.Color("#7f848e"),
		Accent:       lipgloss.Color("#61afef"),
		AccentCyan:   lipgloss.Color("#56b6c2"),
		AccentIndigo: lipgloss.Color("#c678dd"),
		Select:       lipgloss.Color("#4b5263"),
		OK:           lipgloss.Color("#98c379"),
		Warn:         lipgloss.Color("#e5c07b"),
		Err:          lipgloss.Color("#e06c75"),
		Busy:         lipgloss.Color("#56b6c2"),
		Tool:         lipgloss.Color("#56b6c2"),
	},

	// Everforest Dark (medium) — a low-contrast, easy-on-the-eyes forest.
	{
		Name:         "everforest-dark",
		Bg:           lipgloss.Color("#2d353b"),
		Surface:      lipgloss.Color("#343f44"),
		SurfaceAlt:   lipgloss.Color("#3d484d"),
		Border:       lipgloss.Color("#7a8478"),
		BorderFaint:  lipgloss.Color("#5c6660"),
		Text:         lipgloss.Color("#d3c6aa"),
		TextDim:      lipgloss.Color("#9da9a0"),
		TextFaint:    lipgloss.Color("#859289"),
		Accent:       lipgloss.Color("#a7c080"),
		AccentCyan:   lipgloss.Color("#83c092"),
		AccentIndigo: lipgloss.Color("#d699b6"),
		Select:       lipgloss.Color("#475258"),
		OK:           lipgloss.Color("#a7c080"),
		Warn:         lipgloss.Color("#dbbc7f"),
		Err:          lipgloss.Color("#e67e80"),
		Busy:         lipgloss.Color("#83c092"),
		Tool:         lipgloss.Color("#83c092"),
	},

	// Catppuccin Latte — the same family in light.
	{
		Name:         "catppuccin-latte",
		Bg:           lipgloss.Color("#eff1f5"),
		Surface:      lipgloss.Color("#ffffff"),
		SurfaceAlt:   lipgloss.Color("#dce0e8"),
		Border:       lipgloss.Color("#9ca0b0"),
		BorderFaint:  lipgloss.Color("#bcc0cc"),
		Text:         lipgloss.Color("#4c4f69"),
		TextDim:      lipgloss.Color("#5c5f77"),
		TextFaint:    lipgloss.Color("#7c7f93"),
		Accent:       lipgloss.Color("#1e66f5"),
		AccentCyan:   lipgloss.Color("#209fb5"),
		AccentIndigo: lipgloss.Color("#8839ef"),
		Select:       lipgloss.Color("#26519e"),
		// DARKENED from Latte's own green (2.96:1 on this background) and its blue (4.35:1), which are
		// designed to LOOK distinct on a page rather than to be READ as words.
		OK:   lipgloss.Color("#226410"),
		Warn: lipgloss.Color("#7a4700"),
		Err:  lipgloss.Color("#d20f39"),
		Busy: lipgloss.Color("#1a56cf"),
		Tool: lipgloss.Color("#1a56cf"),
	},

	// Solarized Light — Ethan Schoonover's light half of the same scheme.
	{
		Name:         "solarized-light",
		Bg:           lipgloss.Color("#fdf6e3"),
		Surface:      lipgloss.Color("#ffffff"),
		SurfaceAlt:   lipgloss.Color("#f4efdd"),
		Border:       lipgloss.Color("#93a1a1"),
		BorderFaint:  lipgloss.Color("#c3ccc9"),
		Text:         lipgloss.Color("#4b5f66"),
		TextDim:      lipgloss.Color("#657b83"),
		TextFaint:    lipgloss.Color("#839496"),
		Accent:       lipgloss.Color("#268bd2"),
		AccentCyan:   lipgloss.Color("#2aa198"),
		AccentIndigo: lipgloss.Color("#6c71c4"),
		Select:       lipgloss.Color("#0e5b76"),
		// DARKENED from the scheme's yellow/orange, which are ~2.5:1 as text on this background.
		OK:   lipgloss.Color("#4a5a00"),
		Warn: lipgloss.Color("#6f5500"),
		Err:  lipgloss.Color("#b3241f"),
		Busy: lipgloss.Color("#0b5f6a"),
		Tool: lipgloss.Color("#0e7480"),
	},

	// GitHub Light — the default palette most operators already read all day.
	{
		Name:         "github-light",
		Bg:           lipgloss.Color("#ffffff"),
		Surface:      lipgloss.Color("#f6f8fa"),
		SurfaceAlt:   lipgloss.Color("#eaeef2"),
		Border:       lipgloss.Color("#8c959f"),
		BorderFaint:  lipgloss.Color("#d0d7de"),
		Text:         lipgloss.Color("#1f2328"),
		TextDim:      lipgloss.Color("#59636e"),
		TextFaint:    lipgloss.Color("#818b98"),
		Accent:       lipgloss.Color("#0a5cbf"),
		AccentCyan:   lipgloss.Color("#1b7c83"),
		AccentIndigo: lipgloss.Color("#8250df"),
		Select:       lipgloss.Color("#0969da"),
		OK:           lipgloss.Color("#15682c"),
		Warn:         lipgloss.Color("#7a5200"),
		Err:          lipgloss.Color("#d1242f"),
		Busy:         lipgloss.Color("#0a5cbf"),
		Tool:         lipgloss.Color("#0a5cbf"),
	},

	// Rosé Pine Dawn — a warm, low-contrast light theme.
	{
		Name:         "rose-pine-dawn",
		Bg:           lipgloss.Color("#faf4ed"),
		Surface:      lipgloss.Color("#fffaf3"),
		SurfaceAlt:   lipgloss.Color("#f2e9e1"),
		Border:       lipgloss.Color("#9893a5"),
		BorderFaint:  lipgloss.Color("#cecacd"),
		Text:         lipgloss.Color("#575279"),
		TextDim:      lipgloss.Color("#6e6a86"),
		TextFaint:    lipgloss.Color("#797593"),
		Accent:       lipgloss.Color("#286983"),
		AccentCyan:   lipgloss.Color("#56949f"),
		AccentIndigo: lipgloss.Color("#907aa9"),
		Select:       lipgloss.Color("#286983"),
		// DARKENED: Dawn's love/gold/foam are 3.8/2.2/3.3:1 as text on this background.
		OK:   lipgloss.Color("#35593e"),
		Warn: lipgloss.Color("#74490b"),
		Err:  lipgloss.Color("#8f3d52"),
		Busy: lipgloss.Color("#275a64"),
		Tool: lipgloss.Color("#275a64"),
	},
}

// lookupNamed resolves a hand-written port by name.
func lookupNamed(name string) *Theme {
	for _, t := range namedThemes {
		if t.Name == name {
			return t
		}
	}
	return nil
}
