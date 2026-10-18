//go:build windows

package main

import (
	"fmt"
	"os"
)

// discoverReapPoint is a NO-OP on Windows, and the build tag is what makes that true rather than a
// comment claiming it.
//
// The probe makes a real orphan — a short-lived `/bin/sh` spawns a sleeper and exits, and we observe
// where the sleeper lands — and then SIGKILLs its own sleeper. Neither exists here: there is no
// `/bin/sh` in the sense the probe assumes, and `syscall.Kill` is absent from the Windows syscall
// package, which is why the UNTAGGED file broke the RELEASE BUILD rather than degrading at runtime.
//
// SO THERE IS NO DISCOVERED REAP POINT, and the sweep falls back to the ones it can know: PID 1,
// which reapPoints() always includes. The consequence is bounded and worth stating: the probe exists
// because reparenting stops at the nearest ancestor carrying PR_SET_CHILD_SUBREAPER rather than
// necessarily at PID 1, and on a host-resident plane that ancestor is the user session manager. Where
// that attribute is unreadable from outside — which is the situation this platform is in — the sweep
// simply matches less, and a missed orphan is a leak rather than a wrong kill. That is the direction
// this guard is built to fail in everywhere else too.
func discoverReapPoint() int { return 0 }

// killOrphans is a NO-OP on Windows — CONFIRMED INTENTIONAL, and now LOUD instead of silent.
//
// WHY THE NO-OP STANDS. The sweep kills a process when (a) pgrep matches it, (b) it carries the plane spawn
// marker and (c) it has been reparented away from its starter. Every one of those three readings is /proc or
// SIGTERM work: (b) reads /proc/<pid>/environ, (c) reads /proc/<pid>/status, and the kill itself is a signal
// Windows has no equivalent for. The guard fails in ONE direction on every platform — a missed orphan is a
// LEAK, a wrong kill is the operator's own `opencode` dying — and on Windows there is no way to establish the
// marker at all, so "match nothing" is the safe half of that asymmetry rather than a gap to be plugged with
// a heuristic (pkill-by-image-name is exactly the wrong-kill case).
//
// THE COST, WHICH USED TO BE SILENT: when a Windows plane CRASHES or is force-stopped, its `opencode serve`
// and `orchicon mcp` children are NOT collected. They keep running and keep holding ports and memory until
// the operator stops them by hand. The previous version of this file said nothing at all, so the only way to
// find out was to notice the machine getting slower — which is the discovery this message exists to prevent.
//
// os.Stderr IS THE RIGHT STREAM, not a logger: serveForeground calls killOrphans() AFTER
// logging.RedirectStdStreams has pointed fd 2 at the instance's rotating log, so this lands in the operator's
// own log with no logger threaded through the boot path.
func killOrphans() {
	fmt.Fprintf(os.Stderr, "warning: orphan sweep is inert on this platform: a CRASHED plane's opencode and "+
		"`orchicon mcp` children are left running\n")
	fmt.Fprintln(os.Stderr, "  why: no /proc and no SIGTERM — see cmd/orchicon/serve_reap_windows.go")
	fmt.Fprintln(os.Stderr, "  cleanup: stop the plane with `orchicon plane stop`, then look in Task Manager → "+
		"Details for `opencode` and `orchicon mcp` processes and end them from there")
}
