//go:build !windows

package main

// port_owner_unix.go — "who is listening on this port" for the POSIX platforms. `lsof` is used
// because it is the one tool that answers this on Linux, macOS and the BSDs alike; when it is absent
// the answer is "cannot tell", which the caller treats as no information rather than as a free port.

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func lookupPortOwners(ports []int) ([]portOwner, error) {
	args := []string{"-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pn"}
	cmd := exec.Command("lsof", args...)
	// lsof exits 1 when it finds nothing, which is not an error for this question.
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	want := make(map[int]bool, len(ports))
	for _, p := range ports {
		want[p] = true
	}
	var owners []portOwner
	var curPID int
	seen := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err == nil {
				curPID = pid
			}
		case strings.HasPrefix(line, "n"):
			// n127.0.0.1:4222 or n*:4222
			_, portStr, ok := strings.Cut(line[1:], ":")
			if !ok {
				continue
			}
			port, err := strconv.Atoi(strings.TrimSpace(portStr))
			if err != nil || !want[port] || seen[port] || curPID <= 0 {
				continue
			}
			seen[port] = true
			owners = append(owners, portOwner{Port: port, PID: curPID, Name: processNameUnix(curPID)})
		}
	}
	return owners, nil
}

// processNameUnix is the process's command name, read from /proc where that exists (Linux) and from
// ps otherwise (macOS has no /proc). An unreadable process yields "" rather than an error: it may
// simply have exited between lsof and this call.
func processNameUnix(pid int) string {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil {
		return strings.TrimSpace(string(b))
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
