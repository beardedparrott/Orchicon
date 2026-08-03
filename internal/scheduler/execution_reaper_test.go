package scheduler

import "testing"

// TestReaperConsecutiveNotAlive verifies the reaper only crosses the reap
// threshold after N consecutive not-alive probes, that an alive answer
// resets the streak, and that pruning drops counters for executions that
// are no longer running.
func TestReaperConsecutiveNotAlive(t *testing.T) {
	r := NewExecutionReaper(nil, nil, nil, nil, nil)

	if got := r.bump("ex1"); got != 1 {
		t.Fatalf("first bump: got %d, want 1", got)
	}
	if got := r.bump("ex1"); got != 2 {
		t.Fatalf("second bump: got %d, want 2", got)
	}
	// An alive answer resets the streak.
	r.forget("ex1")
	if got := r.bump("ex1"); got != 1 {
		t.Fatalf("bump after forget: got %d, want 1", got)
	}

	r.bump("ex2")
	r.bump("ex2")
	r.bump("ex3")

	// Prune everything except ex2 — ex1 and ex3 counters must drop.
	r.prune(map[string]bool{"ex2": true})
	r.notAliveMu.Lock()
	if _, ok := r.notAlive["ex1"]; ok {
		r.notAliveMu.Unlock()
		t.Fatalf("ex1 counter not pruned")
	}
	if _, ok := r.notAlive["ex3"]; ok {
		r.notAliveMu.Unlock()
		t.Fatalf("ex3 counter not pruned")
	}
	if got := r.notAlive["ex2"]; got != 2 {
		r.notAliveMu.Unlock()
		t.Fatalf("ex2 counter: got %d, want 2", got)
	}
	r.notAliveMu.Unlock()
}
