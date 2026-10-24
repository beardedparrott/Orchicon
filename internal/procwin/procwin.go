// Package procwin starts helper processes without letting them flash a console window on Windows.
//
// THE COMPLAINT, THREE TIMES. It has been reported as
//
//	"ORCHICON on Windows pops up a ton of command prompt windows"
//	"No, it's happening in the desktop app when you install a new server until it says setup complete"
//	"There are still random command prompts in windows that pop up every so often and go away quickly"
//
// Each round fixed the sites the previous report named, and the windows came back — because the cause is not
// a site, it is a PROPERTY OF THE PARENT. A console-subsystem executable started by a process that has no
// console is given a BRAND NEW WINDOW by Win32. That is the documented default, not a misconfiguration. And
// several of this product's long-lived processes legitimately have no console:
//
//   - the PLANE (`serve --detach`) is started DETACHED so it outlives the terminal that launched it;
//   - the SERVICES SUPERVISOR likewise, so postgres/nats/telemetry survive a terminal close;
//   - `orchicon install` / `uninstall` are spawned detached by the desktop, because a minutes-long download
//     must not die with the GUI;
//   - the MSI's custom action has no console at all.
//
// EVERY child any of those starts therefore opens a window — and so does every child of those children, and
// the grandchildren again. One install could open dozens; a plane doing periodic work (a version probe, a git
// read, an MCP transport starting) opens one "every so often, that goes away quickly".
//
// THE FIX IS AN ATTRIBUTE, AND IT PROPAGATES. CREATE_NO_WINDOW gives the child a console that is NOT VISIBLE
// rather than no console at all. Anything that child starts then inherits that hidden console and opens no
// window of its own, and so on down the tree. So applying it at the top of each branch we start is enough —
// there is no need to reach into a third-party tool's own spawns.
//
// WHY THIS IS A PACKAGE AND NOT FOUR COPIES. Earlier rounds duplicated a six-line helper into cmd/orchicon,
// internal/planecontrol, internal/containerengine and internal/runtime, because exporting one would have made
// those packages import each other. A stdlib-only LEAF avoids that entirely: importing it adds no transitive
// dependency, so every package — including ones with layering tests — may use it. One implementation, one
// guard (procwin_test.go), and no copies to drift.
package procwin

import (
	"context"
	"os/exec"
)

// Command starts a helper tool that must never open a console window.
//
// USE IT FOR ANYTHING THIS PROGRAM RUNS FOR ITSELF: probes, one-off initialisers, engine calls, git and gh
// reads, version queries, subprocess transports. A tool whose window the operator is meant to SEE does not
// belong here, and nothing in this codebase spawns one except the interactive-console relaunch, which uses
// CREATE_NEW_CONSOLE deliberately and says so.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	Quiet(cmd)
	return cmd
}

// Quiet applies the no-window attribute to a command the caller has already built — for the cases where the
// command needs extra wiring (Env, Stdout, process-group ownership) that Command cannot know about.
//
// IT COMPOSES rather than assigns. Several call sites set SysProcAttr themselves (a process group for build
// cancellation, a detach for a daemon), and an assignment here would silently drop that — turning a cosmetic
// fix into a real one, which is the worst trade available.
func Quiet(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	applyNoWindow(cmd)
}

// CommandContext is Command with a deadline. Prefer it wherever the tool could hang: an engine whose socket is
// wedged, a probe against a port that accepts but never answers, a `docker exec` into a container that is
// being torn down.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	Quiet(cmd)
	return cmd
}
