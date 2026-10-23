package server

// bindretry_test.go — the retry must fire for ephemeral exhaustion and for NOTHING else.
//
// The interesting half of the helper is the refusals. A retry that also swallowed a privileged-port or
// not-on-this-host failure would turn a clear, permanent error into a 500ms delay followed by the same
// error — and worse, it would make a REAL bind bug look like host flakiness, which is the failure mode
// this whole change exists to avoid.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// eaddrinuse builds the error shape net.Listen actually returns, so the classifier is tested against the
// real wrapping rather than against a bare sentinel.
func eaddrinuse() error {
	return &net.OpError{
		Op:  "listen",
		Net: "tcp",
		Err: &net.AddrError{Err: syscall.EADDRINUSE.Error(), Addr: "127.0.0.1:0"},
	}
}

// A TRANSIENT EXHAUSTION IS RIDDEN OUT: fail twice, then succeed.
func TestRetryBindRidesOutTransientExhaustion(t *testing.T) {
	prev := bindRetryDelay
	bindRetryDelay = time.Millisecond
	defer func() { bindRetryDelay = prev }()

	attempts := 0
	out := retryBind(func() (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, eaddrinuse()
		}
		// Succeed — the value is irrelevant; the retry is what is under test.
		return nil, nil
	})
	if out.tries != 3 {
		t.Errorf("retryBind gave up after %d tries; it must keep going while the cause is exhaustion", out.tries)
	}
	if out.err != nil && attempts < 3 {
		t.Errorf("retryBind stopped early: %v", out.err)
	}
}

// AND IT GIVES UP, BOUNDED — a permanently exhausted host must still end the test.
func TestRetryBindIsBounded(t *testing.T) {
	attempts := 0
	out := retryBind(func() (net.Listener, error) {
		attempts++
		return nil, eaddrinuse()
	})
	if attempts != bindRetryAttempts {
		t.Errorf("retryBind tried %d times, want exactly %d (a bounded wait, not an unbounded one)",
			attempts, bindRetryAttempts)
	}
	if out.ln != nil {
		t.Error("retryBind returned a listener despite every attempt failing")
	}
	if !out.ephemeral {
		t.Error("the outcome does not record that the cause was exhaustion, so the caller cannot word " +
			"the failure as the host being out of ports")
	}
	msg := outOfPortsMessage("startListeners", out.err)
	for _, want := range []string{"NOT a collision", "ip_local_port_range", "TIME-WAIT"} {
		if !containsStr(msg, want) {
			t.Errorf("the out-of-ports message does not mention %q, so the operator is left to guess:\n%s",
				want, msg)
		}
	}
}

// A REAL BIND FAILURE IS NOT RETRIED — it reaches the caller on the FIRST attempt.
//
// This is the assertion that keeps the retry from becoming a disguise: a privileged port, an address that
// is not on this host, a malformed addr must all fail immediately and unchanged.
func TestRetryBindDoesNotRetryOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"permission denied (privileged port)", &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EACCES}},
		{"cannot assign requested address (not on this host)", &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRNOTAVAIL}},
		{"unparseable address", errors.New(`listen tcp: address 192.0.2.1:9: missing port`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			out := retryBind(func() (net.Listener, error) {
				attempts++
				return nil, tc.err
			})
			if attempts != 1 {
				t.Errorf("retryBind tried %d times for %v; a non-ephemeral failure must fail on the "+
					"FIRST attempt so a real bind bug is not disguised as host flakiness", attempts, tc.err)
			}
			if out.ephemeral {
				t.Errorf("the outcome claims %v was exhaustion", tc.err)
			}
			if out.err == nil {
				t.Error("the real failure was swallowed")
			}
		})
	}
}

// THE CLASSIFIER ITSELF, including the portable-constant caveat it documents.
func TestIsEphemeralExhaustion(t *testing.T) {
	if !isEphemeralExhaustion(syscall.EADDRINUSE) {
		t.Error("the bare EADDRINUSE errno must classify as exhaustion")
	}
	if !isEphemeralExhaustion(eaddrinuse()) {
		t.Error("a wrapped EADDRINUSE (the real net.Listen shape) must classify as exhaustion")
	}
	if !isEphemeralExhaustion(fmt.Errorf("startListeners: %w", syscall.EADDRINUSE)) {
		t.Error("a doubly-wrapped EADDRINUSE must classify as exhaustion")
	}
	for _, no := range []error{nil, syscall.EACCES, syscall.EADDRNOTAVAIL, errors.New("boom")} {
		if isEphemeralExhaustion(no) {
			t.Errorf("%v was classified as ephemeral exhaustion", no)
		}
	}
}

// containsStr is a local substring helper (this package has no shared one).
func containsStr(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// THE FLAKY FORM MUST NOT COME BACK.
//
// A future edit that binds port 0 directly — or calls startListeners with a bare t.Fatalf — reintroduces
// exactly the failure that was reported: four tests failing a build because the HOST momentarily had no
// ephemeral port to give. The helper is the fix; this is the guard that keeps it used.
func TestEphemeralBindsGoThroughTheHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		// The helper's own file is where the raw bind is allowed to live, and THIS file necessarily
		// mentions both patterns (it is the test of the helper, including its failure messages).
		if name == "bindtest_test.go" || name == "bindretry_test.go" {
			continue
		}
		scanned++
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		if strings.Contains(src, ":= s.startListeners(") {
			t.Errorf("%s calls s.startListeners directly and then asserts on the error. Bind through "+
				"mustStartListeners instead: port 0 asks the kernel for any free port, so EADDRINUSE "+
				"there means the HOST was out of them, not that the test collided with itself.", name)
		}
		if strings.Contains(src, `net.Listen("tcp", "127.0.0.1:0")`) {
			t.Errorf("%s binds a raw ephemeral loopback. Use mustHoldLoopback so a momentary exhaustion "+
				"does not fail the build.", name)
		}
	}
	// VACUITY GUARD: a walk that found no test files proves nothing.
	if scanned < 3 {
		t.Fatalf("scanned only %d test files — the walk is not looking where it thinks", scanned)
	}
}
