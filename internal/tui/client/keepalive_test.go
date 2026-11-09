package client

// keepalive_test.go — a password-mode session must never meet an expiry during an idle stretch.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// newKeepaliveClient builds a Clients with a refreshable session whose Refresh counts its calls.
func newKeepaliveClient(refreshes *int32) *Clients {
	holder := &tokenHolder{token: "old"}
	sc := &SessionClient{Token: holder.get, SetToken: holder.set, cell: holder}
	sc.Refresh = func(ctx context.Context) (string, error) {
		atomic.AddInt32(refreshes, 1)
		return "new", nil
	}
	return &Clients{Session: sc}
}

// THE TOKEN IS REFRESHED WITHOUT ANY REQUEST FAILING FIRST.
func TestKeepaliveRefreshesBeforeExpiryWithoutAny401(t *testing.T) {
	var refreshes int32
	c := newKeepaliveClient(&refreshes)

	// A 10s lifetime -> a 5s period (lifetime/2, floored at keepaliveMinInterval). One period is
	// enough to prove the timer fires with no request in flight.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var onRefreshed int32
	k := c.StartKeepalive(ctx, 10*time.Second, func() { atomic.AddInt32(&onRefreshed, 1) })
	defer k.Stop()

	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&refreshes) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&refreshes); got == 0 {
		t.Fatal("the keeper never refreshed: a 900s access token on an idle TUI expires silently, and " +
			"the operator's next action is what discovers it — the reported 'authentication timeouts " +
			"after some time'")
	}
	// The refreshed credential must actually be installed, or the keeper is decorative.
	if got := c.Session.Token(); got != "new" {
		t.Errorf("token after the keeper fired = %q, want the refreshed %q", got, "new")
	}
	if atomic.LoadInt32(&onRefreshed) == 0 {
		t.Error("onRefreshed did not fire, so live streams would keep using the old credential")
	}
}

// A SHORT TTL MUST NOT BECOME A HOT LOOP.
func TestKeepalivePeriodIsFloored(t *testing.T) {
	var refreshes int32
	c := newKeepaliveClient(&refreshes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A 1s lifetime would otherwise mean a 500ms period.
	k := c.StartKeepalive(ctx, 1*time.Second, nil)
	defer k.Stop()

	time.Sleep(2 * time.Second)
	// In 2s a 500ms period would fire ~4 times; the floor (5s) means zero.
	if got := atomic.LoadInt32(&refreshes); got > 0 {
		t.Errorf("the keeper refreshed %d times in 2s on a 1s lifetime — the period must be floored at "+
			"%s, or a very short TTL turns into a refresh loop", got, keepaliveMinInterval)
	}
}

// NO LIFETIME MEANS NO GUESSING.
func TestKeepaliveIsInertWithoutALifetime(t *testing.T) {
	var refreshes int32
	c := newKeepaliveClient(&refreshes)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, lifetime := range []time.Duration{0, -1 * time.Second} {
		k := c.StartKeepalive(ctx, lifetime, nil)
		k.Stop() // must return promptly: the goroutine was never started
	}
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&refreshes); got != 0 {
		t.Errorf("the keeper refreshed %d times with no known lifetime", got)
	}
}

// AN API-KEY CLIENT HAS NO SESSION, SO THE KEEPER IS A NO-OP — and Stop must still be safe.
func TestKeepaliveOnAnAPIKeyClientIsSafe(t *testing.T) {
	c := &Clients{} // Session == nil: api-key mode
	k := c.StartKeepalive(context.Background(), 900*time.Second, nil)
	k.Stop()
	k.Stop() // idempotent: a caller that always Stops must not panic
}

// STOP ACTUALLY STOPS, so /connect's replacement does not leave a keeper refreshing a dead session.
func TestKeepaliveStopEndsTheGoroutine(t *testing.T) {
	var refreshes int32
	c := newKeepaliveClient(&refreshes)
	k := c.StartKeepalive(context.Background(), 10*time.Second, nil)
	time.Sleep(200 * time.Millisecond)
	k.Stop()

	before := atomic.LoadInt32(&refreshes)
	time.Sleep(6 * time.Second) // longer than the 5s period
	if after := atomic.LoadInt32(&refreshes); after != before {
		t.Errorf("the keeper refreshed %d more time(s) after Stop — a replaced client set's keeper "+
			"must not keep running", after-before)
	}
}
