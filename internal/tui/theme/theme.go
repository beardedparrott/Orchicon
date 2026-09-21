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

	// Transparent makes the APP BACKGROUND unpainted, so whatever the terminal has behind orch — its own
	// background colour, its transparency, a compositor's blur — shows through the frame. The operator, on
	// opencode's themes: "I would like a couple of semi-transparent background themes for dark and light."
	//
	// WHAT IT DOES NOT DO, and this is the whole design: the TINTS STAY. Panels, bubbles, selection fills,
	// code chips and the diff surface keep their palette colours, so a transparent theme still reads as a
	// structured UI rather than as bare text on the operator's wallpaper. The operator, confirming exactly
	// that: "Yes the tints should definitely be there."
	//
	// WHY THERE IS NO ALPHA. A terminal cell has a foreground and a background and nothing in between: there
	// is no opacity channel to set, so "80% transparent" is not a value that can be expressed. What IS
	// expressible is "this cell is not painted at all", which is what every transparent theme in every TUI
	// means by the word — and it composes correctly with a transparent terminal, where the app's own idea of
	// a background would otherwise be the one thing blocking the effect. The palette's Bg colour is kept on
	// the struct (derivations still need it, and the contrast gates still measure against it); it is simply
	// not painted.
	Transparent bool

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

// transparentSuffix marks a palette whose app background is left unpainted. It is a SUFFIX RULE rather
// than a table: `Lookup` resolves `<any palette>-transparent` by taking that palette and setting its
// Transparent flag, so EVERY theme has a see-through variant and a new palette gains one for free — no
// second copy of its colours to keep in step, and no way for the pair to drift apart.
const transparentSuffix = "-transparent"

// transparentListed are the transparent variants shown in the picker.
//
// THE OPERATOR ASKED FOR "a couple of semi-transparent background themes for dark and light", so the LIST is
// deliberately a couple each rather than every palette doubled: the picker is a list an operator reads, and
// 50 rows of `-transparent` twins would bury the palettes they are actually choosing between. The suffix
// rule above means nothing is LOST by that — `/theme forest-transparent` still works for any palette,
// which is the escape hatch a short list is allowed to have.
var transparentListed = []string{"obsidian", "forest", "tokyo-night", "lumen", "light", "github-light"}

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
	// The hand-written community ports (theme_named.go), then the generated families. Both are listed
	// before the transparent variants so a reader of `/theme` sees the palettes first and the see-through
	// modes after them.
	out = append(out, namedThemes...)
	for _, t := range derivedThemes {
		out = append(out, t)
	}
	// The transparent variants are appended LAST rather than interleaved: they are a MODE of a palette rather
	// than a family of their own, and the picker groups the list into DARK/LIGHT sections anyway (see
	// IsDark), so appending here costs no grouping correctness and keeps the palette list readable.
	//
	// THIS WALKS findBase, NOT Lookup. Lookup reads the registry, so calling it here would be an
	// initialisation cycle (registry -> buildRegistry -> Lookup -> registry) — and the cycle is the
	// compiler catching a real ordering question rather than a formality.
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
	// A TRANSPARENT THEME IS ADAPTED TO THE TERMINAL FIRST — see transparent_adapt.go. It happens HERE rather
	// than in the palette or in the render path because this is the single point where "this palette is being
	// used" is known, and because everything downstream (the styles, the markdown tokens, the contrast gates,
	// the operator's screen) must agree on ONE effective palette. The registry is never mutated: Lookup hands
	// back the pristine palette, the adaptation is a copy, so switching back and forth cannot compound it.
	if t.Transparent {
		t = adaptTransparentForTerminal(t)
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
//
// A `<palette>-transparent` name resolves to that palette with its app background unpainted — see
// transparentSuffix. The suffix is handled AFTER the exact lookup so a palette literally named with those
// characters would still win, and the base is resolved by exact name, so there is no recursion.
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
// globals. It deliberately reads no registry — buildRegistry builds that registry, so a dependency in this
// direction would be an initialisation cycle.
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
// theme. An explicit preference (the config's `theme`, or ORCHICON_THEME) always wins — this is only
// the fallback, so changing it changes how orch looks out of the box and nothing at all for an
// operator who has already chosen a palette.
//
// SLATE, at the operator's request: "I think Slate is pretty sleek and professional. Let's make that the
// default theme over ember." It is the `slate` palette from theme_palettes.go — the low-saturation grey-blue
// family — and NOT "slate-light" beside it, which is the same family in light mode. Lookup matches the name
// exactly, so a typo here would fall back to the base dark palette without an error; theme_test.go asserts
// this name resolves, and that it is a DARK palette.
//
// (It previously named `forest`, also at the operator's request; this supersedes that choice. `forest` is
// unchanged and still selectable — only the out-of-the-box default moved.)
const DefaultName = "slate"

// Resolved active colors, re-pointed by Use. Render paths read these via
// the styles; direct color reads stay possible for layout math.
var (
	// Bg is the APP BACKGROUND, and it is UNPAINTED (an empty colour) on a transparent theme — see
	// Theme.Transparent. Every consumer already paints through it (ScreenBg, screenBase, the kit2 panels,
	// the tab bar, the diff panel), so a transparent theme needs no per-site special cases: the value they
	// all read is the one that changes.
	Bg      lipgloss.TerminalColor
	Surface lipgloss.TerminalColor
	// ComposerFill is the COMPOSER's background, which is the one fill that follows the app background's
	// transparency rather than the panels'. The operator: "Composer should also be transparent as well on
	// transparent themes." It is a token of its own so the dock never has to know whether the active theme
	// is transparent — it asks for the composer's fill and gets the right answer either way.
	ComposerFill lipgloss.TerminalColor
	// PanelBg is the background every PANEL paints — a screen's pane, a rail, a dialog, a picker. It is the
	// palette's own Bg colour and it is ALWAYS PAINTED, transparent theme or not.
	//
	// THIS IS THE DISTINCTION A TRANSPARENT THEME TURNS ON, and getting it wrong is what made the light
	// variants unreadable: the panes were painting through theme.Bg, so "transparent app background" silently
	// became "transparent panes" too — and a light palette's dark text landed on the OPERATOR'S dark terminal
	// with nothing behind it. Measured on lumen-transparent against a dark terminal: a screen of black text on
	// black. The operator: "the light transparent themes are almost impossible to see/read."
	//
	// So the rule is: the APP BACKGROUND (the frame fill, the gaps between panes, the tab bar strip, the footer
	// strip, the composer) may be left to the terminal; a PANEL always carries its tint, because a panel is a
	// surface with text on it and it has to be legible regardless of what the terminal looks like.
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

	// ComposerBg is the BACKGROUND-ONLY form of the COMPOSER's fill. It differs from SurfaceBg on a
	// transparent theme, where the composer is unpainted and the panels are not — so the composer's own
	// repair must ask for this one, and never for SurfaceBg.
	ComposerBg = lipgloss.NewStyle()

	// PanelBgStyle is the BACKGROUND-ONLY form of PanelBg: the tint a panel's rows, padding and reset repairs
	// are painted with. Always painted, on every theme — see PanelBg.
	PanelBgStyle = lipgloss.NewStyle()

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

	// ComposerSelect paints the composer's rows while the WHOLE buffer is selected (ctrl+a).
	//
	// It is the SAME selection fill the list rows and the active tab already use, so "selected" means one
	// thing across the TUI rather than one per surface. It exists because the operator reported the missing
	// half of the feature: "when I hit ctrl+a in the composer it DOES select all but it doesn't actually
	// show the cursor highlight over all of the text, it just gives you a little message."
	ComposerSelect = lipgloss.NewStyle()

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
	// THE EFFECTIVE BACKGROUND, which is the whole of the transparency mechanism.
	//
	// An EMPTY lipgloss colour renders NO background sequence at all (verified against the library: a style
	// whose only property is `Background(lipgloss.Color(""))` emits nothing), so "transparent" here is not
	// a simulation or a very-dark colour — the cells are genuinely not painted.
	//
	// That choice also makes the frame's repair machinery degrade correctly rather than fight it:
	// bgOpaque and RepairAfterResets both derive their re-assert sequence from a zero-width render of
	// ScreenBg and RETURN THE LINE UNCHANGED when that render is empty. On a transparent theme there is no
	// background to re-assert, which is exactly true — the code path already existed for "no background",
	// and was documented as such long before this theme needed it.
	bg := t.Bg
	composerFill := t.Surface
	surface := t.Surface
	if t.Transparent {
		bg = lipgloss.Color("")
		composerFill = lipgloss.Color("")
		// Surface is the INACTIVE-TAB and panel fill, so it goes with the background. SurfaceAlt does NOT: it is
		// the code chip / code block / diff fill, which is CONTENT rather than a surface — see
		// transparent_adapt.go for why those keep a fill.
		surface = lipgloss.Color("")
	}
	Bg = bg
	ComposerFill = composerFill
	// THE PANEL TINT FOLLOWS THE TRANSPARENCY, like the app background and the composer.
	//
	// The operator: "now the transparents are not transparent at all ... the terminal should bleed through
	// everywhere but with the light tint of the color scheme coming through." The previous cut kept panels
	// painted to make a light transparent theme readable on a dark terminal, which is a real problem — but a
	// panel that paints a fill is exactly what stops the bleed-through, so the readability is solved in the
	// FOREGROUNDS instead (see adaptTransparentForTerminal) and the panels get out of the way with everything
	// else.
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
	//
	// IT WAS RECOMPUTED FROM t.Bg, which is the same value TODAY and a second source of truth forever: a test
	// that disabled PanelBg (to prove the pane-tint behaviour was what made a transparent theme readable)
	// left the STYLE still painting, so the assertion stayed green and the proof was vacuous. Deriving one from
	// the other makes the drift impossible and the proof real.
	PanelBgStyle = lipgloss.NewStyle().Background(PanelBg)
	// ComposerBg is the BACKGROUND-ONLY composer fill, for the resets the composer's own rows repair —
	// the same shape as SurfaceBg, and unpainted on a transparent theme so the repair has nothing to
	// re-assert there (the operator's "Composer should also be transparent as well on transparent themes").
	ComposerBg = lipgloss.NewStyle().Background(composerFill)

	// Bubbles: a clearly different fill for the operator's own messages vs the
	// model's, with the bubble's own text colour (the plain text colour is
	// chosen against the BACKGROUND, not against a lifted bubble).
	// The bubble FILLS are derived from the palette's own Bg hex, deliberately NOT from the effective
	// background above: on a transparent theme the effective value is unpainted, and a bubble derived from
	// "no colour" would be a bubble derived from black. The palette's colour is what the tint is made of.
	bu, bm := bubbleFills(string(t.Bg), string(t.Accent))
	// ON A TRANSPARENT THEME NEITHER BAND IS FILLED, so the chat surface is as see-through as the rest of the
	// theme. The operator, on the first version: "Message blocks and composer are not transparent. I thought we
	// decided on changing the tint on those items so they look semi-transparent as well?" — and they are right
	// that the two were inconsistent: the app background and the composer had been left to the terminal while
	// the transcript's bands stayed solid.
	//
	// THE TWO SPEAKERS ARE STILL TELLABLE APART, which is what the fills were for: the operator's band already
	// carries its "You" label (chat.view's userBandLabel), so the label does the job the fill was doing. That
	// is why both can go rather than one, and it is the honest shape of "semi-transparent".
	//
	// (Nothing here can blend: a terminal cell has a foreground and a background and no alpha, so "80% opaque"
	// is not expressible — see Theme.Transparent. Leaving the fill off is what every TUI means by the word.)
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
		Background(composerFill).
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
	// THE CARET KEEPS THE PALETTE'S SOLID COLOUR, EVEN ON A TRANSPARENT THEME, and the caret gate is what
	// caught it: with the background unpainted the caret's character inherited the terminal's own colour,
	// which nothing here can certify as legible on the block behind it. That is not a test to relax — a
	// caret is a FILLED BLOCK, not a surface, so the honest rendering is the palette's own colour. On a
	// transparent theme the frame is see-through and the cursor is solid, which is exactly how a terminal
	// cursor behaves.
	ComposerCursor = lipgloss.NewStyle().Foreground(t.Text).Background(t.Bg)
	// The white-on-Select pairing is the same one MenuRowSel and ListItemSelected use, and it is contrast-gated
	// for every palette by TestSelectionFillCarriesWhiteText — so a theme that cannot carry the fill fails the
	// suite rather than painting an unreadable composer.
	ComposerSelect = lipgloss.NewStyle().Foreground(white).Background(t.Select)
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
		Background(bg).
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

// OpaquePanel is Opaque for a PANEL: the padding and the reset repair use the PANEL's tint rather than the app
// background's.
//
// IT EXISTS BECAUSE THE TWO DIVERGED, and the divergence is the whole point of a transparent theme. On a solid
// theme they are the same colour, so one function did for both; on a transparent theme the app background is
// UNPAINTED, and a panel padded or repaired through it would punch an invisible — or worse, a
// terminal-coloured — hole through the panel it is drawing.
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
//
// TWO SPELLINGS, because two producers and only one of them was being caught:
//
//   - "\x1b[0m" — lipgloss/termenv's own terminator, and the form the shell's own rows carry;
//   - "\x1b[m"  — a reset with an OMITTED parameter, which is identical in effect. It is what the
//     bubbles VIEWPORT writes when it pads a line out to the pane width.
//
// MISSING THE SECOND WAS A REAL BUG, and its symptom is the operator's "every theme has a weird color
// in the whitespace in work items. It just doesn't look great. We shouldn't be filling in spaces with
// colors like that."
//
// The mechanism, measured: a detail pane renders its body through a bubbles VIEWPORT, which pads every
// short line by writing `\x1b[m` and THEN the spaces. The pane's tint is painted by the OUTER style, so
// the inner `\x1b[m` clears it and the padding after it carries NO background at all — on a pane whose
// tint differs from the terminal's own background, that whitespace renders in the TERMINAL's colour
// instead of the pane's. Every markdown line shorter than the pane showed it (a heading, a bullet's last
// wrap, a paragraph's tail), which is why it read as coloured bands and why it was never theme-specific:
// the hole is punched in whatever the active palette is.
//
// `\x1b[m` and `\x1b[0m` differ at their third byte, so neither is a prefix of the other and the scan
// below cannot mis-match one as the other.
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
