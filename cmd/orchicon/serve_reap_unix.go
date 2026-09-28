//go:build !windows

package main

// serve_reap_unix.go — the orphan sweep's reap-point PROBE, which is inherently Unix-shaped: it makes
// a real orphan by spawning `/bin/sh`, and it kills the probe's own sleeper with SIGKILL. None of that
// exists on Windows, and `syscall.Kill` is absent from the Windows syscall package — so this file's
// contents used to sit in serve.go UNTAGGED and broke the RELEASE BUILD for windows/amd64 and
// windows/arm64 rather than merely degrading at runtime on those hosts.
//
// THAT IS THE SAME BUG THIS REPOSITORY ALREADY FIXED ONCE. internal/mcpclient's sweep is split for
// exactly this reason, and its windows file says so in the same words — "the untagged file broke the
// RELEASE BUILD for windows/amd64 and windows/arm64". The lesson did not travel with the fix, so the
// pattern is worth stating where the code is: A FILE THAT SHELLS OUT TO /bin/sh OR CALLS syscall.Kill
// IS A UNIX FILE, and if it lives in an untagged file it is a Windows build failure waiting for the
// next release rather than a bug someone notices on a Windows host.

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// discoverReapPoint finds where THIS environment hands orphans, by making one:
// a short-lived shell spawns a sleeper and exits, and we observe where the
// sleeper lands. Returns 0 when it cannot be established.
//
// WHY A PROBE RATHER THAN REASONING ABOUT THE PROCESS TREE. The reap point is
// the nearest ancestor carrying PR_SET_CHILD_SUBREAPER, and that attribute is
// not readable from outside the process that holds it. An earlier version of
// this walked our own ancestry and took the outermost non-init ancestor, on the
// theory that a login session's manager sits there. That theory is WRONG
// whenever anything sits ABOVE the real reaper — a container entrypoint, a
// nested subreaper — and it fails SILENTLY, because the walk still returns a
// plausible-looking pid; the sweep then matches nothing and reports success.
// (Demonstrated by planting a subreaper beneath an outer ancestor: the orphan
// landed on the planted reaper while the walk returned the outer process.)
// Making one real orphan and observing it is exact in any topology, and costs
// one subprocess per sweep.
func discoverReapPoint() int {
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 30 >/dev/null 2>&1 & echo $!; echo $$").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0
	}
	spawner, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	// The probe's own sleeper is ours to clean up, whatever we learn.
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	deadline := time.Now().Add(reapProbeTimeout)
	for time.Now().Before(deadline) {
		if pp, ok := parentPID(pid); ok && pp != spawner {
			return pp
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0
}
