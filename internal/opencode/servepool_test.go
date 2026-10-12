package opencode

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

func testResolution(id string) mcpclient.Resolution {
	return mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: id, Command: []string{"/bin/" + id}}},
		},
	}
}

// TestHostServePoolDisjointSetsGetDistinctServesAndDataDirs proves AC 6:
// two disjoint resolved sets map to two DISTINCT serves, each with its OWN
// data dir (two serve processes must never share one sqlite dir), and the
// empty set maps to the default serve.
func TestHostServePoolDisjointSetsGetDistinctServesAndDataDirs(t *testing.T) {
	base := t.TempDir()
	p := NewHostServePool(slog.Default(), base, "", ProfileWorker)

	def, err := p.ServeFor(context.Background(), mcpclient.Resolution{}, nil)
	if err != nil {
		t.Fatalf("empty set: %v", err)
	}
	a, err := p.ServeFor(context.Background(), testResolution("alpha"), nil)
	if err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	b, err := p.ServeFor(context.Background(), testResolution("beta"), nil)
	if err != nil {
		t.Fatalf("set beta: %v", err)
	}
	if def == a || def == b || a == b {
		t.Fatalf("disjoint sets must not share a serve (def=%p a=%p b=%p)", def, a, b)
	}
	if a.dataDir == b.dataDir {
		t.Fatalf("two pooled serves share a data dir %q — a sqlite dir must never be shared", a.dataDir)
	}
	if a.dataDir == def.dataDir {
		t.Fatalf("a pooled serve must not share the default data dir %q", def.dataDir)
	}
	if !strings.HasPrefix(a.dataDir, base) {
		t.Fatalf("pooled data dir %q is not under the base %q", a.dataDir, base)
	}
	if filepath.Dir(a.dataDir) != base {
		t.Fatalf("pooled data dir %q is not a direct child of the base %q", a.dataDir, base)
	}
	if p.Size() != 2 {
		t.Fatalf("expected 2 pooled serves, got %d", p.Size())
	}
}

// TestHostServePoolSameSetReusesServe proves the key is stable: the same
// resolved set (even in a different ORDER) reuses one serve, so a live serve
// is not duplicated per request.
func TestHostServePoolSameSetReusesServe(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	one := mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
		{Spec: mcpclient.ServerSpec{ID: "a"}}, {Spec: mcpclient.ServerSpec{ID: "b"}},
	}}
	two := mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
		{Spec: mcpclient.ServerSpec{ID: "b"}}, {Spec: mcpclient.ServerSpec{ID: "a"}},
	}}
	first, err := p.ServeFor(ctx, one, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := p.ServeFor(ctx, two, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("the same set in a different order must share one serve")
	}
	if p.Size() != 1 {
		t.Fatalf("expected 1 pooled serve, got %d", p.Size())
	}
}

// TestHostServePoolCredentialsDistinguishServes proves two sets with the SAME
// server ids but DIFFERENT resolved credentials land on DIFFERENT serves —
// so a project's token is never shared with another project's serve.
func TestHostServePoolCredentialsDistinguishServes(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	mk := func(tok string) mcpclient.Resolution {
		return mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "shared", URL: "https://x", Headers: map[string]string{"Authorization": tok}}},
		}}
	}
	a, err := p.ServeFor(ctx, mk("Bearer token-A"), nil)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := p.ServeFor(ctx, mk("Bearer token-B"), nil)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a == b {
		t.Fatalf("two different credentials for the same server id must not share a serve")
	}
}

// TestHostServePoolCapRefusesLoudly proves the cap: a NEW key beyond the cap
// is refused with an error NAMING the set, and a LIVE serve is never evicted.
func TestHostServePoolCapRefusesLoudly(t *testing.T) {
	t.Setenv("ORCHICON_HOST_SERVE_POOL_CAP", "2")
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	if _, err := p.ServeFor(ctx, testResolution("one"), nil); err != nil {
		t.Fatalf("one: %v", err)
	}
	if _, err := p.ServeFor(ctx, testResolution("two"), nil); err != nil {
		t.Fatalf("two: %v", err)
	}
	_, err := p.ServeFor(ctx, testResolution("three"), nil)
	if err == nil {
		t.Fatalf("expected a loud refusal beyond the cap")
	}
	if !strings.Contains(err.Error(), "three") {
		t.Fatalf("the refusal must NAME the set that could not be served: %v", err)
	}
	// The existing serves are untouched (no eviction mid-session).
	if p.Size() != 2 {
		t.Fatalf("expected the live serves to survive, got %d", p.Size())
	}
	// An ALREADY-KEYED set still resolves at the cap.
	if _, err := p.ServeFor(ctx, testResolution("one"), nil); err != nil {
		t.Fatalf("an existing key must still resolve at the cap: %v", err)
	}
}

// TestHostServePoolAdoptsDefaultServe proves the empty set reuses the
// ALREADY-CONSTRUCTED default serve the plane holds — not a second manager
// over the same data dir.
func TestHostServePoolAdoptsDefaultServe(t *testing.T) {
	base := t.TempDir()
	def := NewHostServe(slog.Default(), base, "")
	p := NewHostServePool(slog.Default(), base, "", ProfileWorker)
	p.SetDefaultServe(def)
	got, err := p.ServeFor(context.Background(), mcpclient.Resolution{}, nil)
	if err != nil {
		t.Fatalf("empty set: %v", err)
	}
	if got != def {
		t.Fatalf("the empty set must map to the adopted default serve")
	}
	if p.DefaultServe() != def {
		t.Fatalf("DefaultServe must return the adopted instance")
	}
}

// TestHostServePoolInteractiveProfileKeepsItsOwnServes proves the profile is
// part of the key AND the serve: an Ask (interactive) pool never builds a
// worker-profile serve, so Ask's config can never ride a worker serve.
func TestHostServePoolInteractiveProfileKeepsItsOwnServes(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileInteractive)
	hs, err := p.ServeFor(context.Background(), testResolution("alpha"), nil)
	if err != nil {
		t.Fatalf("interactive set: %v", err)
	}
	if hs.PermissionProfile() != ProfileInteractive {
		t.Fatalf("interactive pool built a %q serve", hs.PermissionProfile())
	}
	// The same set in the worker profile keys DIFFERENTLY (a distinct serve).
	worker := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	if hostServeSetKey(ProfileInteractive, testResolution("alpha"), nil) ==
		hostServeSetKey(ProfileWorker, testResolution("alpha"), nil) {
		t.Fatalf("the permission profile must be part of the pool key")
	}
	_ = worker
}

// TestHostServePoolKeysOnTheWorkerSet is step 5's correctness property, not a
// performance one: a host serve registers one agent per step worker, so a serve
// built WITHOUT a worker's agent cannot serve that worker's session — the session
// would fall back to the default agent and lose its prompt. The pair (resolved MCP
// set, worker set) is therefore the serve's identity.
func TestHostServePoolKeysOnTheWorkerSet(t *testing.T) {
	ctx := context.Background()
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	set := testResolution("alpha")
	wa := []adapter.WorkerAgent{{Name: "orchicon-worker-a", Prompt: "prompt A"}}
	wb := []adapter.WorkerAgent{{Name: "orchicon-worker-b", Prompt: "prompt B"}}

	a, err := p.ServeFor(ctx, set, wa)
	if err != nil {
		t.Fatalf("ServeFor(worker a): %v", err)
	}
	b, err := p.ServeFor(ctx, set, wb)
	if err != nil {
		t.Fatalf("ServeFor(worker b): %v", err)
	}
	if a == b {
		t.Fatal("two DIFFERENT workers on one MCP set share a serve — the second session would run without its agent")
	}

	// The SAME worker set is reused, or every execution would start a process.
	a2, err := p.ServeFor(ctx, set, wa)
	if err != nil {
		t.Fatalf("ServeFor(worker a again): %v", err)
	}
	if a2 != a {
		t.Error("an identical (set, worker) pair must reuse its serve")
	}

	// A byte-identical agent under a different SLICE is the same identity: the key
	// hashes content, not slice identity.
	a3, err := p.ServeFor(ctx, set, []adapter.WorkerAgent{{Name: "orchicon-worker-a", Prompt: "prompt A"}})
	if err != nil {
		t.Fatalf("ServeFor(worker a copy): %v", err)
	}
	if a3 != a {
		t.Error("the key must hash agent CONTENT, so an equal agent set reuses the serve")
	}
}

// TestHostServePoolWorkerOnlySetIsItsOwnServe pins the behaviour change that worker
// keying introduces, because it has an operator-visible consequence: a session with
// NO project MCP servers but WITH a worker agent can no longer use the shared
// default serve (that serve carries no agents), so it gets its own pooled serve.
//
// That is deliberate — correctness over topology — and it is why the pool cap now
// also bounds distinct workers. A plane that exceeds it refuses LOUDLY rather than
// serving a session without its prompt.
func TestHostServePoolWorkerOnlySetIsItsOwnServe(t *testing.T) {
	ctx := context.Background()
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)

	bare, err := p.ServeFor(ctx, mcpclient.Resolution{}, nil)
	if err != nil {
		t.Fatalf("ServeFor(empty, no workers): %v", err)
	}
	if bare != p.DefaultServe() {
		t.Error("the empty set with NO workers must still be the shared default serve — an unconfigured plane keeps today's topology")
	}

	withWorker, err := p.ServeFor(ctx, mcpclient.Resolution{}, []adapter.WorkerAgent{{Name: "orchicon-worker-a", Prompt: "p"}})
	if err != nil {
		t.Fatalf("ServeFor(empty, one worker): %v", err)
	}
	if withWorker == bare {
		t.Error("a worker needs its own serve even with no MCP servers: the default serve carries no agents")
	}
}

// TestHostServeConfigCarriesItsWorkerAgents pins the wiring from the pool's worker
// set into the document the serve actually bakes: pool keying is pointless if the
// agents never reach the config.
func TestHostServeConfigCarriesItsWorkerAgents(t *testing.T) {
	withShape(t, ConfigShapeV2, func() {
		hs := NewHostServe(slog.Default(), t.TempDir(), "")
		hs.SetWorkers([]adapter.WorkerAgent{{Name: "orchicon-worker-a", Prompt: "THE WORKER PROMPT"}})
		doc := decodeDoc(t, hs.serveConfig())
		agents, ok := doc["agents"].(map[string]any)
		if !ok {
			t.Fatalf("the host serve config carries no agents: %v", doc)
		}
		e, ok := agents["orchicon-worker-a"].(map[string]any)
		if !ok {
			t.Fatalf("the pooled worker agent is missing from %v", agents)
		}
		if sys, _ := e["system"].(string); sys != "THE WORKER PROMPT" {
			t.Errorf("agent system = %q, want the worker's prompt", sys)
		}
	})
}

// TestPoolCapDegradesForWorkersButRefusesForMCPSets pins the asymmetry that the
// two dimensions justify — and it is the fix for a REGRESSION the worker keying
// would otherwise have introduced.
//
// An in-process population (a plane with no runtime daemon, or a local-mode
// project) previously shared served by MCP set. Keying on workers meant the 5th
// distinct worker would hit the cap and FAIL a run that used to work. That failure
// is unnecessary: a serve without the worker's agent only costs the message trim
// (agent selection fails, so the adapter sends the full prompt), whereas a serve
// with the WRONG MCP set would leak one project's servers into another session.
//
// So worker exhaustion must degrade, and MCP-set exhaustion must still refuse.
func TestPoolCapDegradesForWorkersButRefusesForMCPSets(t *testing.T) {
	ctx := context.Background()

	t.Run("a worker that cannot fit degrades to the MCP set's serve", func(t *testing.T) {
		t.Setenv("ORCHICON_HOST_SERVE_POOL_CAP", "2")
		p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
		// Fill the pool with distinct MCP sets — the dimension that must stay fatal.
		for _, s := range []string{"one", "two"} {
			if _, err := p.ServeFor(ctx, testResolution(s), nil); err != nil {
				t.Fatalf("filling the pool: %v", err)
			}
		}
		// Give one of those sets a worker that cannot fit — the pool is at its cap
		// of 2, so this key is new.
		workers := []adapter.WorkerAgent{{Name: "orchicon-worker-x", Prompt: "p"}}
		hs, err := p.ServeFor(ctx, testResolution("one"), workers)
		if err != nil {
			t.Fatalf("a worker that cannot fit must DEGRADE, not fail the run: %v", err)
		}
		if hs == nil {
			t.Fatal("degradation returned no serve at all")
		}
		// It must be the SAME MCP set's serve (no leak), just without agents — and
		// that is exactly what makes the degradation safe, so it is asserted.
		bare, err := p.ServeFor(ctx, testResolution("one"), nil)
		if err != nil {
			t.Fatalf("re-resolving the same set: %v", err)
		}
		if hs != bare {
			t.Error("the degraded serve must be the SAME MCP set's serve — a different set would leak its servers into this session")
		}
	})

	t.Run("a new MCP set that cannot fit still refuses loudly", func(t *testing.T) {
		t.Setenv("ORCHICON_HOST_SERVE_POOL_CAP", "2")
		p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
		for _, s := range []string{"one", "two"} {
			if _, err := p.ServeFor(ctx, testResolution(s), nil); err != nil {
				t.Fatalf("filling the pool: %v", err)
			}
		}
		_, err := p.ServeFor(ctx, testResolution("three"), nil)
		if err == nil {
			t.Fatal("a wrong MCP set is a correctness violation and must refuse, not degrade")
		}
		if !strings.Contains(err.Error(), "at capacity") {
			t.Errorf("the refusal must name the condition: %v", err)
		}
	})
}
