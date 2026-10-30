package main

// runtime_allowlist_tags_test.go — THE DAEMON'S VARIANT TAGS MUST BE TAGS THAT EXIST.
//
// Reported from a real install on Windows. The Runtime Images page offered a fetch of the dev variant, and it
// failed:
//
//	Error: unable to copy from source docker://ghcr.io/beardedparrott/orchicon-runtime:dev-latest:
//	reading manifest dev-latest in ghcr.io/beardedparrott/orchicon-runtime: manifest unknown
//
// runtimeImagesAllowlist built "<repo>:<variant>-<tag>", which for the default tag "latest" gives
// ":dev-latest" and ":gui-latest". release.yml publishes ":dev" and ":gui". So the allowlist named two images
// that have never existed — and because the catalogue seeder reconciles each stock row's TAG to what the daemon
// reports, those names became the tags the operator's rows carried and the fetch targets.
//
// This function had NO TEST, which is why the identical bug had already been fixed twice elsewhere in the tree
// (internal/runtimeimage/seed.go and runtimeVariantRef) while this copy kept producing the broken spelling.

import (
	"os"
	"strings"
	"testing"
)

// TestAllowlistNeverNamesALatestVariantTag is the reported failure, stated directly.
func TestAllowlistNeverNamesALatestVariantTag(t *testing.T) {
	t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
	t.Setenv("ORCHICON_RUNTIME_IMAGE", "") // default: .../orchicon-runtime:latest

	refs := runtimeImagesAllowlist("")
	if len(refs) == 0 {
		t.Fatal("no default variant tags at all — the daemon would offer nothing to fetch")
	}
	for _, ref := range refs {
		if strings.HasSuffix(ref, "-latest") {
			t.Errorf("the allowlist names %q, which NOTHING publishes — a fetch of it fails with "+
				"\"manifest unknown\". release.yml pushes the FLOATING tags :gui and :dev.", ref)
		}
	}
	// And the ones it SHOULD name, it does.
	joined := strings.Join(refs, " ")
	for _, want := range []string{"orchicon-runtime:gui", "orchicon-runtime:dev"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, refs)
		}
	}
}

// TestAllowlistAgreesWithThePublishPolicy is the cross-file contract, INVERTED from what it used to assert.
//
// It used to demand that every tag the allowlist names is one release.yml PUSHES. That was right while the
// variants were published. They are not: the variants carry third-party adapter CLIs, so release.yml pushes the
// base alone and the variants are built on the operator's machine.
//
// The invariant that actually matters is now an agreement between two questions:
//
//	what is PUBLISHED   -> a registry ref, PULLED
//	what is NOT         -> a local name, BUILT
//
// So a tag the allowlist names must either be pushed by release.yml OR be a local name. What must never happen
// is a variant named as a REGISTRY ref while nothing publishes it — that is the "manifest unknown" failure an
// operator already reported.
func TestAllowlistAgreesWithThePublishPolicy(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Skipf("release workflow not readable here: %v", err)
	}
	pushed := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "docker push ") {
			continue
		}
		ref := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "docker push ")), `"`)
		if i := strings.LastIndex(ref, ":"); i > 0 {
			pushed[ref[i+1:]] = true
		}
	}
	if len(pushed) == 0 {
		t.Skip("no docker push lines found — the workflow shape changed, so this check cannot speak")
	}

	t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
	t.Setenv("ORCHICON_RUNTIME_IMAGE", "")
	for _, ref := range runtimeImagesAllowlist("") {
		i := strings.LastIndex(ref, ":")
		if i < 0 {
			t.Fatalf("ref %q has no tag", ref)
		}
		tag := ref[i+1:]
		local := !strings.Contains(ref[:i], "/")

		if local {
			// A LOCAL name is BUILT. It must not also be published under a registry ref, because then the
			// same variant would exist twice and the catalogue would reconcile the row to whichever the
			// daemon happened to report.
			if pushed[tag] {
				t.Errorf("the allowlist names the LOCAL tag %q, but release.yml also pushes a tag %q — one "+
					"variant would exist as both a pull and a build", ref, tag)
			}
			continue
		}
		if !pushed[tag] {
			t.Errorf("the allowlist names %q, a REGISTRY ref, but release.yml never pushes the tag %q — every "+
				"fetch would fail with \"manifest unknown\". Published tags are %v", ref, tag, keysOf(pushed))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAllowlistNamesLocalVariantTags is the routing guarantee, and it reverses an earlier expectation
// deliberately.
//
// The variants used to be published, so the allowlist emitted "<repo>:gui-<tag>". They are not published any
// more — they carry third-party adapter CLIs — so RELEASE publishes the base alone and the variants are BUILT
// on the operator's machine. The allowlist therefore has to name their LOCAL tags.
//
// THIS IS NOT COSMETIC: the catalogue seeder reconciles each stock row's tag to whatever the daemon reports,
// and FetchRuntimeImage routes on isRegistryRef — registry ref means PULL. A registry ref here would make every
// variant row something to pull from a repository that no longer publishes it, i.e. "manifest unknown" again.
func TestAllowlistNamesLocalVariantTags(t *testing.T) {
	for _, image := range []string{
		"",                                                      // the default
		"ghcr.io/beardedparrott/orchicon-runtime:latest",        // explicit
		"registry.internal:5000/mirror/orchicon-runtime:latest", // a mirror with a PORT
	} {
		t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
		t.Setenv("ORCHICON_RUNTIME_IMAGE", image)

		refs := runtimeImagesAllowlist("")
		if len(refs) != 2 {
			t.Fatalf("image %q: want 2 variant refs, got %v", image, refs)
		}
		for _, ref := range refs {
			// A registry ref is "host/path:tag"; a local one is "name:tag". The distinguishing feature is
			// the SLASH before the first colon, which is exactly what internal/runtimeimage's
			// isRegistryRef tests for.
			name := ref[:strings.LastIndex(ref, ":")]
			if strings.Contains(name, "/") {
				t.Errorf("image %q: the allowlist names %q, which is a REGISTRY reference — the fetch would "+
					"PULL it, and nothing publishes the variants any more", image, ref)
			}
		}
		// And the PORT in a mirror must not be mistaken for the tag separator.
		if refs[0] != "orchicon-runtime:gui" || refs[1] != "orchicon-runtime:dev" {
			t.Errorf("image %q: got %v, want the local names [orchicon-runtime:gui orchicon-runtime:dev]",
				image, refs)
		}
	}
}

// TestAllowlistKeepsAnExplicitCSV is the other arm: an operator who names the images gets exactly what they named.
func TestAllowlistKeepsAnExplicitCSV(t *testing.T) {
	got := runtimeImagesAllowlist("ghcr.io/me/a:1,ghcr.io/me/b:2")
	if len(got) != 2 || got[0] != "ghcr.io/me/a:1" || got[1] != "ghcr.io/me/b:2" {
		t.Fatalf("an explicit allowlist must be passed through untouched: %v", got)
	}
}

// TestAllowlistIsTheSameWhateverTheTag is the consequence of the change above: the variant names no longer
// depend on the base's tag at all, because they are local builds rather than release artefacts.
func TestAllowlistIsTheSameWhateverTheTag(t *testing.T) {
	for _, tag := range []string{"latest", "local", "v0.5.44", ""} {
		t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
		t.Setenv("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:"+tag)
		refs := runtimeImagesAllowlist("")
		if len(refs) != 2 || refs[0] != "orchicon-runtime:gui" || refs[1] != "orchicon-runtime:dev" {
			t.Errorf("tag %q: got %v, want [orchicon-runtime:gui orchicon-runtime:dev]", tag, refs)
		}
	}
}
