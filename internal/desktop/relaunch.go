package desktop

// relaunch.go — RESTARTING THE APP, which is what switching servers actually needs.

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// RelaunchFlag is the internal flag the replacement process is started with, carrying the pid it
// must wait for.
const RelaunchFlag = "--await-pid"

// RelaunchArgs is the argv a replacement should be started with, given the instance to open and the
// route.
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

// ParseAwaitPID reads the pid a replacement must wait for out of its own argv, and returns the argv
// with the flag removed.
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

// WaitForExit blocks until pid is gone, or the deadline passes.
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

// StartReplacement spawns a detached copy of this app that will wait for selfPID to exit and then
// open on instance/route.
func StartReplacement(self string, selfPID int, instance, route string) (int, error) {
	if strings.TrimSpace(self) == "" {
		return 0, errNoSelf
	}
	return startReplacement(self, RelaunchArgs(selfPID, instance, route))
}
