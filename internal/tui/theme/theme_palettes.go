package theme

import "github.com/charmbracelet/lipgloss"

// theme_palettes.go — the TUI's extended palette set.
//
// The GUI ships ten light and ten dark themes that differ mainly by the HUE of
// their background and accent, with consistent lightness/saturation patterns
// per mode (frontend/src/lib/themes.ts + the [data-theme] blocks in
// frontend/src/index.css). The TUI mirrors that character — forest, ocean,
// violet, ember, amber, rose, teal, crimson, slate — but is NOT a copy:
//
//   - palettes are DERIVED from HSL specs, exactly the way the GUI builds its
//     tokens, so a family stays internally consistent and a new one is a few
//     numbers rather than 15 hand-picked hexes;
//   - every derived palette is then clamped to TUI CONTRAST FLOORS. A browser
//     can afford a 91%-lightness border because the DOM supplies structure
//     through spacing, shadow and border-radius. A terminal has only the
//     border — and it carries each panel's title — so borders are floored
//     where a hairline would render the layout invisible.
//
// theme/contrast_test.go gates the whole registry, so a spec that produces an
// unreadable palette fails the build instead of shipping.

// paletteSpec is one palette in the same HSL terms the GUI uses, plus the
// floors that make it a TERMINAL palette.
type paletteSpec struct {
	name string
	dark bool

	bgH, bgS, bgL float64

	accentH, accentS, accentL float64

	// textH/textL are per-mode (near-white on dark, near-black on light).
	textH, textS, textL float64
}

// clampL keeps a lightness inside [lo, hi].
func clampL(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mod360 wraps a hue into [0, 360).
func mod360(h float64) float64 {
	h = float64(int(h) % 360)
	if h < 0 {
		h += 360
	}
	return h
}

// theme derives the full TUI palette. The lightness relationships mirror the
// GUI's: surfaces step away from the background, the border is floored for
// legibility, and the accent family supplies the selection/interactive fills
// (kept dark enough to carry white text).
func (s paletteSpec) theme() Theme {
	bgS := s.bgS
	if bgS > 40 {
		bgS = 40 // a terminal background is never as saturated as a CSS one
	}

	bg := hsl(s.bgH, bgS, s.bgL)

	var surface, surfaceAlt string
	var border, borderFaint string
	var textDim, textFaint string

	if s.dark {
		surface = hsl(s.bgH, bgS, clampL(s.bgL+4, 0, 30))
		surfaceAlt = hsl(s.bgH, bgS, clampL(s.bgL+9, 0, 36))
		// Floored: on a dark terminal a border below ~28% lightness vanishes.
		border = hsl(s.bgH, clampL(bgS, 18, 30), clampL(s.bgL+21, 30, 46))
		borderFaint = hsl(s.bgH, clampL(bgS, 16, 26), clampL(s.bgL+15, 24, 38))
		textDim = hsl(s.textH, 20, 68)
		textFaint = hsl(s.textH, 16, 54)
	} else {
		surface = hsl(s.bgH, clampL(bgS, 6, 30), 100)
		surfaceAlt = hsl(s.bgH, clampL(bgS, 8, 30), clampL(s.bgL-4, 88, 96))
		// Floored: the GUI's 91% border on a 98% background is ~1.1:1 — an
		// invisible separator on a terminal.
		border = hsl(s.bgH, clampL(bgS, 6, 26), 64)
		borderFaint = hsl(s.bgH, clampL(bgS, 6, 22), 78)
		textDim = hsl(s.textH, 19, 38)
		textFaint = hsl(s.textH, 16, 52)
	}

	// The fill backgrounds (selected rows, tabs, chips) carry WHITE text, and a
	// filled region is a different job from an accent foreground: the accent may
	// be vivid because it draws thin strokes, while a FILL must be dark enough
	// for white to read on it. Deriving the fill by rotating the accent hue was
	// wrong outright — ember's +45 landed on yellow-green — so the fill keeps
	// the family hue and is clamped dark.
	fillL := clampL(s.accentL, 24, 34)
	// Hues that carry more luminance at the same lightness need a lower cap to
	// leave white text legible: the yellow/green-yellow band first, then the
	// green-through-cyan band (both fail a 3:1 white-on-fill check at 34%).
	switch h := mod360(s.accentH); {
	case h >= 40 && h <= 110:
		fillL = clampL(s.accentL, 20, 27)
	case h > 110 && h <= 210:
		fillL = clampL(s.accentL, 20, 30)
	}
	selectFill := hsl(s.accentH, clampL(s.accentS, 45, 90), fillL)

	ok, warn, err, busy := darkStatus.OK, darkStatus.Warn, darkStatus.Err, darkStatus.Busy
	if !s.dark {
		ok, warn, err, busy = lightStatus.OK, lightStatus.Warn, lightStatus.Err, lightStatus.Busy
	}

	return Theme{
		Name:         s.name,
		Bg:           lipgloss.Color(bg),
		Surface:      lipgloss.Color(surface),
		SurfaceAlt:   lipgloss.Color(surfaceAlt),
		Border:       lipgloss.Color(border),
		BorderFaint:  lipgloss.Color(borderFaint),
		Text:         lipgloss.Color(hsl(s.textH, s.textS, s.textL)),
		TextDim:      lipgloss.Color(textDim),
		TextFaint:    lipgloss.Color(textFaint),
		Accent:       lipgloss.Color(hsl(s.accentH, clampL(s.accentS, 40, 90), s.accentL)),
		AccentCyan:   lipgloss.Color(selectFill),
		AccentIndigo: lipgloss.Color(hsl(s.accentH+30, clampL(s.accentS, 45, 90), s.accentL)),
		Select:       lipgloss.Color(selectFill),
		OK:           lipgloss.Color(ok),
		Warn:         lipgloss.Color(warn),
		Err:          lipgloss.Color(err),
		Busy:         lipgloss.Color(busy),
	}
}

// statusSet is the per-mode status palette. Status semantics must not shift
// with the theme (the GUI keeps them constant across its themes too) — but
// LEGIBILITY must, because the same hue that glows on a near-black background
// disappears on a near-white one.
//
// MEASURED, and this is why the light set was re-picked. Against the light
// themes' backgrounds the previous values were:
//
//	Warn #d97706  2.66:1   <- the operator's "green text ... VERY hard to read"
//	OK   #059669  3.08:1
//	Busy #0e7490  4.17:1
//	Err  #e11d48  4.19:1
//
// All four are below the 4.5:1 WCAG AA floor for body text, and the gate did
// not catch it because it demanded only 2.5:1 (see TestStructuralContrast).
// These are the darker members of the same hue families, so a status reads the
// same way it always did — just legibly on a light surface. The dark set is
// untouched: it measures 6.4-13.9:1 already.
type statusSet struct{ OK, Warn, Err, Busy string }

var (
	darkStatus  = statusSet{"#34d399", "#fbbf24", "#fb7185", "#22d3ee"}
	lightStatus = statusSet{"#065f46", "#92400e", "#be123c", "#155e75"}
)

// --- the palette set -----------------------------------------------------
//
// The accent hue defines each family's character, mirroring the GUI's themes:
// forest green, ocean blue, violet purple, ember orange-red, amber, rose, teal,
// crimson red, slate grey. The background carries the same hue at low
// saturation, so the whole surface is tinted rather than just the accents.
var paletteSpecs = []paletteSpec{
	// Dark: near-black backgrounds, near-white text.
	{name: "obsidian", dark: true, bgH: 222, bgS: 35, bgL: 7, accentH: 199, accentS: 89, accentL: 52, textH: 210, textS: 40, textL: 98},
	{name: "ember", dark: true, bgH: 12, bgS: 30, bgL: 8, accentH: 25, accentS: 89, accentL: 58, textH: 20, textS: 20, textL: 96},
	{name: "forest", dark: true, bgH: 145, bgS: 20, bgL: 7, accentH: 160, accentS: 60, accentL: 48, textH: 142, textS: 30, textL: 96},
	{name: "ocean", dark: true, bgH: 210, bgS: 30, bgL: 8, accentH: 199, accentS: 89, accentL: 52, textH: 205, textS: 30, textL: 96},
	{name: "violet", dark: true, bgH: 270, bgS: 25, bgL: 8, accentH: 263, accentS: 70, accentL: 62, textH: 265, textS: 25, textL: 96},
	{name: "amber", dark: true, bgH: 38, bgS: 25, bgL: 8, accentH: 38, accentS: 92, accentL: 58, textH: 40, textS: 25, textL: 96},
	{name: "rose", dark: true, bgH: 350, bgS: 25, bgL: 8, accentH: 346, accentS: 77, accentL: 60, textH: 350, textS: 25, textL: 96},
	{name: "teal", dark: true, bgH: 175, bgS: 25, bgL: 8, accentH: 173, accentS: 80, accentL: 48, textH: 173, textS: 25, textL: 96},
	{name: "crimson", dark: true, bgH: 0, bgS: 25, bgL: 8, accentH: 0, accentS: 75, accentL: 58, textH: 0, textS: 20, textL: 96},
	{name: "slate", dark: true, bgH: 215, bgS: 12, bgL: 10, accentH: 215, accentS: 25, accentL: 62, textH: 215, textS: 15, textL: 95},

	// Screen-phosphor pair (the operator's "amber on black and a green on black
	// kind of like fallout inspired"). The reference is a CRT terminal: a
	// genuinely BLACK screen (no tint to speak of) with a single saturated
	// phosphor that glows — the classic amber is ~#ffb000 and the green ~#41ff00,
	// both at HIGH saturation and MID lightness. An earlier pass pushed the text
	// toward near-white, which is what made it read as "a dark theme with a warm
	// accent" rather than a glowing screen; the phosphor stays saturated here and
	// only its lightness rises.
	{name: "crt-amber", dark: true, bgH: 36, bgS: 45, bgL: 1, accentH: 40, accentS: 100, accentL: 50, textH: 40, textS: 100, textL: 60},
	{name: "crt-green", dark: true, bgH: 120, bgS: 45, bgL: 1, accentH: 105, accentS: 100, accentL: 50, textH: 105, textS: 100, textL: 56},

	// Light: near-white tinted backgrounds, near-black text.
	{name: "lumen", dark: false, bgH: 210, bgS: 40, bgL: 98, accentH: 199, accentS: 89, accentL: 36, textH: 222, textS: 47, textL: 11},
	{name: "ember-light", dark: false, bgH: 20, bgS: 100, bgL: 98, accentH: 350, accentS: 89, accentL: 42, textH: 222, textS: 47, textL: 11},
	{name: "forest-light", dark: false, bgH: 142, bgS: 40, bgL: 98, accentH: 160, accentS: 84, accentL: 32, textH: 222, textS: 47, textL: 11},
	{name: "ocean-light", dark: false, bgH: 199, bgS: 100, bgL: 98, accentH: 199, accentS: 89, accentL: 36, textH: 222, textS: 47, textL: 11},
	{name: "violet-light", dark: false, bgH: 270, bgS: 50, bgL: 98, accentH: 263, accentS: 70, accentL: 44, textH: 222, textS: 47, textL: 11},
	{name: "amber-light", dark: false, bgH: 38, bgS: 100, bgL: 98, accentH: 30, accentS: 92, accentL: 40, textH: 222, textS: 47, textL: 11},
	{name: "rose-light", dark: false, bgH: 350, bgS: 100, bgL: 98, accentH: 346, accentS: 77, accentL: 44, textH: 222, textS: 47, textL: 11},
	{name: "teal-light", dark: false, bgH: 173, bgS: 60, bgL: 98, accentH: 173, accentS: 80, accentL: 32, textH: 222, textS: 47, textL: 11},
	{name: "slate-light", dark: false, bgH: 210, bgS: 20, bgL: 98, accentH: 215, accentS: 25, accentL: 42, textH: 222, textS: 47, textL: 11},
}

// derivedThemes is the spec set built into palettes once at startup.
var derivedThemes = func() []*Theme {
	out := make([]*Theme, 0, len(paletteSpecs))
	for _, s := range paletteSpecs {
		t := s.theme()
		out = append(out, &t)
	}
	return out
}()

// IsDark reports whether a palette is a dark one. It reads the DERIVED spec
// (the palette's own declared mode) and falls back to the background's
// luminance for the hand-written palettes (dark, light, gruvbox-*). The Themes
// pane uses it to group the picker into dark and light sections.
func IsDark(name string) bool {
	for _, s := range paletteSpecs {
		if s.name == name {
			return s.dark
		}
	}
	if t := Lookup(name); t != nil {
		return isDarkHex(string(t.Bg))
	}
	return true
}

// lookupDerived resolves a derived palette by name.
func lookupDerived(name string) *Theme {
	for _, t := range derivedThemes {
		if t.Name == name {
			return t
		}
	}
	return nil
}
