package orchicon

import (
	"context"
	"testing"
	"time"
)

// A wall-clock deadline and a cancellation both land on ctx.Done(), and the native loop used to
// report BOTH as "cancelled". That label is what made a 3600s budget gate look like an unexplained
// cancellation: three runs were stopped at exactly one hour and every one of them said "cancelled".
func TestContextEndReasonDistinguishesDeadlineFromCancel(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if got := contextEndReason(ctx); got != "wall_clock_timeout" {
			t.Errorf("deadline reported as %q, want wall_clock_timeout", got)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		<-ctx.Done()
		if got := contextEndReason(ctx); got != "cancelled" {
			t.Errorf("cancel reported as %q, want cancelled", got)
		}
	})

	t.Run("parent with deadline wrapping a cancel", func(t *testing.T) {
		// The real shape: Start() layers a cancel over ctx and may layer a deadline on top. Whichever
		// fires, the reason must name the CAUSE, not the wrapper.
		parent, pcancel := context.WithCancel(context.Background())
		ctx, cancel := context.WithTimeout(parent, time.Nanosecond)
		defer cancel()
		defer pcancel()
		<-ctx.Done()
		if got := contextEndReason(ctx); got != "wall_clock_timeout" {
			t.Errorf("wrapped deadline reported as %q, want wall_clock_timeout", got)
		}
	})
}
