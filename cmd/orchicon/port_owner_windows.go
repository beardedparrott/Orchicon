//go:build windows

package main

// port_owner_windows.go — "who is listening on this port", answered with netstat, which is present on
// every Windows install. There is no syscall for this that does not require elevation or a driver:
// the mapping from port to PID lives in the TCP table, and netstat -ano is its documented reader.

import (
	"os/exec"
	"strconv"
	"strings"
)

// lookupPortOwners returns the process listening on each port, for ports that HAVE a listener.
//
// A port with no listener yields no entry — that is the "the port is free, so this is not a bind
// conflict" answer, and the caller relies on the empty result to say nothing rather than to warn.
func lookupPortOwners(ports []int) ([]portOwner, error) {
	want := make(map[int]bool, len(ports))
	for _, p := range ports {
		want[p] = true
	}
	// -a all connections, -n numeric (no DNS, so it is fast and cannot hang), -o show the owning PID.
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return nil, err
	}
	return parseNetstatOwners(string(out), want, processName), nil
}

// parseNetstatOwners is lookupPortOwners' pure half: given netstat output and the ports we care
// about, return their listeners. Separated so it can be tested against real output shapes without
// running netstat — and because the LISTENING filter below is the part that is easy to get wrong.
func parseNetstatOwners(out string, want map[int]bool, nameOf func(int) string) []portOwner {
	// pid -> name, resolved once: a crash-looping component usually conflicts with a SAME-NAMED
	// process from a previous run, and "nats-server.exe (pid 26792)" says that at a glance while
	// "pid 26792" sends the operator to a task manager.
	names := map[int]string{}
	var owners []portOwner
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// Proto  Local Address  Foreign Address  State  PID
		if len(f) < 4 || !strings.HasPrefix(strings.ToUpper(f[0]), "TCP") {
			continue
		}
		// LISTENING only: an outbound connection from the port would otherwise be reported as an
		// owner of a listener that does not exist.
		if len(f) >= 5 && !strings.EqualFold(f[len(f)-2], "LISTENING") {
			continue
		}
		// THE PORT IS AFTER THE LAST COLON. An IPv6 local address is "[::]:3100", and splitting on
		// the FIRST colon yields "::]:3100" — not a number — so every IPv6 listener would be
		// silently invisible. That is not hypothetical here: on the affected Windows instance the
		// telemetry children bind [::], so a first-colon split would have found nothing to report
		// exactly when the diagnosis mattered.
		i := strings.LastIndex(f[1], ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(f[1][i+1:])
		if err != nil || !want[port] || seen[port] {
			continue
		}
		pid, err := strconv.Atoi(f[len(f)-1])
		if err != nil || pid <= 0 {
			continue
		}
		seen[port] = true
		name, ok := names[pid]
		if !ok {
			name = nameOf(pid)
			names[pid] = name
		}
		owners = append(owners, portOwner{Port: port, PID: pid, Name: name})
	}
	return owners
}

// processName is the executable's base name, or "" when it cannot be read (a process that exited
// between netstat and this call, or one this user may not inspect).
func processName(pid int) string {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(fields[0]), `"`)
}
