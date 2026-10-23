package planecontrol

// quiet.go — the one way this package runs a helper tool, so none of them can open a console window.
//
// WHY IT LIVES HERE AS WELL AS IN cmd/orchicon. The two packages cannot share an unexported helper, and
// exporting one from either would make the other depend on a package it has no business importing
// (planecontrol's own imports test forbids exactly that). Six lines duplicated behind a guard that lists
// both copies is cheaper than a layering violation — and the guard is what stops them drifting.
//
// WHAT WENT WRONG WITHOUT IT. `orchicon install` is spawned DETACHED by the desktop (a minutes-long
// download must not die with the GUI), and a detached process has no console. A console-subsystem child of
// a console-less process is given a BRAND NEW WINDOW by Win32. So every helper this package ran — `docker
// network inspect` via BridgeIP, `taskkill` on every stop — put a window on the operator's screen for the
// length of the install. That is the second report of the same complaint.

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

// quietCommandContext is quietCommand with a deadline, for a tool that could hang.
func quietCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcAttrNoWindow(cmd)
	return cmd
}
