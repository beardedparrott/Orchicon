package main

// install_console_flag_test.go — THE MSI'S INSTALL MUST OUTLIVE THE WIZARD.
//
// The reported failure, at the very end of a working install:
//
//	"it runs through the entire install and then closes out at the window at the end."
//
// The MSI's custom action is `install --non-interactive --instance dev --launch-desktop` with
// Return="asyncNoWait": the action returns immediately, so the wizard finishes and exits while the install is
// still working — and without a console of its own the install dies with the console it was sharing.
//
// --non-interactive used to mean BOTH "ask nothing" and "open no window", which conflated two different needs.
// These tests pin the separation, because collapsing it again is a one-word edit that silently breaks the last
// step of the wizard.

import (
	"os"
	"strings"
	"testing"
)

// TestConsoleAndInteractivityAreIndependentFlags is the split itself.
func TestConsoleAndInteractivityAreIndependentFlags(t *testing.T) {
	cases := []struct {
		name            string
		args            []string
		wantNonInteract bool
		wantConsole     bool
	}{
		{"the MSI's install line", []string{"--non-interactive", "--console", "--instance", "dev", "--launch-desktop"}, true, true},
		{"non-interactive alone", []string{"--non-interactive"}, true, false},
		{"console alone", []string{"--console"}, false, true},
		{"neither", []string{"--instance", "dev"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasNonInteractiveFlag(c.args); got != c.wantNonInteract {
				t.Errorf("hasNonInteractiveFlag = %v, want %v", got, c.wantNonInteract)
			}
			if got := hasConsoleFlag(c.args); got != c.wantConsole {
				t.Errorf("hasConsoleFlag = %v, want %v", got, c.wantConsole)
			}
		})
	}
}

// TestTheMSICustomActionPassesConsole reads the MSI definition. It cannot run, but the one thing that went wrong
// is a command line, and a command line can be checked.
func TestTheMSICustomActionPassesConsole(t *testing.T) {
	b, err := os.ReadFile("../../deploy/desktop/windows/Orchicon.wxs")
	if err != nil {
		t.Skipf("MSI definition not readable here: %v", err)
	}
	src := string(b)
	line := ""
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, "ExeCommand=\"install ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("no install custom action found in Orchicon.wxs — the wizard would install binaries and never set the stack up")
	}
	if !strings.Contains(line, "--console") {
		t.Fatalf("the MSI's install action does not pass --console: %s\n"+
			"With Return=asyncNoWait the wizard exits immediately, so the install dies with the shared console "+
			"and the operator sees it 'close out at the window at the end'", strings.TrimSpace(line))
	}
	if !strings.Contains(line, "--non-interactive") {
		t.Errorf("the MSI's install action must stay non-interactive: %s", strings.TrimSpace(line))
	}
}
