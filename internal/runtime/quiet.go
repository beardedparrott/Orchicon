package runtime

// quiet.go — the one way this package runs a helper tool, so none of them can open a console window.
//
// WHY THE RUNTIME DAEMON NEEDS IT. It is started DETACHED (cmd/orchicon's startDetachedDaemon), so it has no
// console — and Win32 gives a console-subsystem child of a console-less process a BRAND NEW WINDOW. This
// package spawns a great many: the engine (build / pull / exec / inspect), initdb, pg_ctl, createdb,
// pg_isready, nats-server and `orchicon serve`. Every one of them was a window on the operator's screen, and
// during an install the daemon is starting up and probing, so they arrived in a burst — which is what "it's
// happening in the desktop app when you install a new server until it says setup complete" describes.
//
// THE SAME SIX LINES EXIST IN cmd/orchicon, internal/planecontrol AND internal/containerengine, and each copy
// is deliberate: none of those packages may import another (cmd/orchicon is package main, and the others are
// layer-checked). One guard lists every copy, which is what keeps them from drifting apart.

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
