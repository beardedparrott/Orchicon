package runtime

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newLeaseTestPool builds a Daemon + pool with docker/create stubs and a
// live serve stand-in, wired the same way TestPoolCheckoutReuseAndInvalidation
// is, plus a scratch ExePath so the host-inputs fingerprint is deterministic.
// Returns a removedNames func capturing every `docker rm -f` the pool issued.
func newLeaseTestPool(t *testing.T) (*Daemon, *daemonPool, func(runID string) CreateRequest, func() []string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health", "/session":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	exe := filepath.Join(t.TempDir(), "orchicon")
	if err := os.WriteFile(exe, []byte("test daemon binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var removed []string
	d := &Daemon{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ExePath:   exe,
		ghTokenFn: func() string { return "" },
		dockerFn: func(args ...string) (string, error) {
			if len(args) >= 3 && args[0] == "rm" && args[1] == "-f" {
				mu.Lock()
				removed = append(removed, args[2])
				mu.Unlock()
				return "", nil
			}
			// checkout's reuse-path liveness probe (inspect → Running).
			if len(args) >= 3 && args[0] == "inspect" && args[2] == "{{.State.Running}}" {
				return "true", nil
			}
			return "", nil
		},
		createFn: func(name string, req CreateRequest) (*CreateResponse, error) {
			return &CreateResponse{
				Name: name, Running: true,
				ServePort: 1, ServePassword: "pw", ServeURL: srv.URL,
			}, nil
		},
	}
	d.pool = newDaemonPool(d)
	reqFor := func(runID string) CreateRequest {
		return CreateRequest{WorkflowID: runID, Image: "img:v1"}
	}
	removedNames := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), removed...)
	}
	return d, d.pool, reqFor, removedNames
}

// ageLease artificially ages a run's lease entry behind the pool lock.
func ageLease(t *testing.T, p *daemonPool, runID string, d time.Duration) {
	t.Helper()
	p.mu.Lock()
	if ent := p.entries[p.leased[runID]]; ent != nil {
		ent.lastUsed = ent.lastUsed.Add(-d)
	}
	p.mu.Unlock()
}

// TestLeaseRenewalKeepsLiveRun pins the renewal contract: an idempotent
// re-checkout of the same run bumps the lease's lastUsed, so a live run that
// renews every ≤30s (the plane's 30s adopt sweep → EnsureForRun → Create →
// checkout fast path) is NEVER reaped no matter how long the run lasts —
// while a lease that stops renewing is reaped once it ages past leaseMaxAge
// (the aborted-run leak class: dead run, immortal container).
func TestLeaseRenewalKeepsLiveRun(t *testing.T) {
	_, p, reqFor, _ := newLeaseTestPool(t)

	if _, err := p.checkout(context.Background(), "run-1", reqFor("run-1")); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// Age the lease past the default 30m window, then have the "live run"
	// renew. The renewal must refresh lastUsed...
	ageLease(t, p, "run-1", 31*time.Minute)
	if _, err := p.checkout(context.Background(), "run-1", reqFor("run-1")); err != nil {
		t.Fatalf("renewal checkout: %v", err)
	}
	if _, err := p.checkout(context.Background(), "run-1", reqFor("run-1")); err != nil {
		t.Fatalf("renewal checkout: %v", err)
	}
	p.mu.Lock()
	ent := p.entries[p.leased["run-1"]]
	p.mu.Unlock()
	if ent == nil {
		t.Fatal("expected the renewed lease entry to exist")
	}
	if time.Since(ent.lastUsed) > time.Minute {
		t.Fatalf("renewal must bump lastUsed (got %v ago)", time.Since(ent.lastUsed))
	}

	// ...and the renewed lease must survive a reap pass even though it was
	// stale the moment before renewal (idle-before-renew is exactly the
	// pattern the adopt sweep produces on every active run).
	p.reapStaleLeases()
	p.mu.Lock()
	_, stillLeased := p.leased["run-1"]
	name := ""
	if stillLeased {
		name = p.leased["run-1"]
	}
	p.mu.Unlock()
	if !stillLeased {
		t.Fatal("a live run that renews must NEVER be reaped (renewal happens before the reap pass sees it)")
	}
	if name != ent.name {
		t.Fatalf("renewed lease must keep the same container: got %q want %q", name, ent.name)
	}

	// The SAME aging WITHOUT a renewal = the abandoned run: reaped, container
	// removed, mapping gone.
	ageLease(t, p, "run-1", 31*time.Minute)
	p.reapStaleLeases()
	p.mu.Lock()
	_, live := p.leased["run-1"]
	_, entryGone := p.entries[ent.name]
	p.mu.Unlock()
	if live {
		t.Fatal("a lease idle past the window with no renewal must be reaped (abandoned-run leak class)")
	}
	if entryGone {
		t.Fatalf("stale lease's entry %s must be dropped from the inventory", ent.name)
	}
}

// TestStaleLeaseContainerRemoved pins that the stale-lease reap physically
// removes the run's container (not just the mapping).
func TestStaleLeaseContainerRemoved(t *testing.T) {
	_, p, reqFor, removedNames := newLeaseTestPool(t)
	if _, err := p.checkout(context.Background(), "run-1", reqFor("run-1")); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	p.mu.Lock()
	name := p.leased["run-1"]
	p.mu.Unlock()

	ageLease(t, p, "run-1", 31*time.Minute)
	p.reapStaleLeases()

	found := false
	for _, n := range removedNames() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("stale lease's container %s must be docker-rm'd, removed=%v", name, removedNames())
	}
}

// TestDanglingLeaseMappingReaped pins the second leak shape: a lease map
// entry pointing at an already-dropped entry (no container behind it) is
// cleared by the reap pass instead of surviving forever.
func TestDanglingLeaseMappingReaped(t *testing.T) {
	_, p, reqFor, removedNames := newLeaseTestPool(t)
	if _, err := p.checkout(context.Background(), "run-1", reqFor("run-1")); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	p.mu.Lock()
	name := p.leased["run-1"]
	delete(p.entries, name) // container dropped elsewhere; mapping lingers
	p.mu.Unlock()

	p.reapStaleLeases()
	p.mu.Lock()
	_, ok := p.leased["run-1"]
	p.mu.Unlock()
	if ok {
		t.Fatal("a dangling lease mapping must be cleared by reapStaleLeases")
	}
	// The reap still attempts the rm (idempotent docker no-op if absent).
	found := false
	for _, n := range removedNames() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("dangling lease must attempt rm of its container name, removed=%v", removedNames())
	}
}

// TestLeaseMaxAgeOverride pins the env override of the staleness window
// (ORCHICON_RUNTIME_LEASE_MAX) and the default fallback on an invalid value.
func TestLeaseMaxAgeOverride(t *testing.T) {
	_, p, _, _ := newLeaseTestPool(t)
	t.Setenv("ORCHICON_RUNTIME_LEASE_MAX", "2s")
	if got := p.leaseMaxAge(); got != 2*time.Second {
		t.Fatalf("leaseMaxAge = %v, want the 2s override", got)
	}
	t.Setenv("ORCHICON_RUNTIME_LEASE_MAX", "not-a-duration")
	if got := p.leaseMaxAge(); got != 30*time.Minute {
		t.Fatalf("invalid override must fall back to the 30m default, got %v", got)
	}
}