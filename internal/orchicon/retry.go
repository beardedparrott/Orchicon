package orchicon

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// RetryPolicy configures transient-failure retries (D11).
type RetryPolicy struct {
	MaxAttempts int           // default 3 (first try + 2 retries)
	BaseDelay   time.Duration // default 500ms
	MaxDelay    time.Duration // default 8s
	// NoJitter disables the default full-jitter delay variation (D11).
	NoJitter bool
	// Sleep allows tests to intercept delays; nil means a ctx-aware sleep.
	Sleep func(d time.Duration)
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 8 * time.Second
	}
	return p
}

// RetryAfter parses a Retry-After header value (delta-seconds or HTTP-date).
func RetryAfter(h string, now time.Time) (time.Duration, bool) {
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// retryableStatus reports whether an HTTP status is transient per D11
// (408/409/429/5xx). 403 is deliberately absent: it is a transport flip
// signal, not a retry.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

// isConnectionErr reports whether err is a transport-level failure (as
// opposed to a provider response): net errors, timeouts, connection
// resets/refused, TLS handshake failures, client timeouts.
func isConnectionErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if v, ok := err.(interface{ Timeout() bool }); ok && v.Timeout() {
		return true
	}
	s := err.Error()
	for _, marker := range []string{
		"connection refused", "connection reset", "EOF", "broken pipe",
		"no such host", "i/o timeout", "TLS handshake", "proxyconnect",
		"server closed idle connection", "Client.Timeout", "connect:",
	} {
		if containsFoldString(s, marker) {
			return true
		}
	}
	return false
}

func containsFoldString(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && containsFold([]byte(s), []byte(sub))
}

// backoffDelay computes the exponential backoff delay with full jitter for
// attempt n (0-based), honoring an optional Retry-After hint.
func backoffDelay(p RetryPolicy, attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := p.BaseDelay
	for i := 0; i < attempt && d < p.MaxDelay; i++ {
		d <<= 1
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if p.NoJitter || d <= p.BaseDelay {
		return d
	}
	// full jitter: uniform in [base, d]
	return p.BaseDelay + time.Duration(rand.Int63n(int64(d-p.BaseDelay)+1))
}

// doWithRetries runs fn for attempts 0..MaxAttempts-1. fn returns
// (retryable, err, retryAfter): when retryable is true, err != nil and
// attempts remain, sleep backoff (honoring retryAfter) and retry;
// otherwise return the last error. The Retry-After hint from attempt N
// shapes the delay before attempt N+1.
func doWithRetries(ctx context.Context, p RetryPolicy, fn func(attempt int) (retryable bool, err error, retryAfter time.Duration)) error {
	p = p.withDefaults()
	var (
		lastErr        error
		lastRetryAfter time.Duration
	)
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if attempt > 0 {
			sleep(ctx, p, backoffDelay(p, attempt-1, lastRetryAfter))
		}
		retryable, err, ra := fn(attempt)
		if err == nil || !retryable {
			return err
		}
		lastErr, lastRetryAfter = err, ra
	}
	return lastErr
}

// sleep waits d, aborting early when ctx is cancelled; test hook overrides.
func sleep(ctx context.Context, p RetryPolicy, d time.Duration) {
	if p.Sleep != nil {
		p.Sleep(d)
		return
	}
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
