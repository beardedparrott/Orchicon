package opencode

import (
	"strings"
	"testing"
)

// The two real captures from the run that motivated this: a /tmp tmpfs that hit 100%, and a container at the
// cap of its memory limit with /tmp only 35% full.
func TestParseResourceProbeClassifiesTheObservedFailures(t *testing.T) {
	tmpFull := `mem=3588964352 4294967296
swap=0 0
events=low 0 high 0 max 5345 oom 0 oom_kill 0
tmp=2097152 2097152 0
biggest=1900000:/tmp/go-build123 100:/tmp/x
store=8165872:opencode.db-wal
storelog=`
	r := parseResourceProbe(tmpFull)
	if !r.Exhausted || !strings.Contains(r.Reason, "/tmp is full") {
		t.Fatalf("a 100%% full /tmp was not classified as exhausted: %+v", r)
	}
	if !strings.Contains(r.Biggest, "go-build123") {
		t.Errorf("the biggest /tmp consumer was lost: %q", r.Biggest)
	}

	memCap := `mem=4120000000 4294967296
tmp=2097152 736736 1360416
events=max 15764 oom 0`
	r = parseResourceProbe(memCap)
	if !r.Exhausted || !strings.Contains(r.Reason, "memory is at its cap") || strings.Contains(r.Reason, "/tmp") {
		t.Fatalf("memory at the cap with a roomy /tmp was misclassified: %+v", r)
	}
}

func TestParseResourceProbeHealthyAndUncappedAndGarbage(t *testing.T) {
	healthy := "mem=776196096 4294967296\ntmp=2097152 24060 2073092\n"
	if r := parseResourceProbe(healthy); r.Exhausted {
		t.Errorf("a healthy container was flagged exhausted: %+v", r)
	}
	// `max` (no cap) must read as "no cap", never as 0 bytes of headroom.
	uncapped := "mem=9000000000 max\ntmp=2097152 100 2097052\n"
	if r := parseResourceProbe(uncapped); r.Exhausted {
		t.Errorf("an uncapped container was flagged exhausted: %+v", r)
	}
	// Garbage in: no panic, no false alarm.
	for _, in := range []string{"", "nonsense", "tmp=a b c\nmem=x y", "tmp=0 0 0"} {
		if r := parseResourceProbe(in); r.Exhausted {
			t.Errorf("garbage %q produced an exhaustion verdict: %+v", in, r)
		}
	}
}

// The probe must tolerate every missing source (cgroup v1, no store yet), so one absent file cannot blank
// the whole report — it reads each fact separately, discarding errors.
func TestResourceProbeCommandReadsEachFactSeparately(t *testing.T) {
	for _, want := range []string{
		"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.events",
		"df -kP /tmp", "/tmp/orchicon-serve-data/opencode",
	} {
		if !strings.Contains(resourceProbeCommand, want) {
			t.Errorf("the probe no longer reads %q", want)
		}
	}
	if strings.Contains(resourceProbeCommand, "set -e") {
		t.Error("the probe must not abort on the first missing file")
	}
}
