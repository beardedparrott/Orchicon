package antigravity_test

// procspec_platform_test.go — the argv is a SINGLE-ELEMENT list, so a binary path containing a space
// never gets split.
//
// WHY THIS IS A REAL DEFECT AND NOT A THEORETICAL ONE. On Windows the agy installer lands under
// `%LOCALAPPDATA%\Programs\agy\agy.exe`, and `%LOCALAPPDATA%` is `C:\Users\<name>\AppData\Local` — a
// user name with a space in it is common. The claude adapter has the SCARS of getting this wrong and
// documents it at internal/claude/permissions.go (a hook path quoted precisely because Windows install
// paths contain spaces). acp.LaunchOptions.Argv renders exec.Cmd argv, which is NOT a shell string:
// each element is passed verbatim, so a space inside one is harmless PROVIDED nobody joins the argv
// into a command line and splits it back. This test pins that the space stays inside element 0.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity"
	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
)

// TestSpawnSpecKeepsASpaceInTheBinaryPath is AC2's argv half: a Windows install path with a space in
// EVERY component survives as one argv element.
func TestSpawnSpecKeepsASpaceInTheBinaryPath(t *testing.T) {
	const bin = `C:\Users\a b\AppData\Local\Programs\agy\agy.exe`

	spec, err := antigravity.BuildSpawnSpec(bin, antigravity.SpawnSettings{AuthMethod: "oauth-personal"}, "", nil)
	if err != nil {
		t.Fatalf("BuildSpawnSpec: %v", err)
	}
	if len(spec.Argv) == 0 {
		t.Fatal("BuildSpawnSpec returned an empty argv")
	}
	// ONE ELEMENT, WITH THE SPACE INTACT. A shell-joined argv would have produced
	// `C:\Users\a` as element 0 and `b\AppData\...` as element 1.
	if spec.Argv[0] != bin {
		t.Fatalf("argv[0] = %q, want the whole binary path %q — a space in a Windows install path must not "+
			"split the argument", spec.Argv[0], bin)
	}
	if !strings.Contains(spec.Argv[0], "a b") {
		t.Fatalf("argv[0] = %q lost the space in the user name", spec.Argv[0])
	}
	// NO SHELL QUOTING IS ADDED. exec.Cmd takes argv literally; embedding quotes would make the path
	// literally contain them and the spawn would fail on a name that looks correct in a log.
	if strings.ContainsAny(spec.Argv[0], `"'`) {
		t.Errorf("argv[0] = %q contains a quote character — argv elements are passed verbatim, so a quoted "+
			"path names a DIFFERENT file", spec.Argv[0])
	}
	// The ACP subcommand is still its own element, so the space did not swallow it.
	if len(spec.Argv) < 2 || spec.Argv[1] != "acp" {
		t.Fatalf("argv = %v, want argv[1] == \"acp\"", spec.Argv)
	}
	// Nothing in the argv is the API key, which is the invariant that must survive any quoting change.
	for _, a := range spec.Argv {
		if strings.Contains(a, testAPIKey) {
			t.Fatalf("argv carries the API key in %q — the key belongs only in the child's environment", a)
		}
	}
}

// TestSpawnSpecNeverPassesSandbox pins the cross-platform decision: `--sandbox` is NEVER on the argv.
// agy's own sandbox is per-OS (macOS sandbox-exec, Linux user namespaces, Win11 24H2+), so opting in
// would make the same worker behave differently on each platform — and would fail to launch on a host
// without the facility.
func TestSpawnSpecNeverPassesSandbox(t *testing.T) {
	for _, method := range authmethod.Methods() {
		spec, err := antigravity.BuildSpawnSpec("agy", antigravity.SpawnSettings{AuthMethod: method}, testAPIKey, nil)
		if err != nil {
			// A method that requires a key gets one above, so this is unexpected.
			t.Fatalf("BuildSpawnSpec(%q): %v", method, err)
		}
		for _, a := range spec.Argv {
			if a == "--sandbox" {
				t.Fatalf("argv for auth method %q contains --sandbox — agy's own sandbox is per-OS "+
					"(macOS sandbox-exec, Linux user namespaces, Win11 24H2+), so passing it makes a run "+
					"behave differently on each platform and breaks on a host without the facility. Argv: %v",
					method, spec.Argv)
			}
		}
	}
}

// TestSpawnSpecBinaryDefaultsToAgyOnPath keeps the "" case honest: with no resolved binary, the argv
// names the PATH lookup `agy` rather than an absolute path invented here.
func TestSpawnSpecBinaryDefaultsToAgyOnPath(t *testing.T) {
	spec, err := antigravity.BuildSpawnSpec("", antigravity.SpawnSettings{AuthMethod: "oauth-personal"}, "", nil)
	if err != nil {
		t.Fatalf("BuildSpawnSpec: %v", err)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] != "agy" {
		t.Fatalf("argv = %v, want argv[0] == \"agy\"", spec.Argv)
	}
	// A bare name is a PATH lookup by exec.Cmd; an absolute path here would break every host whose
	// agy is somewhere else.
	if filepath.IsAbs(spec.Argv[0]) {
		t.Errorf("argv[0] = %q is absolute, want the bare PATH name", spec.Argv[0])
	}
}
