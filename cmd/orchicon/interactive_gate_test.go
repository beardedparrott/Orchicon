package main

// interactive_gate_test.go — an INSTALL must never take over a terminal that is not watching it.
//
// THE FAILURE, reported twice. A `make instance-rebuild` started from a non-interactive context opened
// /dev/tty (the controlling terminal of the process) unconditionally, so it wrote its port banners into the
// operator's terminal — which was running the `orch` TUI:
//
//	"it was actually printing the terminal to the orch console and pushing the ui up and all of the words
//	 were spaced out from each other"                                        (operator, second occurrence)
//
// and then BLOCKED on a read nobody would answer, so the command that started it never returned.
//
// Two duties, pinned here: do not open the terminal when nothing is watching, and do not skip the prompt for
// a wrapper that genuinely has a human (it says so with ORCHICON_INSTALL_INTERACTIVE=1).

import (
	"os"
	"strings"
	"testing"
)

func TestInstallIsInteractive(t *testing.T) {
	cases := []struct {
		env  string
		want bool
		why  string
	}{
		{"1", true, "an explicit 1 means a human is there (the Windows wrapper streams stdout through a pipe)"},
		{"true", true, "the boolean spelling"},
		{"0", false, "an explicit 0 must win over a terminal stdout"},
		{"false", false, "the boolean spelling"},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("ORCHICON_INSTALL_INTERACTIVE", c.env)
			if got := installIsInteractive(); got != c.want {
				t.Errorf("ORCHICON_INSTALL_INTERACTIVE=%q -> %v, want %v (%s)", c.env, got, c.want, c.why)
			}
		})
	}
}

// THE ONE THAT MATTERS: with stdout redirected (a log, a pipe — every build and every backgrounded
// rebuild), the install must NOT be interactive, because the /dev/tty it would open belongs to whatever
// else is on that terminal.
func TestInstallIsNonInteractiveWhenStdoutIsNotATerminal(t *testing.T) {
	t.Setenv("ORCHICON_INSTALL_INTERACTIVE", "")
	if err := os.Unsetenv("ORCHICON_INSTALL_INTERACTIVE"); err != nil {
		t.Fatal(err)
	}
	// The test binary's stdout is a pipe (go test captures it), so this is the redirected case by
	// construction — the same condition as `make instance-rebuild > log`.
	if isCharDevice(os.Stdout) {
		t.Skip("stdout is a terminal here; this assertion needs the redirected shape")
	}
	if installIsInteractive() {
		t.Fatal("a redirected install reports itself interactive — it would open /dev/tty and write into " +
			"whatever terminal is attached (the operator's TUI), then block on a read nobody answers")
	}
	// And promptTerminal must therefore refuse to hand back a terminal at all.
	if f, owned := promptTerminal(); f != nil {
		if owned {
			_ = f.Close()
		}
		t.Fatal("promptTerminal handed back a terminal for a non-interactive install")
	}
}

// A SOURCE PIN on the gate's position: it must be the FIRST thing promptTerminal decides, so no future edit
// can open /dev/tty before asking whether anyone is watching.
func TestPromptTerminalChecksInteractivityFirst(t *testing.T) {
	body := functionBody(t, "install_ports.go", "func promptTerminal() (*os.File, bool) {")
	gate := strings.Index(body, "installIsInteractive()")
	open := strings.Index(body, `"/dev/tty"`)
	if gate < 0 {
		t.Fatal("promptTerminal does not consult installIsInteractive — a backgrounded install would take over the operator's terminal again")
	}
	if open < 0 {
		t.Fatal("promptTerminal no longer opens /dev/tty — update this test's anchor")
	}
	if gate > open {
		t.Errorf("the interactivity check comes AFTER the /dev/tty open (gate at %d, open at %d) — the "+
			"terminal is already claimed by then", gate, open)
	}
}

// The runtime-image question follows the same rule: a backgrounded install must not block on a prompt.
func TestRuntimeImagePromptIsGatedToo(t *testing.T) {
	body := functionBody(t, "install_runtime_images.go", "func resolveInstallRuntimeImagesFor(instance string, promptOut io.Writer, answerIn io.Reader, imageTag string, forcePrompt bool) ([]runtimeImageChoice, error) {")
	if !strings.Contains(body, "installIsInteractive()") {
		t.Error("resolveInstallRuntimeImagesFor does not consult installIsInteractive — a redirected install " +
			"would still prompt for runtime images")
	}
}
