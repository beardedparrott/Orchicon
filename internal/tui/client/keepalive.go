package client

// keepalive.go — refresh the access token BEFORE it expires, not after a request fails.
//
// THE OPERATOR: "The TUI still has some weird authentication timeouts after some time and /connect does
// not always connect back gracefully causing me to close orch altogether and restart it. This is not
// good. We can't expect users to have to do this. I think gracefully reconnecting behind the scenes or
// some keep alive to always stay connected is a much better approach."
//
// They are right, and the reason the reactive interceptor alone cannot deliver it is structural rather
// than a bug in that code.
//
// THE TOKEN EXPIRES ON A CLOCK; THE INTERCEPTOR ONLY NOTICES ON A FAILURE. refresh.go's interceptor
// recovers from a 401 by minting a new token and retrying — which is exactly right for a request that
// HAPPENS to arrive after expiry. But a TUI is mostly IDLE between actions: the operator reads a
// transcript, thinks, comes back. During that time the open streams are silent (nothing is being
// published), so no request is in flight to receive a 401, and the access token (default TTL 900s)
// quietly ages out.
//
// The FIRST thing the operator then does — send a message, switch screens, run a command — is what
// discovers the expiry. If that first request is a UNARY it recovers transparently. If it is the CHAT
// TURN's own long-lived session, the failure surfaces mid-turn as the operator's "weird authentication
// timeouts after some time", and the turn is lost rather than retried.
//
// SO THE FIX IS TO NOT LET IT EXPIRE. A keeper refreshes on a timer at a FRACTION of the token's own
// lifetime, so the credential is always young when it is used and the 401 path stays what it should be:
// a rare safety net for a clock skew or a revoked token, not the ordinary way a session survives.
//
// IT IS A FRACTION, NOT A FIXED INTERVAL, because the TTL is a tenant SETTING (session_access_token_ttl_
// seconds, validated 30s..86400s). A fixed "every 5 minutes" would refresh once per token on a 900s TTL
// and eleven times over on a 30s one — the first is fine and the second is a busy-loop. Half the
// lifetime, floored at a few seconds, is correct for every value the server accepts.

import (
	"context"
	"sync"
	"time"
)

// keepaliveMinInterval floors the refresh period, so a very short TTL cannot produce a hot loop.
const keepaliveMinInterval = 5 * time.Second

// Keepalive refreshes a session's access token before it expires.
//
// Start it once per client set; Stop it when the set is replaced (a /connect builds a new one). It is
// safe to call Stop more than once.
type Keepalive struct {
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// StartKeepalive begins refreshing the session on a period derived from the token's lifetime.
//
// lifetime is the token TTL the SERVER reported (LoginResponse/RefreshResponse expires_in). A
// non-positive lifetime means "the server did not say", and the keeper does nothing: guessing a period
// from an unknown lifetime is how a client ends up refreshing every request or never.
//
// onRefreshed fires after each SUCCESSFUL refresh, so the shell can re-seat anything holding the old
// credential (SessionClient.OnRefreshed already covers the 401 path; this covers the timer path).
//
// A refresh FAILURE is not fatal to the keeper: the next tick retries, and the reactive interceptor
// still covers any request that goes out in the meantime. A session whose refresh token has genuinely
// expired (24h) will keep failing here, and the first real request then surfaces the re-auth banner —
// which is the correct, visible outcome rather than a silent dead session.
func (c *Clients) StartKeepalive(ctx context.Context, lifetime time.Duration, onRefreshed func()) *Keepalive {
	k := &Keepalive{stop: make(chan struct{}), done: make(chan struct{})}
	sc := c.Session
	if sc == nil || sc.Refresh == nil || lifetime <= 0 {
		// Nothing to keep alive (api-key mode, or no TTL published): close immediately so a caller
		// that always Stops does not leak a goroutine that was never started.
		close(k.done)
		return k
	}

	period := lifetime / 2
	if period < keepaliveMinInterval {
		period = keepaliveMinInterval
	}

	go func() {
		defer close(k.done)
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				tok, err := sc.Refresh(rctx)
				cancel()
				if err != nil || tok == "" {
					continue // the reactive interceptor is still the safety net
				}
				if sc.SetToken != nil {
					sc.SetToken(tok)
				}
				if onRefreshed != nil {
					onRefreshed()
				}
			}
		}
	}()
	return k
}

// Stop ends the keeper and waits for its goroutine to exit, so a caller replacing the client set does
// not leave a goroutine refreshing a dead session's token.
func (k *Keepalive) Stop() {
	if k == nil {
		return
	}
	k.stopOnce.Do(func() { close(k.stop) })
	<-k.done
}
