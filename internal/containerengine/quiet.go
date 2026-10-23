package containerengine

// quiet.go — the one way this package runs a helper tool, so none of them can open a console window.
//
// WHY THIS PACKAGE NEEDS IT TOO. The console-window complaint has now been reported twice, and both times
// the reason was SCOPE rather than disagreement: the fix went into cmd/orchicon, and the operator still saw
// windows — "No, it's happening in the desktop app when you install a new server until it says 'setup
// complete'." An install runs DETACHED (a minutes-long download must not die with the GUI), a detached
// process has NO CONSOLE, and Win32 gives a console-subsystem child of a console-less process a BRAND NEW
// WINDOW. The engine probe below is exactly such a tool, and it runs several times per install.
//
// Three copies of six lines exist across this repo (here, cmd/orchicon, internal/planecontrol) because none
// of these packages may import the others — cmd/orchicon is package main, and planecontrol's imports test
// forbids depending on this package's neighbours. A shared copy behind a guard that lists every copy is
// cheaper than a layering violation, and the guard is what stops them drifting.

import (
	"context"
	"os/exec"
)

// quietCommand starts a helper tool that must never open a console window.
func quietCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	setProcAttrNoWindow(cmd)
	return cmd
}

// quietCommandContext is quietCommand with a deadline, for a tool that could hang. The engine probe is the
// main caller: a wedged engine socket must not stall an install.
func quietCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcAttrNoWindow(cmd)
	return cmd
}
