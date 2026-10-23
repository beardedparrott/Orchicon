package desktop

// relaunch.go — RESTARTING THE APP, which is what switching servers actually needs.
//
// REPORTED: "Hitting connect technically works but there are bugs. When you connect to another instance, the
// top menu disappears and you are left on the manage server screen. If you close the desktop app and re-open,
// THEN you are connected to the other instance. We may want to build in an ability to just auto restart the
// desktop app once someone hits connect to the new instance."
//
// THE OPERATOR'S DIAGNOSIS AND THEIR PROPOSED FIX ARE BOTH RIGHT, and "just restart it" is not a workaround
// for a bug — it is the correct model. A window is initialised from a great deal at process start: the
// injected desktopInfo payload, the proxy's target, the session store bound to the OLD plane's database, the
// version tags. `switchPlane` can retarget the PROXY, but a reload only re-reads the parts that are rebuilt
// from the document. The session is the one that bites: a different instance is a different database with
// different users, so the reload re-bootstraps against the new plane while the app's chrome is still shaped
// by the old one — which is exactly "the top menu disappears and you are left on the manage server screen".
//
// Rather than chase each piece of process-scoped state and hope the list is complete, the app does what a
// person does: it restarts. One code path, and it cannot miss a piece of state somebody adds later.
//
// THE NEW PROCESS WAITS FOR THIS ONE TO DIE. Two windows must never be on screen at once, and on Windows the
// replacement cannot take the single-instance slot while the original holds it. So the relauncher is spawned
// detached with the parent's pid and blocks until that pid is gone (bounded — see WaitForExit) before it
// begins. That is also what makes "close the app and re-open it" honest: the new process starts from exactly
// the state a hand restart would.

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// RelaunchFlag is the internal flag the replacement process is started with, carrying the pid it must wait
// for. It is deliberately NOT a public CLI flag: an operator has no reason to pass it, and it exists only so
// the spawned child knows to defer its own startup.
const RelaunchFlag = "--await-pid"

// RelaunchArgs is the argv a replacement should be started with, given the instance to open and the route.
//
// BOTH facts are passed EXPLICITLY rather than left to the remembered preference: the preference is written
// best-effort, and a switch that failed to persist must still open the right server.
func RelaunchArgs(selfPID int, instance, route string) []string {
	args := []string{RelaunchFlag, strconv.Itoa(selfPID)}
	if instance != "" {
		args = append(args, "--instance", instance)
	}
	if r := SafeRoute(route); r != "" {
		args = append(args, "--route", r)
	}
	return args
}

// ParseAwaitPID reads the pid a replacement must wait for out of its own argv, and returns the argv with the
// flag removed. ok is false when the flag is absent — an ordinary launch.
//
// IT CONSUMES ITS ARGUMENTS, so what is left can be handed to the normal flag parser unmodified. A relaunch
// that leaked its own flag into flag.Parse would fail on an undefined flag and exit without a window, which
// is a far worse failure than the one it is fixing.
func ParseAwaitPID(argv []string) (pid int, rest []string, ok bool) {
	rest = make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] != RelaunchFlag {
			rest = append(rest, argv[i])
			continue
		}
		if i+1 >= len(argv) {
			continue // a flag with no value is ignored rather than fatal
		}
		n, err := strconv.Atoi(strings.TrimSpace(argv[i+1]))
		if err != nil || n <= 0 {
			i++ // consume the bad value too, so it cannot be read as a positional argument
			continue
		}
		pid, ok = n, true
		i++
	}
	return pid, rest, ok
}

// WaitForExit blocks until pid is gone, or the deadline passes. It returns whether the pid went away.
//
// POLLING RATHER THAN AN OS WAIT, because the parent is not this process's child: on Unix a non-child cannot
// be waited for at all, and on Windows OpenProcess+WaitForSingleObject would need platform files for one
// bounded loop. 40ms is a compromise — fast enough that the new window appears promptly, cheap enough that a
// process which exits in 300ms costs eight syscalls.
//
// THE DEADLINE IS LOAD-BEARING. If the parent never dies (a wedged shutdown), the operator must still get an
// app rather than an invisible process waiting for ever. Better two windows than none.
func WaitForExit(pid int, timeout time.Duration, alive func(int) bool, sleep func(time.Duration)) bool {
	if pid <= 0 || alive == nil {
		return true
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		sleep(40 * time.Millisecond)
	}
	return !alive(pid)
}

// errNoSelf is returned when the running executable cannot be located, which makes a relaunch impossible.
var errNoSelf = errors.New("desktop: cannot locate this executable to restart it")

// StartReplacement spawns a detached copy of this app that will wait for selfPID to exit and then open on
// instance/route. It returns the new process's pid.
//
// DETACHED, because it must outlive the process that starts it — that is the entire point. The platform
// halves differ in how "detached" is spelled, so they are in their own files.
func StartReplacement(self string, selfPID int, instance, route string) (int, error) {
	if strings.TrimSpace(self) == "" {
		return 0, errNoSelf
	}
	return startReplacement(self, RelaunchArgs(selfPID, instance, route))
}
