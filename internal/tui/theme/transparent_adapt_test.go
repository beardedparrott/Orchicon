package theme

// transparent_adapt_test.go — a transparent theme is legible on WHATEVER the terminal is.
//
// The operator: "the light transparent themes are almost impossible to see/read", and then, once the fill-based
// fix made them readable but opaque: "now the transparents are not transparent at all ... the terminal should
// bleed through everywhere."
//
// Both complaints at once is the requirement, and it can only be met in the FOREGROUNDS: the surfaces are
// unpainted, so the palette's colours are drawn on the terminal's own background and have to be chosen for it.
// These tests hold that on BOTH grounds, and hold the promise that a palette already suited to the ground is
// left completely alone.

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// withGround pins the terminal ground for a test, so the adaptation is deterministic without a real tty.
func withGround(t *testing.T, ground string) {
	t.Helper()
	t.Setenv(SetTerminalBgEnv, ground)
	t.Cleanup(func() { Use(DefaultName) })
}

// ON EITHER GROUND, EVERY TRANSPARENT PALETTE'S FOREGROUNDS CLEAR THEIR FLOORS.
//
// This is the readability half, and it is checked against the ACTIVE palette because that is what renders —
// the adaptation happens in Use, so a test that read the registry instead would be checking a palette nobody
// sees.
func TestTransparentThemesAreLegibleOnEitherGround(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	for _, ground := range []string{"dark", "light"} {
		withGround(t, ground)
		for _, name := range []string{
			"forest-transparent", "tokyo-night-transparent", "obsidian-transparent",
			"lumen-transparent", "light-transparent", "github-light-transparent",
		} {
			if !Use(name) {
				t.Fatalf("%s: Use failed", name)
			}
			th := Active()
			bg := string(th.Bg)

			if r := contrastRatio(string(th.Text), bg); r < 4.5 {
				t.Errorf("%s on a %s terminal: body text is %.2f:1 against the ground %s — unreadable, which is "+
					"the operator's \"almost impossible to see/read\"", name, ground, r, bg)
			}
			if r := contrastRatio(string(th.TextDim), bg); r < 3.0 {
				t.Errorf("%s on a %s terminal: dim text is %.2f:1", name, ground, r)
			}
			if r := contrastRatio(string(th.Border), bg); r < 1.8 {
				t.Errorf("%s on a %s terminal: pane borders are %.2f:1 — the titles they carry would vanish",
					name, ground, r)
			}
			// Body text on the RAISED fill, which is where code spans and diff lines draw.
			if r := contrastRatio(string(th.Text), string(th.SurfaceAlt)); r < 4.5 {
				t.Errorf("%s on a %s terminal: the code chip is %.2f:1", name, ground, r)
			}
		}
	}
}

// AND A PALETTE ALREADY SUITED TO THE GROUND IS LEFT BYTE-IDENTICAL.
//
// This is the promise that keeps the adaptation from being a repaint: a dark transparent theme on a dark
// terminal must render exactly as it did before the adaptation existed, or every one of these themes would
// change under the operator the moment they switched to it. Asserted token by token rather than by eye.
func TestAPaletteSuitedToTheGroundIsUnchanged(t *testing.T) {
	withGround(t, "dark")
	for _, name := range []string{"forest-transparent", "tokyo-night-transparent", "obsidian-transparent"} {
		src := Lookup(name)
		if src == nil {
			t.Fatalf("%s does not resolve", name)
		}
		if !Use(name) {
			t.Fatalf("%s: Use failed", name)
		}
		got := Active()
		for _, c := range []struct {
			token        string
			want, actual string
		}{
			{"Text", string(src.Text), string(got.Text)},
			{"TextDim", string(src.TextDim), string(got.TextDim)},
			{"TextFaint", string(src.TextFaint), string(got.TextFaint)},
			{"Border", string(src.Border), string(got.Border)},
			{"BorderFaint", string(src.BorderFaint), string(got.BorderFaint)},
			{"Accent", string(src.Accent), string(got.Accent)},
			{"OK", string(src.OK), string(got.OK)},
			{"Warn", string(src.Warn), string(got.Warn)},
			{"Err", string(src.Err), string(got.Err)},
			{"Busy", string(src.Busy), string(got.Busy)},
			{"Tool", string(src.Tool), string(got.Tool)},
		} {
			if c.want != c.actual {
				t.Errorf("%s on a dark terminal: %s moved from %s to %s — a palette that already suits the "+
					"ground must render exactly as it did", name, c.token, c.want, c.actual)
			}
		}
		// The GROUND is recorded as the palette's background, which is what the contrast gates measure against.
		// It is not painted — see buildStyles — but reporting it here is what lets every existing gate measure
		// the pairing the operator actually sees, with no special case in any of them.
		if string(got.Bg) != "#000000" {
			t.Errorf("%s: the recorded ground is %s, want the dark terminal's #000000", name, got.Bg)
		}
	}
}

// THE GROUND IS WHAT THE TERMINAL SAYS, AND THE OVERRIDE WINS.
//
// The override matters as much as the detection: a transparent terminal has, by definition, been configured so
// that its "background colour" is not what is behind the app — so the OSC reply is absent or a lie, and the
// operator is the only reliable source.
func TestTheTerminalGroundFollowsTheOverride(t *testing.T) {
	withGround(t, "light")
	if got := terminalGroundHex(); got != "#ffffff" {
		t.Errorf("with %s=light the ground is %s, want #ffffff", SetTerminalBgEnv, got)
	}
	withGround(t, "dark")
	if got := terminalGroundHex(); got != "#000000" {
		t.Errorf("with %s=dark the ground is %s, want #000000", SetTerminalBgEnv, got)
	}
	// An unrecognised value is treated as unset rather than as an error, so a typo cannot black-hole the theme.
	t.Setenv(SetTerminalBgEnv, "banana")
	if got := terminalGroundHex(); got != "#000000" && got != "#ffffff" {
		t.Errorf("an unrecognised override produced %q, want the detected ground", got)
	}
}

// SWITCHING TO AND FROM A TRANSPARENT THEME CANNOT COMPOUND.
//
// The adaptation is applied in Use, and it must always start from the PRISTINE palette — otherwise each switch
// would nudge the colours a little further toward the readable end until the theme bore no resemblance to
// itself. Asserted by switching twice and comparing.
func TestAdaptationDoesNotCompound(t *testing.T) {
	withGround(t, "light")
	if !Use("lumen-transparent") {
		t.Fatal("Use failed")
	}
	first := *Active()
	if !Use("forest") {
		t.Fatal("switching away failed")
	}
	if !Use("lumen-transparent") {
		t.Fatal("switching back failed")
	}
	second := *Active()
	if string(first.Text) != string(second.Text) || string(first.Accent) != string(second.Accent) ||
		string(first.Border) != string(second.Border) {
		t.Errorf("the second application differs from the first (%s/%s/%s vs %s/%s/%s) — the adaptation is "+
			"compounding instead of starting from the palette",
			first.Text, first.Accent, first.Border, second.Text, second.Accent, second.Border)
	}
}

// THE REGISTRY IS NEVER MUTATED. Lookup must keep handing back the declared palette, or the picker's listing, the
// IsDark grouping and every future Use would be reading an adapted copy.
func TestTheAdaptationDoesNotTouchTheRegistry(t *testing.T) {
	withGround(t, "light")
	before := *Lookup("tokyo-night-transparent")
	if !Use("tokyo-night-transparent") {
		t.Fatal("Use failed")
	}
	after := *Lookup("tokyo-night-transparent")
	if string(before.Text) != string(after.Text) || string(before.Bg) != string(after.Bg) {
		t.Errorf("the registry entry changed from (%s on %s) to (%s on %s)",
			before.Text, before.Bg, after.Text, after.Bg)
	}
	if IsDark("tokyo-night-transparent") != IsDark("tokyo-night") {
		t.Error("the transparent variant's mode no longer matches its base palette's")
	}
}
