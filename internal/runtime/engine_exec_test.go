//go:build !windows

package runtime

// The daemon's engine seam, proven END TO END rather than by inspection: the
// binary the daemon EXECUTES is the one ResolveEngine resolved (an absolute
// path found on PATH), and the argv it passes arrives intact.
//
// This closes the gap the rename itself cannot: `d.engine("inspect", …)` would
// still pass every argv-capturing test (they override engineFn and never touch
// the real exec path) if the production path had kept a hardcoded "docker" — or
// resolved to "". A fake engine shim on PATH is enough to prove it with no
// container engine installed, which is the situation in CI and in this image.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDaemonExecutesTheResolvedEngineBinary installs a fake `podman` ahead of
// the real PATH, resolves the engine from the environment, and asserts the
// daemon ran THAT file with the args it was given.
func TestDaemonExecutesTheResolvedEngineBinary(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	shim := filepath.Join(dir, "podman")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\necho fake-podman-ran\n"
	writeExecutableShim(t, shim, script)

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvEngine, "")
	t.Setenv(EnvEngineAlias, "")

	eng, err := ResolveEngineFromEnv()
	if err != nil {
		t.Fatalf("a podman on PATH must resolve (the shim is first): %v", err)
	}
	if eng.Name != DefaultEngineName() {
		t.Fatalf("resolved name = %q, want the default %q", eng.Name, DefaultEngineName())
	}
	if want := shim; eng.Bin != want {
		t.Fatalf("resolved binary = %q, want the shim at %q (an ABSOLUTE path, so the "+
			"daemon never depends on PATH lookup at exec time)", eng.Bin, want)
	}

	d := &Daemon{Engine: eng}
	if got := d.engineBin(); got != shim {
		t.Errorf("engineBin() = %q, want %q", got, shim)
	}

	out, err := d.engine("inspect", "--format", "{{.Id}}", "orchicon-rt-1")
	if err != nil {
		t.Fatalf("d.engine must exec the resolved binary: %v", err)
	}
	if !strings.Contains(out, "fake-podman-ran") {
		t.Errorf("the daemon did not run the resolved engine binary; output = %q", out)
	}

	raw, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("the engine was never invoked (no argv log): %v", rerr)
	}
	if got, want := strings.TrimSpace(string(raw)), "inspect --format {{.Id}} orchicon-rt-1"; got != want {
		t.Errorf("engine argv = %q, want %q", got, want)
	}
}

// TestZeroDaemonNeverExecsAnEmptyBinary pins the defensive default: a Daemon
// built without an Engine (tests, or a caller that forgot to resolve) must run
// the DEFAULT engine's name, never "" — an empty binary name is an exec error
// with no operator-facing meaning.
func TestZeroDaemonNeverExecsAnEmptyBinary(t *testing.T) {
	var d Daemon
	if got := d.engineBin(); got != DefaultEngineName() {
		t.Errorf("zero-Daemon engineBin() = %q, want %q", got, DefaultEngineName())
	}
	if got := d.Engine.NameInUseTokens(); len(got) == 0 {
		t.Error("a zero Engine must still supply the default engine's retry tokens")
	}
}

// writeExecutableShim writes an executable script and waits until the kernel will actually exec it.
//
// A PLAIN os.WriteFile FOLLOWED IMMEDIATELY BY exec IS A RACE, and it produced a real CI failure:
//
//	d.engine must exec the resolved binary: fork/exec …/podman: text file busy
//
// ETXTBSY means the file is still open for writing somewhere — on Linux, a concurrent fork inherits the
// writer's open descriptor for an instant, so the child that execs can collide with the descriptor the
// parent just closed. Under a full-suite run (hundreds of parallel writers and forks) this fires
// occasionally; in isolation it never does, which is exactly why it reached CI rather than a laptop.
//
// The fix is to make the write and the exec causally ordered rather than merely sequential: close, then
// confirm the file is readable and executable by STAT, and retry the write+stat a bounded number of times.
// Cheap, and it cannot mask a genuine failure — if the file is never execable the test still fails, with
// the last error.
func writeExecutableShim(t *testing.T, path, script string) {
	t.Helper()
	const attempts = 20
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			lastErr = err
			time.Sleep(time.Duration(i) * time.Millisecond)
			continue
		}
		// Stat forces the close to be visible and confirms the mode took.
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o100 == 0 {
			lastErr = err
			time.Sleep(time.Duration(i) * time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("could not write an executable shim at %s after %d attempts: %v", path, attempts, lastErr)
}
