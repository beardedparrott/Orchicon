package main

// main_test.go — the SHAPE of a shell's TUI options, pinned at the call site.
//
// cmd/orch cannot read tui.App's unexported launchDir/launchPromptArmed (tui is another package), so the
// BEHAVIOURAL pin — that the second shell still scopes to the launch directory without re-asking — lives in
// internal/tui/project_scope_reconnect_test.go, which can read those fields. What is checkable HERE is the
// mechanically verifiable contract of shellLaunchOptions: the DIRECTORY is passed on every shell, and the
// PROMPT option is added only on the first.

import (
	"testing"

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
