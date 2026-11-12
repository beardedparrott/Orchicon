//go:build !windows

package main

// port_owner_unix.go — "who is listening on this port" for the POSIX platforms.

import (
	"os"
	"strconv"
	"strings"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

func lookupPortOwners(ports []int) ([]portOwner, error) {
	args := []string{"-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pn"}
	cmd := procwin.Command("lsof", args...)
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
// ps otherwise (macOS has no /proc).
func processNameUnix(pid int) string {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil {
		return strings.TrimSpace(string(b))
	}
	out, err := procwin.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
