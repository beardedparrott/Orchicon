package main

// daemon_data_dir_test.go — A LAZY CONFIG IS NOT A CONFIG AT STARTUP.
//
// The runtime daemon resolved its container binary and populated the refusal's data-dir field from
// `daemonCfg.DataDir`. That variable is filled in by daemonPool's sync.Once — the first time the
// database is opened — which has NOT happened when the daemon is constructed. So both reads saw the
// zero value:
//
//   - ResolveContainerExe was handed "" as the data dir, so it searched beside the daemon's executable
//     only, and never looked in <data dir>/bin at all;
//   - ContainerExeDataDir was "", so the refusal printed the literal placeholder "<data dir>" where a
//     real path belongs.
//
// Both were reported as "the search path moved but the file is still missing", with the placeholder
// visible in the operator's quoted message. Neither was reachable by a test of the resolver alone,
// because the resolver was given the right answer for an empty string.
//
// These assertions are SOURCE-LEVEL on purpose: the defect is the ORDER of two statements in a function
// that needs a database, an engine and a socket to run. Pinning "this value is not read from the lazy
// config" is the only check that bites without booting a daemon.

import (
	"os"
	"strings"
	"testing"
)

func readRuntimeGo(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	return string(b)
}

// TestTheDaemonsDataDirIsNotReadFromTheLazyConfig is the regression assertion.
func TestTheDaemonsDataDirIsNotReadFromTheLazyConfig(t *testing.T) {
	src := readRuntimeGo(t)

	// The lazy variable must not be used for either construction-time value.
	for _, bad := range []string{
		"os.Getenv(runtime.ContainerExeEnv), daemonCfg.DataDir",
		"ContainerExeDataDir: daemonCfg.DataDir",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("%q reads daemonCfg, which is the ZERO value at daemon start (daemonPool fills it in "+
				"lazily) — the container-binary search silently loses the data dir and the refusal prints "+
				"\"<data dir>\"", bad)
		}
	}

	// And the eager accessor is what is used instead.
	if !strings.Contains(src, "func daemonDataDir() string") {
		t.Fatal("daemonDataDir is missing — the data dir must be resolved eagerly, not from daemonCfg")
	}
	if strings.Count(src, "daemonDataDir()") < 3 { // definition + the two call sites
		t.Errorf("daemonDataDir() is not used for both construction-time values; got %d occurrences",
			strings.Count(src, "daemonDataDir()"))
	}
}

// TestTheResolvedDataDirIsARealPath pins the other half: the value that reaches the refusal is a path,
// never the placeholder the resolver falls back to when it is handed nothing.
func TestTheResolvedDataDirIsARealPath(t *testing.T) {
	t.Setenv("ORCHICON_DATA_DIR", "/tmp/orchicon-datadir-test")
	if got := daemonDataDir(); got != "/tmp/orchicon-datadir-test" {
		t.Fatalf("daemonDataDir() = %q, want the configured state dir", got)
	}
	// With no override the per-OS default must still be a real directory, not empty: an empty value is
	// exactly the failure above.
	t.Setenv("ORCHICON_DATA_DIR", "")
	if got := daemonDataDir(); strings.TrimSpace(got) == "" {
		t.Fatal("daemonDataDir() is empty with no ORCHICON_DATA_DIR — the refusal would print its placeholder")
	}
}
