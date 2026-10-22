package client

// keepalive_test.go — a password-mode session must never meet an expiry during an idle stretch.
//
// THE OPERATOR: "The TUI still has some weird authentication timeouts after some time and /connect does
// not always connect back gracefully causing me to close orch altogether and restart it."
//
// The reactive 401 interceptor cannot prevent that on its own, and the reason is structural: it only
// notices an expiry when a request HAPPENS to go out. A TUI is mostly idle between actions, so the
// access token (default TTL 900s) ages out silently while the operator reads a transcript — and the
// first action afterwards is what discovers it. When that action is a chat turn's own long-lived
// session, the turn is lost rather than retried.
//
// These tests pin the two halves of the fix: it refreshes BEFORE the deadline, and it does not invent a
// schedule it was not given.

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
//
// This is the whole point: no 401 is ever involved. A keeper on a short lifetime must mint a new token
// on its own, so the credential is young when the operator finally acts.
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
//
// The TTL is a tenant setting, validated 30s..86400s. "Refresh at half the lifetime" is right for all of
// them, but a pathological small value must still be floored — a busy-loop against /auth/refresh is a
// worse failure than the expiry it prevents.
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
//
// An api-key profile has no expiry, and a plane that did not publish expires_in gives no schedule.
// Inventing a period from an unknown lifetime is how a client ends up refreshing on every request or
// never; both are worse than leaving the reactive interceptor to do its job.
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
//
// /connect builds a WHOLE NEW client set; a keeper left running would keep minting tokens nobody reads
// and keep firing OnRefreshed into a shell that has moved on.
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
