package screenkit

// systemzone_test.go — the client's answer to "which zone am I in?", and the one value it must never give.
//
// These assertions hold in ANY zone the test machine is in, which is the point: the function reports the
// SYSTEM's zone, so a test that hard-coded "America/Chicago" would only pass on the author's laptop. What
// is asserted instead are the properties that must be true wherever it runs.

import (
	"strings"
	"testing"
	"time"
)

// THE ONE FORBIDDEN ANSWER.
//
// time.Local.String() returns the literal "Local" whenever the zone comes from /etc/localtime with no TZ
// env var — the normal desktop configuration. Passing that on would be the worst possible outcome, because
// time.LoadLocation("Local") SUCCEEDS: it means "whatever machine reads it", so the schedule would be
// stored happily and then fire in the SERVER's zone (UTC in a container) — the operator's 09:00 landing at
// 03:00 with every check green.
func TestSystemZoneNameNeverReturnsThePseudoZoneLocal(t *testing.T) {
	if got := SystemZoneName(); got == "Local" {
		t.Fatal("SystemZoneName returned \"Local\" — it resolves to whichever machine reads it, so a schedule " +
			"stamped with it would fire in the server's zone")
	}
}

// WHATEVER IT RETURNS IS EITHER EMPTY OR A ZONE THAT MEANS THE SAME THING EVERYWHERE.
//
// Empty is a legitimate answer (the caller reports it rather than guessing). Anything non-empty must be a
// real IANA zone: region-qualified, loadable, and — for the same reason as above — not the pseudo-zone.
func TestSystemZoneNameIsEitherEmptyOrAnUnambiguousZone(t *testing.T) {
	got := SystemZoneName()
	if got == "" {
		return // honest "could not determine", which the caller surfaces
	}
	if got == "Local" {
		t.Fatalf("returned the pseudo-zone Local")
	}
	if got != "UTC" && got != "GMT" && !strings.Contains(got, "/") {
		t.Errorf("SystemZoneName returned %q, which is not region-qualified. Bare abbreviations like CST "+
			"resolve in the IANA database to FIXED offsets that ignore daylight saving, so a schedule "+
			"stamped with one would be an hour wrong all summer", got)
	}
	if _, err := time.LoadLocation(got); err != nil {
		t.Errorf("SystemZoneName returned %q, which does not load: %v", got, err)
	}
}

// THE TWO PROPERTIES ARE STABLE ACROSS CALLS — it reads the machine, and the machine does not change
// between calls in a test run. A resolver that wandered would stamp a different zone on each schedule.
func TestSystemZoneNameIsStable(t *testing.T) {
	first := SystemZoneName()
	if second := SystemZoneName(); second != first {
		t.Errorf("SystemZoneName changed between calls: %q then %q", first, second)
	}
}
