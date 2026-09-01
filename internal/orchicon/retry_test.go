package orchicon

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- Retry / backoff / resilience (D11) ---------------------------------------

func TestRetryableStatus(t *testing.T) {
	for _, code := range []int{408, 409, 429, 500, 502, 503, 529} {
		if !retryableStatus(code) {
			t.Fatalf("%d must be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 422} {
		if retryableStatus(code) {
			t.Fatalf("%d must NOT be retryable (403 is a transport flip, not a retry)", code)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	d, ok := RetryAfter("3", time.Now())
	if !ok || d != 3*time.Second {
		t.Fatalf("delta-seconds = %v ok=%v", d, ok)
	}
	d, ok = RetryAfter("-1", time.Now())
	if ok {
		t.Fatal("negative must be rejected")
	}
	d, ok = RetryAfter("", time.Now())
	if ok {
		t.Fatal("empty must be rejected")
	}
	future := time.Now().Add(2 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	d, ok = RetryAfter(future, time.Now())
	if !ok || d <= 0 {
		t.Fatalf("http-date = %v ok=%v", d, ok)
	}
}

func TestBackoffExponentialWithJitter(t *testing.T) {
	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 800 * time.Millisecond}
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		d := backoffDelay(p, 3, 0) // 100→200→400→800 capped
		if d < p.BaseDelay || d > p.MaxDelay {
			t.Fatalf("delay %v out of [base,max]", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter must vary the delay")
	}
	// Retry-After hint wins over backoff.
	if d := backoffDelay(p, 2, 5*time.Second); d != 5*time.Second {
		t.Fatalf("retry-after hint ignored: %v", d)
	}
}

func TestDoWithRetriesExhaustion(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, Sleep: func(time.Duration) {}}
	attempts := 0
	err := doWithRetries(context.Background(), p, func(attempt int) (bool, error, time.Duration) {
		attempts++
		return true, errors.New("still down"), 0
	})
	if err == nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	// Non-retryable fails immediately.
	attempts = 0
	err = doWithRetries(context.Background(), p, func(attempt int) (bool, error, time.Duration) {
		attempts++
		return false, errors.New("auth"), 0
	})
	if attempts != 1 || err == nil {
		t.Fatalf("non-retryable must stop: attempts=%d", attempts)
	}
	// Success first try.
	attempts = 0
	err = doWithRetries(context.Background(), p, func(attempt int) (bool, error, time.Duration) {
		attempts++
		return false, nil, 0
	})
	if err != nil || attempts != 1 {
		t.Fatalf("success: attempts=%d err=%v", attempts, err)
	}
}

func TestIsConnectionErr(t *testing.T) {
	for _, s := range []string{"connection refused", "connection reset by peer", "no such host", "i/o timeout", "Client.Timeout exceeded"} {
		if !isConnectionErr(errors.New(s)) {
			t.Fatalf("%q must classify as connection error", s)
		}
	}
	if isConnectionErr(nil) {
		t.Fatal("nil is not a connection error")
	}
	if isConnectionErr(context.Canceled) {
		t.Fatal("cancellation is not a connection error")
	}
}
