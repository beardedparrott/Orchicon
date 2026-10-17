package scheduler

// Stale-version retry: a `db: not found` from an optimistic-concurrency UPDATE
// is a LOST RACE, not a missing row.
//
// THE FIELD FAILURE THIS EXISTS FOR (run 01M54EJY52ZW1NSDAQ5X07JN38, sort 5 of
// the Podman feature, 2026-10-17). The run finished with its PR merged, and two
// writes that used a version captured EARLIER in the same pass both missed:
//
//	03:10:22 WARN workflow run reconcile failed  error="mark task step terminal: db: not found"
//	03:46:43 WARN step-success: update execution PR fields (best-effort)
//	         execution=01M54FE48… error="db: not found"
//
// The first aborted the whole pass (rolling back an upstream step's terminal
// mark); the second was best-effort, so it degraded quietly and the operator
// saw a completed run with no PR link. Both are the same shape: another
// reconciler bumped the row between the read and the write.
//
// WHY A RETRY MUST REFETCH. Re-sending the same expectedVersion cannot succeed —
// the UPDATE matches zero rows every single time, so a blind `sleep; retry`
// retries a request that is already known to be unsatisfiable. Each attempt
// therefore RE-READS the row to take its current version, which is what makes
// the retry able to converge. That is safe inside our transaction because the
// tenant tx runs READ COMMITTED (`db.BeginTenantTx`), so a re-read sees the row
// the other reconciler has since committed.
//
// The first attempt is IMMEDIATE. The conflicting write has usually already
// committed by the time we get here, so the common case converges with no delay
// at all; the delay only applies once a re-read has failed to help, which is
// the rare case of sustained contention.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

const (
	// staleVersionAttempts bounds the retries. Three is deliberate: the
	// conflict is between two reconcilers, so it resolves on the next read in
	// virtually every case, and a long retry inside a reconcile transaction
	// holds row locks the other reconciler needs.
	staleVersionAttempts = 3
	// staleVersionDelay is the wait between attempts AFTER the first, which is
	// immediate. Default 2s balances "give the other pass time to commit"
	// against "do not hold this transaction open".
	defaultStaleVersionDelay = 2 * time.Second
)

// staleVersionDelay is the inter-attempt wait, overridable for operators
// (ORCHICON_STALE_VERSION_RETRY_DELAY, a Go duration) because the right value is
// a property of the deployment's reconciler cadence, not of the code.
func staleVersionDelay() time.Duration {
	if v := os.Getenv("ORCHICON_STALE_VERSION_RETRY_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultStaleVersionDelay
}

// retryStaleVersion runs fn until it stops losing the optimistic-concurrency
// race. fn is expected to re-read whatever it writes, on every attempt — the
// helper cannot do that for it, and an fn that reuses a captured version will
// simply burn all its attempts.
//
// Only db.ErrNotFound is retried: any other error is a genuine failure and is
// returned at once, so this can never mask a real problem behind a delay.
func retryStaleVersion[T any](ctx context.Context, log *slog.Logger, op string, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= staleVersionAttempts; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, db.ErrNotFound) {
			return zero, err
		}
		lastErr = err
		if attempt == staleVersionAttempts {
			break
		}
		if log != nil {
			// Warn, not Debug: this is the signal that a write nearly became a
			// silent loss. It is the log line an operator needs when a PR link
			// or a terminal mark goes missing, and before this helper existed
			// there was no such line at all on the best-effort path.
			log.Warn("stale-version write lost the race, refetching and retrying",
				"op", op, "attempt", attempt, "of", staleVersionAttempts, "error", err)
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(staleVersionDelay()):
		}
	}
	return zero, lastErr
}
