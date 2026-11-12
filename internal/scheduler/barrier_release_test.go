package scheduler

// barrier_release_test.go — A BARRIER THAT CANNOT BE SATISFIED MUST RELEASE, NOT DEADLOCK.

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
