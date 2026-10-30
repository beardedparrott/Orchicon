package runtimeimage

// publish_policy_test.go — WHAT WE PUBLISH AND WHAT WE BUILD MUST AGREE, IN BOTH DIRECTIONS.
//
// The variants carry third-party adapter CLIs (opencode, Claude Code). An image Orchicon builds and
// serves is Orchicon DISTRIBUTING them; built on the operator's machine from the embedded template, the
// operator installs for themselves. So the variants must NOT be published, and the base must be.
//
// Two independent mechanisms enforce that, and this test is the seam between them:
//
//   - release.yml decides what is PUSHED;
//   - PublishedTag/LocalTag decide what a catalogue row points at, and FetchRuntimeImage routes on
//     isRegistryRef (registry ref -> PULL, anything else -> BUILD).
//
// They could drift silently. Publishing a variant while its tag stays local would leave the image
// unreachable; giving a variant a registry ref while unpublishing it would make every fetch a guaranteed
// "manifest unknown" — the exact failure an operator already reported once. This reads the workflow and
// the catalogue and asserts they describe the same world.

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

	// And no UNPUBLISHED variant may be pushed. This is the licensing half: pushing it would make
	// Orchicon a distributor of a third-party CLI.
	//
	// MATCH ON THE PUSHED REF, not on a substring of it. The first version of this looked for
	// `push "orchicon-runtime:dev"` and sailed past the real line
	// `docker push "ghcr.io/beardedparrott/orchicon-runtime:dev"`, because the registry prefix sits
	// between "push" and the name — a guard that cannot see the thing it guards. It now extracts every
	// pushed tag and compares TAG NAMES.
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
