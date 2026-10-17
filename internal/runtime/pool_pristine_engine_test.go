package runtime

// pool_pristine_engine_test.go — AC 1's REAL-ENGINE tier.
//
// pool_reset_test.go proves the reset ORDER hermetically (remove → wait for the
// name → create from a fresh name with the identical spec → serve probe before
// pool entry). What it CANNOT prove is the property the sandbox's premise rests
// on: that a real engine's `rm -f` + recreate really hands back a container with
// nothing carried over — installed packages, /tmp contents, and opencode
// session/data all gone.
//
// That needs a real engine, so this test is GATED on ORCHICON_RUNTIME_TEST_ENGINE
// (podman|docker) and SKIPS with an explicit reason when it is unset — the CI
// runtime container has no engine binary. THE SKIP MUST BE STATED WHEREVER THIS
// SUITE IS REPORTED: a green run that never touched a container is not evidence
// for AC 1, and this file's skip reason says exactly that.
//
//   ORCHICON_RUNTIME_TEST_ENGINE=podman go test ./internal/runtime/ -run TestPristineResetRealEngine -v
//   (optional) ORCHICON_RUNTIME_TEST_IMAGE=<image with /bin/sh + sleep>
//
// If a target engine ever reuses an overlay upper across same-name containers,
// THIS TEST IS WHERE IT SHOWS UP, and it is BF-3(b): a blocking finding for that
// platform, recorded rather than worked around.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// pristineStateProbes are the three classes of state a run leaves behind, each
// expressed as a shell test that must SUCCEED (i.e. the path is absent) in the
// container that run B receives.
var pristineStateProbes = []string{
	"test ! -e /tmp/ac1-marker",                                     // /tmp contents
	"test ! -e /tmp/ac1-installed-package",                          // an "installed package"
	"test ! -e \"$HOME/.local/share/opencode/storage/ac1-session\"", // opencode session/data
}

// pristineStateWriter is the state run A plants, in ONE exec so a partial write
// cannot masquerade as a clean boundary.
const pristineStateWriter = `set -e
echo run-a > /tmp/ac1-marker
mkdir -p /tmp/ac1-pkgdir && echo pkg > /tmp/ac1-installed-package
mkdir -p "$HOME/.local/share/opencode/storage" && echo session > "$HOME/.local/share/opencode/storage/ac1-session"
`

// TestPristineResetRealEngine writes state in run A, releases (the pool's removal
// primitive), receives the recreated container as run B, and asserts every piece
// of run A's state is gone (AC 1).
func TestPristineResetRealEngine(t *testing.T) {
	requested := strings.TrimSpace(os.Getenv("ORCHICON_RUNTIME_TEST_ENGINE"))
	if requested == "" {
		t.Skip("ORCHICON_RUNTIME_TEST_ENGINE is unset: the pristine-reset REAL-ENGINE tier did NOT run. " +
			"AC 1 is proven hermetically by pool_reset_test.go (removal order + name release + fresh name + " +
			"identical spec + serve probe), but the engine-level claim — nothing survives rm -f + recreate — " +
			"requires a host with podman or docker. Run this on a podman host before treating BF-3(b) as settled.")
	}
	eng, err := ResolveEngine(requested)
	if err != nil {
		t.Skipf("engine %q is not usable on this host (%v) — the REAL-ENGINE pristine tier did NOT run", requested, err)
	}
	image := strings.TrimSpace(os.Getenv("ORCHICON_RUNTIME_TEST_IMAGE"))
	if image == "" {
		image = "orchicon-runtime:local"
	}

	d := &Daemon{
		Engine: eng,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if out, err := d.engine("image", "inspect", image); err != nil {
		t.Skipf("image %q is not present for %s (%s) — set ORCHICON_RUNTIME_TEST_IMAGE; the REAL-ENGINE "+
			"pristine tier did NOT run", image, eng.EngineNameForError(), strings.TrimSpace(out))
	}

	name := fmt.Sprintf("orchicon-pristine-%d", time.Now().UnixNano())
	// createRun runs ONE container from the same name + spec — the "identical
	// spec" half of the reset. It returns the argv it used so run A and run B are
	// provably created the same way.
	createRun := func() []string {
		t.Helper()
		args := []string{"run", "-d", "--name", name,
			"--label", "orchicon.workflow=pristine-reset-test",
			image, "sleep", "1800",
		}
		if out, err := d.engine(args...); err != nil {
			d.removeContainer(name)
			t.Skipf("could not start %q with /bin/sh + sleep on %s (%s) — set ORCHICON_RUNTIME_TEST_IMAGE "+
				"to an image that has them; the REAL-ENGINE pristine tier did NOT run",
				image, eng.EngineNameForError(), strings.TrimSpace(out))
		}
		return args
	}
	cleanup := func() { d.removeContainer(name) }
	defer cleanup()

	// RUN A: a real container, real state written into its writable layer.
	runAArgs := createRun()
	if out, err := d.engine("exec", name, "sh", "-c", pristineStateWriter); err != nil {
		t.Fatalf("run A could not write its state (%s): %v", strings.TrimSpace(out), err)
	}
	// Sanity: the state really is there in run A, or the absence in run B would
	// prove nothing.
	for _, probe := range pristineStateProbes {
		present := strings.Replace(probe, "test ! -e ", "test -e ", 1)
		if out, err := d.engine("exec", name, "sh", "-c", present); err != nil {
			t.Fatalf("run A did not write its state (%s failed: %s)", present, strings.TrimSpace(out))
		}
	}

	// RELEASE: the pool's removal primitive — `rm -f` plus the bounded wait for
	// the engine to release the name (ADR-0013 §6, D5.2).
	if !d.removeContainer(name) {
		t.Fatalf("%s did not release the container name %q — a lingering name is exactly what fails the "+
			"recreate on podman", eng.EngineNameForError(), name)
	}

	// RUN B: the same spec from the same name. A fresh container, not a restart.
	runBArgs := createRun()
	if len(runAArgs) != len(runBArgs) {
		t.Fatalf("run A and run B were created from different argv: %v vs %v", runAArgs, runBArgs)
	}
	for i := range runAArgs {
		if runAArgs[i] != runBArgs[i] {
			t.Fatalf("run A and run B were created from different argv: %v vs %v", runAArgs, runBArgs)
		}
	}

	// THE ASSERTION: nothing of run A crossed the boundary.
	for _, probe := range pristineStateProbes {
		if out, err := d.engine("exec", name, "sh", "-c", probe); err != nil {
			t.Fatalf("PRISTINE RESET VIOLATED on %s: %q failed after rm -f + recreate (%s) — run B inherited "+
				"state from run A, so the sandbox boundary is not a boundary on this platform (BF-3(b))",
				eng.EngineNameForError(), probe, strings.TrimSpace(out))
		}
	}
}
