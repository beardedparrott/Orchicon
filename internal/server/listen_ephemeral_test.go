package server

// listen_ephemeral_test.go — a port-0 bind must survive a transient "no port right now".
//
// REPORTED, from a full `go test ./...` on a loaded host:
//
//	--- FAIL: TestWildcardPrimarySkipsDoomedBridgeBind
//	    startListeners: server: bind primary http :0: listen tcp :0: bind: address already in use
//	--- FAIL: TestPrimaryListenerServes
//	    startListeners: server: bind primary http 127.0.0.1:0: listen tcp 127.0.0.1:0: bind: address already in use
//
// A request for PORT 0 means "give me ANY free port", so failing is essentially impossible — and that is
// exactly why a single failure must not be taken as a definitive answer. The host was momentarily out of
// allocatable ephemeral ports; nothing else can own a port the kernel has not yet chosen, so "none right
// now" is transient by definition. The same package passed repeatedly in isolation minutes later, and no
// listener leak was found (fd count flat at 4 across a run).
//
// THE SAFETY PROPERTY IS THAT THIS CANNOT MASK A REAL COLLISION: a real conflict names a SPECIFIC port, and
// specific ports are not retried. That is the assertion that matters here.

import (
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A SPECIFIC PORT STILL FAILS IMMEDIATELY — retrying it would only delay the answer the operator needs.
func TestSpecificPortIsNotRetried(t *testing.T) {
	// Hold a port through the SHARED helper, not a raw net.Listen.
	//
	// A raw port-0 bind here trips TestEphemeralBindsGoThroughTheHelper — correctly: this package must not
	// assume the host has an ephemeral port to give at any given instant. There are now TWO layers for that,
	// and they are complementary rather than duplicated:
	//
	//   listenRetryingEphemeral (production)  the PLANE retries a port-0 bind, so a transient exhaustion is
	//                                         not a failed boot
	//   mustHoldLoopback       (tests)        a TEST retries the same way, so it is not a failed build
	held := mustHoldLoopback(t)
	defer func() { _ = held.Close() }()
	addr := held.Addr().String()

	// Speed the retry up so a WRONG implementation fails fast rather than after the real backoff.
	prevAttempts, prevBackoff := ephemeralBindAttempts, ephemeralBindBackoff
	ephemeralBindAttempts, ephemeralBindBackoff = 5, time.Millisecond
	defer func() { ephemeralBindAttempts, ephemeralBindBackoff = prevAttempts, prevBackoff }()

	start := time.Now()
	if _, err := listenRetryingEphemeral(addr); err == nil {
		t.Fatalf("binding the port %s another listener already holds must FAIL, not succeed", addr)
	}
	// Five attempts at 1ms is ~4ms; anything near the real backoff means the retry ran for a fixed port.
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("a SPECIFIC port collided in %s — it must fail on the FIRST attempt. Retrying a named "+
			"port cannot help (the owner does not go away) and delays the collision the operator needs "+
			"to see", elapsed)
	}
}

// AN EPHEMERAL BIND SUCCEEDS ON THE FIRST TRY when the host is healthy, so the retry costs nothing in the
// ordinary case.
func TestEphemeralBindSucceedsImmediately(t *testing.T) {
	prevAttempts, prevBackoff := ephemeralBindAttempts, ephemeralBindBackoff
	ephemeralBindAttempts, ephemeralBindBackoff = 5, time.Second // a huge backoff: proves it never waits
	defer func() { ephemeralBindAttempts, ephemeralBindBackoff = prevAttempts, prevBackoff }()

	start := time.Now()
	ln, err := listenRetryingEphemeral("127.0.0.1:0")
	if err != nil {
		t.Fatalf("a healthy host must bind an ephemeral port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the first attempt took %s — the retry must not sleep when the bind succeeds", elapsed)
	}
	if ln.Addr().(*net.TCPAddr).Port == 0 {
		t.Error("the listener reported port 0, so it did not actually bind an ephemeral port")
	}
}

// THE WILDCARD FORM RETRIES TOO. `:0` (no host) is the shape the failing tests use, and it takes a
// different code path through SplitHostPort than `127.0.0.1:0`.
func TestWildcardEphemeralBindIsRecognised(t *testing.T) {
	ln, err := listenRetryingEphemeral(":0")
	if err != nil {
		t.Fatalf("binding :0 must succeed on a healthy host: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if ln.Addr().(*net.TCPAddr).Port == 0 {
		t.Error(":0 did not bind an ephemeral port")
	}
}

// AN UNPARSEABLE ADDRESS IS NOT RETRIED — it is a caller bug, and reporting it once is the useful answer.
func TestUnparseableAddrIsNotRetried(t *testing.T) {
	if _, err := listenRetryingEphemeral("not-an-address"); err == nil {
		t.Fatal("an unparseable address must fail")
	}
}

// THE RETRY ACTUALLY ENGAGES — asserted by failing the first attempts, not by reading the loop.
//
// Without this, "there is a retry" and "the retry never runs" are the same test result: the healthy-host
// cases above pass either way, because they never reach the loop.
func TestEphemeralRetryEngagesAfterTransientFailures(t *testing.T) {
	prevAttempts, prevBackoff, prevFn := ephemeralBindAttempts, ephemeralBindBackoff, listenFn
	ephemeralBindAttempts, ephemeralBindBackoff = 5, time.Millisecond
	defer func() {
		ephemeralBindAttempts, ephemeralBindBackoff, listenFn = prevAttempts, prevBackoff, prevFn
	}()

	// Fail the first two attempts with the EXACT error the host produced, then succeed for real.
	var calls int32
	listenFn = func(network, addr string) (net.Listener, error) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			return nil, errors.New("listen tcp :0: bind: address already in use")
		}
		return net.Listen(network, addr)
	}

	ln, err := listenRetryingEphemeral(":0")
	if err != nil {
		t.Fatalf("the retry did not survive two transient 'address already in use' failures: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("listen was attempted %d times, want 3 (two failures, then success)", got)
	}
}

// AND IT GIVES UP BOUNDED, returning the FIRST error so the message names the address asked for.
func TestEphemeralRetryIsBounded(t *testing.T) {
	prevAttempts, prevBackoff, prevFn := ephemeralBindAttempts, ephemeralBindBackoff, listenFn
	ephemeralBindAttempts, ephemeralBindBackoff = 3, time.Millisecond
	defer func() {
		ephemeralBindAttempts, ephemeralBindBackoff, listenFn = prevAttempts, prevBackoff, prevFn
	}()

	var calls int32
	listenFn = func(string, string) (net.Listener, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("listen tcp :0: bind: address already in use")
	}

	_, err := listenRetryingEphemeral(":0")
	if err == nil {
		t.Fatal("a permanently unavailable ephemeral range must still fail")
	}
	if !strings.Contains(err.Error(), ":0") {
		t.Errorf("the returned error must name the address that was asked for, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("listen was attempted %d times, want exactly the bounded 3 — an unbounded retry would "+
			"hang a plane boot", got)
	}
}
