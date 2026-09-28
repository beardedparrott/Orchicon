//go:build windows

package main

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
