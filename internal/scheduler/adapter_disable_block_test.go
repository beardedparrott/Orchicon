package scheduler

// Tests for the AC 3 classification contract: a dispatch whose adapter kind
// can never resolve (disabled, or declared but unregistered) is PERMANENT —
// the reconciler blocks the work item instead of requeueing it, which would
// spin ready→failed→ready forever with the reason attached only to the
// execution. Transient adapter failures (unreachable mid-dispatch) keep the
// existing requeue-with-backoff path.
//
// The DB side effect (WorkItemBlocked + failed_to_start with the reason) is
// produced by markFailedToStartPermanent, which mirrors the proven
// markFailedToStart transition exactly and differs only in the terminal
// status it writes for a standalone task.

import (
	"fmt"
	"testing"
)

// permanentDispatchFailure (reconciler.go) is the classification the
// reconciler's dispatch path actually applies — this test drives the
// PRODUCTION predicate directly, never a copy of it, so it cannot pass
// against a stale re-implementation.
func TestPermanentDispatchFailureClassification(t *testing.T) {
	d := NewDispatcher()
	d.Register("orchicon", &fakeBridge{name: "orchicon"})
	d.Disable("claude")

	cases := []struct {
		name string
		kind string
		want bool
	}{
		{"administratively disabled kind", "claude", true},
		{"declared-but-unregistered kind", "claude-code", true},
		{"registered and enabled kind", "orchicon", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Resolve(tc.kind)
			// A resolvable kind is by definition not a permanent failure.
			got := false
			if err != nil {
				got = permanentDispatchFailure(err)
			}
			if got != tc.want {
				t.Errorf("permanentDispatchFailure(Resolve(%q)) = %v, want %v (err=%v)", tc.kind, got, tc.want, err)
			}
		})
	}

	// A TRANSIENT adapter failure (the bridge was resolved, then the
	// adapter was unreachable) must keep the requeue path: it is not one of
	// the two permanent sentinels.
	transient := fmt.Errorf("adapter unreachable mid-dispatch: dial tcp 127.0.0.1:9: connection refused")
	if permanentDispatchFailure(transient) {
		t.Error("a transient adapter-unreachable error was classified permanent — it must keep requeueing with backoff")
	}
	// And the wrapped forms still classify (the reconciler sees wrapped
	// errors on the RPC/recovery paths).
	if !permanentDispatchFailure(fmt.Errorf("dispatch failed: %w", ErrAdapterDisabled)) {
		t.Error("a wrapped ErrAdapterDisabled was not classified permanent")
	}
	if !permanentDispatchFailure(fmt.Errorf("dispatch failed: %w", ErrAdapterKindUnregistered)) {
		t.Error("a wrapped ErrAdapterKindUnregistered was not classified permanent")
	}
}
