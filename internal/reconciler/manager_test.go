package reconciler

import (
	"testing"
	"time"
)

// TestDequeueSingleNotReadyKeyDoesNotSpin is a regression test for the
// field incident where a single not-ready key made dequeue busy-loop
// forever: the old code captured `now` once and re-appended the not-ready
// key to the end, so `len(q.ordered) > 0` was always true and the loop
// never returned — pinning a core and starving the reconciler (observed at
// 150% CPU with the advisory lock never renewed). The fixed dequeue must
// return ok=false after one bounded rotation pass.
func TestDequeueSingleNotReadyKeyDoesNotSpin(t *testing.T) {
	q := newWorkQueue("workflow")
	q.enqueue("run-1")
	// Put the key in backoff so it is not ready yet.
	q.markFailed("run-1")

	// The manager calls dequeue on every heartbeat. If it hung, the test
	// would hang. Run it many times quickly to prove bounded behavior.
	for i := 0; i < 10_000; i++ {
		_, ok := q.dequeue()
		if ok {
			t.Fatalf("dequeue returned a not-ready key as ready")
		}
	}
}

// TestDequeueReadyKeyIsReturnedAfterBackoff verifies that a key in backoff
// becomes retrievable once its readyAt passes — the rotation keeps it in the
// queue instead of dropping it.
func TestDequeueReadyKeyIsReturnedAfterBackoff(t *testing.T) {
	q := newWorkQueue("workflow")
	q.enqueue("run-1")
	q.markFailed("run-1") // readyAt = now + 2s

	// Not ready yet.
	if k, ok := q.dequeue(); ok {
		t.Fatalf("expected no ready key, got %q", k)
	}

	// Force readyAt into the past and confirm it is returned.
	q.mu.Lock()
	if e, ok := q.pending["run-1"]; ok {
		e.readyAt = time.Now().Add(-time.Second)
	}
	q.mu.Unlock()

	k, ok := q.dequeue()
	if !ok {
		t.Fatalf("expected ready key after backoff elapsed")
	}
	if k != "run-1" {
		t.Fatalf("expected run-1, got %q", k)
	}
}

// TestDequeueFIFOWithBackoffMixing ensures FIFO order is preserved when
// some keys are ready and some are not, and that the scan is bounded.
func TestDequeueFIFOWithBackoffMixing(t *testing.T) {
	q := newWorkQueue("workflow")
	q.enqueue("run-a")
	q.enqueue("run-b")
	q.markFailed("run-b") // run-b not ready; run-a ready

	k, ok := q.dequeue()
	if !ok || k != "run-a" {
		t.Fatalf("expected run-a first, got %q ok=%v", k, ok)
	}
	// run-b must still be pending (rotated, not dropped).
	if _, ok := q.pending["run-b"]; !ok {
		t.Fatalf("run-b was dropped from the queue")
	}
}
