// Package reconciler is the Kubernetes-style reconcile loop framework
// (docs/03_Scheduler_and_Runtime_Design.md §2, docs/01 §5).
package reconciler

import (
	"context"
	"time"
)

// Result instructs the work queue how to proceed after a reconcile pass.
type Result struct {
	// RequeueAfter, if non-zero, schedules another pass for this key.
	RequeueAfter time.Duration
	// Error, if non-nil, advances the retry budget and is surfaced as an
	// event + telemetry span (no silent failures — docs/03 §1).
	Error error
}

// Reconciler is implemented by each entity's control loop.
type Reconciler interface {
	// Kind returns the reconciler kind, used for work-queue and
	// leadership election keying.
	Kind() string
	// Reconcile reconciles the object identified by key.
	Reconcile(ctx context.Context, key string) Result
}
