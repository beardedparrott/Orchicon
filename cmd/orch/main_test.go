package main

// main_test.go — the SHAPE of a shell's TUI options, pinned at the call site.
//
// cmd/orch cannot read tui.App's unexported launchDir/launchPromptArmed (tui is another package), so the
// BEHAVIOURAL pin — that the second shell still scopes to the launch directory without re-asking — lives in
// internal/tui/project_scope_reconnect_test.go, which can read those fields. What is checkable HERE is the
// mechanically verifiable contract of shellLaunchOptions: the DIRECTORY is passed on every shell, and the
// PROMPT option is added only on the first.

import (
	"fmt"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/tui"
)

// THE LAUNCH DIRECTORY IS PASSED ON EVERY SHELL; THE PROMPT IS ARMED ONLY ON THE FIRST.
//
// This is the split that was collapsed into one decision. On a continuation the old code passed "" as the
// directory, which tui.WithLaunchDir reads as "do nothing" — so the directory must be present in the option
// set whether or not this is the first shell, and the extra option must appear only for the first shell.
func TestShellLaunchOptionsThreadsTheDirectoryOnEveryShell(t *testing.T) {
	const dir = "/home/me/projects/orch"
	cases := []struct {
		name       string
		firstShell bool
		launchDir  string
		wantOpts   int
	}{
		{"first shell, real directory", true, dir, 2},
		{"continuation, real directory (the regression)", false, dir, 1},
		{"first shell, Getwd failed", true, "", 2},
		{"continuation, Getwd failed", false, "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := shellLaunchOptions(c.firstShell, c.launchDir)
			if len(opts) != c.wantOpts {
				t.Fatalf("shellLaunchOptions(%v, %q) returned %d options, want %d — the FIRST option is "+
					"WithLaunchDir (always) and the second is WithLaunchPrompt (first shell only)",
					c.firstShell, c.launchDir, len(opts), c.wantOpts)
			}
			if opts[0] == nil {
				t.Error("the first option (the launch directory) is nil — the directory must be passed on " +
					"every shell for the rail's workspace default to survive a reconnect")
			}
		})
	}
}

// THE ARMING OPTION IS ABSENT ON A CONTINUATION. Stated on its own because it is the property that keeps the
// fix from re-introducing the nagging: if the second option were present on a continuation, the reconnect
// would re-ask the project question.
func TestTheContinuationCarriesNoPromptOption(t *testing.T) {
	first := shellLaunchOptions(true, "/home/me/projects/orch")
	cont := shellLaunchOptions(false, "/home/me/projects/orch")
	if len(first) != 2 {
		t.Fatalf("the first shell has %d options, want 2 (directory + prompt)", len(first))
	}
	if len(cont) != 1 {
		t.Fatalf("the continuation has %d options, want 1 (directory only)", len(cont))
	}
	// The directory option is the SAME on both shells: the continuation does not lose it.
	// (AppOption is an opaque func value in this package, so identity is pinned by count above and by the
	// behavioural test in internal/tui; here we only assert the option set is non-nil.)
	if cont[0] == nil {
		t.Error("the continuation's directory option is nil")
	}
}

// THE OPTIONS ARE REAL tui.AppOptions — a compile-time guard that shellLaunchOptions' return type stays the
// one tui.NewApp accepts, so main's `runShell(profile, opts...)` cannot drift from it.
var _ []tui.AppOption = shellLaunchOptions(true, "/home/me/projects/orch")

// TestParseArgsReadsInstance — the flag forms an operator will actually type.
func TestParseArgsReadsInstance(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--instance", "prod"}, "prod"},
		{[]string{"-i", "dev"}, "dev"},
		{[]string{"--instance", "test", "--insecure"}, "test"},
		{[]string{"--profile", "x", "--instance", "prod"}, "prod"},
		{[]string{}, ""},
	} {
		if got := parseArgs(tc.args).instance; got != tc.want {
			t.Errorf("parseArgs(%v).instance = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestInstanceSelectionSetsTheEnvSoPlaneVerbsAgree — connecting to an instance and
// ACTING on that instance must be the same answer.
//
// internal/tui's instanceForProfile reads ORCHICON_INSTANCE first, before deriving an
// instance from the profile's port. Without this, `orch --instance prod` would connect
// to prod while /plane restart acted on whatever the port suggested — or on nothing,
// leaving the operator believing they had restarted the plane they were looking at.
func TestInstanceSelectionSetsTheEnvSoPlaneVerbsAgree(t *testing.T) {
	t.Setenv("ORCHICON_INSTANCE", "") // start from nothing, and restore afterwards
	os.Unsetenv("ORCHICON_INSTANCE")

	got, err := selectInstance("prod")
	if err != nil {
		t.Fatalf("selectInstance(prod): %v", err)
	}
	// The URL comes from the SHARED table — the same number the installer writes and the
	// desktop app connects to.
	if want := fmt.Sprintf("http://127.0.0.1:%d", controlport.Defaults()["prod"]); got != want {
		t.Errorf("selectInstance(prod) = %q, want %q", got, want)
	}
	// AND THE PROCESS IS PINNED: this is the half that makes /plane verbs act on the
	// instance the shell is connected to.
	if env := os.Getenv("ORCHICON_INSTANCE"); env != "prod" {
		t.Errorf("ORCHICON_INSTANCE = %q after selectInstance(prod), want \"prod\" — /plane restart "+
			"would act on a different instance than the one being displayed", env)
	}

	// An empty name is a no-op: no env write, no URL, so a bare `orch` is unchanged.
	os.Unsetenv("ORCHICON_INSTANCE")
	got, err = selectInstance("")
	if err != nil || got != "" {
		t.Errorf("selectInstance(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}
	if env := os.Getenv("ORCHICON_INSTANCE"); env != "" {
		t.Errorf("a bare launch set ORCHICON_INSTANCE=%q", env)
	}
}

// TestUnknownInstanceIsRefused — never substituted with a real plane.
func TestUnknownInstanceIsRefused(t *testing.T) {
	for _, name := range []string{"staging", "prdo", "DEV"} {
		if _, err := selectInstance(name); err == nil {
			t.Errorf("selectInstance(%q) succeeded — an unrecognised instance must be refused, not "+
				"mapped onto a real one", name)
		}
	}
}
