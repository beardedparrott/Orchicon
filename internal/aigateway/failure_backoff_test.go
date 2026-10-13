package aigateway

// failure_backoff_test.go — A FAILURE IS A CONDITION, NOT A FACT, SO IT IS NOT CACHED LIKE ONE.
//
// One constant used to govern both, and that was the defect:
//
//	if d.cache != nil && time.Since(d.cached) < d.ttl { ... }        // a FACT — fine to cache
//	if ... d.lastErr != nil && time.Since(d.failedAt) < d.ttl { ... } // a CONDITION — 5 minutes is not
//
// A successful list is worth caching: the catalog changes rarely and re-running the CLI costs a
// subprocess plus a service round trip. A failure is worth only a SHORT guard, whose purpose is to stop
// a spawn storm during a real outage — not to assert anything about the world.
//
// The measured cost of conflating them: opencode's managed service evicts and lazily re-boots a
// location, `opencode models` returns an empty list inside that window, and the resulting blip was
// served for FIVE MINUTES on a cold cache (which every rebuild creates) with no prior list to fall
// back on — while the picker's retry key re-read the same cached error, so it could not retry.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAFailureIsNotServedBeyondItsBackoff(t *testing.T) {
	// THE REGRESSION PIN. A failure older than the backoff must cause a REAL probe, not a re-served
	// error — this is the case that used to hold for five minutes.
	bin, calls := emptyThenModelsStub(t, 0) // a healthy CLI
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	// Simulate a failure that happened just outside the backoff window, with a COLD cache — exactly the
	// state a rebuild leaves behind.
	d.failedAt = time.Now().Add(-(d.failureBackoff + time.Second))
	d.lastErr = fmt.Errorf("opencode listed no models")

	models, err := d.fetchOrCache(context.Background())
	if err != nil {
		t.Fatalf("a failure past its backoff must be retried, not re-served: %v", err)
	}
	if len(models) == 0 {
		t.Error("expected a real probe to return models")
	}
	if n := strings.Count(mustRead(t, calls), "models"); n == 0 {
		t.Error("no probe was made — the cached failure was served instead of retrying")
	}
}

func TestAFailureWithinTheBackoffIsServedWithTheMiss(t *testing.T) {
	// The guard still does its job: a failure seconds old is not re-probed on every call, or an outage
	// would spawn a subprocess per read. What changed is only HOW LONG that holds.
	bin, calls := emptyThenModelsStub(t, 0)
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	d.failedAt = time.Now()
	d.lastErr = fmt.Errorf("boom")

	_, err := d.fetchOrCache(context.Background())
	if err == nil {
		t.Fatal("a failure inside the backoff must be surfaced without re-probing")
	}
	if n := strings.Count(mustRead(t, calls), "models"); n != 0 {
		t.Errorf("a failure inside the backoff spawned %d probe(s), want 0 — the guard exists to "+
			"prevent exactly that", n)
	}
}

func TestASuccessIsStillCachedForItsTTL(t *testing.T) {
	// The OTHER half, and the reason the constants are separate rather than the backoff simply being
	// shortened for everything: a fresh success must NOT re-probe.
	bin, calls := emptyThenModelsStub(t, 0)
	d := NewModelDiscoverer(discardLog(), bin)
	d.SetModelsArgs([]string{"models"})

	// Prime it once.
	if _, err := d.fetchOrCache(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	before := strings.Count(mustRead(t, calls), "models")

	if _, err := d.fetchOrCache(context.Background()); err != nil {
		t.Fatalf("second read: %v", err)
	}
	after := strings.Count(mustRead(t, calls), "models")
	if after != before {
		t.Errorf("a fresh success re-probed (%d -> %d invocations) — successTTL must still cache it",
			before, after)
	}
	if d.successTTL <= d.failureBackoff {
		t.Errorf("successTTL (%s) must be much longer than failureBackoff (%s): one is how long a FACT "+
			"is trusted, the other how long a CONDITION is remembered before retrying",
			d.successTTL, d.failureBackoff)
	}
	if d.failureBackoff > 30*time.Second {
		t.Errorf("failureBackoff is %s — long enough to look permanent to an operator pressing the "+
			"retry key, which is the defect this split fixes", d.failureBackoff)
	}
}
