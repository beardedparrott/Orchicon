//go:build windows

package main

// port_owner_windows.go — "who is listening on this port", answered with netstat, which is present
// on every Windows install.

import (
	"strconv"
	"strings"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// lookupPortOwners returns the process listening on each port, for ports that HAVE a listener.
func lookupPortOwners(ports []int) ([]portOwner, error) {
	want := make(map[int]bool, len(ports))
	for _, p := range ports {
		want[p] = true
	}
	// -a all connections, -n numeric (no DNS, so it is fast and cannot hang), -o show the owning PID.
	out, err := procwin.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return nil, err
	}
	return parseNetstatOwners(string(out), want, processName), nil
}

// parseNetstatOwners is lookupPortOwners' pure half: given netstat output and the ports we care
// about, return their listeners.
func parseNetstatOwners(out string, want map[int]bool, nameOf func(int) string) []portOwner {
	// pid -> name, resolved once: a crash-looping component usually conflicts with a SAME-NAMED process
	// from a previous run.
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
		// THE PORT IS AFTER THE LAST COLON.
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
	out, err := procwin.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return ""
	}
	// tasklist writes a HUMAN MESSAGE when nothing matches, and it is not a name:
	//
	//	INFO: No tasks are running which match the specified criteria.
	//
	// which arrived in an operator-facing message as
	//   "port 5432 is still held by INFO: No tasks are running which match the specified criteria. (pid 30488)"
	// — worse than saying nothing, because it looks like a process called INFO:. Only a quoted CSV name is a
	// name; anything else means "this process is gone, or we cannot see it".
	fields := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(fields) == 0 {
		return ""
	}
	name := strings.Trim(strings.TrimSpace(fields[0]), `"`)
	if !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return ""
	}
	return name
}
