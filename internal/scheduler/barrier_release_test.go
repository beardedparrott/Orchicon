package scheduler

// barrier_release_test.go — A BARRIER THAT CANNOT BE SATISFIED MUST RELEASE, NOT DEADLOCK.
//
// The barrier waits until `expected` dispatches are in flight, so tests can assert
// bounded concurrency without timing flakes. Its original form waited on
// `<-d.done` with NO escape: `done` closes only when the count REACHES `expected`,
// so if fewer dispatches arrived the barrier blocked FOR EVER, dispatchInline's
// WaitGroup.Wait never returned, and the test binary hung until the 10-minute
// panic timeout.
//
// That is not hypothetical — it is how the package wedged during the diff work:
// scheduler.test sat for 8+ minutes with a goroutine parked in DispatchTask, and
// the symptom an operator sees is "the tests hang", not "a test failed". A hang
// is strictly worse than a failure: it costs the whole package's run, it reports
// nothing about which assertion was involved, and on a shared DB it holds
// connections while it waits.
//
// These tests pin the two escapes that make the mismatch a FAILURE rather than a
// deadlock: a shortfall releases at the deadline and RECORDS it, and a cancelled
// context releases immediately rather than waiting the deadline out.

import (
	"context"
	"testing"
	"time"
)

// A barrier that never reaches `expected` must RELEASE, not deadlock — the
// property that turns a 10-minute package timeout into a clear failure.
func TestBarrierReleasesInsteadOfDeadlocking(t *testing.T) {
	d := &barrierDispatcher{expected: 2, done: make(chan struct{}), deadline: 200 * time.Millisecond}
	// Only ONE dispatch arrives; the second never will.
	if err := d.DispatchTask(context.Background(), "", "step-1"); err != nil {
		t.Fatalf("dispatch returned %v", err)
	}
	if !d.shortfall() {
		t.Error("the barrier did not record the shortfall — a test could not tell 'released' from 'reached'")
	}
}

// And with a cancelled context it returns promptly rather than waiting the deadline.
func TestBarrierReleasesOnContextCancel(t *testing.T) {
	d := &barrierDispatcher{expected: 5, done: make(chan struct{}), deadline: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := d.DispatchTask(ctx, "", "step-1"); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a cancelled context took %v to release — it must not wait the deadline", elapsed)
	}
}
