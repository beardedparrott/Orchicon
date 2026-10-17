package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/containerengine"
)

// engineStatusCache serves GET /enginez — whether runtime containers are available on THIS
// host, and why not when they are not.
//
// WHY A PLAIN JSON ENDPOINT NEXT TO /versionz: the UI polls it from the page where execution
// mode is chosen (AC5), so it must be cheap and it must not need a proto change or a
// `buf generate` for one operational status. The status is built by
// internal/containerengine.Inspect — the same package the installer decided with — so the UI
// cannot claim an availability the install did not produce.
//
// THE MEMO IS DELIBERATE. Every probe may exec `podman machine list` and `podman --version`;
// a browser polling once a second must not fork a process a second. 30s is the window; a
// caller that knows better (the page right after an install) passes ?refresh=1.
type engineStatusCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	probe containerengine.Probe
	at    time.Time
	st    containerengine.Status
	set   bool
}

func newEngineStatusCache(ttl time.Duration, p containerengine.Probe) *engineStatusCache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &engineStatusCache{ttl: ttl, probe: p}
}

// status returns the memoised status, or a fresh probe when the memo has expired or refresh
// is requested.
func (c *engineStatusCache) status(ctx context.Context, refresh bool) containerengine.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && c.set && time.Since(c.at) < c.ttl {
		return c.st
	}
	c.st = containerengine.Inspect(ctx, c.probe)
	c.at = time.Now()
	c.set = true
	return c.st
}

func (c *engineStatusCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// A monitoring probe must never cache this for a browser: the whole point is to reflect
	// the current host state (after an install, after a `podman machine start`).
	w.Header().Set("Cache-Control", "no-store")
	st := c.status(r.Context(), r.URL.Query().Get("refresh") != "")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		// The status is already computed; a write error means the client went away.
		return
	}
}
