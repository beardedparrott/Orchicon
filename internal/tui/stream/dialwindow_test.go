package stream

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// collectStatuses drains a status channel until pred is satisfied or the
// deadline passes, returning everything seen.
func collectStatuses(t *testing.T, ch <-chan Status, pred func(Status) bool, within time.Duration) []Status {
	t.Helper()
	deadline := time.After(within)
	var seen []Status
	for {
		select {
		case st := <-ch:
			seen = append(seen, st)
			if pred(st) {
				return seen
			}
		case <-deadline:
			return seen
		}
	}
}

// A server-streaming call does not return from Open until the server sends its
// FIRST message (the client makes the request synchronously and waits for
// response headers). A subscription whose server stays quiet must therefore
// still read as CONNECTED — otherwise the footer showed "connecting…" forever
// with no error, no retry and no explanation, while the app was perfectly
// connected (the operator could see and edit work items the whole time).
func TestOpenWhileTheDialIsStillInFlight(t *testing.T) {
	release := make(chan struct{})
	statuses := make(chan Status, 32)
	sub := New(Config[string]{
		Name: "quiet",
		Open: func(_ context.Context, _ int64) (func() (string, error), error) {
			<-release // the server never flushes headers
			return func() (string, error) { select {} }, nil
		},
		OnStatus:   func(s Status) { statuses <- s },
		DialWindow: 40 * time.Millisecond,
	})
	defer func() {
		close(release)
		sub.Close()
	}()

	seen := collectStatuses(t, statuses, func(s Status) bool { return s == StatusOpen }, time.Second)
	if len(seen) == 0 || seen[len(seen)-1] != StatusOpen {
		t.Fatalf("a dial still in flight must report open, saw %v", seen)
	}
	if got := sub.Status(); got != StatusOpen {
		t.Fatalf("status = %q, want open", got)
	}
}

// An immediate failure still wins the race: a rejected stream (unimplemented,
// unauthenticated, connection refused) must report error, never a false "open".
func TestImmediateDialErrorWinsTheRace(t *testing.T) {
	statuses := make(chan Status, 32)
	sub := New(Config[string]{
		Name: "rejected",
		Open: func(_ context.Context, _ int64) (func() (string, error), error) {
			return nil, errors.New("connection refused")
		},
		OnStatus:   func(s Status) { statuses <- s },
		DialWindow: 5 * time.Second, // long: the error must not wait for it
	})
	defer sub.Close()

	seen := collectStatuses(t, statuses, func(s Status) bool { return s == StatusError }, time.Second)
	if len(seen) == 0 || seen[len(seen)-1] != StatusError {
		t.Fatalf("an immediate dial error must report error, saw %v", seen)
	}
}

// The dial error is forwarded to OnError, so the shell can name the reason.
func TestDialErrorIsForwardedToOnError(t *testing.T) {
	errs := make(chan error, 8)
	sub := New(Config[string]{
		Name: "rejected",
		Open: func(_ context.Context, _ int64) (func() (string, error), error) {
			return nil, errors.New("connect: connection refused")
		},
		OnError: func(err error) { errs <- err },
	})
	defer sub.Close()

	select {
	case err := <-errs:
		if err == nil || err.Error() != "connect: connection refused" {
			t.Fatalf("forwarded error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the dial error must be forwarded to OnError")
	}
}

// Close must not wait forever for a recv() that ignores cancellation.
//
// Close runs on EVERY tab switch, so an unbounded wait there is a UI freeze, not
// just a test problem. This is the exact shape that hung the suite for the full 10m
// timeout (blocking `make rebuild-dev`): the loop sits in an uninterruptible recv()
// and wg.Wait() never returns.
func TestCloseIsBoundedWhenRecvIgnoresCancellation(t *testing.T) {
	sub := New(Config[string]{
		Name: "wedged",
		Open: func(_ context.Context, _ int64) (func() (string, error), error) {
			// A transport that never returns and never observes ctx.
			return func() (string, error) { select {} }, nil
		},
		DialWindow: 10 * time.Millisecond,
	})
	// Let the loop actually enter recv() (past the DialWindow).
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		sub.Close()
		close(done)
	}()
	select {
	case <-done:
		// bounded — correct
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a recv() that ignores cancellation — a tab switch would freeze the UI forever")
	}
}

// A NORMAL Close (the loop exits on cancel) must still return promptly, so the bound
// never becomes the common path.
func TestCloseReturnsPromptlyOnAHealthyStream(t *testing.T) {
	sub := New(Config[string]{
		Name: "healthy",
		Open: func(_ context.Context, _ int64) (func() (string, error), error) {
			return func() (string, error) { return "", io.EOF }, nil
		},
		DialWindow: 10 * time.Millisecond,
	})
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	sub.Close()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("a healthy Close took %v — the grace period must not be the normal path", elapsed)
	}
}
