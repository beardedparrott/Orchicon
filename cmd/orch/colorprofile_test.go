package main

import (
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/muesli/termenv"
)

// envMap is a termenv.Environ over a literal map, so a case can state the terminal it is describing
// without touching the test process's own environment.
type envMap map[string]string

func (e envMap) Getenv(key string) string { return e[key] }

func (e envMap) Environ() []string {
	out := make([]string, 0, len(e))
	for k, v := range e {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// resolve runs the app's own decision against a described terminal.
func resolve(tty bool, env envMap) termenv.Profile {
	return terminalColorProfile(termenv.NewOutput(io.Discard, termenv.WithTTY(tty), termenv.WithEnvironment(env)))
}

// TestColorProfileFollowsTheTerminal pins the contract this file exists for: the profile is the TERMINAL's
// advertisement, read the way termenv reads it.
//
// The first three cases are the ones the hand-rolled `COLORTERM == "truecolor" || COLORTERM == "24bit"`
// check this replaced got WRONG — each is marked, so a future "simplification" back to an env equality
// check fails here instead of quietly returning.
func TestColorProfileFollowsTheTerminal(t *testing.T) {
	cases := []struct {
		name string
		tty  bool
		env  envMap
		want termenv.Profile
		why  string
	}{
		{
			name: "colorterm truecolor",
			tty:  true,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "xterm-256color"},
			want: termenv.TrueColor,
		},
		{
			name: "colorterm 24bit",
			tty:  true,
			env:  envMap{"COLORTERM": "24bit", "TERM": "xterm-256color"},
			want: termenv.TrueColor,
		},
		{
			// THE CASE THE OLD CHECK MISSED. termenv lowercases COLORTERM before matching; an exact
			// equality does not, so a terminal advertising 24-bit in mixed case was left undetected.
			name: "colorterm in mixed case",
			tty:  true,
			env:  envMap{"COLORTERM": "TrueColor", "TERM": "xterm-256color"},
			want: termenv.TrueColor,
			why:  "termenv matches COLORTERM case-insensitively; the old equality check did not",
		},
		{
			// THE CASE THE OLD CHECK GOT BACKWARDS. screen cannot render 24-bit, so termenv degrades to
			// ANSI256 — forcing TrueColor there mangles every colour on screen.
			name: "truecolor under gnu screen",
			tty:  true,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "screen-256color"},
			want: termenv.ANSI256,
			why:  "screen cannot render 24-bit; forcing TrueColor there is worse than degrading",
		},
		{
			name: "truecolor under tmux (which can)",
			tty:  true,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "screen-256color", "TERM_PROGRAM": "tmux"},
			want: termenv.TrueColor,
			why:  "tmux advertises itself through TERM_PROGRAM, which is how screen is told apart",
		},
		{
			name: "a truecolor terminal that names itself in TERM",
			tty:  true,
			env:  envMap{"TERM": "xterm-kitty"},
			want: termenv.TrueColor,
			why:  "kitty/alacritty/wezterm/ghostty/contour/rio are recognised without COLORTERM",
		},
		{
			name: "an ordinary 256-colour terminal",
			tty:  true,
			env:  envMap{"TERM": "xterm-256color"},
			want: termenv.ANSI256,
		},
		{
			name: "a plain xterm",
			tty:  true,
			env:  envMap{"TERM": "xterm"},
			want: termenv.ANSI,
		},
		{
			// THE THIRD CASE THE OLD CHECK OVERRODE. An explicit profile bypasses EnvColorProfile's
			// no-colour handling, so NO_COLOR was ignored whenever COLORTERM said truecolor.
			name: "NO_COLOR beats truecolor",
			tty:  true,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "xterm-256color", "NO_COLOR": "1"},
			want: termenv.Ascii,
			why:  "an explicit profile bypasses NO_COLOR; the delegated path honours it",
		},
		{
			name: "CLICOLOR=0 beats truecolor",
			tty:  true,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "xterm-256color", "CLICOLOR": "0"},
			want: termenv.Ascii,
		},
		{
			name: "not a terminal",
			tty:  false,
			env:  envMap{"COLORTERM": "truecolor", "TERM": "xterm-256color"},
			want: termenv.Ascii,
			why:  "no terminal on stdout means no escape sequences, which is the point of the TTY check",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.tty, tc.env)
			if got != tc.want {
				msg := "%s: profile = %s, want %s"
				args := []any{tc.name, got.Name(), tc.want.Name()}
				if tc.why != "" {
					msg += " — " + tc.why
				}
				t.Errorf(msg, args...)
			}
		})
	}
}

// TestColorProfileIsResolvedNotHardcoded guards the regression the fix removed: main must not pin a profile
// from an environment variable it reads itself. A `COLORTERM` comparison in cmd/orch is the bug, not a
// shortcut, because it cannot see the TERM, TERM_PROGRAM, NO_COLOR or TTY facts that decide the answer.
func TestColorProfileIsResolvedNotHardcoded(t *testing.T) {
	// An environment with NO colour advert at all, and a TTY: a hand-rolled check has nothing to match on
	// and would leave lipgloss's implicit default in place, while the real decision still says Ascii for an
	// unidentifiable terminal — so the two are only distinguishable through the resolver.
	if got := resolve(true, envMap{}); got != termenv.Ascii {
		t.Errorf("an unidentifiable terminal must resolve to %s, got %s", termenv.Ascii.Name(), got.Name())
	}

	// And the resolver must be a function OF the environment, not a constant: two environments that differ
	// only in TERM must not produce the same answer.
	kitty := resolve(true, envMap{"TERM": "xterm-kitty"})
	plain := resolve(true, envMap{"TERM": "xterm"})
	if kitty == plain {
		t.Errorf("TERM is not reaching the decision: both xterm-kitty and xterm resolved to %s", kitty.Name())
	}
	if !strings.Contains(kitty.Name(), "True") {
		t.Errorf("xterm-kitty resolved to %s, want TrueColor", kitty.Name())
	}
}
