package runtimeimage

// publish_policy_test.go — WHAT WE PUBLISH AND WHAT WE BUILD MUST AGREE, IN BOTH DIRECTIONS.

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseWorkflowPublishesOnlyTheBase(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Skipf("release workflow not readable here: %v", err)
	}
	src := string(b)

	// Every PUBLISHED variant must appear in a `docker push`. Taking the catalogue as the source of
	// truth means the assertion cannot be satisfied by editing the workflow alone.
	repo := "ghcr.io/beardedparrott/orchicon-runtime"
	for _, v := range stockVariants {
		tag, published := v.PublishedTag(repo)
		if !published {
			continue
		}
		tagName := tag[strings.LastIndex(tag, ":")+1:]
		if !strings.Contains(src, "orchicon-runtime:"+tagName) {
			t.Errorf("%s is marked PUBLISHED (tag %q) but release.yml never pushes %q — the catalogue would "+
				"offer a fetch that pulls something nothing published", v.Variant, tag, tagName)
		}
	}

	// And no UNPUBLISHED variant may be pushed.
	pushedTags := map[string]bool{}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "docker push ") {
			continue
		}
		ref := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "docker push ")), `"`)
		if i := strings.LastIndex(ref, ":"); i > 0 {
			pushedTags[ref[i+1:]] = true
		}
	}
	if len(pushedTags) == 0 {
		t.Fatal("found no `docker push` lines — the workflow shape changed and this guard can no longer speak")
	}
	for _, v := range stockVariants {
		if _, published := v.PublishedTag(repo); published {
			continue
		}
		for tagName := range pushedTags {
			if tagName == v.Variant || strings.HasPrefix(tagName, v.Variant+"-") {
				t.Errorf("%s is NOT published, but release.yml pushes the tag %q. That image carries a "+
					"third-party adapter CLI — Orchicon would be distributing it. Build it locally instead "+
					"(LocalTag + isRegistryRef already route a fetch to BUILD).", v.Variant, tagName)
			}
		}
	}
}

// TestNoVariantIsBothPublishedAndPulled guards the routing invariant directly, so a future edit that
// half-publishes a variant fails here rather than on an operator's machine.
func TestNoVariantIsBothPublishedAndPulled(t *testing.T) {
	repo := "ghcr.io/beardedparrott/orchicon-runtime"
	for _, v := range stockVariants {
		tag := v.CatalogTag(repo)
		_, published := v.PublishedTag(repo)
		registry := isRegistryRef(tag)

		switch {
		case published && !registry:
			t.Errorf("%s: published but NOT a registry ref (%q) — the row would be BUILT, and the published "+
				"image would never be used", v.Variant, tag)
		case !published && registry:
			t.Errorf("%s: a registry ref (%q) but NOT published — every fetch would pull something nothing "+
				"publishes (\"manifest unknown\")", v.Variant, tag)
		}
	}
}
