package runtimeimage

// catalog_test.go — the shipped catalog, and how a fetch decides pull vs build.
//
// Two things here were got wrong before they were got right, and both are pure functions so both are
// pinned here rather than discovered on a host.

import "testing"

// TestPublishedStockTagsCoverEveryVariant — the catalog must name an artifact for EVERY shipped
// template, or a variant exists as a Dockerfile nobody can fetch.
//
// This is what makes the Runtime Images page list all hosted images on a host that has never had a
// container engine (Windows before WSL/podman): the rows are seeded from these tags, not from what a
// daemon happens to report.
func TestPublishedStockTagsCoverEveryVariant(t *testing.T) {
	tags := publishedStockTags()
	if len(tags) != len(stockVariants) {
		t.Fatalf("publishedStockTags() returned %d tags for %d shipped variants", len(tags), len(stockVariants))
	}
	// Every tag must map BACK to a distinct variant.
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		v, ok := resolveVariantByTag(tag)
		if !ok {
			t.Errorf("tag %q did not resolve to a variant", tag)
			continue
		}
		if seen[v.Variant] {
			t.Errorf("variant %q is produced by more than one tag — a round trip through the catalog "+
				"must be a bijection, or a row is seeded twice", v.Variant)
		}
		seen[v.Variant] = true
	}
	for _, v := range stockVariants {
		if !seen[v.Variant] {
			t.Errorf("shipped variant %q has no published tag — its template exists and cannot be "+
				"fetched", v.Variant)
		}
	}
	// The BASE is the repository's default tag, with no variant suffix. A ":base-latest" would name
	// an artifact the daemon never publishes.
	if tags[0] != PublishedStockBase {
		t.Errorf("the base tag is %q, want %q — the base IS the repository default", tags[0], PublishedStockBase)
	}
}

// TestIsRegistryRefIsThePullVersusBuildDecision — the reported fetch failure, as a table.
//
// The first fix routed this on the row's SOURCE, which is the wrong axis: a STOCK image is either
// kind. `orchicon-runtime:local` is built by a dev host from a shipped Dockerfile (Source=stock, must
// BUILD), while the released `ghcr.io/...` is the same source and must PULL.
//
// The engine's own rule is what decides it, so the table is written against podman's behaviour: a
// first path component that looks like a host (a dot, a port, or "localhost") is registry-qualified;
// anything else is a short name, which on a fresh podman without registries.conf is precisely the
// string that produced
//
//	short-name "orchicon-runtime:local" did not resolve to an alias
func TestIsRegistryRefIsThePullVersusBuildDecision(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool // true = PULL, false = BUILD
		why  string
	}{
		{"ghcr.io/beardedparrott/orchicon-runtime:latest", true, "a hosted release: download it"},
		{"ghcr.io/beardedparrott/orchicon-runtime:gui-latest", true, "hosted variant"},
		{"docker.io/library/ubuntu:24.04", true, "explicit registry host"},
		{"localhost:5000/x:1", true, "a registry on a port"},
		{"localhost/x:1", true, "localhost is a registry host by definition"},
		{"ghcr.io/org/name@sha256:abc", true, "a digest reference is still registry-qualified"},
		{"orchicon-runtime:local", false, "the reported failure: locally built, never pulled"},
		{"orchicon-runtime:local-gui", false, "locally built variant"},
		{"beardedparrott/orchicon-runtime:latest", false, "a slash but NO host dot is a Docker Hub short name"},
		{"ubuntu:24.04", false, "no path at all"},
		{"", false, "empty is not a reference"},
	} {
		if got := isRegistryRef(tc.tag); got != tc.want {
			t.Errorf("isRegistryRef(%q) = %v, want %v (%s)", tc.tag, got, tc.want, tc.why)
		}
	}
}
