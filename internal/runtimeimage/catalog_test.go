package runtimeimage

// catalog_test.go — the shipped catalog, and how a fetch decides pull vs build.
//
// Two things here were got wrong before they were got right, and both are pure functions so both are
// pinned here rather than discovered on a host.

import (
	"strings"
	"testing"
)

// TestPublishedStockTagsAreTheRegistrysOwn — the catalog must name tags that ACTUALLY EXIST.
//
// The first version derived them from the variant NAME — ":gui-latest", ":dev-latest" — and all three
// were wrong. Measured against the registry:
//
//	:latest      EXISTS
//	:gui-latest  MISSING
//	:dev-latest  MISSING
//
// so every fetch of a seeded row failed exactly as the operator reported:
//
//	reading manifest dev-latest in ghcr.io/beardedparrott/orchicon-runtime: manifest unknown
//
// release.yml pushes `:latest`, `:gui`, `:dev` (plus per-version tags). Those are the catalog, and
// web-research is NOT in it — it has a shipped Dockerfile but is never built or pushed, so seeding it
// created a row whose only possible action was a guaranteed failure.
func TestPublishedStockTagsAreTheRegistrysOwn(t *testing.T) {
	got := publishedStockTags()
	want := []string{
		"ghcr.io/beardedparrott/orchicon-runtime:latest",
		"ghcr.io/beardedparrott/orchicon-runtime:gui",
		"ghcr.io/beardedparrott/orchicon-runtime:dev",
	}
	if len(got) != len(want) {
		t.Fatalf("publishedStockTags() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %q, want %q — these are the tags release.yml pushes", i, got[i], want[i])
		}
	}
	// NO INVENTED SUFFIXES. "-latest" on a variant produced three tags that do not exist.
	for _, tag := range got {
		if strings.HasSuffix(tag, "-latest") && tag != PublishedStockBase {
			t.Errorf("tag %q carries a -latest suffix; only the BASE is published at :latest", tag)
		}
	}
}

// TestPublishedStockTagsCoverEveryVariant — the catalog must name an artifact for EVERY shipped
// template, or a variant exists as a Dockerfile nobody can fetch.
//
// This is what makes the Runtime Images page list all hosted images on a host that has never had a
// container engine (Windows before WSL/podman): the rows are seeded from these tags, not from what a
// daemon happens to report.
func TestPublishedStockTagsCoverEveryVariant(t *testing.T) {
	tags := publishedStockTags()
	// ONE TAG PER *PUBLISHED* VARIANT — not per shipped variant. web-research has a template and no
	// registry tag, so the catalog is deliberately shorter than stockVariants.
	publishedCount := 0
	for _, v := range stockVariants {
		if _, ok := v.PublishedTag("repo"); ok {
			publishedCount++
		}
	}
	if len(tags) != publishedCount {
		t.Fatalf("publishedStockTags() returned %d tags for %d published variants", len(tags), publishedCount)
	}
	// Every tag must map BACK to a distinct variant.
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		v, ok := resolveVariant(tag, PublishedStockBase)
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
	// EVERY VARIANT THAT IS PUBLISHED must be covered; a variant with no registry tag
	// (web-research today) is deliberately absent rather than seeding an unfetchable row.
	for _, v := range stockVariants {
		_, published := v.PublishedTag("repo")
		if published && !seen[v.Variant] {
			t.Errorf("shipped variant %q has a published tag and is not in the catalog", v.Variant)
		}
		if !published && seen[v.Variant] {
			t.Errorf("variant %q is NOT published, yet the catalog lists it — its rows can only be "+
				"fetched from a registry that does not carry it", v.Variant)
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
