package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Regression for the operator's "the light theme was abysmal — I couldn't see
// anything". The GUI's border tokens are tasteful hairlines in a browser, but in
// a TUI the border is the ONLY thing separating panes AND carries each panel's
// title, so a ~1.1:1 border against the background makes the whole layout read
// as blank. Structural colours must stay legible in BOTH themes.
func TestStructuralContrast(t *testing.T) {
	for _, name := range Names() {
		Use(name)
		th := Active()
		bg := string(th.Bg)

		// Body text must be comfortably readable.
		if r := contrastRatio(string(th.Text), bg); r < 4.5 {
			t.Errorf("%s: Text contrast %.2f vs background %s, want >= 4.5", name, r, bg)
		}
		// Dimmed text (hints, meta) still needs to be readable.
		if r := contrastRatio(string(th.TextDim), bg); r < 3.0 {
			t.Errorf("%s: TextDim contrast %.2f vs background, want >= 3.0", name, r)
		}
		// Structural lines: the pane borders that carry the titles.
		if r := contrastRatio(string(th.Border), bg); r < 1.8 {
			t.Errorf("%s: Border contrast %.2f vs background — pane borders and their titles will be invisible", name, r)
		}
		// The inline-code chip draws the theme's body text on the raised fill (SurfaceAlt). It must be
		// legible as WORDS, since a code span is text — and this is the pair that replaced reverse
		// video, which could not be made legible at all because it is relative to whatever it lands
		// in. Gated at the same floor as body text.
		if r := contrastRatio(string(th.Text), string(th.SurfaceAlt)); r < 4.5 {
			t.Errorf("%s: the inline-code chip (Text on SurfaceAlt) is %.2f:1, want >= 4.5 — code spans "+
				"would be as hard to read as the reverse-video rendering they replaced", name, r)
		}
		// Status colours carry MEANING in text (a state on a row, a verdict, a
		// "failed"), so they have to be readable as words, not merely
		// distinguishable from the background.
		//
		// This gate used to demand 2.5:1, which is below even the WCAG floor for
		// NON-text UI components (3:1) — and the light palettes sat in the gap it
		// left: Warn measured 2.66:1 and OK 3.08:1 on a near-white background,
		// which the operator reported as "green text ... in light themes it is
		// VERY hard to read". A gate that passes unreadable text is worse than no
		// gate, because it certifies the problem.
		//
		// The floor is therefore the 4.5:1 body-text requirement on a LIGHT
		// background. Dark backgrounds keep the 3:1 UI floor: the dark set already
		// measures 6.4-13.9:1 (the one exception is gruvbox-dark's 3.80:1 red,
		// which is that palette's deliberate, readable-on-black signature colour —
		// raising it would mean shipping a different palette under Gruvbox's name).
		//
		// "Light" is DERIVED from the background's luminance rather than read from
		// a declared flag, so the requirement cannot disagree with the palette it
		// is measuring.
		minStatus := 3.0
		if relLuminance(bg) > 0.5 {
			minStatus = 4.5
		}
		for label, c := range map[string]lipgloss.Color{"OK": th.OK, "Warn": th.Warn, "Err": th.Err, "Busy": th.Busy} {
			if r := contrastRatio(string(c), bg); r < minStatus {
				t.Errorf("%s: %s contrast %.2f vs background %s, want >= %.1f (status text must be READABLE, not just distinguishable)",
					name, label, r, bg, minStatus)
			}
		}
	}
	Use(DefaultName)
}

// The two themes must be genuinely different palettes (a switch that changes
// nothing is a silent failure).
func TestThemesDiffer(t *testing.T) {
	if string(Dark.Bg) == string(Light.Bg) {
		t.Fatal("dark and light share a background — the switch would be a no-op")
	}
	if contrastRatio(string(Dark.Text), string(Dark.Bg)) < 4.5 {
		t.Error("dark theme body text is too low-contrast")
	}
}

// Every registered theme must be selectable, uniquely named, non-empty, and
// must actually re-derive the styles (a theme that renders identically to the
// previous one is a silent failure).
func TestRegistryIsUsable(t *testing.T) {
	names := Names()
	if len(names) < 2 {
		t.Fatalf("registry has %d themes, want at least 2", len(names))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			t.Fatal("a theme has an empty name")
		}
		if seen[n] {
			t.Fatalf("duplicate theme name %q in the registry", n)
		}
		seen[n] = true
		if Lookup(n) == nil {
			t.Fatalf("Lookup(%q) = nil but Names() lists it", n)
		}
		if !Use(n) {
			t.Fatalf("Use(%q) failed", n)
		}
		if Active().Name != n {
			t.Fatalf("Use(%q) left active = %q", n, Active().Name)
		}
		// The derived styles must carry the palette's colours.
		if got := string(Dark.Bg); n == Dark.Name && string(Active().Bg) != got {
			t.Fatalf("%s: active Bg = %q, want %q", n, Active().Bg, got)
		}
		if ScreenBg.Render("x") == "" {
			t.Fatalf("%s: ScreenBg renders nothing — the background is unset", n)
		}
	}
	if Lookup("definitely-not-a-theme") != nil {
		t.Fatal("Lookup must return nil for an unknown name")
	}
	if Use("definitely-not-a-theme") {
		t.Fatal("Use must reject an unknown name")
	}
	Use(DefaultName)
}

// The SELECTION FILL carries WHITE text (selected rows, the active tab pill,
// the file-selection chip). A fill that is too light renders the selection as a
// blank block — and this is exactly what the extended palette set risked, since
// each family's fill lightness comes from its accent.
func TestSelectionFillCarriesWhiteText(t *testing.T) {
	const white = "#f8fafc"
	for _, name := range Names() {
		Use(name)
		th := Active()
		if r := contrastRatio(white, string(th.Select)); r < 3.0 {
			t.Errorf("%s: white on the selection fill (%s) is %.2f:1 — a selected row reads as a blank block", name, th.Select, r)
		}
		// The fill must also be visible against the pane background.
		if r := contrastRatio(string(th.Select), string(th.Bg)); r < 1.5 {
			t.Errorf("%s: the selection fill (%s) is indistinguishable from the background", name, th.Select)
		}
	}
	Use(DefaultName)
}

// The palette set must cover the requested families in both modes, so the
// operator can actually choose greens, blues, purples, greys, etc.
func TestPaletteFamiliesPresent(t *testing.T) {
	have := map[string]bool{}
	for _, n := range Names() {
		have[n] = true
	}
	want := []string{
		// dark families (greens, blues, purples, blacks, greys…)
		"obsidian", "forest", "ocean", "violet", "ember", "amber", "rose", "teal", "crimson", "slate",
		// light families (light green, light purple, light grey…)
		"lumen", "forest-light", "ocean-light", "violet-light", "ember-light", "amber-light", "rose-light", "teal-light", "slate-light",
		// the ported pair that existing configs reference
		"dark", "light", "gruvbox-dark", "gruvbox-light",
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("palette %q is missing from the registry", n)
		}
	}
	if len(Names()) < 20 {
		t.Errorf("registry has %d palettes, want the full family set (>= 20)", len(Names()))
	}
}

// The operator's ask: the user's bubble must be OBVIOUSLY lighter than the
// model's (the GUI's relationship), on every palette — not merely a different
// token. Both fills must also keep the text readable and stay distinguishable
// from the screen background.
func TestBubbleContrast(t *testing.T) {
	for _, name := range Names() {
		Use(name)
		th := Active()
		bg := string(th.Bg)
		user := BubbleUser.GetBackground()
		model := BubbleModel.GetBackground()

		// A TRANSPARENT THEME'S BANDS ARE UNPAINTED — deliberately, so the chat surface is see-through like the
		// rest of the theme. There is no fill to measure, so the requirement inverts: both bands must paint
		// NOTHING (a band that quietly kept a fill would be the operator's "message blocks ... are not
		// transparent"), and the speakers must still be tellable apart by something other than a fill — which
		// the transcript provides as the operator's "You" label (see chat.view's userBandLabel).
		if th.Transparent {
			for label, c := range map[string]lipgloss.TerminalColor{"user": user, "model": model} {
				if c != nil && string(colorHex(c)) != "" {
					t.Errorf("%s: the %s band is FILLED (%s) on a transparent theme — the chat surface would "+
						"not be see-through", name, label, colorHex(c))
				}
			}
			// And its text must still be the palette's own, readable on the PANEL it sits on.
			if r := contrastRatio(string(th.Text), string(th.Surface)); r < 3.0 {
				t.Errorf("%s: the band text on the panel fill is %.2f:1 — the transcript would be unreadable "+
					"even though the bands themselves are see-through", name, r)
			}
			continue
		}

		if user == nil || model == nil {
			t.Fatalf("%s: bubble fills are unset", name)
		}
		us, ms := colorHex(user), colorHex(model)
		if us == "" || ms == "" {
			t.Fatalf("%s: bubble fills are not concrete colours (%v / %v)", name, user, model)
		}
		if us == ms {
			t.Fatalf("%s: user and model bubbles are the SAME colour (%s)", name, us)
		}
		// The two fills must be visibly separated from each other.
		if r := contrastRatio(us, ms); r < 1.18 {
			t.Errorf("%s: user(%s) vs model(%s) bubble contrast is %.2f — they read as the same bubble", name, us, ms, r)
		}
		// Each fill must be distinct from the screen background it sits on.
		for label, c := range map[string]string{"user": us, "model": ms} {
			if r := contrastRatio(c, bg); r < 1.04 {
				t.Errorf("%s: the %s bubble (%s) is indistinguishable from the background (%s)", name, label, c, bg)
			}
		}
		// Text must stay readable on BOTH fills — measured against the colour the bubble ACTUALLY DRAWS,
		// via bubbleText. That is not a loosening: bubbleText is what picks the bubble's foreground, and it
		// is allowed to move the palette's text out of the way on a fill the text cannot be read on (see its
		// own note — a mid-tone foreground such as One Dark's has no room on a lifted bubble). Measuring
		// th.Text here instead would demand readability from a colour this code never draws, which would
		// either fail a readable pair or force every palette's bubbles to be flattened for the worst case.
		for label, c := range map[string]string{"user": us, "model": ms} {
			drawn := string(bubbleText(c, *th))
			if r := contrastRatio(drawn, c); r < 4.0 {
				t.Errorf("%s: the bubble text (%s) on the %s bubble (%s) is %.2f:1 — unreadable",
					name, drawn, label, c, r)
			}
		}
	}
	Use(DefaultName)
}

// colorHex extracts the #rrggbb from a lipgloss colour.
func colorHex(c lipgloss.TerminalColor) string {
	if c == nil {
		return ""
	}
	type stringer interface{ String() string }
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	if s, ok := c.(lipgloss.Color); ok {
		return string(s)
	}
	return ""
}

// The two speakers are separated by a full-width background BAND (user lighter,
// model darker) rather than by text colour, so the pairing is the bubble fills'
// problem and is gated by TestBubbleContrast. There is deliberately no
// text-colour gate here: tinting only the glyphs was rejected by the operator.

// NOTHING THAT MARKS TEXT MAY USE REVERSE VIDEO.
//
// Reverse video (SGR 7) cannot respect a theme: it inverts whatever the terminal is ALREADY showing,
// so the same style renders as a black block on a light terminal and a white one on a dark terminal.
// It also ignores the app's palette entirely, which is worse when the terminal's own colours disagree
// with the chosen theme — the common case, since the theme is an app setting.
//
// The operator, on a diff in light mode: "The black and green are both hard to read in light mode ...
// some weird black text background you can't see anything." Those blocks were DiffEmphasis, which was
// Reverse(true). This gate keeps the class of bug from coming back.
//
// The two deliberate exceptions, which are NOT text marking: a one-cell caret bar (a cursor is
// conventionally a reversed cell) and md's inline-code FALLBACK, used only when a caller declares no
// surface — every real call site passes one, so the chip is what renders.
func TestNoTextMarkingUsesReverseVideo(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	Use(DefaultName)
	defer Use(DefaultName)

	for label, style := range map[string]lipgloss.Style{
		"DiffEmphasis": DiffEmphasis,
		"DiffAdd":      DiffAdd,
		"DiffDel":      DiffDel,
		"DiffCtx":      DiffCtx,
		"ListItem":     ListItem,
		"Text":         lipgloss.NewStyle().Foreground(Text),
	} {
		out := style.Render("marked text")
		if strings.Contains(out, "\x1b[7m") {
			t.Errorf("%s renders with reverse video, which inverts against the terminal instead of the "+
				"theme — a black block on a light terminal: %q", label, out)
		}
	}

	// And the emphasis still MARKS the span: if it stopped distinguishing anything, the diff would
	// lose the intra-line change indicator entirely.
	out := DiffEmphasis.Render("changed")
	if out == "changed" {
		t.Error("DiffEmphasis renders its text identically to plain text, so a diff no longer shows " +
			"WHICH part of the line changed")
	}
}
