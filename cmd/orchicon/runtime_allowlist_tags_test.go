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

// TestAllowlistFollowsTheReleaseWorkflow reads release.yml and checks the allowlist against what is actually
// pushed, so the two cannot drift again. A build that offers an image nobody publishes is a build whose fetch
// button is a guaranteed failure.
func TestAllowlistFollowsTheReleaseWorkflow(t *testing.T) {
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
		// Record the TAG portion of every pushed ref (the part after the final colon), with any
		// ${ORCHICON_VERSION} left as-is: a per-version tag is not what the allowlist names.
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
		if !pushed[tag] {
			t.Errorf("the allowlist names %q but release.yml never pushes the tag %q — published tags are %v",
				ref, tag, keysOf(pushed))
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

// TestAllowlistHonoursAMirror keeps a private registry working: the repository comes from ORCHICON_RUNTIME_IMAGE,
// and only the TAG RULE is shared. Hardcoding ghcr.io here would silently ignore the operator's mirror and hand
// them refs in someone else's namespace.
func TestAllowlistHonoursAMirror(t *testing.T) {
	t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
	t.Setenv("ORCHICON_RUNTIME_IMAGE", "registry.internal:5000/mirror/orchicon-runtime:latest")

	refs := runtimeImagesAllowlist("")
	if len(refs) != 2 {
		t.Fatalf("want 2 variant refs, got %v", refs)
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "registry.internal:5000/mirror/orchicon-runtime:") {
			t.Errorf("ref %q does not follow the mirrored repository — a PORT in the registry host must not be "+
				"mistaken for the tag separator", ref)
		}
	}
	if refs[0] != "registry.internal:5000/mirror/orchicon-runtime:gui" ||
		refs[1] != "registry.internal:5000/mirror/orchicon-runtime:dev" {
		t.Errorf("wrong variant refs for a mirror: %v", refs)
	}
}

// TestAllowlistKeepsAnExplicitCSV is the other arm: an operator who names the images gets exactly what they named.
func TestAllowlistKeepsAnExplicitCSV(t *testing.T) {
	got := runtimeImagesAllowlist("ghcr.io/me/a:1,ghcr.io/me/b:2")
	if len(got) != 2 || got[0] != "ghcr.io/me/a:1" || got[1] != "ghcr.io/me/b:2" {
		t.Fatalf("an explicit allowlist must be passed through untouched: %v", got)
	}
}

// TestAllowlistNonReleaseTagKeepsTheVariantPrefix pins the half that was ALREADY right: a locally-built tag like
// "local" or a version is spelled "<variant>-<tag>", and must not be collapsed.
func TestAllowlistNonReleaseTagKeepsTheVariantPrefix(t *testing.T) {
	t.Setenv("ORCHICON_RUNTIME_IMAGES", "")
	t.Setenv("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:local")
	refs := runtimeImagesAllowlist("")
	want := []string{"ghcr.io/beardedparrott/orchicon-runtime:gui-local", "ghcr.io/beardedparrott/orchicon-runtime:dev-local"}
	if len(refs) != 2 || refs[0] != want[0] || refs[1] != want[1] {
		t.Fatalf("got %v, want %v", refs, want)
	}
}
