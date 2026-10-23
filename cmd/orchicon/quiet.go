package main

// quiet.go — ONE WAY TO RUN A HELPER TOOL, so that none of them can open a console window by being
// forgotten.
//
// THE REPORT WAS THE SAME COMPLAINT TWICE, AND THE SECOND TIME IT WAS STILL THERE.
//
//	"ORCHICON on Windows pops up a ton of command prompt windows"
//	"It popped up a ton of command windows and the server screen was blinking like it was having a stroke"
//
// The first time the fix went into supervisor.start — and it was the RIGHT fix, for the site the report
// named. But it was applied PER CALL SITE, and this program spawns far more console tools than that one:
// pg_isready, initdb, createdb, netstat, tasklist, docker, and every `--version` probe. Each of them was
// left to remember the same three lines, and every one that forgot opened a window.
//
// WHY A WINDOW APPEARS AT ALL. A console-subsystem executable given no console by its parent gets a BRAND
// NEW ONE — that is the Win32 default, not a bug. So the moment this program runs with no console of its
// own, EVERY console child it starts pops a window. And it does run that way, routinely:
//
//   - the SERVICES supervisor is detached (it must outlive the terminal), so its children — postgres,
//     nats, victoriametrics, otel-collector — each got a window, and reapRestart gave them another on
//     every restart (backoff 2s, 4s, 8s…), which is the "trying to start over and over" report;
//   - `orchicon install` is spawned DETACHED by instancectl (a download must not die with the GUI), so
//     its probes did the same;
//   - the MSI's custom action has no console at all.
//
// AND THE WORST OFFENDER WAS A LOOP, NOT AN EVENT. `orchicon install` waits for services by polling: up to
// SIXTY TIMES, once a second, each poll running `pg_isready`. None of those set CREATE_NO_WINDOW, so one
// install could open sixty console windows over the minute it spent waiting — which is what "the screen was
// blinking like it was having a stroke" describes far better than any CSS on the page.
//
// SO THE FIX IS AT THE SPAWN HELPER RATHER THAN AT THE REPORT. Every non-interactive helper tool goes through
// quietCommand/quietCommandContext, and those apply the attribute once. A tool added tomorrow inherits it by
// using the helper; a tool that forgets is caught by TestEveryHelperToolIsSpawnedWindowless.
//
// IT IS SAFE FOR EVERY CALLER HERE, including the interactive ones. CREATE_NO_WINDOW suppresses the WINDOW.
// It does not suppress the process's stdout/stderr handles, which every caller here already captures through
// a pipe — so `initdb`'s errors are still readable, `pg_isready`'s exit code is still meaningful, and a
// per-command deadline (engineCommand) still applies. No site in this package wants a helper tool's window;
// the two that DO want a console — console_windows.go's own relaunch, and `serve --detach`'s detached plane —
// deliberately do not use these helpers.
//
// A NO-OP ON UNIX, where a process has no console to allocate and setProcAttrNoWindow does nothing. That is
// why there is one file and not two: the difference is already encapsulated in that attribute setter.

import (
	"context"
	"os/exec"
)

// quietCommand starts a helper tool that must never open a console window.
//
// Use this for anything this program runs FOR ITSELF — a probe, a one-off initialiser, an engine call, a
// version query. A tool whose window the operator is meant to SEE (an editor, a pager, a prompt) does not
// belong here, and nothing in this package spawns one.
func quietCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	setProcAttrNoWindow(cmd)
	return cmd
}

// quietCommandContext is quietCommand with a deadline attached. Prefer it wherever the tool could hang —
// an engine whose socket is wedged, or a probe against a port that is accepting but not answering.
func quietCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcAttrNoWindow(cmd)
	return cmd
}
