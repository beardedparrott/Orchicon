// Package theme is the single source of lipgloss styling for the orch
// TUI. Every later TUI feature (chat dock, diff sidebar, …) imports this
// package; nothing else constructs lipgloss styles.
//
// The color tokens are PORTED from the GUI's HSL design tokens
// (frontend/src/index.css: :root defaults = the light theme,
// [data-theme="orchicon-dark"] .dark = the dark theme) so the TUI and
// GUI can be diffed token-for-token. Tokens stay in HSL here (same
// notation as the CSS custom properties) and resolve to hex at init.
//
// Two themes ship: dark (default — the GUI's dark glass look) and light.
// theme.Use(name) switches the ACTIVE palette and re-derives every named
// style; all render paths read the style vars at render time, so a
// switch repaints the whole shell. Backgrounds are always SOLID (no
// alpha/transparency — terminal cells cannot alpha-blend) and every
// foreground/background pair is chosen for readable contrast.
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

// Theme is one named palette. Every color is a concrete (resolved) color
// — no AdaptiveColor: the ACTIVE theme's values apply verbatim, so the
// terminal's own dark/light preference never fights the chosen theme.
type Theme struct {
	Name string

	// Surfaces (from the GUI's --background / --card / --secondary /
	// --muted / --border / --input tokens).
	Bg, Surface, SurfaceAlt, Border, BorderFaint lipgloss.Color

	// Text (from --foreground / --muted-foreground plus a faint step).
	Text, TextDim, TextFaint lipgloss.Color

	// Accents (from --primary and the --nav-active-from/to gradient).
	Accent, AccentCyan, AccentIndigo lipgloss.Color

	// Select is the SELECTION FILL behind white text (selected list rows, the
	// active tab pill, the file-selection chip). It is deliberately separate
	// from the accents: an accent may be vivid because it draws thin strokes,
	// whereas a fill must be dark enough for white to read on it.
	Select lipgloss.Color

	// Tool is a TOOL ROW's name — the transcript's ledger of what the model called.
	//
	// IT IS ITS OWN TOKEN rather than a borrow of Busy, which is what it was. The value is the same
	// today; what changes is that a tool row can no longer be recoloured by a future change to the
	// MEANING of "busy". Busy means an in-flight turn, while a tool row records calls already made, and
	// borrowing a status token for a structural element is the same trap the GUI fell into when its
	// accent wore the theme's ERROR colour.
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
	// leaving it unset would render tool rows with NO colour on this theme while ember's had one — a
	// theme-dependent change in what the transcript shows, which the operator would meet as "the tool rows
	// are grey on dark".
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

// GruvboxDark is a TUI-native palette (Morhetz's Gruvbox, dark). Terminal
// themes are chosen HERE rather than derived from the GUI: the palette is
// validated by theme/contrast_test.go, which the GUI tokens cannot satisfy as
// written (their borders are browser hairlines).
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

// registry is the selectable theme set, in /theme listing order. It is
// TUI-OWNED: these palettes are chosen for terminal contrast and are NOT a
// copy of the GUI's CSS tokens (bar dark/light, which are ported and then
// adjusted where a browser hairline would vanish on a terminal — see Light).
//
// "dark" and "light" are the long-standing base names (persisted in operator
// configs, so they are never renamed); the derived families from
// theme_palettes.go follow.
var registry = buildRegistry()

// buildRegistry assembles the base palettes plus the derived families, in a
// stable listing order (bases first, then dark families, then light).
func buildRegistry() []*Theme {
	out := []*Theme{&Dark, &Light, &GruvboxDark, &GruvboxLight}
	for _, t := range derivedThemes {
		out = append(out, t)
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
	active = t
	buildStyles(*t)
	// The markdown renderer's inline-code chip follows the palette like every other token. It is
	// pushed here rather than pulled by md, because md must not know about themes (it is a leaf that
	// the screens and the chat renderer all share) — and because a chip that a caller forgets to
	// configure would be invisible until it bit.
	//
	// SurfaceAlt is the raised fill, so the chip reads as a chip: a tinted block distinct from the
	// surface it sits on, with the theme's own body text on it. Both are contrast-gated (see
	// TestStructuralContrast), which is what makes the pair legible in light AND dark palettes — the
	// whole point, since reverse video could not be made legible at all.
	md.SetCodeChip(string(t.Accent), string(t.SurfaceAlt))
	// THE STRUCTURAL ACCENT, which colours headings and list markers. The second argument is the body
	// text — the colour the accent is drawn INSIDE — playing the same role as the chip's first argument,
	// and for the same reason: a `\x1b[39m` close would drop the rest of the line to the terminal's
	// default foreground.
	md.SetAccentColor(string(t.Accent), string(t.Text))
	// AND THE CODE BLOCK, which is a filled block rather than a bordered one so a copy of it is clean code
	// (see md.codeBlock). The fill is the theme's raised surface and the text is the body colour, so the
	// block reads as a block and its contents read as code — not as prose on a slightly different grey.
	md.SetCodeBlock(string(t.Text), string(t.SurfaceAlt))
	return true
}

// Lookup resolves a theme name (nil when unknown).
func Lookup(name string) *Theme {
	if t := lookupDerived(name); t != nil {
		return t
	}
	for _, t := range registry {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// DefaultName is the launch default: the palette used when the config and the environment name no
// theme. An explicit preference (the config's `theme`, or ORCHICON_THEME) always wins — this is only
// the fallback, so changing it changes how orch looks out of the box and nothing at all for an
// operator who has already chosen a palette.
//
// FOREST, at the operator's request: "I would like forest to be the default theme for the TUI". It is the
// `forest` palette from theme_palettes.go — the deep-green family — and NOT "forest-light" beside it, which is
// the same family in light mode. Lookup matches the name exactly, so a typo here would fall back to the base
// dark palette without an error; theme_test.go asserts this name resolves, and that it is a DARK palette.
const DefaultName = "forest"

// Resolved active colors, re-pointed by Use. Render paths read these via
// the styles; direct color reads stay possible for layout math.
var (
	Bg           lipgloss.TerminalColor
	Surface      lipgloss.TerminalColor
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

// Named styles. All TUI features consume these instead of building their
// own — theming drift is contained to this file. buildStyles re-derives
// them on every theme switch.
var (
	// Full-screen chrome: the opaque app background (every cell the
	// shell paints, so nothing bleeds through from beneath alt-screen).
	ScreenBg = lipgloss.NewStyle()

	// Chat bubble fills (bubble.go): derived per palette and used by the chat
	// transcript, which paints each message as a FULL-WIDTH band — the
	// operator's lighter, the model's darker — rather than tinting just the
	// text.
	BubbleUser  = lipgloss.NewStyle()
	BubbleModel = lipgloss.NewStyle()

	// SurfaceBg is the BACKGROUND-ONLY form of the surface token, for repairs
	// that must re-assert a surface background without adding a border or
	// padding. Never render content through it.
	SurfaceBg = lipgloss.NewStyle()

	// Tab bar: inactive tabs are dim glass, the active tab carries the
	// GUI nav's cyan→indigo active gradient (approximated with the filled
	// indigo pill the mockup shows).
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

	StatusOK   = lipgloss.NewStyle()
	StatusWarn = lipgloss.NewStyle()
	StatusErr  = lipgloss.NewStyle()
	// ToolName paints a TOOL ROW's name and ToolMeta its arguments and result.
	//
	// ToolName is its OWN token (Theme.Tool) rather than a borrow of StatusBusy, which is what it was: a
	// tool row is the ledger of calls already made, while Busy means an in-flight turn. The same colour
	// today, but a change to what "busy" means can no longer recolour the transcript's tool rows.
	ToolName = lipgloss.NewStyle()
	ToolMeta = lipgloss.NewStyle()

	StatusBusy = lipgloss.NewStyle()

	HelpOverlay = lipgloss.NewStyle()
	ErrorText   = lipgloss.NewStyle()
	HintText    = lipgloss.NewStyle()

	// ComposerCursor paints the composer's caret. It is WRITTEN PRE-REVERSED and must stay that way — see
	// its assignment in buildStyles, which explains why.
	ComposerCursor = lipgloss.NewStyle()

	// ReasoningBlock paints a reasoning ("thinking") block's BODY, and ReasoningLabel its header. They
	// exist because the GUI gives reasoning a look of its own — violet, collapsible, labelled "reasoning ·
	// thinking…" or "·· 60,909 chars" — and the TUI was rendering the same content as a dim paragraph
	// indistinguishable from a hint line. The operator: "No reasoning block."
	//
	// The colour is the palette's indigo (the GUI's violet token has no direct terminal equivalent; indigo
	// is the same family and is already the palette's "attention, not error" accent). The BODY is
	// deliberately dimmer than the label: reasoning is context for reading a reply, not the reply, and
	// making it compete with the model's own words would be the noise the operator complained about on
	// the executions list.
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

	// PickerChip marks the CHOSEN chip in the model picker's ADAPTER/PROVIDER strip: plain theme
	// text with an UNDERLINE. No fill, no border — see buildStyles.
	PickerChip = lipgloss.NewStyle()
)

func buildStyles(t Theme) {
	Bg = t.Bg
	Surface = t.Surface
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

	ScreenBg = lipgloss.NewStyle().Background(t.Bg)
	SurfaceBg = lipgloss.NewStyle().Background(t.Surface)

	// Bubbles: a clearly different fill for the operator's own messages vs the
	// model's, with the bubble's own text colour (the plain text colour is
	// chosen against the BACKGROUND, not against a lifted bubble).
	bu, bm := bubbleFills(string(t.Bg), string(t.Accent))
	BubbleUser = lipgloss.NewStyle().Background(lipgloss.Color(bu)).Foreground(bubbleText(bu, t))
	BubbleModel = lipgloss.NewStyle().Background(lipgloss.Color(bm)).Foreground(bubbleText(bm, t))

	TabInactive = lipgloss.NewStyle().Foreground(t.TextDim).Background(t.Surface).Padding(0, 1)
	TabActive = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select).Padding(0, 1)
	TabBar = lipgloss.NewStyle().Background(t.Bg).Padding(0, 1)
	TabBarUnderline = lipgloss.NewStyle().Foreground(t.Border).Background(t.Bg)

	MenuPanel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.Border).
		Background(t.Surface).
		Foreground(t.Text)
	MenuTitle = lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Background(t.Surface)
	MenuRow = lipgloss.NewStyle().Foreground(t.Text).Background(t.Surface)
	MenuRowSel = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select)

	// Footer is STRAIGHT THEME TEXT — no background band. The operator: "The bottom connection
	// information is better but you put a black background box on it. I think it would be better to
	// just leave those as straight text. No background box at all."
	//
	// It had a Surface background, which is what made the row read as a filled strip; with the cells
	// now genuinely painted (see bgOpaque) that strip became visible where it used to be an invisible
	// near-match. The background is gone, so the footer is text on the screen's own background.
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
		Background(t.Surface).
		Padding(0, 2)

	StatusOK = lipgloss.NewStyle().Foreground(t.OK)
	StatusWarn = lipgloss.NewStyle().Foreground(t.Warn)
	StatusErr = lipgloss.NewStyle().Foreground(t.Err)
	StatusBusy = lipgloss.NewStyle().Foreground(t.Busy)
	// ToolName paints a TOOL ROW's name, ToolMeta its arguments and result.
	//
	// ToolName is its OWN token (Theme.Tool) rather than a borrow of StatusBusy, which is what it was: a
	// tool row is the ledger of calls already made, while Busy means an in-flight turn. The same colour
	// today, but a change to what "busy" means can no longer recolour the transcript's tool rows.
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
	//
	// bubbles renders the visible cursor as `m.Style.Inline(true).Reverse(true).Render(char)`
	// (cursor/cursor.go), so the terminal SWAPS whatever this style sets before anything reaches the screen.
	// The effective BLOCK colour is therefore this style's FOREGROUND, and the effective CHARACTER colour is
	// its BACKGROUND — the opposite of how it reads.
	//
	// It was `Background(AccentCyan).Foreground(Bg)`, which reverses to a BLOCK of Bg: #0c0f18, the near-black
	// surface token, on every dark palette. So the caret was a black block — the operator's "The cursor in
	// the composer should be white and not black on ALL dark themes on every page". Measured SGR before the
	// fix: `\x1b[7;38;2;12;15;24;48;2;7;182;213m` — reverse, then fg=#0c0f18 (which the terminal promotes to
	// the background).
	//
	// Setting foreground=Text and background=Bg gives the block the theme's own TEXT colour and the character
	// its own SURFACE colour — so on every dark palette the caret is the light colour and on every light one
	// it is the dark colour, which is what a caret is on both. It also stays legible by construction: the two
	// tokens are contrast-gated against each other (see TestStructuralContrast).
	ComposerCursor = lipgloss.NewStyle().Foreground(t.Text).Background(t.Bg)
	ReasoningLabel = lipgloss.NewStyle().Foreground(t.AccentIndigo).Bold(true)
	ReasoningBody = lipgloss.NewStyle().Foreground(t.TextDim)
	SpinnerStyle = lipgloss.NewStyle().Foreground(t.AccentCyan)

	DiffAdd = lipgloss.NewStyle().Foreground(t.OK).Background(t.SurfaceAlt)
	DiffDel = lipgloss.NewStyle().Foreground(t.Err).Background(t.SurfaceAlt)
	DiffCtx = lipgloss.NewStyle().Foreground(t.TextDim)
	// DiffEmphasis marks the CHANGED span inside an add/del line. It was Reverse(true) — reverse
	// video, which is theme-blind by construction: it inverts whatever the terminal is already
	// showing, so on a light terminal the marked span became a solid BLACK block and on a dark one a
	// white one. The operator, on a diff in light mode: "The black and green are both hard to read in
	// light mode ... some weird black text background you can't see anything." Those blocks were this
	// span.
	//
	// It is now weight + underline, which marks the span in BOTH modes, keeps the row's own add/del
	// colouring intact (the point of the emphasis is WHERE, not what colour), and cannot invert.
	DiffEmphasis = lipgloss.NewStyle().Bold(true).Underline(true)
	DiffHeader = lipgloss.NewStyle().Foreground(t.AccentIndigo).Bold(true)
	DiffLineNoOld = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffLineNoNew = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffGutter = lipgloss.NewStyle().Foreground(t.TextFaint)
	DiffPanel = lipgloss.NewStyle().
		Background(t.Bg).
		Border(lipgloss.RoundedBorder(), false, false, false, true).
		BorderForeground(t.Border)
	DiffTabActive = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select).Padding(0, 1)
	DiffTabInactive = lipgloss.NewStyle().Foreground(t.TextDim).Background(t.Surface).Padding(0, 1)
	DiffFileSel = lipgloss.NewStyle().Foreground(white).Bold(true).Background(t.Select)
	DiffClose = lipgloss.NewStyle().Foreground(t.TextFaint).Bold(true)
	// PickerChip is the model picker's chosen chip: plain theme text, UNDERLINED.
	//
	// The operator, having seen the outlined version: "I think we should just make those normal
	// theme text with an underline showing selection and avoid any kind of a background color or
	// border at all." Two earlier attempts got this wrong in opposite directions — a FILLED chip
	// (white on the Select fill, a near-black block) and then an OUTLINE (two border rules in the
	// accent colour) — and both read as a box rather than as a word. An underline marks the selection
	// without adding a fill or a frame, so the label sits on the surface's own background and the
	// strip can never be wider than its text.
	PickerChip = lipgloss.NewStyle().Foreground(t.Text).Underline(true)
	DiffBadgeAdd = lipgloss.NewStyle().Foreground(t.OK)
	DiffBadgeDel = lipgloss.NewStyle().Foreground(t.Err)
}

func init() { buildStyles(Dark) }

// Opaque pins one rendered row to exactly w cells with the app background
// painted on EVERY cell, and repairs the inner-reset hole that makes the
// terminal bleed through.
//
// Why this exists: any inner style (a panel border, a title, a list row, a
// status pill) ends its span with its own \x1b[0m reset. That reset turns the
// background OFF for every cell after it — including the padding this helper
// adds — so those cells render on the TERMINAL's own background. With a
// non-default terminal background (Konsole matrix, transparent setups) the
// operator sees straight through the "opaque" frame: panels riddled with
// see-through regions.
//
// The repair re-emits the background SGR immediately after each reset, so no
// cell is ever left unpainted. The shell has done this for its own rows since
// the opaque-frame work (bgOpaque in internal/tui/app.go); kit2's panels and
// dialogs compose their own rows and were never covered — this is the shared
// implementation both can use.
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

// RepairAfterResets re-asserts a BACKGROUND-ONLY style's background after
// every SGR reset in an already-rendered string.
//
// The style passed must carry a background and NOTHING else (no border, no
// padding): the repair derives its re-assert sequence from the style's own
// render, and a bordered/padded style renders a BOX — injecting border glyphs
// into the row. That is the bug that garbled the composer (the operator's
// "additional painted text area at the bottom"); the guard below now rejects
// such a style outright instead of corrupting the row.
func RepairAfterResets(s string, bg lipgloss.Style) string {
	const reset = "\x1b[0m"
	paint := bg.Render("")
	if paint == "" || strings.Contains(paint, "\n") {
		return s // no background to assert, or a non-background-only style
	}
	if !strings.HasSuffix(paint, reset) {
		return s
	}
	open := strings.TrimSuffix(paint, reset)
	if open == "" {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, reset)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		rest := s[i+len(reset):]
		b.WriteString(s[:i+len(reset)])
		// Re-assert only when MORE CELLS follow on this line. Two cases where
		// it must not:
		//   - nothing follows at all (end of the block);
		//   - the next thing is a line break, so this line has no more cells.
		// In both, a re-assert would leave this block's background ON for
		// whatever the CALLER paints on the same row — the shell pads every
		// row to the terminal width, so the composer's surface colour bled
		// across the rest of the row past its right border (the operator's
		// "blue box riding off the pane").
		// An immediately-following escape needs no repair either (it sets its
		// own state, and its reset is handled on the next pass).
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
