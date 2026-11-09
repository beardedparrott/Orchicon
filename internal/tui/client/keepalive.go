package client

// keepalive.go — refresh the access token BEFORE it expires, not after a request fails. THE TOKEN
// EXPIRES ON A CLOCK; THE INTERCEPTOR ONLY NOTICES ON A FAILURE.

import (
	"context"
	"sync"
	"time"
)

// keepaliveMinInterval floors the refresh period, so a very short TTL cannot produce a hot loop.
const keepaliveMinInterval = 5 * time.Second

// Keepalive refreshes a session's access token before it expires.
type Keepalive struct {
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// StartKeepalive begins refreshing the session on a period derived from the token's lifetime.
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
