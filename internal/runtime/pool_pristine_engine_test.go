package runtime

// What it CANNOT prove is the property the sandbox's premise rests on: that a real engine's `rm -f`
// + recreate really hands back a container with nothing carried over — installed packages.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// pristineStateProbes are the three classes of state a run leaves behind, each expressed as a shell
// test that must SUCCEED (i.e. the path is absent) in the container that run B receives.
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

// TestPristineResetRealEngine writes state in run A, releases (the pool's removal primitive),
// receives the recreated container as run B, and asserts every piece of run A's state is gone (AC
// 1).
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
	// containerID reads the engine's id for the container, so run A and run B can be shown to be
	// DIFFERENT containers (a "reset" that resumed the previous one would keep its id.
	containerID := func() string {
		t.Helper()
		out, err := d.engine("inspect", "--format", "{{.Id}}", name)
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("could not read the container id for %q (%s): %v", name, strings.TrimSpace(out), err)
		}
		return strings.TrimSpace(out)
	}
	// createRun runs ONE container from the same name + spec — the "identical spec" half of the reset.
	createRun := func(fatal bool) []string {
		t.Helper()
		args := []string{"run", "-d", "--name", name,
			"--label", "orchicon.workflow=pristine-reset-test",
			image, "sleep", "1800",
		}
		if out, err := d.engine(args...); err != nil {
			d.removeContainer(name)
			if fatal {
				t.Fatalf("run B could not be created after the removal primitive reported the name released "+
					"(%s) — on %s this is the lingering-name failure the reset's bounded wait exists to "+
					"prevent, i.e. a reset that silently fails (BF-3(b) / ADR-0013 D5.2): %v",
					strings.TrimSpace(out), eng.EngineNameForError(), err)
			}
			t.Skipf("could not start %q with /bin/sh + sleep on %s (%s) — set ORCHICON_RUNTIME_TEST_IMAGE "+
				"to an image that has them; the REAL-ENGINE pristine tier did NOT run",
				image, eng.EngineNameForError(), strings.TrimSpace(out))
		}
		return args
	}
	cleanup := func() { d.removeContainer(name) }
	defer cleanup()

	// RUN A: a real container, real state written into its writable layer.
	runAArgs := createRun(false)
	idA := containerID()
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

	// RUN B: the same spec from the same name.
	runBArgs := createRun(true)
	if idB := containerID(); idB == idA {
		t.Fatalf("run B is the SAME container as run A (id %s) — the removal primitive did not recreate "+
			"it, so the writable layer was carried across the run boundary on %s (BF-3(b))",
			idA, eng.EngineNameForError())
	}
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
