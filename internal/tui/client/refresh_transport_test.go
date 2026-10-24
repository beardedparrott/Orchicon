package client

// refresh_transport_test.go — the refresh path must not leak a connection per attempt.
//
// THE INCIDENT. A TUI whose access token had expired sat polling for about an hour. `RefreshToken` built a
// `&http.Transport{}` PER CALL and never closed it, so every failed refresh abandoned a transport AND its
// connection. Measured on the reporting machine: one TUI held 28,226 ESTABLISHED sockets to the plane's
// control port; the host's ephemeral range was full (28,333 in use of 28,231 available) and every later
// `bind()` on the machine failed — which is how it surfaced, as a `make rebuild-dev` unable to open a single
// listener.
//
// REPRODUCED in isolation before the fix: 40 calls in the old shape left 40 connections established and
// nothing closed them. These assert the opposite, by counting REAL server-side connections rather than by
// reasoning about the code.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// countingServer is a 401 server that counts how many TCP connections it ACCEPTED — which is exactly the
// resource the leak consumed.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A refresh that fails the way an expired/rejected refresh token does.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &opened
}

// TestRefreshTokenDoesNotLeakConnections is the guard for the incident.
func TestRefreshTokenDoesNotLeakConnections(t *testing.T) {
	srv, opened := countingServer(t)

	const attempts = 25
	for i := 0; i < attempts; i++ {
		_, err := RefreshToken(context.Background(), srv.URL, true, "expired-refresh-token")
		if err == nil {
			t.Fatal("the 401 server should have failed the refresh; the test is not exercising the failure path")
		}
	}

	// ONE CONNECTION, REUSED. The old shape opened `attempts` of them and closed none, so this bound is the
	// whole point: a couple of connections is keep-alive doing its job, `attempts` is the leak.
	if got := opened.Load(); got > 3 {
		t.Fatalf("%d refresh attempts opened %d connections. A per-attempt connection that is never closed is "+
			"the leak that exhausted the host's ephemeral port range (28,226 sockets from one TUI); the "+
			"transport must be shared, not built per call", attempts, got)
	}
}

// TestRefreshTransportIsShared pins the mechanism, so a future edit cannot quietly go back to `&http.Transport{}`.
func TestRefreshTransportIsShared(t *testing.T) {
	a := refreshTransportFor(false)
	b := refreshTransportFor(false)
	if a != b {
		t.Error("each call built a NEW transport. That is the leak: a transport per attempt, none ever closed")
	}
	if a == refreshTransportFor(true) {
		t.Error("the insecure and verifying modes share one transport, so the insecure flag would silently " +
			"apply to a verifying client (or vice versa)")
	}
}

// TestRefreshTransportIsBounded: this client is long-lived and may retry, so the pool needs a ceiling — reuse
// must not become "hold every connection open for ever".
func TestRefreshTransportIsBounded(t *testing.T) {
	tr := refreshTransportFor(false)
	if tr.MaxIdleConns <= 0 || tr.MaxIdleConnsPerHost <= 0 {
		t.Errorf("unbounded idle pool (MaxIdleConns=%d, MaxIdleConnsPerHost=%d); a retry storm could hold "+
			"connections open without limit", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Error("no IdleConnTimeout: idle connections would never expire")
	}
}
