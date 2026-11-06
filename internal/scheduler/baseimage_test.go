package scheduler

// baseimage_test.go — THE RESOLVE CHAIN'S FALLBACK MUST BE AN IMAGE THE DAEMON
// ACCEPTS.
//
// THE DEFECT THIS PINS. The chain (explicit work-item image -> project default ->
// base) ended at db.BaseRuntimeImage, the literal "orchicon-runtime:base". No host
// has that tag: a release tags the base ghcr.io/beardedparrott/orchicon-runtime:
// latest and a dev host builds it as orchicon-runtime:local. So the last resort for
// any imageless work item was a tag the runtime daemon's allowlist refuses —
// observed on a real run as:
//
//	base "orchicon-runtime:base" -> image not allowed: orchicon-runtime:base
//
// The fallback leg was dead by construction. The tests below make its LIVENESS the
// property: whatever the chain resolves to must be an image the daemon on this host
// would let through, which is the only thing that makes the leg worth having.

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestUseDaemonBaseImageReplacesThePlaceholder is the direct regression: after the
// barrier, the fallback is the daemon's tag rather than the placeholder — and the
// placeholder is precisely the value that used to reach the engine.
func TestUseDaemonBaseImageReplacesThePlaceholder(t *testing.T) {
	const daemonTag = "orchicon-runtime:local" // what a dev host actually builds
	t.Setenv(EnvRuntimeBaseImage, daemonTag)

	got := UseDaemonBaseImage()
	if got != daemonTag {
		t.Fatalf("UseDaemonBaseImage() = %q, want the daemon's configured tag %q", got, daemonTag)
	}
	if got := db.RuntimeBaseImage(); got != daemonTag {
		t.Fatalf("db.RuntimeBaseImage() = %q, want %q — the resolve chain still holds the "+
			"placeholder the daemon refuses", got, daemonTag)
	}
	// The specific string that failed on a real run must no longer be reachable.
	if db.RuntimeBaseImage() == "orchicon-runtime:base" {
		t.Error("the resolve chain still resolves to \"orchicon-runtime:base\", which no host has " +
			"and the daemon rejects with `image not allowed`")
	}
}

// TestFallbackIsNotTheUnusablePlaceholder goes at the SAME property from the other
// side: with nothing configured, the barrier must produce the published default the
// daemon itself defaults to — never the bare placeholder. A default is allowed to be
// a tag this particular host has not pulled yet; it is NOT allowed to be a name that
// cannot exist.
func TestFallbackIsNotTheUnusablePlaceholder(t *testing.T) {
	t.Setenv(EnvRuntimeBaseImage, "")

	if got := UseDaemonBaseImage(); got != DefaultRuntimeBaseImage {
		t.Fatalf("UseDaemonBaseImage() with no env = %q, want the published default %q", got, DefaultRuntimeBaseImage)
	}
	if db.RuntimeBaseImage() == db.BaseRuntimeImage {
		t.Errorf("the fallback stayed at the placeholder %q — the resolve chain's last leg names "+
			"an image the daemon's allowlist refuses", db.BaseRuntimeImage)
	}
	// The published default must be a REGISTRY reference. A bare name:tag is a short
	// name that a minimal host cannot resolve (the same class as the documented
	// `short-name "orchicon-runtime:local" did not resolve to an alias`).
	if !strings.Contains(strings.SplitN(DefaultRuntimeBaseImage, "/", 2)[0], ".") {
		t.Errorf("DefaultRuntimeBaseImage = %q is not registry-qualified (its first path component "+
			"has no dot, so it is a short name a minimal host may not resolve)", DefaultRuntimeBaseImage)
	}
}

// TestImageForRunUsesTheLiveFallback is the choke-point assertion: every leg of the
// chain funnels through imageForRun, so an EMPTY resolution must come back as the
// live fallback. This is the function a new caller would use, and it is where the
// placeholder leaked out.
func TestImageForRunUsesTheLiveFallback(t *testing.T) {
	const daemonTag = "ghcr.io/beardedparrott/orchicon-runtime:latest"
	t.Setenv(EnvRuntimeBaseImage, daemonTag)
	UseDaemonBaseImage()

	if got := imageForRun("", true); got != daemonTag {
		t.Errorf("imageForRun(\"\") = %q, want the live fallback %q", got, daemonTag)
	}
	// An explicit resolution still wins — the fallback is a last resort, not an
	// override.
	if got := imageForRun("orchicon-runtime:orchicon-dev", true); got != "orchicon-runtime:orchicon-dev" {
		t.Errorf("imageForRun(explicit) = %q, want the explicit image", got)
	}
}

// TestResolvedBaseImageIsInTheDaemonAllowlist pins the AGREEMENT that makes the fix
// meaningful rather than cosmetic: the value the resolve chain falls back to must be
// one the daemon's own create-gate would accept.
//
// It mirrors the gate's first two branches (internal/runtime/images.go imageAllowed:
// exactly the configured base, or an operator-allowlisted variant) rather than
// calling them, because the gate also consults the engine for UI-built images and
// this must stay a pure unit test. The property asserted is the one that broke: the
// fallback is EQUAL to the daemon's configured base, so the first branch accepts it
// without any engine call at all.
func TestResolvedBaseImageIsInTheDaemonAllowlist(t *testing.T) {
	// The daemon reads ORCHICON_RUNTIME_IMAGE for its base (cmd/orchicon/runtime.go).
	// Both halves must read the SAME variable or they can disagree.
	if EnvRuntimeBaseImage != "ORCHICON_RUNTIME_IMAGE" {
		t.Fatalf("EnvRuntimeBaseImage = %q, but the runtime daemon reads ORCHICON_RUNTIME_IMAGE — "+
			"the two halves of this agreement would read different variables", EnvRuntimeBaseImage)
	}

	for _, daemonBase := range []string{
		"orchicon-runtime:local",                             // a dev host's build
		"ghcr.io/beardedparrott/orchicon-runtime:latest",      // a release
		"ghcr.io/beardedparrott/orchicon-runtime:dev-latest", // a published variant
	} {
		t.Setenv(EnvRuntimeBaseImage, daemonBase)
		UseDaemonBaseImage()

		fallback := imageForRun("", true) // the leg that used to be dead
		if fallback != daemonBase {
			t.Errorf("fallback = %q, want the daemon's base %q", fallback, daemonBase)
		}
		// imageAllowed's FIRST branch: `name == "" || name == d.Image` -> allowed.
		if fallback != daemonBase {
			t.Errorf("the fallback would not satisfy the daemon's allowlist first branch")
		}
	}
}

// TestBaseBarrierIgnoresAnEmptyOrBlankValue: an empty tag must never be published,
// or the resolve chain would hand the daemon "" and get `no image specified` instead
// of a container — trading one silent failure for another.
func TestBaseBarrierIgnoresAnEmptyOrBlankValue(t *testing.T) {
	t.Setenv(EnvRuntimeBaseImage, "orchicon-runtime:local")
	UseDaemonBaseImage()
	before := db.RuntimeBaseImage()

	db.SetBaseRuntimeImage("")
	db.SetBaseRuntimeImage("   ")
	if got := db.RuntimeBaseImage(); got != before {
		t.Errorf("an empty/blank publish changed the fallback from %q to %q — the fallback must "+
			"never become empty", before, got)
	}
}

// TestDaemonDefaultAndSchedulerDefaultAgree: the scheduler's default must equal the
// daemon's own default, or a split deployment whose env is unset resolves a fallback
// that is not the image the daemon has.
func TestDaemonDefaultAndSchedulerDefaultAgree(t *testing.T) {
	// The daemon default, read from the source that sets it. A literal here would
	// drift the moment that default changes.
	const daemonDefaultMarker = `env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest")`
	src, err := os.ReadFile("../../cmd/orchicon/runtime.go")
	if err != nil {
		t.Fatalf("read the daemon's runtime.go: %v", err)
	}
	if !strings.Contains(string(src), daemonDefaultMarker) {
		t.Errorf("cmd/orchicon/runtime.go no longer defaults the base image to %q — update "+
			"DefaultRuntimeBaseImage in baseimage.go to match, or the plane and the daemon will "+
			"disagree about the fallback", DefaultRuntimeBaseImage)
	}
}
