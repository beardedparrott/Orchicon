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

// TestConsoleFlagSurvivesTheWholeParse is the regression that broke the MSI's install.
//
// The two parsers do different jobs: parseInstallFlags extracts the WIZARD's flags and hands the remainder to
// parseInstanceTarget, which deliberately REJECTS any flag it does not know. So `--console` left in `rest` made
// the install exit immediately with
//
//	unknown flag "--console"
//
// — the reported "it pops up and then closes immediately". Testing that hasConsoleFlag saw the flag was never
// enough; the flag has to be CONSUMED, and that is what this pins.
func TestConsoleFlagSurvivesTheWholeParse(t *testing.T) {
	// The MSI's exact command line, minus the exe name.
	args := []string{"--non-interactive", "--console", "--instance", "dev", "--launch-desktop"}

	flags, rest, err := parseInstallFlags(args)
	if err != nil {
		t.Fatalf("parseInstallFlags: %v", err)
	}
	if !flags.WantsConsole {
		t.Error("--console was not recorded — the flag was acted on by a PEEK, but the parse must also consume it")
	}
	if !flags.NonInteractive {
		t.Error("--non-interactive was not recorded")
	}
	for _, a := range rest {
		if a == "--console" || a == "-console" {
			t.Fatalf("--console survived into the remainder, which goes to parseInstanceTarget — that parser "+
				"rejects unknown flags, so the install exits instantly: rest=%v", rest)
		}
	}
	// THE PROOF: the remainder must be ACCEPTABLE to the instance parser.
	if _, err := parseInstanceTarget(rest, "dev"); err != nil {
		t.Fatalf("the install's own argv is rejected downstream: %v\nrest=%v", err, rest)
	}
}

// TestAnUnknownFlagIsStillRejected keeps the strictness this fix works around: consuming --console must not make
// the parser permissive in general, because a wizard answer that gets dropped installs something nobody chose.
func TestAnUnknownFlagIsStillRejected(t *testing.T) {
	_, rest, perr := parseInstallFlags([]string{"--nonsense-flag", "--instance", "dev"})
	if perr != nil {
		return // rejected at the first parser: also fine
	}
	if _, err := parseInstanceTarget(rest, "dev"); err == nil {
		t.Fatalf("an unknown flag reached the instance parser and was accepted: rest=%v", rest)
	}
}
