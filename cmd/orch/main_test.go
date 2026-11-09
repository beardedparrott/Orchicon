package main

// main_test.go — the SHAPE of a shell's TUI options, pinned at the call site.

import (
	"fmt"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/controlport"
	"github.com/beardedparrott/orchicon/internal/tui"
)

// THE LAUNCH DIRECTORY IS PASSED ON EVERY SHELL; THE PROMPT IS ARMED ONLY ON THE FIRST.
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

// THE ARMING OPTION IS ABSENT ON A CONTINUATION.
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

// TestInstanceSelectionSetsTheEnvSoPlaneVerbsAgree — connecting to an instance and ACTING on that
// instance must be the same answer.
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
