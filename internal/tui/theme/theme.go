// Package theme is the single source of lipgloss styling for the orch TUI.
package theme

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/md"
)

// hsl converts the GUI's `H S% L%` token notation to a hex color.
func hsl(h, s, l float64) string {
	hn := h / 360
	sn := s / 100
	ln := l / 100
	var r, g, b float64
	if sn == 0 {
		r, g, b = ln, ln, ln
	} else {
		var q float64
		if ln < 0.5 {
			q = ln * (1 + sn)
		} else {
			q = ln + sn - ln*sn
		}
		p := 2*ln - q
		r = hueToRGB(p, q, hn+1.0/3.0)
		g = hueToRGB(p, q, hn)
		b = hueToRGB(p, q, hn-1.0/3.0)
	}
	to8 := func(v float64) uint8 {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		return uint8(v*255 + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", to8(r), to8(g), to8(b))
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t++
	}
	if t > 1 {
		t--
	}
	switch {
	case t < 1.0/6.0:
		return p + (q-p)*6*t
	case t < 0.5:
		return q
	case t < 2.0/3.0:
		return p + (q-p)*(2.0/3.0-t)*6
	default:
		return p
	}
}

// Theme is one named palette.
type Theme struct {
	Name string

	// Transparent makes the APP BACKGROUND unpainted, so whatever the terminal has behind orch — its
	// own background colour, its transparency, a compositor's blur — shows through the frame.
	Transparent bool

	// Surfaces (from the GUI's --background / --card / --secondary /
	// --muted / --border / --input tokens).
	Bg, Surface, SurfaceAlt, Border, BorderFaint lipgloss.Color

	// Text (from --foreground / --muted-foreground plus a faint step).
	Text, TextDim, TextFaint lipgloss.Color

	// Accents (from --primary and the --nav-active-from/to gradient).
	Accent, AccentCyan, AccentIndigo lipgloss.Color

	// Select is the SELECTION FILL behind white text (selected list rows, the active tab pill, the
	// file-selection chip).
	Select lipgloss.Color

	// Tool is a TOOL ROW's name — the transcript's ledger of what the model called.
	Tool lipgloss.Color

	// Status.
	OK, Warn, Err, Busy lipgloss.Color
}

// Dark is the default theme: the GUI's orchicon-dark palette.
// Token sources (frontend/src/index.css [data-theme="orchicon-dark"] .dark):
//
//	--background 222 35% 7%   --card 222 35% 11%      --secondary 222 30% 15%
//	--muted/--border 217 28% 17%                      --input 217 28% 20%
//	--foreground 210 40% 98%  --muted-foreground 215 20% 68%
//	--primary 199 89% 52%     --nav-active-from 189 94% 43%
//	--nav-active-to 239 84% 67%
var Dark = Theme{
	Name:         "dark",
	Bg:           lipgloss.Color(hsl(222, 35, 7)),
	Surface:      lipgloss.Color(hsl(222, 35, 11)),
	SurfaceAlt:   lipgloss.Color(hsl(222, 30, 15)),
	Border:       lipgloss.Color(hsl(217, 28, 28)),
	BorderFaint:  lipgloss.Color(hsl(217, 28, 22)),
	Text:         lipgloss.Color(hsl(210, 40, 98)),
	TextDim:      lipgloss.Color(hsl(215, 20, 68)),
	TextFaint:    lipgloss.Color(hsl(215, 16, 52)),
	Accent:       lipgloss.Color(hsl(199, 89, 52)),
	AccentCyan:   lipgloss.Color(hsl(189, 94, 43)),
	AccentIndigo: lipgloss.Color(hsl(239, 84, 67)),
	Select:       lipgloss.Color(hsl(189, 94, 34)),
	OK:           lipgloss.Color("#34d399"),
	Warn:         lipgloss.Color("#fbbf24"),
	Err:          lipgloss.Color("#fb7185"),
	Busy:         lipgloss.Color("#22d3ee"),
	// Tool mirrors Busy here, as for every generated palette: a tool row's colour is its own token, and
	// leaving it unset would render tool rows with NO colour on this theme while ember's had one.
	Tool: lipgloss.Color("#22d3ee"),
}

// Light is the GUI's default (light) palette. Token sources (:root in
// frontend/src/index.css):
//
//	--background 210 40% 98%  --card 0 0% 100%   --secondary/--muted 210 20% 96%
//	--border 214 32% 91%      --foreground 222 47% 11%
//	--muted-foreground 215 19% 38%               --primary 199 89% 36%
//
// TUI DELIBERATE DIVERGENCE: Border/BorderFaint are darker than the GUI's
// tokens (91%/94% → 64%/78%). In a browser a 91%-lightness border on a 98%
// background is a tasteful hairline; in a TUI that hairline is the ONLY thing
// separating panes and it CARRIES THE PANEL TITLE, so at ~1.1:1 contrast the
// whole layout reads as invisible (the operator's "the light theme was
// abysmal — I couldn't see anything"). Darkened to ~3:1 / ~2:1 so structure
// and titles are legible. Asserted by TestStructuralContrast.
var Light = Theme{
	Name:         "light",
	Bg:           lipgloss.Color(hsl(210, 40, 98)),
	Surface:      lipgloss.Color(hsl(0, 0, 100)),
	SurfaceAlt:   lipgloss.Color(hsl(210, 20, 94)),
	Border:       lipgloss.Color(hsl(214, 24, 64)),
	BorderFaint:  lipgloss.Color(hsl(214, 24, 78)),
	Text:         lipgloss.Color(hsl(222, 47, 11)),
	TextDim:      lipgloss.Color(hsl(215, 19, 38)),
	TextFaint:    lipgloss.Color(hsl(215, 16, 52)),
	Accent:       lipgloss.Color(hsl(199, 89, 36)),
	AccentCyan:   lipgloss.Color(hsl(188, 86, 32)),
	AccentIndigo: lipgloss.Color(hsl(234, 89, 60)),
	Select:       lipgloss.Color(hsl(188, 86, 31)),
	OK:           lipgloss.Color("#065f46"),
	Warn:         lipgloss.Color("#92400e"),
	Err:          lipgloss.Color("#be123c"),
	Busy:         lipgloss.Color("#155e75"),
	Tool:         lipgloss.Color("#155e75"),
}

// GruvboxDark is a TUI-native palette (Morhetz's Gruvbox, dark).
var GruvboxDark = Theme{
	Name:         "gruvbox-dark",
	Bg:           lipgloss.Color("#282828"),
	Surface:      lipgloss.Color("#3c3836"),
	SurfaceAlt:   lipgloss.Color("#504945"),
	Border:       lipgloss.Color("#7c6f64"),
	BorderFaint:  lipgloss.Color("#665c54"),
	Text:         lipgloss.Color("#ebdbb2"),
	TextDim:      lipgloss.Color("#bdae93"),
	TextFaint:    lipgloss.Color("#a89984"),
	Accent:       lipgloss.Color("#83a598"),
	AccentCyan:   lipgloss.Color("#8ec07c"),
	AccentIndigo: lipgloss.Color("#d3869b"),
	Select:       lipgloss.Color("#427b58"),
	OK:           lipgloss.Color("#b8bb26"),
	Warn:         lipgloss.Color("#fabd2f"),
	Err:          lipgloss.Color("#fb4934"),
	Busy:         lipgloss.Color("#83a598"),
	Tool:         lipgloss.Color("#83a598"),
}

// GruvboxLight is the light variant of the same palette.
var GruvboxLight = Theme{
	Name:         "gruvbox-light",
	Bg:           lipgloss.Color("#fbf1c7"),
	Surface:      lipgloss.Color("#f9f5d7"),
	SurfaceAlt:   lipgloss.Color("#ebdbb2"),
	Border:       lipgloss.Color("#a89984"),
	BorderFaint:  lipgloss.Color("#bdae93"),
	Text:         lipgloss.Color("#3c3836"),
	TextDim:      lipgloss.Color("#7c6f64"),
	TextFaint:    lipgloss.Color("#928374"),
	Accent:       lipgloss.Color("#076678"),
	AccentCyan:   lipgloss.Color("#427b58"),
	AccentIndigo: lipgloss.Color("#8f3f71"),
	Select:       lipgloss.Color("#427b58"),
	OK:           lipgloss.Color("#5f5b06"),
	Warn:         lipgloss.Color("#7a5000"),
	Err:          lipgloss.Color("#9d0006"),
	Busy:         lipgloss.Color("#076678"),
	Tool:         lipgloss.Color("#076678"),
}

// transparentSuffix marks a palette whose app background is left unpainted.
const transparentSuffix = "-transparent"

// transparentListed are the transparent variants shown in the picker.
var transparentListed = []string{"obsidian", "forest", "tokyo-night", "lumen", "light", "github-light"}

// registry is the selectable theme set, in /theme listing order.
var registry = buildRegistry()

// buildRegistry assembles the base palettes plus the derived families, in a
// stable listing order (bases first, then dark families, then light).
func buildRegistry() []*Theme {
	out := []*Theme{&Dark, &Light, &GruvboxDark, &GruvboxLight}
	// The hand-written community ports (theme_named.go), then the generated families.
	out = append(out, namedThemes...)
	for _, t := range derivedThemes {
		out = append(out, t)
	}
	// The transparent variants are appended LAST rather than interleaved: they are a MODE of a palette
	// rather than a family of their own.
	for _, base := range transparentListed {
		if src := findBase(base); src != nil {
			out = append(out, transparentCopy(src, base+transparentSuffix))
		}
	}
	return out
}

var active = &Dark

// Active returns the active palette.
func Active() *Theme { return active }

// Names lists the selectable themes in registry (listing) order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, t := range registry {
		out = append(out, t.Name)
	}
	return out
}

// Use switches the active theme and re-derives every named style.
// Returns false for an unknown name (no change).
func Use(name string) bool {
	t := Lookup(name)
	if t == nil {
		return false
	}
	// A TRANSPARENT THEME IS ADAPTED TO THE TERMINAL FIRST — see transparent_adapt.go.
	if t.Transparent {
		t = adaptTransparentForTerminal(t)
	}
	active = t
	buildStyles(*t)
	// The markdown renderer's inline-code chip follows the palette like every other token.
	md.SetCodeChip(string(t.Accent), string(t.SurfaceAlt))
	// THE STRUCTURAL ACCENT, which colours headings and list markers.
	md.SetAccentColor(string(t.Accent), string(t.Text))
	// AND THE CODE BLOCK, which is a filled block rather than a bordered one so a copy of it is clean
	// code (see md.codeBlock).
	md.SetCodeBlock(string(t.Text), string(t.SurfaceAlt))
	// AND THE SEARCH MATCH BAND.
	md.SetSearchHighlight(string(t.Text), string(t.Warn), string(t.Text), string(t.SurfaceAlt))
	return true
}

// Lookup resolves a theme name (nil when unknown).
func Lookup(name string) *Theme {
	if t := findBase(name); t != nil {
		return t
	}
	if base, ok := strings.CutSuffix(name, transparentSuffix); ok {
		if src := findBase(base); src != nil {
			return transparentCopy(src, name)
		}
	}
	return nil
}

// findBase resolves a BASE palette by EXACT name: the derived families first, then the hand-written
// globals.
func findBase(name string) *Theme {
	if t := lookupNamed(name); t != nil {
		return t
	}
	if t := lookupDerived(name); t != nil {
		return t
	}
	switch name {
	case Dark.Name:
		return &Dark
	case Light.Name:
		return &Light
	case GruvboxDark.Name:
		return &GruvboxDark
	case GruvboxLight.Name:
		return &GruvboxLight
	}
	return nil
}

// transparentCopy is the same palette with its app background left unpainted, under a new name.
func transparentCopy(base *Theme, name string) *Theme {
	cp := *base
	cp.Name = name
	cp.Transparent = true
	return &cp
}

// DefaultName is the launch default: the palette used when the config and the environment name no
// theme.
const DefaultName = "forest"

// Resolved active colors, re-pointed by Use. Render paths read these via
// the styles; direct color reads stay possible for layout math.
var (
	// Bg is the APP BACKGROUND, and it is UNPAINTED (an empty colour) on a transparent theme — see
	// Theme.Transparent.
	Bg      lipgloss.TerminalColor
	Surface lipgloss.TerminalColor
	// ComposerFill is the COMPOSER's background, which is the one fill that follows the app
	// background's transparency rather than the panels'.
	ComposerFill lipgloss.TerminalColor
	// PanelBg is the background every PANEL paints — a screen's pane, a rail, a dialog, a picker.
	PanelBg      lipgloss.TerminalColor
	SurfaceAlt   lipgloss.TerminalColor
	Border       lipgloss.TerminalColor
	BorderFaint  lipgloss.TerminalColor
	Text         lipgloss.TerminalColor
	TextDim      lipgloss.TerminalColor
	TextFaint    lipgloss.TerminalColor
	AccentCyan   lipgloss.TerminalColor
	AccentIndigo lipgloss.TerminalColor
	OK           lipgloss.TerminalColor
	Warn         lipgloss.TerminalColor
	Err          lipgloss.TerminalColor
	Busy         lipgloss.TerminalColor
)

// Named styles.
var (
	// Full-screen chrome: the opaque app background (every cell the
	// shell paints, so nothing bleeds through from beneath alt-screen).
	ScreenBg = lipgloss.NewStyle()

	// Chat bubble fills (bubble.go): derived per palette and used by the chat transcript, which paints
	// each message as a FULL-WIDTH band — the operator's lighter, the model's darker.
	BubbleUser  = lipgloss.NewStyle()
	BubbleModel = lipgloss.NewStyle()

	// SurfaceBg is the BACKGROUND-ONLY form of the surface token, for repairs that must re-assert a
	// surface background without adding a border or padding.
	SurfaceBg = lipgloss.NewStyle()

	// ComposerBg is the BACKGROUND-ONLY form of the COMPOSER's fill.
	ComposerBg = lipgloss.NewStyle()

	// PanelBgStyle is the BACKGROUND-ONLY form of PanelBg: the tint a panel's rows, padding and reset repairs
	// are painted with. Always painted, on every theme — see PanelBg.
	PanelBgStyle = lipgloss.NewStyle()

	// Tab bar: inactive tabs are dim glass, the active tab carries the GUI nav's cyan→indigo active
	// gradient (approximated with the filled indigo pill the mockup shows).
	TabInactive     = lipgloss.NewStyle()
	TabActive       = lipgloss.NewStyle()
	TabBar          = lipgloss.NewStyle()
	TabBarUnderline = lipgloss.NewStyle()

	// Tab dropdown menu (the mockup's submenu panel under a tab).
	MenuPanel  = lipgloss.NewStyle()
	MenuTitle  = lipgloss.NewStyle()
	MenuRow    = lipgloss.NewStyle()
	MenuRowSel = lipgloss.NewStyle()

	Footer             = lipgloss.NewStyle()
	FooterVersionDrift = lipgloss.NewStyle()

	ListTitle        = lipgloss.NewStyle()
	ListItem         = lipgloss.NewStyle()
	ListItemSelected = lipgloss.NewStyle()
	ListMeta         = lipgloss.NewStyle()
	DetailKey        = lipgloss.NewStyle()
	DetailValue      = lipgloss.NewStyle()
	PaneBorder       = lipgloss.NewStyle()

	// ComposerBox is the bottom chat composer's bordered panel (Composer
	// 2.0): rounded border + inner horizontal padding over the surface fill.
	ComposerBox = lipgloss.NewStyle()

	// ComposerSelect paints the composer's rows while the WHOLE buffer is selected (ctrl+a).
	ComposerSelect = lipgloss.NewStyle()

	StatusOK   = lipgloss.NewStyle()
	StatusWarn = lipgloss.NewStyle()
	StatusErr  = lipgloss.NewStyle()
	// ToolName paints a TOOL ROW's name and ToolMeta its arguments and result.
	ToolName = lipgloss.NewStyle()
	ToolMeta = lipgloss.NewStyle()

	StatusBusy = lipgloss.NewStyle()

	// FullsendBadge is the composer's FULLSEND indicator.
	FullsendBadge = lipgloss.NewStyle()

	HelpOverlay = lipgloss.NewStyle()
	ErrorText   = lipgloss.NewStyle()
	HintText    = lipgloss.NewStyle()

	// ComposerCursor paints the composer's caret. It is WRITTEN PRE-REVERSED and must stay that way — see
	// its assignment in buildStyles, which explains why.
	ComposerCursor = lipgloss.NewStyle()

	// ReasoningBlock paints a reasoning ("thinking") block's BODY, and ReasoningLabel its header.
	ReasoningLabel = lipgloss.NewStyle()
	ReasoningBody  = lipgloss.NewStyle()
	SpinnerStyle   = lipgloss.NewStyle()

	DiffAdd         = lipgloss.NewStyle()
	DiffDel         = lipgloss.NewStyle()
	DiffCtx         = lipgloss.NewStyle()
	DiffEmphasis    = lipgloss.NewStyle()
	DiffHeader      = lipgloss.NewStyle()
	DiffLineNoOld   = lipgloss.NewStyle()
	DiffLineNoNew   = lipgloss.NewStyle()
	DiffGutter      = lipgloss.NewStyle()
	DiffPanel       = lipgloss.NewStyle()
	DiffTabActive   = lipgloss.NewStyle()
	DiffTabInactive = lipgloss.NewStyle()
	DiffFileSel     = lipgloss.NewStyle()
	DiffClose       = lipgloss.NewStyle()
	DiffBadgeAdd    = lipgloss.NewStyle()
	DiffBadgeDel    = lipgloss.NewStyle()
	// DiffScrollTrack / DiffScrollThumb are the diff pane's scrollbar column (a track plus a
	// proportional thumb).
	DiffScrollTrack = lipgloss.NewStyle()
	DiffScrollThumb = lipgloss.NewStyle()
	// DiffResizeHandle is the diff rail's resize grip — the pane's last cell, which the shell's divider
	// hit-test owns.
	DiffResizeHandle = lipgloss.NewStyle()

	// PickerChip marks the CHOSEN chip in the model picker's ADAPTER/PROVIDER strip: plain theme
	// text with an UNDERLINE. No fill, no border — see buildStyles.
	PickerChip = lipgloss.NewStyle()
)

func buildStyles(t Theme) {
	// THE EFFECTIVE BACKGROUND, which is the whole of the transparency mechanism.
	bg := t.Bg
	composerFill := t.Surface
	surface := t.Surface
	if t.Transparent {
		bg = lipgloss.Color("")
		composerFill = lipgloss.Color("")
		// Surface is the INACTIVE-TAB and panel fill, so it goes with the background.
		surface = lipgloss.Color("")
	}
	Bg = bg
	ComposerFill = composerFill
	// THE PANEL TINT FOLLOWS THE TRANSPARENCY, like the app background and the composer.
	PanelBg = bg
	Surface = surface
	SurfaceAlt = t.SurfaceAlt
	Border = t.Border
	BorderFaint = t.BorderFaint
	Text = t.Text
	TextDim = t.TextDim
	TextFaint = t.TextFaint
	AccentCyan = t.AccentCyan
	AccentIndigo = t.AccentIndigo
	OK = t.OK
	Warn = t.Warn
	Err = t.Err
	Busy = t.Busy

	// --nav-active-fg territory: readable on both the cyan and indigo
	// accent pills in both themes.
	white := lipgloss.Color("#f8fafc")

	ScreenBg = lipgloss.NewStyle().Background(bg)
	SurfaceBg = lipgloss.NewStyle().Background(surface)
	// PanelBgStyle is the BACKGROUND-ONLY panel tint, derived FROM PanelBg rather than recomputed.
	PanelBgStyle = lipgloss.NewStyle().Background(PanelBg)
	// ComposerBg is the BACKGROUND-ONLY composer fill, for the resets the composer's own rows repair —
	// the same shape as SurfaceBg.
	ComposerBg = lipgloss.NewStyle().Background(composerFill)

	// Bubbles: a clearly different fill for the operator's own messages vs the model's, with the
	// bubble's own text colour (the plain text colour is chosen against the BACKGROUND.
	bu, bm := bubbleFills(string(t.Bg), string(t.Accent))
	// ON A TRANSPARENT THEME NEITHER BAND IS FILLED, so the chat surface is as see-through as the rest
	// of the theme.
	if t.Transparent {
		bu, bm = "", ""
	}
	BubbleUser = bubbleBand(bu, t)
	BubbleModel = bubbleBand(bm, t)

	TabInactive = lipgloss.NewStyle().Foreground(t.TextDim).Background(surface).Padding(0, 1)
	TabActive = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select).Padding(0, 1)
	TabBar = lipgloss.NewStyle().Background(bg).Padding(0, 1)
	TabBarUnderline = lipgloss.NewStyle().Foreground(t.Border).Background(bg)

	MenuPanel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Background(t.Surface).
		Foreground(t.Text)
	MenuTitle = lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Background(t.Surface)
	MenuRow = lipgloss.NewStyle().Foreground(t.Text).Background(t.Surface)
	MenuRowSel = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select)

	// Footer is STRAIGHT THEME TEXT — no background band.
	Footer = lipgloss.NewStyle().Foreground(t.TextDim).Padding(0, 1)
	FooterVersionDrift = lipgloss.NewStyle().Foreground(t.Warn).Bold(true)

	ListTitle = lipgloss.NewStyle().Foreground(t.Text).Bold(true)
	ListItem = lipgloss.NewStyle().Foreground(t.Text)
	ListItemSelected = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select)
	ListMeta = lipgloss.NewStyle().Foreground(t.TextDim)
	DetailKey = lipgloss.NewStyle().Foreground(t.TextDim)
	DetailValue = lipgloss.NewStyle().Foreground(t.Text)
	PaneBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Border)
	ComposerBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Background(composerFill).
		Padding(0, 2)

	StatusOK = lipgloss.NewStyle().Foreground(t.OK)
	StatusWarn = lipgloss.NewStyle().Foreground(t.Warn)
	StatusErr = lipgloss.NewStyle().Foreground(t.Err)
	StatusBusy = lipgloss.NewStyle().Foreground(t.Busy)
	// The badge paints the WARN fill and the composer's own fill as its text, so the glyphs read as a
	// knockout block rather than as coloured text.
	FullsendBadge = lipgloss.NewStyle().Bold(true).Background(t.Warn).Foreground(composerFill)
	// ToolName paints a TOOL ROW's name, ToolMeta its arguments and result.
	ToolName = lipgloss.NewStyle().Foreground(t.Tool)
	ToolMeta = lipgloss.NewStyle().Foreground(t.TextFaint)

	HelpOverlay = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.AccentIndigo).
		Background(t.Surface).
		Foreground(t.Text).
		Padding(1, 2)
	ErrorText = lipgloss.NewStyle().Foreground(t.Err)
	HintText = lipgloss.NewStyle().Foreground(t.TextDim)

	// THE COMPOSER'S CARET IS WRITTEN PRE-REVERSED, and that is not a mistake to tidy up.
	ComposerCursor = lipgloss.NewStyle().Foreground(t.Text).Background(t.Bg)
	// The white-on-Select pairing is the same one MenuRowSel and ListItemSelected use, and it is
	// contrast-gated for every palette by TestSelectionFillCarriesWhiteText.
	ComposerSelect = lipgloss.NewStyle().Foreground(white).Background(t.Select)
	ReasoningLabel = lipgloss.NewStyle().Foreground(t.AccentIndigo).Bold(true)
	ReasoningBody = lipgloss.NewStyle().Foreground(t.TextDim)
	SpinnerStyle = lipgloss.NewStyle().Foreground(t.AccentCyan)

	DiffAdd = lipgloss.NewStyle().Foreground(t.OK).Background(t.SurfaceAlt)
	DiffDel = lipgloss.NewStyle().Foreground(t.Err).Background(t.SurfaceAlt)
	DiffCtx = lipgloss.NewStyle().Foreground(t.TextDim)
	// DiffEmphasis marks the CHANGED span inside an add/del line.
	DiffEmphasis = lipgloss.NewStyle().Bold(true).Underline(true)
	DiffHeader = lipgloss.NewStyle().Foreground(t.AccentIndigo).Bold(true)
	DiffLineNoOld = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffLineNoNew = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffGutter = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffPanel = lipgloss.NewStyle().
		Background(bg).
		Border(lipgloss.RoundedBorder(), false, false, false, true).
		BorderForeground(t.Border)
	DiffTabActive = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select).Padding(0, 1)
	DiffTabInactive = lipgloss.NewStyle().Foreground(t.TextDim).Background(t.Surface).Padding(0, 1)
	DiffFileSel = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select)
	DiffClose = lipgloss.NewStyle().Foreground(t.TextFaint).Bold(true)
	// The diff pane's scrollbar column: a faint TRACK with a more prominent THUMB, so the operator can
	// see that there is more content and where they are in it.
	DiffScrollTrack = lipgloss.NewStyle().Foreground(t.BorderFaint)
	DiffScrollThumb = lipgloss.NewStyle().Foreground(t.TextDim).Bold(true)
	// The handle steps UP to TextDim — the same weight as the thumb, one step brighter than the track —
	// so a grip the operator needs to find is not the faintest thing on the pane.
	DiffResizeHandle = lipgloss.NewStyle().Foreground(t.TextDim)
	// PickerChip is the model picker's chosen chip: plain theme text, UNDERLINED.
	PickerChip = lipgloss.NewStyle().Foreground(t.Text).Underline(true)
	DiffBadgeAdd = lipgloss.NewStyle().Foreground(t.OK)
	DiffBadgeDel = lipgloss.NewStyle().Foreground(t.Err)
}

func init() { buildStyles(Dark) }

// Opaque pins one rendered row to exactly w cells with the app background painted on EVERY cell,
// and repairs the inner-reset hole that makes the terminal bleed through.
func Opaque(s string, w int) string {
	if w < 1 {
		return s
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return repairResets(ScreenBg.Render(s))
}

// OpaquePanel is Opaque for a PANEL: the padding and the reset repair use the PANEL's tint rather
// than the app background's.
func OpaquePanel(s string, w int) string {
	if w < 1 {
		return s
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return RepairAfterResets(PanelBgStyle.Render(s), PanelBgStyle)
}

// resets are the SGR sequences that CLEAR a background, in every spelling a producer here emits.
var resets = []string{"\x1b[0m", "\x1b[m"}

// nextReset returns the offset of the EARLIEST reset in s and that reset's length, or (-1, 0) when the
// string carries none.
func nextReset(s string) (int, int) {
	best, size := -1, 0
	for _, r := range resets {
		if i := strings.Index(s, r); i >= 0 && (best < 0 || i < best) {
			best, size = i, len(r)
		}
	}
	return best, size
}

// RepairAfterResets re-asserts a BACKGROUND-ONLY style's background after every SGR reset in an
// already-rendered string.
func RepairAfterResets(s string, bg lipgloss.Style) string {
	const terminator = "\x1b[0m"
	paint := bg.Render("")
	if paint == "" || strings.Contains(paint, "\n") {
		return s // no background to assert, or a non-background-only style
	}
	if !strings.HasSuffix(paint, terminator) {
		return s
	}
	open := strings.TrimSuffix(paint, terminator)
	if open == "" {
		return s
	}
	var b strings.Builder
	for {
		i, n := nextReset(s)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		rest := s[i+n:]
		b.WriteString(s[:i+n])
		// Re-assert only when MORE CELLS follow on this line.
		if rest == "" || strings.HasPrefix(rest, "\n") {
			s = rest
			continue
		}
		if !strings.HasPrefix(rest, "\x1b[") {
			b.WriteString(open)
		}
		s = rest
	}
}

// repairResets is Opaque's ScreenBg-specialised repair.
func repairResets(s string) string { return RepairAfterResets(s, ScreenBg) }
