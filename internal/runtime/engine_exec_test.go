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
)

// TestDaemonExecutesTheResolvedEngineBinary installs a fake `podman` ahead of
// the real PATH, resolves the engine from the environment, and asserts the
// daemon ran THAT file with the args it was given.
func TestDaemonExecutesTheResolvedEngineBinary(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	shim := filepath.Join(dir, "podman")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\necho fake-podman-ran\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

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
