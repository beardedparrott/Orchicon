package server

// periodic_test.go — THE CADENCE, MEASURED.
//
// Two sweeps in this package were written as a `time.After` case sitting beside a
// ticker's case inside one `for/select`. `time.After` builds a fresh timer on every
// iteration, so the shorter duration won every race and the ticker never fired at
// all: both sweeps, documented as "at boot and every 30s", were running roughly once
// a second. No error, no failing test, no log line — just thirty times the intended
// load, indefinitely.
//
// That is not a defect a test can catch by asserting that a function was CALLED, so
// these assert the only thing that distinguishes right from wrong: HOW OFTEN, given
// a warm-up and an interval that are deliberately far apart.

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// THE INTERVAL GOVERNS THE CADENCE, NOT THE WARM-UP.
//
// The two durations are chosen to be unmistakable: a warm-up of 10ms beside an
// interval of 40ms means the buggy shape calls fn ~25 times in 250ms while the
// correct one calls it ~6. The bounds are wide on purpose — this asserts the ORDER
// OF MAGNITUDE, which is exactly what the bug changed, and stays stable on a loaded
// machine where exact tick counts are not reproducible.
func TestTheIntervalGovernsTheCadenceNotTheWarmUp(t *testing.T) {
	const (
		warmup   = 10 * time.Millisecond
		interval = 40 * time.Millisecond
		window   = 250 * time.Millisecond
	)
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOnStartupThenEvery(ctx, warmup, interval, func() { atomic.AddInt64(&calls, 1) })

	time.Sleep(window)
	got := atomic.LoadInt64(&calls)
	cancel()

	// The lower bound proves the TICKER path runs repeatedly at all — a helper that
	// ran once at boot and then stalled would be a different bug.
	if got < 3 {
		t.Errorf("fn ran %d times in %v with a %v interval (expected at least 3). Either the ticker is not "+
			"running, or the sweep is doing far less work than it claims", got, window, interval)
	}
	// The upper bound is the regression guard: at the warm-up cadence this would be
	// ~25. Anything near that means the interval is being ignored, which is the
	// `time.After`-beside-ticker bug.
	if got > 12 {
		t.Errorf("fn ran %d times in %v — close to the WARM-UP cadence (%v), not the interval (%v). This is "+
			"the signature of `case <-time.After(warmup)` sitting beside `case <-ticker.C` in one select: the "+
			"fresh timer wins every iteration and the ticker never fires, so the sweep runs at the warm-up "+
			"rate forever with nothing to show for it", got, window, warmup, interval)
	}
}

// THE WARM-UP IS HONOURED AND COMES FIRST — fn does not run before it elapses, and
// does run once when it does.
//
// This is the "sweep at boot" half of the contract. An interval of an hour makes the
// ticker irrelevant for the duration of the test, so the single call observed is
// provably the post-warm-up one.
func TestTheWarmUpIsSequentialAndRunsOnce(t *testing.T) {
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOnStartupThenEvery(ctx, 80*time.Millisecond, time.Hour, func() { atomic.AddInt64(&calls, 1) })

	// Well inside the warm-up: nothing yet. Asserting this is the point — a helper
	// that ran fn immediately would make "sweep at boot" mean "sweep on every
	// iteration of the enclosing loop" for any caller that re-entered it.
	time.Sleep(30 * time.Millisecond)
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Errorf("fn ran %d time(s) during an 80ms warm-up — the warm-up is not being awaited", n)
	}

	time.Sleep(120 * time.Millisecond)
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Errorf("fn ran %d times after the warm-up, want exactly 1 (the boot sweep; the ticker is an hour away)", n)
	}
}

// CANCELLATION STOPS IT, promptly, and without running fn again.
func TestCancellationStopsTheSweep(t *testing.T) {
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runOnStartupThenEvery(ctx, 5*time.Millisecond, 20*time.Millisecond, func() { atomic.AddInt64(&calls, 1) })
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runOnStartupThenEvery did not return after ctx was cancelled — a plane shutting down would " +
			"hang on this goroutine")
	}
	stopped := atomic.LoadInt64(&calls)
	time.Sleep(80 * time.Millisecond) // several intervals' worth
	if after := atomic.LoadInt64(&calls); after != stopped {
		t.Errorf("fn ran %d more time(s) after cancellation — the sweep outlives its context", after-stopped)
	}
}

// A NON-POSITIVE INTERVAL IS INERT, NOT FATAL.
//
// `time.NewTicker` PANICS on a non-positive duration and an unrecovered panic in a
// `go` statement takes the whole process down — so a misconfigured sweep would kill
// the plane it was supposed to be tidying, at boot, before anything could log why.
func TestADegenerateIntervalDoesNotPanic(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("runOnStartupThenEvery panicked on interval %v: %v — a bad sweep configuration must "+
						"not take down the control plane", interval, r)
				}
			}()
			runOnStartupThenEvery(context.Background(), time.Millisecond, interval, func() {
				t.Errorf("fn ran, but a non-positive interval means no sweep was requested")
			})
		}()
	}
	// A nil fn is a no-op sweep rather than a nil dereference on the first tick.
	runOnStartupThenEvery(context.Background(), 0, time.Millisecond, nil)
}

// EVERY SWEEP IN THIS PACKAGE GOES THROUGH THE HELPER.
//
// The bug arrived by COPYING a neighbouring loop, so fixing the instances without
// removing the shape leaves the next sweep one paste away from the same defect. This
// asserts the shape is gone at the source: no raw `time.After` anywhere in
// server.go, which means no `select` there can pair a fresh timer against a ticker.
//
// It is a source assertion rather than a behavioural one deliberately — the failure
// it prevents is invisible to behaviour until it has run for hours, and reading the
// file is the only thing that catches it at review time.
func TestEverySweepInThisPackageUsesTheHelper(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	if n := strings.Count(string(src), "time.After("); n != 0 {
		t.Errorf("server.go contains %d raw `time.After(` call(s). Every recurring sweep must use "+
			"runOnStartupThenEvery (periodic.go): a `time.After` case beside a ticker's case in one select makes "+
			"the ticker unreachable and runs the sweep at the warm-up rate forever. If a genuine one-shot timer is "+
			"needed, give it its own select or its own helper rather than adding it to a sweep loop.", n)
	}
}
