package server

// bindtest_test.go — binding an EPHEMERAL port is a shared, finite resource, and a test must not fail a
// build when the host momentarily has none to give.
//
// THE REPORTED FAILURE:
//
//	--- FAIL: TestPrimaryListenerServes
//	    listen_test.go:137: startListeners: server: bind primary http 127.0.0.1:0: listen tcp
//	                        127.0.0.1:0: bind: address already in use
//	--- FAIL: TestWildcardPrimarySkipsDoomedBridgeBind  (same, on ":0")
//	--- FAIL: TestServeExtraFailureIsNonFatal          (same)
//	--- FAIL: TestServeExtraBindsWhenAddressAppears    (failed holding an address)
//
// FOUR TESTS, ONE PACKAGE, ALL ON THEIR FIRST BIND. That shape is the diagnosis:
//
//   - Port 0 asks the kernel for ANY free port, so `bind: address already in use` here does NOT mean a
//     test collided with itself. It means the kernel had nothing to allocate AT THAT INSTANT.
//   - This package opens about FOUR sockets in a whole run (measured with `ss`, per-package), so the
//     suite is not the thing filling the pool.
//   - The package passes 20x on its own and the whole suite passes 3x on a quiet host, and it is green
//     in CI.
//
// So the cause is a HOST condition — a machine running containers, a dev stack and the full test suite in
// parallel can momentarily exhaust the ephemeral range — and the defect on OUR side is that the tests
// treated a finite, shared resource as guaranteed. A suite that fails a build for that teaches people to
// re-run until it passes, which is worse than no test.
//
// THE RETRY IS NARROW ON PURPOSE. It is bounded, and it retries EADDRINUSE only. A genuine bind failure —
// a privileged port, an address that is not on this host, a malformed addr — fails immediately and
// unchanged, because retrying those would hide a real bug behind a delay. When the attempts are exhausted
// the message says what is actually wrong and how to check, so nobody goes hunting for a collision that
// never happened.

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bindRetryAttempts is the bounded wait: 20 x bindRetryDelay ≈ 500ms, long enough to ride out an
// allocation burst and short enough that a genuinely broken host still fails quickly.
const bindRetryAttempts = 20

// bindRetryDelay is a VAR, not a const, so a test can shrink it — the same seam pattern this package
// already uses for extraBindRetryInterval (a test of a retry loop should not spend half a second proving
// that it loops).
var bindRetryDelay = 25 * time.Millisecond

// isEphemeralExhaustion reports whether err is the kernel declining to allocate a port.
//
// The error is matched STRUCTURALLY first (both Unix and Windows wrap a syscall.Errno that errors.Is
// resolves) and by text second, because the Windows errno for this does not always satisfy errors.Is
// against the portable constant and a test helper must not be the thing that fails on one platform.
func isEphemeralExhaustion(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}

// bindOutcome is what retryBind learned: the listener, or why it gave up.
type bindOutcome struct {
	ln    net.Listener
	err   error
	tries int
	// ephemeral is true when the LAST failure was the kernel declining to allocate a port, which is the
	// only cause worth wording as "the host is out of them".
	ephemeral bool
}

// retryBind runs bind until it succeeds, retrying ONLY an ephemeral-port allocation failure.
//
// IT IS A SEPARATE FUNCTION SO THE DECISION IS TESTABLE. The interesting behaviour here is not "it retries"
// — it is "it retries THIS and nothing else", and a loop buried inside a t.Fatalf wrapper cannot be
// asserted at all. A genuine bind failure must reach the caller on the FIRST attempt, unchanged: retrying a
// privileged port or an address that is not on this host would turn a clear error into a delay.
func retryBind(bind func() (net.Listener, error)) bindOutcome {
	out := bindOutcome{}
	for try := 1; try <= bindRetryAttempts; try++ {
		ln, err := bind()
		out.tries = try
		if err == nil {
			out.ln = ln
			return out
		}
		out.err = err
		out.ephemeral = isEphemeralExhaustion(err)
		if !out.ephemeral {
			return out // a real bind failure: reported as it is, on the first attempt
		}
		time.Sleep(bindRetryDelay)
	}
	return out
}

// outOfPortsMessage words the exhaustion failure so nobody hunts for a collision that never happened.
func outOfPortsMessage(what string, last error) string {
	return what + ": the HOST could not allocate an ephemeral port.\n" +
		"  The bind asked for port 0 (any free port), so this is NOT a collision inside these tests —\n" +
		"  the machine's ephemeral range had nothing to give. Check it with:\n" +
		"    sysctl net.ipv4.ip_local_port_range   (Linux: the range port 0 allocates from)\n" +
		"    ss -s; ss -tan | grep -c TIME-WAIT    (how many sockets the host is holding)\n" +
		"    netsh int ipv4 show excludedportrange protocol=tcp   (Windows: reserved blocks)\n" +
		"  last error: " + last.Error()
}

// mustStartListeners starts s's listeners, retrying a transient ephemeral-port allocation failure.
//
// Retrying the whole call is safe: startListeners binds the primary FIRST and returns on failure, before
// it spawns the bridge goroutine — so a retry cannot leave a second serveExtra behind.
func mustStartListeners(t *testing.T, s *Server, ctx context.Context) net.Listener {
	t.Helper()
	out := retryBind(func() (net.Listener, error) { return s.startListeners(ctx) })
	if out.ln != nil {
		return out.ln
	}
	if out.ephemeral {
		t.Fatalf("%s (gave up after %d attempts)", outOfPortsMessage("startListeners", out.err), out.tries)
	}
	t.Fatalf("startListeners: %v", out.err)
	return nil
}

// mustHoldLoopback binds an ephemeral loopback address in order to HOLD it, retrying the same way.
//
// This is the "something else already owns the address" half of the bridge tests: the value here is the
// exclusive hold, not the number.
func mustHoldLoopback(t *testing.T) net.Listener {
	t.Helper()
	out := retryBind(func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") })
	if out.ln != nil {
		return out.ln
	}
	if out.ephemeral {
		t.Fatalf("%s (gave up after %d attempts)", outOfPortsMessage("hold address", out.err), out.tries)
	}
	t.Fatalf("hold address: %v", out.err)
	return nil
}
