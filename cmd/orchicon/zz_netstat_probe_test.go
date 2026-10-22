//go:build windows

package main

import "testing"

// fixture: real Windows `netstat -ano -p TCP` output shape.
const winNetstat = `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1024
  TCP    0.0.0.0:4222           0.0.0.0:0              LISTENING       26792
  TCP    0.0.0.0:8222           0.0.0.0:0              LISTENING       26792
  TCP    127.0.0.1:5432         0.0.0.0:0              LISTENING       17192
  TCP    127.0.0.1:5432         127.0.0.1:52000        ESTABLISHED     17192
  TCP    [::]:3100              [::]:0                 LISTENING       32952
  TCP    10.0.0.5:49152         93.184.216.34:443      ESTABLISHED     4321
`

func parseNetstatListenersForTest(out string, ports []int) []portOwner {
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	return parseNetstatOwners(out, want, func(int) string { return "x" })
}

func TestNetstatParsingPicksListenersByPort(t *testing.T) {
	got := parseNetstatListenersForTest(winNetstat, []int{4222, 8222, 5432, 3100})
	byPort := map[int]int{}
	for _, o := range got {
		byPort[o.Port] = o.PID
	}
	if byPort[4222] != 26792 || byPort[8222] != 26792 {
		t.Errorf("both nats ports are held by pid 26792; got %v", byPort)
	}
	if byPort[5432] != 17192 {
		t.Errorf("5432 listener is pid 17192; got %v", byPort)
	}
	if byPort[3100] != 32952 {
		t.Errorf("the IPv6 [::]:3100 listener belongs to pid 32952; got %v — an IPv6 address is "+
			"splittable only on its LAST colon, and splitting on the first hides every [::] listener "+
			"(the telemetry children bind exactly that way)", byPort)
	}
	if len(got) != 4 {
		t.Errorf("expected 4 owners, got %d: %v", len(got), got)
	}
	// A port nobody holds yields NO entry: that is the "not a bind conflict" answer.
	if extra := parseNetstatListenersForTest(winNetstat, []int{9999}); len(extra) != 0 {
		t.Errorf("a free port must yield no owner, got %v", extra)
	}
	// An ESTABLISHED row must never count as an owner of a listener.
	only := parseNetstatListenersForTest(winNetstat, []int{49152})
	if len(only) != 0 {
		t.Errorf("49152 appears only as a CLIENT (established) port and must not count as a listener: %v", only)
	}
}
