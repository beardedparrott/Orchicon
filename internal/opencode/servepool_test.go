package opencode

import (
	"context"
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

// TestHostServePoolDisjointSetsGetDistinctServesAndDataDirs proves AC 6:
// two disjoint resolved sets map to two DISTINCT serves, each with its OWN
// data dir (two serve processes must never share one sqlite dir), and the
// empty set maps to the default serve.
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

// TestHostServePoolCapRefusesLoudly proves the cap: a NEW key beyond the cap
// is refused with an error NAMING the set, and a LIVE serve is never evicted.
func TestHostServePoolCapRefusesLoudly(t *testing.T) {
	t.Setenv("ORCHICON_HOST_SERVE_POOL_CAP", "2")
	p := NewHostServePool(slog.Default(), t.TempDir(), "", ProfileWorker)
	ctx := context.Background()
	if _, err := p.ServeFor(ctx, testResolution("one")); err != nil {
		t.Fatalf("one: %v", err)
	}
	if _, err := p.ServeFor(ctx, testResolution("two")); err != nil {
		t.Fatalf("two: %v", err)
	}
	_, err := p.ServeFor(ctx, testResolution("three"))
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
	if _, err := p.ServeFor(ctx, testResolution("one")); err != nil {
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

// TestHostServePoolInteractiveProfileKeepsItsOwnServes proves the profile is
// part of the key AND the serve: an Ask (interactive) pool never builds a
// worker-profile serve, so Ask's config can never ride a worker serve.
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
