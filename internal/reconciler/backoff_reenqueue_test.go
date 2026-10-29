package reconciler

// backoff_reenqueue_test.go — A RE-ENQUEUED FAILURE MUST NOT COME BACK IMMEDIATELY.
//
// Reported as log noise and diagnosed as worse than noise: a workflow run that could not progress was retried
// (and warned about) on every 200ms heartbeat, 5552 times in 22 minutes. The queue already computed an
// exponential backoff in markFailed — and enqueue threw it away, because it hardcoded readyAt = now.
//
// These tests pin the two halves: enqueue honours a recorded failure, and the backoff is the documented shape.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// stubReconciler is the minimum a Manager needs to register a kind.
type stubReconciler struct{ kind string }

func (s *stubReconciler) Kind() string { return s.kind }
func (s *stubReconciler) Reconcile(ctx context.Context, key string) Result {
	return Result{}
}

// TestReenqueueHonoursRecordedFailure is the reported bug: the scan re-enqueues the same broken key every
// heartbeat, and that must not reset its backoff.
func TestReenqueueHonoursRecordedFailure(t *testing.T) {
	q := newWorkQueue("workflow")

	q.enqueue("run-1")
	if _, ok := q.dequeue(); !ok {
		t.Fatal("the first attempt must be ready immediately")
	}
	q.markFailed("run-1") // one failure -> 2s

	// The scan pass re-enqueues on the next heartbeat, 200ms later.
	q.enqueue("run-1")
	if key, ok := q.dequeue(); ok {
		t.Fatalf("re-enqueued key %q was handed back immediately — the backoff was discarded, which is what "+
			"produced a warning every 200ms for 22 minutes", key)
	}
}

// TestBackoffShape pins the schedule and its cap, because the cap is what bounds the noise: every failure
// doubles the wait, and nothing waits longer than five minutes.
func TestBackoffShape(t *testing.T) {
	for _, c := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{5, 32 * time.Second},
		{20, 5 * time.Minute}, // capped, not overflowed
	} {
		if got := backoffFor(c.failures); got != c.want {
			t.Errorf("backoffFor(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

// TestSuccessClearsTheBackoff keeps the fix from becoming a permanent stall: once a key succeeds, the next
// enqueue must be immediate again.
func TestSuccessClearsTheBackoff(t *testing.T) {
	q := newWorkQueue("workflow")
	q.enqueue("run-1")
	q.dequeue()
	q.markFailed("run-1")
	q.markFailed("run-1")

	q.markDone("run-1") // it succeeded

	q.enqueue("run-1")
	if _, ok := q.dequeue(); !ok {
		t.Fatal("after a success the key must be eligible immediately — a backoff that outlives the failure " +
			"would delay every run that ever had a bad pass")
	}
}

// TestFailRunBacksOffTheSameWay proves the Manager-level hook a scan pass now calls produces the queued
// behaviour, rather than a second, divergent policy.
func TestFailRunBacksOffTheSameWay(t *testing.T) {
	m := NewManager(nil, slog.New(slog.DiscardHandler))
	m.Register(&stubReconciler{kind: "workflow"})

	m.FailRun("workflow", "run-1", errors.New("db: not found"))
	m.FailRun("workflow", "run-1", errors.New("db: not found"))

	var q *workQueue
	m.mu.Lock()
	for i := range m.jobs {
		if m.jobs[i].rec.Kind() == "workflow" {
			q = m.jobs[i].queue
		}
	}
	m.mu.Unlock()
	if q == nil {
		t.Fatal("no queue registered for the kind")
	}
	if got := q.failures("run-1"); got != 2 {
		t.Fatalf("failures = %d, want 2 — the hook must record them so the backoff can grow", got)
	}
	// And the delay is the shared one, not a local guess.
	if want := backoffFor(2); want != 4*time.Second {
		t.Fatalf("backoffFor(2) = %v, want 4s", want)
	}
}
