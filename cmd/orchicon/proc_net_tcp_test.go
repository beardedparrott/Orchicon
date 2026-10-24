//go:build !windows

package main

// proc_net_tcp_test.go — the /proc/net/tcp match, which was silently matching nothing.
//
// REPORTED (Linux, `make instance-rebuild instance=prod`):
//
//	Control plane (web UI + API): 8091 is held by a process this tool cannot identify
//	  — find it with `ss -ltnp` / `lsof -i :8091`
//
// while `ss -ltnp` named it immediately: our own `orchicon serve` with ORCHICON_INSTANCE=prod. The holder
// table was not unreadable — it was never matched, because the match set was built as ":1F9B" (with the
// colon) and the extracted field is "1F9B" (without). Every line was skipped, so portHolders always returned
// nothing on Linux, and nothing downstream could tell "no holder" from "a holder I cannot name".
//
// These assert the parse against the REAL shape of the kernel's table, so the class of bug cannot return.

import (
	"testing"
)

// realTable is a verbatim slice of /proc/net/tcp from a live machine, header included, with 8091 (0x1F9B)
// listening on two addresses and one ESTABLISHED connection to it. The header row is what [1:] skips.
const realTable = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   4: 010011AC:1F9B 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 737142006 2
   9: 0100007F:1F9B 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 737118174 2
  37: 0100007F:1F9B 0100007F:CF02 01 00000000:00000000 02:000003CE 00000000  1000        0 1268969630 2
   8: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111111111 2
`

func TestProcNetTCPMatchesThePortItIsGiven(t *testing.T) {
	// THE EXACT CASES FROM THE REPORT. 8091 == 0x1F9B, 8080 == 0x1F90.
	got := procNetTCPInodes(realTable, []int{8091})
	if len(got) != 2 {
		t.Fatalf("found %d listening inodes for 8091, want 2 (its two LISTEN rows) — got %v. The match set "+
			"and the extracted field disagree about the colon, which is the original bug", len(got), got)
	}
	for want := range map[string]bool{"socket:[737142006]": true, "socket:[737118174]": true} {
		if !got[want] {
			t.Errorf("missing %s (the LISTEN row for 8091); got %v", want, got)
		}
	}
	// The ESTABLISHED row to 8091 (state 01) is NOT a holder, and only its own inode would appear under a
	// different port anyway — but assert the state filter explicitly, because "a connection to the port" and
	// "a listener on the port" are different questions and only one of them is a conflict.
	if got["socket:[1268969630]"] {
		t.Error("an ESTABLISHED connection (state 01) was reported as holding the port")
	}
}

func TestProcNetTCPIgnoresPortsItWasNotAskedFor(t *testing.T) {
	got := procNetTCPInodes(realTable, []int{8091})
	if got["socket:[111111111]"] {
		t.Error("a row for port 8080 was matched when only 8091 was asked for — the last-colon split or the "+
			"zero-padded formatting is off, and misattribution is worse than no answer")
	}
	// And the other way round, so neither port is special-cased.
	other := procNetTCPInodes(realTable, []int{8080})
	if len(other) != 1 || !other["socket:[111111111]"] {
		t.Errorf("8080 gave %v, want exactly its own listener", other)
	}
}

func TestProcNetTCPHandlesNoPortsAndGarbage(t *testing.T) {
	// VACUITY: a parser that returned something for any input would satisfy the tests above.
	if got := procNetTCPInodes(realTable, nil); len(got) != 0 {
		t.Errorf("no ports asked for, %d inodes returned", len(got))
	}
	if got := procNetTCPInodes("", []int{8091}); len(got) != 0 {
		t.Errorf("empty table, %d inodes returned", len(got))
	}
	if got := procNetTCPInodes("header only\n", []int{8091}); len(got) != 0 {
		t.Errorf("header-only table, %d inodes returned", len(got))
	}
	// A short/malformed row must be skipped, not panic — this parses kernel output, and a field count can
	// change between versions.
	short := "  sl  local_address rem_address   st\n   4: 010011AC:1F9B\n"
	if got := procNetTCPInodes(short, []int{8091}); len(got) != 0 {
		t.Errorf("malformed row produced %d inodes", len(got))
	}
}
