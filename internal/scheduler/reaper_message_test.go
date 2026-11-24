package scheduler

// THE REAPER'S MESSAGE MUST NOT GUESS.
//
// It used to emit one fixed string for every reap — "execution lost: control plane restarted or
// runtime container gone" — for a status that was sometimes `dispatching`, sometimes a local-mode
// execution that never had a container, and sometimes (measured, prod 01M87T75ZX) an execution that was
// STILL RUNNING. Naming one cause is worse than no detail: it sent every investigation to
// infrastructure and away from the liveness probe, which is where the actual defect was.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

func TestReapMessageNamesTheObservedConditionNotAGuess(t *testing.T) {
	adapter := "adp_orchicon_dev"
	msg := reapFailureMessage(db.ExecutionRow{
		ID: "01M87T75ZX79B682SEG99QPGS8", AdapterID: &adapter,
		Status: domain.ExecutionRunning, HealthState: domain.HealthHealthy,
	}, 3)

	// It must say what was observed.
	for _, want := range []string{
		"no live session",
		"3 consecutive liveness probes",
		"adp_orchicon_dev",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("reap message does not state %q: %s", want, msg)
		}
	}
	// It must not assert a single cause as fact.
	for _, banned := range []string{
		"control plane restarted",
		"runtime container gone",
	} {
		if strings.Contains(msg, banned) {
			t.Errorf("reap message still asserts %q as the cause. It cannot see the cause — only that no "+
				"live session answered. Naming one sent every investigation to the wrong component.", banned)
		}
	}
}

// A recorded unhealthy/stalled state is evidence and must be reported; a healthy one must not be invented.
func TestReapMessageReportsTheLastRecordedState(t *testing.T) {
	withState := reapFailureMessage(db.ExecutionRow{HealthState: domain.HealthStalled}, 3)
	if !strings.Contains(withState, domain.HealthStalled) {
		t.Errorf("a non-healthy recorded state must appear in the message: %s", withState)
	}
	healthy := reapFailureMessage(db.ExecutionRow{HealthState: domain.HealthHealthy}, 3)
	if strings.Contains(healthy, "last recorded health") {
		t.Errorf("a healthy execution must not be described as having a bad recorded state: %s", healthy)
	}
}

// A nil adapter id must render without panicking or printing "<nil>".
func TestReapMessageToleratesAMissingAdapterID(t *testing.T) {
	msg := reapFailureMessage(db.ExecutionRow{}, 3)
	if strings.Contains(msg, "<nil>") || strings.Contains(msg, "%!") {
		t.Errorf("message formatting leaked a nil or verb error: %s", msg)
	}
	if !strings.Contains(msg, "unknown adapter") {
		t.Errorf("a missing adapter must be named as unknown: %s", msg)
	}
}

// The probe count is the reaper's own evidence and must be reported verbatim.
func TestReapMessageCarriesTheProbeCount(t *testing.T) {
	for _, n := range []int{1, 3, 7} {
		msg := reapFailureMessage(db.ExecutionRow{}, n)
		if !strings.Contains(msg, strings.TrimSpace(strings.Repeat(" ", 0))+itoa(n)+" consecutive") {
			t.Errorf("message does not carry probe count %d: %s", n, msg)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
