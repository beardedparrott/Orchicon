package runtime

// pool_reset_test.go — THE PRISTINE RESET AND THE LEASE CONTRACT (AC 1, 2, 5, 6).
//
// The warm pool is a SECURITY guarantee, not an optimisation: on release the
// container is removed and recreated with the identical spec, so nothing from the
// previous run (installed packages, /tmp, opencode sessions/data) crosses the
// boundary. Two things make that real rather than aspirational, and both are
// pinned here:
//
//  1. the predecessor is removed FIRST, and the removal WAITS until the engine
//     has released the name — podman keeps the name reserved for a moment after
//     `rm -f`, and a create that races it fails a reset into a dropped entry;
//  2. the replacement re-enters the clean pool ONLY once its serve answers, so a
//     dud cannot idle there and be handed to the next run.
//
// Everything is asserted through the engine seam (Daemon.engineFn / createFn), so
// the file is hermetic and runs on a host with no container engine installed.
// AC 1's REAL-engine proof lives in pool_pristine_engine_test.go and is gated on
// ORCHICON_RUNTIME_TEST_ENGINE.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetHarness records what the pool asked the engine to do, in order.
type resetHarness struct {
	mu  sync.Mutex
	evs []string // "rm:<name>" | "inspect-exists:<name>" | "inspect-gone:<name>" | "create:<name>"
	// pendingExists makes `inspect {{.Id}}` report the container PRESENT this
	// many times before reporting it gone (podman's name-release lag).
	pendingExists int
	existsAnswers int
	creates       []CreateRequest

	serveStatus int32 // 200 normally; 500 simulates a dud serve

	srv *httptest.Server
	d   *Daemon
	p   *daemonPool

	// onResetCreate, when set, runs inside the createContainer stub for a RESET
	// create (req.WorkflowID == ""). It is how TestCheckoutWithEmptyPoolCreates-
	// FreshWithoutBlocking holds the reset mid-flight.
	onResetCreate func()
}

func newResetHarness(t *testing.T) *resetHarness {
	t.Helper()
	h := &resetHarness{serveStatus: http.StatusOK}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := atomic.LoadInt32(&h.serveStatus); code != http.StatusOK {
			w.WriteHeader(int(code))
			return
		}
		switch r.URL.Path {
		case "/global/health", "/session":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)

	exe := filepath.Join(t.TempDir(), "orchicon")
	if err := os.WriteFile(exe, []byte("reset-test daemon binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ExePath:   exe,
		ghTokenFn: func() string { return "" },
	}
	d.engineFn = h.engine
	d.createFn = h.create
	d.pool = newDaemonPool(d)
	h.d, h.p = d, d.pool
	return h
}

func (h *resetHarness) engine(args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case len(args) >= 3 && args[0] == "rm" && args[1] == "-f":
		h.evs = append(h.evs, "rm:"+args[2])
		return "", nil
	case len(args) >= 3 && args[0] == "inspect" && args[2] == "{{.Id}}":
		if h.existsAnswers < h.pendingExists {
			h.existsAnswers++
			h.evs = append(h.evs, "inspect-exists:"+args[3])
			return "deadbeefcafe", nil
		}
		h.evs = append(h.evs, "inspect-gone:"+args[3])
		return "", nil
	case len(args) >= 3 && args[0] == "inspect" && args[2] == "{{.State.Running}}":
		return "true", nil
	}
	return "", nil
}

func (h *resetHarness) create(name string, req CreateRequest) (*CreateResponse, error) {
	h.mu.Lock()
	h.evs = append(h.evs, "create:"+name)
	h.creates = append(h.creates, req)
	hook := h.onResetCreate
	h.mu.Unlock()
	if hook != nil && req.WorkflowID == "" {
		hook()
	}
	return &CreateResponse{
		Name: name, Running: true, ServePort: 1, ServePassword: "pw", ServeURL: h.srv.URL,
	}, nil
}

func (h *resetHarness) clearEvents() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evs = nil
}

func (h *resetHarness) engineCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.evs...)
}

func (h *resetHarness) createdReqs() []CreateRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]CreateRequest(nil), h.creates...)
}

func (h *resetHarness) removedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.evs {
		if strings.HasPrefix(e, "rm:") {
			out = append(out, strings.TrimPrefix(e, "rm:"))
		}
	}
	return out
}

// entryFor returns a copy of a tracked pool entry's serve identity ("" when the
// entry is not tracked).
func entryFor(p *daemonPool, name string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent := p.entries[name]
	if ent == nil {
		return nil
	}
	cp := *ent
	return &cp
}

// waitClean waits until at least want containers are clean under key.
func waitClean(t *testing.T, p *daemonPool, key string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if names := poolCleanNamesForKey(p, key); len(names) >= want {
			return names
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d clean container(s) under the key (clean=%v)",
		want, poolCleanNamesForKey(p, key))
	return nil
}

func resetTestReq(workflowID string) CreateRequest {
	return CreateRequest{
		WorkflowID:   workflowID,
		Image:        "orchicon-runtime:local",
		Mounts:       []MountSpec{{Source: "/tmp/reset-proj", Dest: "/tmp/reset-proj"}},
		ServeConfig:  `{"worktreeBase":"/tmp/reset-proj"}`,
		ProjectDir:   "/tmp/reset-proj",
		AdapterKinds: []string{"opencode"},
	}
}

// TestResetRemovesBeforeCreatingFresh is AC 1's hermetic proof: on release the
// predecessor is removed BEFORE a fresh container is created, the replacement
// carries the IDENTICAL create spec from a NEW name (so nothing of the previous
// container's writable state can be reached), and its serve is up before it
// enters the clean pool.
func TestResetRemovesBeforeCreatingFresh(t *testing.T) {
	h := newResetHarness(t)
	req := resetTestReq("run-A")
	key := poolEnvKey(req, h.d.hostInputsFingerprint())

	first, err := h.p.checkout(context.Background(), "run-A", req)
	if err != nil {
		t.Fatalf("checkout run-A: %v", err)
	}
	h.clearEvents()

	h.p.release("run-A")
	names := waitClean(t, h.p, key, 1)
	replacement := names[0]

	if replacement == first.Name {
		t.Fatalf("the reset reused the predecessor's container name %q — a kept name means a kept "+
			"writable layer (packages, /tmp, sessions) unless the engine provably recreated it; the reset "+
			"must come from a fresh name", replacement)
	}

	evs := h.engineCalls()
	rmIdx, createIdx := -1, -1
	for i, e := range evs {
		if e == "rm:"+first.Name && rmIdx < 0 {
			rmIdx = i
		}
		if e == "create:"+replacement {
			createIdx = i
			break
		}
	}
	if rmIdx < 0 {
		t.Fatalf("the released container %q was never removed (calls=%v)", first.Name, evs)
	}
	if createIdx < 0 {
		t.Fatalf("no create for the replacement %q (calls=%v)", replacement, evs)
	}
	if createIdx < rmIdx {
		t.Fatalf("the replacement was created BEFORE the predecessor was removed — a reset that creates "+
			"beside the old container leaks its state (calls=%v)", evs)
	}

	// The replacement must be created from the SAME spec, or "pristine" would
	// silently mean "different environment".
	reqs := h.createdReqs()
	if len(reqs) == 0 {
		t.Fatalf("no create request recorded")
	}
	got := reqs[len(reqs)-1]
	if got.WorkflowID != "" {
		t.Fatalf("a reset create must not carry a workflow id (it is unleased), got %q", got.WorkflowID)
	}
	if got.Image != req.Image {
		t.Fatalf("reset changed the image: %q != %q", got.Image, req.Image)
	}
	if got.ServeConfig != req.ServeConfig || got.ProjectDir != req.ProjectDir {
		t.Fatalf("reset changed the serve config/project dir")
	}
	if !reflect.DeepEqual(got.Mounts, req.Mounts) {
		t.Fatalf("reset changed the mount set: %+v != %+v", got.Mounts, req.Mounts)
	}
	if !reflect.DeepEqual(got.AdapterKinds, req.AdapterKinds) {
		t.Fatalf("reset changed the boot profile: %v != %v", got.AdapterKinds, req.AdapterKinds)
	}

	// Serve re-warmed before the entry entered the pool.
	ent := entryFor(h.p, replacement)
	if ent == nil {
		t.Fatalf("the replacement %q is clean but not tracked", replacement)
	}
	if ent.serveURL != h.srv.URL || ent.servePassword != "pw" {
		t.Fatalf("the pooled replacement carries serve %q/%q, want the re-warmed %q/pw",
			ent.serveURL, ent.servePassword, h.srv.URL)
	}
}

// TestResetWaitsForNameRelease pins D-4's bounded wait: podman can keep the name
// reserved for a moment after `rm -f`, so the reset must poll until the engine
// reports the container gone BEFORE it creates.
func TestResetWaitsForNameRelease(t *testing.T) {
	h := newResetHarness(t)
	h.pendingExists = 2 // the engine reports the name still present twice
	req := resetTestReq("run-A")
	key := poolEnvKey(req, h.d.hostInputsFingerprint())

	first, err := h.p.checkout(context.Background(), "run-A", req)
	if err != nil {
		t.Fatalf("checkout run-A: %v", err)
	}
	h.clearEvents()
	h.p.release("run-A")
	waitClean(t, h.p, key, 1)

	evs := h.engineCalls()
	existsBefore, createIdx := 0, -1
	for i, e := range evs {
		if strings.HasPrefix(e, "create:") && createIdx < 0 {
			createIdx = i
			break
		}
		if strings.HasPrefix(e, "inspect-exists:") {
			existsBefore++
		}
	}
	if createIdx < 0 {
		t.Fatalf("the reset never created a replacement (calls=%v)", evs)
	}
	if existsBefore < 2 {
		t.Fatalf("the reset created while the engine still reported %q present (%d polls before the "+
			"create, want >=2) — a create racing a lingering name fails the reset on podman (calls=%v)",
			first.Name, existsBefore, evs)
	}
	for _, e := range evs[:createIdx] {
		if e == "create:"+first.Name {
			t.Fatalf("the replacement reused the predecessor's name while it was still present (calls=%v)", evs)
		}
	}
}

// TestResetDropsEntryWhenServeNotUsable pins D-6: a reset container whose serve
// does not answer never enters the clean pool. Without the probe it would idle
// there for the whole idle window and hand the next run a cold container.
func TestResetDropsEntryWhenServeNotUsable(t *testing.T) {
	h := newResetHarness(t)
	req := resetTestReq("run-A")
	key := poolEnvKey(req, h.d.hostInputsFingerprint())

	if _, err := h.p.checkout(context.Background(), "run-A", req); err != nil {
		t.Fatalf("checkout run-A: %v", err)
	}
	atomic.StoreInt32(&h.serveStatus, http.StatusInternalServerError)
	h.clearEvents()
	h.p.release("run-A")

	// The reset removes the predecessor, creates, probes, then removes the dud.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.removedNames()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	removed := h.removedNames()
	if len(removed) < 2 {
		t.Fatalf("a non-serving reset container was kept (removals=%v) — a dud must be dropped, not "+
			"left in the pool", removed)
	}
	if names := poolCleanNamesForKey(h.p, key); len(names) != 0 {
		t.Fatalf("a non-serving container entered the clean pool: %v", names)
	}
	if poolCleanNames(h.p) != nil && len(poolCleanNames(h.p)) != 0 {
		t.Fatalf("the pool is not empty after a failed reset: %v", poolCleanNames(h.p))
	}
}

// TestResetAtCapRemovesTheResetContainer pins ORCHICON_RUNTIME_POOL_CAP (AC 5):
// at the per-env cap the freshly-reset container is dropped, never accumulated.
func TestResetAtCapRemovesTheResetContainer(t *testing.T) {
	t.Setenv("ORCHICON_RUNTIME_POOL_CAP", "1")
	h := newResetHarness(t)
	key := poolEnvKey(resetTestReq("run-A"), h.d.hostInputsFingerprint())

	a, err := h.p.checkout(context.Background(), "run-A", resetTestReq("run-A"))
	if err != nil {
		t.Fatalf("checkout run-A: %v", err)
	}
	b, err := h.p.checkout(context.Background(), "run-B", resetTestReq("run-B"))
	if err != nil {
		t.Fatalf("checkout run-B: %v", err)
	}
	if a.Name == b.Name {
		t.Fatalf("two concurrent runs were handed ONE container %q (AC 6)", a.Name)
	}

	h.p.release("run-A")
	waitClean(t, h.p, key, 1)
	kept := poolCleanNamesForKey(h.p, key)[0]

	h.p.release("run-B")
	deadline := time.Now().Add(12 * time.Second)
	var atCap string
	for time.Now().Before(deadline) {
		for _, n := range h.removedNames() {
			if n != a.Name && n != b.Name {
				atCap = n
			}
		}
		if atCap != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atCap == "" {
		t.Fatalf("at the per-env cap the reset container was NOT dropped (removals=%v)", h.removedNames())
	}
	names := poolCleanNamesForKey(h.p, key)
	if len(names) != 1 || names[0] != kept {
		t.Fatalf("cap=1 must keep exactly the already-clean container %q; clean=%v", kept, names)
	}
}

// TestConcurrentCheckoutsSameEnvGetDistinctContainers is AC 6: two runs of the
// same environment never share a container, and each lease points at its own.
func TestConcurrentCheckoutsSameEnvGetDistinctContainers(t *testing.T) {
	h := newResetHarness(t)

	var wg sync.WaitGroup
	names := make([]string, 2)
	errs := make([]error, 2)
	runIDs := []string{"run-1", "run-2"}
	for i, runID := range runIDs {
		wg.Add(1)
		go func(i int, runID string) {
			defer wg.Done()
			resp, err := h.p.checkout(context.Background(), runID, resetTestReq(runID))
			if err != nil {
				errs[i] = err
				return
			}
			names[i] = resp.Name
		}(i, runID)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s checkout: %v", runIDs[i], err)
		}
		if names[i] == "" {
			t.Fatalf("%s got no container", runIDs[i])
		}
	}
	if names[0] == names[1] {
		t.Fatalf("two runs of one environment shared the container %q — a run boundary is per run (AC 6)", names[0])
	}
	for i, runID := range runIDs {
		if got := h.p.containerForRun(runID); got != names[i] {
			t.Fatalf("%s is leased %q but was handed %q", runID, got, names[i])
		}
	}
}

// TestCheckoutWithEmptyPoolCreatesFreshWithoutBlocking is AC 2: dispatch never
// blocks on a reset. `release` returns immediately (the reset is off the
// dispatch path), and a checkout that finds the pool empty creates fresh while
// that reset is still in flight.
func TestCheckoutWithEmptyPoolCreatesFreshWithoutBlocking(t *testing.T) {
	h := newResetHarness(t)
	req := resetTestReq("run-A")
	key := poolEnvKey(req, h.d.hostInputsFingerprint())

	resetStarted := make(chan struct{})
	unblock := make(chan struct{})
	var started sync.Once
	h.onResetCreate = func() {
		started.Do(func() { close(resetStarted) })
		<-unblock // hold the reset mid-create
	}

	if _, err := h.p.checkout(context.Background(), "run-A", req); err != nil {
		t.Fatalf("checkout run-A: %v", err)
	}
	start := time.Now()
	h.p.release("run-A")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("release blocked for %v — the reset must run off the dispatch path", elapsed)
	}

	select {
	case <-resetStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("the reset never started")
	}
	// The reset is now parked inside createContainer and the pool is empty.
	if names := poolCleanNamesForKey(h.p, key); len(names) != 0 {
		t.Fatalf("the pool should be empty while the reset is in flight, got %v", names)
	}

	type result struct {
		resp *CreateResponse
		err  error
	}
	fresh := make(chan result, 1)
	go func() {
		resp, err := h.p.checkout(context.Background(), "run-B", resetTestReq("run-B"))
		fresh <- result{resp, err}
	}()
	select {
	case got := <-fresh:
		if got.err != nil {
			t.Fatalf("a checkout that found the pool empty failed: %v", got.err)
		}
		if got.resp.Name == "" {
			t.Fatalf("a checkout that found the pool empty got no container")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a checkout blocked on the in-flight reset — dispatch must never wait for a reset (AC 2)")
	}

	close(unblock)
	waitClean(t, h.p, key, 1)
}

// TestPoolWindowOverridesHonourEnv pins the two window knobs AC 5 names that no
// other test asserted: the idle window (ORCHICON_RUNTIME_POOL_IDLE) and the cap
// (ORCHICON_RUNTIME_POOL_CAP), with their documented defaults.
func TestPoolWindowOverridesHonourEnv(t *testing.T) {
	d := &Daemon{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p := newDaemonPool(d)

	if got := p.idleWindow(); got != 10*time.Minute {
		t.Fatalf("default idle window = %v, want 10m", got)
	}
	if got := p.poolCap(); got != 1 {
		t.Fatalf("default pool cap = %d, want 1", got)
	}
	if got := p.leaseMaxAge(); got != 30*time.Minute {
		t.Fatalf("default lease max age = %v, want 30m", got)
	}

	t.Setenv("ORCHICON_RUNTIME_POOL_IDLE", "3m")
	t.Setenv("ORCHICON_RUNTIME_POOL_CAP", "4")
	t.Setenv("ORCHICON_RUNTIME_LEASE_MAX", "2h")

	if got := p.idleWindow(); got != 3*time.Minute {
		t.Fatalf("ORCHICON_RUNTIME_POOL_IDLE ignored: idle window = %v, want 3m", got)
	}
	if got := p.poolCap(); got != 4 {
		t.Fatalf("ORCHICON_RUNTIME_POOL_CAP ignored: cap = %d, want 4", got)
	}
	if got := p.leaseMaxAge(); got != 2*time.Hour {
		t.Fatalf("ORCHICON_RUNTIME_LEASE_MAX ignored: max age = %v, want 2h", got)
	}

	// An invalid value falls back to the default rather than to zero (a zero
	// idle window would reap every warm container on the first tick).
	t.Setenv("ORCHICON_RUNTIME_POOL_IDLE", "not-a-duration")
	t.Setenv("ORCHICON_RUNTIME_POOL_CAP", "not-an-int")
	if got := p.idleWindow(); got != 10*time.Minute {
		t.Fatalf("invalid ORCHICON_RUNTIME_POOL_IDLE must fall back to 10m, got %v", got)
	}
	if got := p.poolCap(); got != 1 {
		t.Fatalf("invalid ORCHICON_RUNTIME_POOL_CAP must fall back to 1, got %d", got)
	}
}
