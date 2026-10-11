package main

// wsltrucolor_test.go — the WSL true-colour default.
//
// THE CONTRACT IS AN OUTCOME, NOT A DECISION SHAPE. Each case states the terminal
// it is describing, runs the app's own default, and then feeds the resulting
// environment through the REAL termenv resolution — so a case asserts the profile
// the operator ends up with, and a change that supplies the advertisement in the
// wrong form (or in the wrong order relative to NO_COLOR) fails here.
//
// The reported symptom these come from: 'all of the TUI themes are off ... the
// background is black instead of the dark teal color', on a Windows terminal,
// under WSL.

import (
	"testing"

	"github.com/muesli/termenv"
)

// applyDefaultThenResolve is the whole chain: run the app's WSL default against
// the described terminal, then resolve the profile from the resulting environment
// exactly as main does.
func applyDefaultThenResolve(tty bool, env envMap) termenv.Profile {
	getenv := func(k string) string { return env[k] }
	if v := wslTrueColorDefault(tty, getenv); v != "" {
		// Mirror os.Setenv into the described terminal: the applier mutates the
		// process environment, and this map is that environment.
		env[colorTermEnv] = v
	}
	return resolve(tty, env)
}

func TestWSLTrueColorDefault(t *testing.T) {
	cases := []struct {
		name string
		tty  bool
		env  envMap
		want termenv.Profile
		why  string
	}{
		{
			name: "the reported case: Windows Terminal under WSL, 256-colour TERM, no COLORTERM",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"},
			want: termenv.TrueColor,
			why:  "without the supplied advertisement termenv maps TERM=xterm-256color to ANSI256, and every dark theme ground quantises to grey 233/234 — the black background the operator reported",
		},
		{
			name: "WSL_INTEROP is enough on its own",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "WSL_INTEROP": "/run/WSL/8_interop"},
			want: termenv.TrueColor,
			why:  "either WSL marker identifies the distro session",
		},
		{
			name: "a terminal that already advertised is left alone",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "COLORTERM": "truecolor"},
			want: termenv.TrueColor,
			why:  "COLORTERM set means the terminal spoke; our inference must not override it",
		},
		{
			name: "COLORTERM=yes pins ANSI256 even under WSL",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "COLORTERM": "yes", "WSL_DISTRO_NAME": "Ubuntu"},
			want: termenv.ANSI256,
			why:  "an explicit 256-colour advertisement is respected — this is the escape hatch for a terminal that really cannot do 24-bit",
		},
		{
			name: "NO_COLOR still wins",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu", "NO_COLOR": "1"},
			want: termenv.Ascii,
			why:  "the default SUPPLIES an advertisement and lets termenv decide; termenv checks NoColor first, so asking for no colour still gets you none",
		},
		{
			name: "TERM=dumb is left alone",
			tty:  true,
			env:  envMap{"TERM": "dumb", "WSL_DISTRO_NAME": "Ubuntu"},
			want: termenv.Ascii,
			why:  "a terminal that has declared it can do nothing must not be talked into 24-bit colour",
		},
		{
			name: "the opt-out disables the default",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu", TrueColorOptOut: "0"},
			want: termenv.ANSI256,
			why:  "a decision taken on the operator's behalf needs a way to be contradicted",
		},
		{
			name: "not a terminal (piped output)",
			tty:  false,
			env:  envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"},
			want: termenv.Ascii,
			why:  "a piped run writes no escape sequences and its environment is not mutated, matching disableAutoWrap",
		},
		{
			name: "not WSL",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color"},
			want: termenv.ANSI256,
			why:  "outside WSL there is no reason to distrust the advertisement, and termenv is simply correct — the default must not leak into other platforms",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyDefaultThenResolve(tc.tty, tc.env)
			if got != tc.want {
				t.Errorf("profile = %v, want %v\nwhy this case exists: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestWithoutTheDefaultTheReportedCaseIsStillBroken is the NEGATIVE CONTROL, and
// it is the reason to trust the case above. It resolves the SAME terminal the
// operator reported with the default NOT applied, and asserts the broken outcome.
// Without it, the passing case would only show that something in the chain returns
// TrueColor — not that the default is what changed it.
func TestWithoutTheDefaultTheReportedCaseIsStillBroken(t *testing.T) {
	reported := envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"}

	if got := resolve(true, reported); got != termenv.ANSI256 {
		t.Fatalf("the unrepaired terminal resolved to %v, not ANSI256 — this control no longer describes the reported symptom, so the case it justifies proves nothing", got)
	}
	if got := applyDefaultThenResolve(true, envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"}); got != termenv.TrueColor {
		t.Fatalf("with the default applied the same terminal resolved to %v, want TrueColor", got)
	}
}

// TestWSLTrueColorDefaultDoesNotMutateANonTTYEnvironment pins the guard that the
// profile table cannot see: on a piped run the environment is not touched at all,
// rather than touched and then ignored by termenv. The difference is observable to
// anything downstream that reads COLORTERM.
func TestWSLTrueColorDefaultDoesNotMutateANonTTYEnvironment(t *testing.T) {
	env := envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"}
	getenv := func(k string) string { return env[k] }
	if v := wslTrueColorDefault(false, getenv); v != "" {
		t.Errorf("a non-TTY stdout must not be given an advertisement; got %q", v)
	}
	if _, set := env[colorTermEnv]; set {
		t.Error("the environment was mutated for a non-TTY run")
	}
}

// TestWSLTrueColorDefaultIsTheOnlySourceOfTheValue pins that the supplied value is
// the one termenv recognises. '24bit' would also work; a typo or a capitalised
// variant would silently do nothing, and the symptom would be the original bug.
func TestWSLTrueColorDefaultIsTheOnlySourceOfTheValue(t *testing.T) {
	env := envMap{"TERM": "xterm-256color", "WSL_DISTRO_NAME": "Ubuntu"}
	got := wslTrueColorDefault(true, func(k string) string { return env[k] })
	if got != "truecolor" {
		t.Fatalf("supplied %q, want the termenv-recognised %q", got, "truecolor")
	}
}
