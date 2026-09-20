package server

// periodic.go — the ONE way this package runs a recurring sweep.
//
// It exists because the obvious way to write "do this at boot, then every N" is
// wrong in a way that survives review, tests and a running plane without ever
// announcing itself.

import (
	"context"
	"time"
)

// runOnStartupThenEvery runs fn once after warmup, then every interval, until ctx
// is done.
//
// THE SHAPE IS THE WHOLE POINT. This is the loop it replaces:
//
//	for {
//		select {
//		case <-ctx.Done():
//			return
//		case <-time.After(warmup): // ← a FRESH timer on every iteration
//			fn()
//		case <-ticker.C: // ← therefore unreachable
//			fn()
//		}
//	}
//
// `time.After` builds a NEW timer each time the case is evaluated (it is not a
// reusable channel), so the shorter duration wins the race on every single
// iteration and the longer case never fires. The effect is not a crash or an
// error: it is work being done thirty times more often than the comment above it
// claims, forever, with no symptom other than load. Measured on a 1s/30s pair, the
// short branch fired 4 times in 5 seconds and the ticker fired 0.
//
// Two sweeps in this package had exactly that shape (the execution-liveness reaper
// and the MCP stale-child sweep), both documented as "at boot and every 30s" and
// both actually running about once a second. The shape lives in one place now, so
// it cannot be reintroduced by copying a neighbour — and the test beside this file
// pins the cadence rather than trusting the comment.
//
// The warm-up is a SEQUENTIAL phase rather than a case for precisely that reason:
// it must finish before the ticker starts being awaited, so the only things in the
// loop's select are the ticker and cancellation.
//
// fn runs once after the warm-up and then on every tick, so "sweep at boot" is the
// caller's to state rather than a second mechanism. It runs synchronously on this
// goroutine — callers pass a goroutine only if they want one (`go
// runOnStartupThenEvery(...)`), and a slow fn simply delays the next tick instead
// of piling up concurrent runs, which is the behaviour a sweep wants.
//
// A non-positive warmup skips straight to the ticker. A non-positive interval
// returns immediately rather than panicking: `time.NewTicker` panics on a
// non-positive duration, and a misconfigured sweep should be inert, not fatal to
// the plane it is supposed to be tidying. A nil fn is a no-op sweep.
func runOnStartupThenEvery(ctx context.Context, warmup, interval time.Duration, fn func()) {
	if interval <= 0 || fn == nil {
		return
	}
	if warmup > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(warmup):
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}
