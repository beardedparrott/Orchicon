package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

func testResolution(id string) mcpclient.Resolution {
	return mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: id, Command: []string{"/bin/" + id}}},
		},
	}
}

// TestHostServePoolDisjointSetsGetDistinctServesAndDataDirs proves AC 6: two disjoint resolved sets
// map to two DISTINCT serves.
func TestHostServePoolDisjointSetsGetDistinctServesAndDataDirs(t *testing.T) {
	base := t.TempDir()
	p := NewHostServePool(slog.Default(), base, "", ProfileWorker)

	def, err := p.ServeFor(context.Background(), mcpclient.Resolution{})
	if err != nil {
		t.Fatalf("empty set: %v", err)
	}
	a, err := p.ServeFor(context.Background(), testResolution("alpha"))
	if err != nil {
		t.Fatalf("set alpha: %v", err)
	}
	b, err := p.ServeFor(context.Background(), testResolution("beta"))
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

// TestHostServePoolSameSetReusesServe proves the key is stable: the same resolved set (even in a
// different ORDER) reuses one serve, so a live serve is not duplicated per request.
func TestHostServePoolSameSetReusesServe(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	one := mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
		{Spec: mcpclient.ServerSpec{ID: "a"}}, {Spec: mcpclient.ServerSpec{ID: "b"}},
	}}
	two := mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
		{Spec: mcpclient.ServerSpec{ID: "b"}}, {Spec: mcpclient.ServerSpec{ID: "a"}},
	}}
	first, err := p.ServeFor(ctx, one)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := p.ServeFor(ctx, two)
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

// TestHostServePoolCredentialsDistinguishServes proves two sets with the SAME server ids but
// DIFFERENT resolved credentials land on DIFFERENT serves.
func TestHostServePoolCredentialsDistinguishServes(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	mk := func(tok string) mcpclient.Resolution {
		return mcpclient.Resolution{Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "shared", URL: "https://x", Headers: map[string]string{"Authorization": tok}}},
		}}
	}
	a, err := p.ServeFor(ctx, mk("Bearer token-A"))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := p.ServeFor(ctx, mk("Bearer token-B"))
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a == b {
		t.Fatalf("two different credentials for the same server id must not share a serve")
	}
}

// TestHostServePoolAdoptsDefaultServe proves the empty set reuses the ALREADY-CONSTRUCTED default
// serve the plane holds — not a second manager over the same data dir.
func TestHostServePoolAdoptsDefaultServe(t *testing.T) {
	base := t.TempDir()
	def := NewHostServe(slog.Default(), base, "")
	p := NewHostServePool(slog.Default(), base, "", ProfileWorker)
	p.SetDefaultServe(def)
	got, err := p.ServeFor(context.Background(), mcpclient.Resolution{})
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

// TestHostServePoolInteractiveProfileKeepsItsOwnServes proves the profile is part of the key AND
// the serve: an Ask (interactive) pool never builds a worker-profile serve.
func TestHostServePoolInteractiveProfileKeepsItsOwnServes(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileInteractive)
	hs, err := p.ServeFor(context.Background(), testResolution("alpha"))
	if err != nil {
		t.Fatalf("interactive set: %v", err)
	}
	if hs.PermissionProfile() != ProfileInteractive {
		t.Fatalf("interactive pool built a %q serve", hs.PermissionProfile())
	}
	// The same set in the worker profile keys DIFFERENTLY (a distinct serve).
	worker := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	if hostServeSetKey(ProfileInteractive, testResolution("alpha")) ==
		hostServeSetKey(ProfileWorker, testResolution("alpha")) {
		t.Fatalf("the permission profile must be part of the pool key")
	}
	_ = worker
}

// TestHostServePoolHasNoCap is the assertion for the REMOVED cap, and it exists so a future reader
// does not re-add one without reading why it went.
func TestHostServePoolHasNoCap(t *testing.T) {
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	const sets = 24 // comfortably past the old default of 4, and past the tenant's 17
	for i := 0; i < sets; i++ {
		set := testResolution(fmt.Sprintf("set-%02d", i))
		if _, err := p.ServeFor(ctx, set); err != nil {
			t.Fatalf("set %d of %d was refused — there must be no cap: %v", i+1, sets, err)
		}
	}
	if p.Size() != sets {
		t.Errorf("pool holds %d serves, want one per distinct set (%d)", p.Size(), sets)
	}
}

// TestHostServePoolKeyIsTheSetAlone pins that WORKERS are not part of the key.
func TestHostServePoolKeyIsTheSetAlone(t *testing.T) {
	// The key's signature IS the assertion: it takes no workers, so there is nothing for a caller to
	// vary.
	var _ func(PermissionProfile, mcpclient.Resolution) string = hostServeSetKey
}

// TestHostServePoolKeysOnTheResolvedCredential is the INVARIANT that makes a saved secret reach this
// adapter at all: the pool key hashes the RESOLVED env/header VALUES, so a rotation produces a different
// key, a different serve, and therefore fresh bytes — a pooled serve bakes its config once, at creation.
//
// It is pinned because the opposite is a tempting refactor: serverFingerprint carries plaintext
// credentials, and "trim it to ids so nothing sensitive is hashed" would silently restore the
// stale-credential bug for opencode ask and the local worker population, with no test failing.
func TestHostServePoolKeysOnTheResolvedCredential(t *testing.T) {
	withHeader := func(header string) mcpclient.Resolution {
		return mcpclient.Resolution{
			Servers: []mcpclient.ScopedServer{{Spec: mcpclient.ServerSpec{
				ID:      "pixellab",
				URL:     "https://api.pixellab.ai/mcp",
				Headers: map[string]string{"Authorization": header},
			}}},
			SelectedIDs: []string{"pixellab"},
		}
	}

	ctx := context.Background()
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)

	first, err := p.ServeFor(ctx, withHeader("Bearer first-token"))
	if err != nil {
		t.Fatalf("ServeFor (first): %v", err)
	}
	// The SAME set hands back the SAME serve: no respawn per turn.
	same, err := p.ServeFor(ctx, withHeader("Bearer first-token"))
	if err != nil {
		t.Fatalf("ServeFor (unchanged): %v", err)
	}
	if same != first {
		t.Fatal("an unchanged MCP set produced a different serve — every turn would start a new opencode")
	}
	// THE ROTATION: only the resolved credential changed.
	rotated, err := p.ServeFor(ctx, withHeader("Bearer second-token"))
	if err != nil {
		t.Fatalf("ServeFor (rotated): %v", err)
	}
	if rotated == first {
		t.Fatal("a rotated credential produced the SAME serve — its baked config still carries the old token, " +
			"and it would keep sending it (the stale-credential bug, silently reintroduced if the pool key " +
			"stops hashing resolved values)")
	}
	if rotated.dataDir == first.dataDir {
		t.Fatal("the rotated serve reused the unrotated serve's data dir")
	}
	// A header RENAME changes what is sent, so it changes the key too.
	renamed := mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{{Spec: mcpclient.ServerSpec{
			ID:      "pixellab",
			URL:     "https://api.pixellab.ai/mcp",
			Headers: map[string]string{"X-Api-Key": "Bearer first-token"},
		}}},
		SelectedIDs: []string{"pixellab"},
	}
	if other, err := p.ServeFor(ctx, renamed); err != nil {
		t.Fatalf("ServeFor (renamed): %v", err)
	} else if other == first {
		t.Fatal("renaming the header produced the same serve")
	}
}
