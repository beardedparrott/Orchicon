package opencode

import (
	"testing"
)

// TestSessionErrorMessageSurfacesNestedReason verifies that a
// session.error bus event surfaces the provider's readable reason (nested
// at error.data.message) instead of the generic fallback, so the failure
// is diagnosable in the run view.
func TestSessionErrorMessageSurfacesNestedReason(t *testing.T) {
	cases := []struct {
		name string
		evt  BusEvent
		want string
	}{
		{
			name: "top-level message",
			evt:  BusEvent{Type: "session.error", Properties: map[string]any{"error": map[string]any{"message": "model timed out"}}},
			want: "model timed out",
		},
		{
			name: "nested data.message (provider error)",
			evt:  BusEvent{Type: "session.error", Properties: map[string]any{"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "rate limited"}}}},
			want: "rate limited",
		},
		{
			name: "empty error object",
			evt:  BusEvent{Type: "session.error", Properties: map[string]any{"error": map[string]any{}}},
			want: "opencode session error",
		},
		{
			name: "no error property",
			evt:  BusEvent{Type: "session.error", Properties: map[string]any{}},
			want: "opencode session error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionErrorMessage(tc.evt); got != tc.want {
				t.Fatalf("sessionErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCountSessionErrorThreshold verifies the consecutive-error counter
// only recycles once the threshold is reached, and that genuine progress
// (noteSessionProgress) resets it so a single transient failure never
// triggers a container recycle.
func TestCountSessionErrorThreshold(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD", "3")
	a := &Adapter{}

	// Below threshold: count accumulates, no recycle.
	for i := 1; i <= 2; i++ {
		if a.countSessionError() {
			t.Fatalf("countSessionError() = true on attempt %d, want false", i)
		}
		if got := a.sessionErrorCount(); got != i {
			t.Fatalf("counter = %d after %d errors, want %d", got, i, i)
		}
	}

	// A progress signal resets the count.
	r := &sessionRun{a: a}
	r.noteSessionProgress()
	if got := a.sessionErrorCount(); got != 0 {
		t.Fatalf("counter = %d after progress reset, want 0", got)
	}

	// Now accumulate to the threshold: recycle fires.
	for i := 1; i <= 2; i++ {
		if a.countSessionError() {
			t.Fatalf("countSessionError() = true on attempt %d (reset run), want false", i)
		}
	}
	if !a.countSessionError() {
		t.Fatal("countSessionError() = false on threshold attempt, want true")
	}
	// Threshold hit → counter reset for the next wedge.
	if got := a.sessionErrorCount(); got != 0 {
		t.Fatalf("counter = %d after recycle, want 0", got)
	}
}

// TestCountSessionErrorDisabled verifies a threshold < 1 disables the
// recycle entirely.
func TestCountSessionErrorDisabled(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_ERROR_RECYCLE_THRESHOLD", "0")
	a := &Adapter{}
	for i := 0; i < 10; i++ {
		if a.countSessionError() {
			t.Fatalf("countSessionError() = true with threshold 0 on attempt %d", i)
		}
	}
}
