package main

// quiet_scope_test.go — EVERY package that spawns from a console-less process must use its quiet helper.
//
// THE COMPLAINT WAS REPORTED TWICE, and the second time the reason was SCOPE:
//
//	"ORCHICON on Windows pops up a ton of command prompt windows"
//	"No, it's happening in the desktop app when you install a new server until it says 'setup complete'"
//
// The first fix went into cmd/orchicon. But an install is a SUBTREE: `orchicon install` is spawned detached
// by the desktop (no console), and it reaches internal/containerengine (the engine probe),
// internal/planecontrol (the bridge address, the plane verbs) and internal/runtime (the daemon, which is
// itself started detached). Win32 gives a console-subsystem child of a console-less process a BRAND NEW
// WINDOW, so a single helper that forgot the attribute anywhere in that subtree put a window on the screen —
// and during an install those arrive in a burst.
//
// quiet_test.go covers this package. THIS file covers the other three, so a package added to the subtree
// later is caught here rather than by the next person installing on Windows.
//
// THE HELPER IS DUPLICATED ON PURPOSE. None of these packages may import another (cmd/orchicon is package
// main; planecontrol's imports test forbids depending on its neighbours), so each carries six lines behind
// this guard. A shared copy would be a layering violation to save six lines.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quietPackages are the packages that spawn host tools while running with no console of their own, with the
// files in each that do the spawning.
//
// FILES NOT LISTED, deliberately — a console is what they are for:
//   - cmd/orchicon/console_windows.go      creates the setup's own console
//   - cmd/orchicon/install_detach_windows.go  the visible setup relaunch
//   - internal/runtime/procattr_*.go       process-group helpers, not spawn sites
//
// repoPrefix is how this test reaches the repository root: the go test working directory is THIS package.
const repoPrefix = "../../"

var quietPackages = map[string][]string{
	"internal/planecontrol":    {"env.go", "hostproc_windows.go", "planecontrol.go"},
	"internal/containerengine": {"detect.go"},
	"internal/runtime":         {"agent.go", "daemon.go", "images.go", "lifecycle.go"},
}

func TestEveryPackageInTheInstallSubtreeIsQuiet(t *testing.T) {
	inspected := 0
	for pkg, files := range quietPackages {
		// THE HELPER MUST EXIST IN EVERY ONE. A package with no quiet.go has no way to spawn quietly, so
		// its call sites cannot be right however they read.
		if _, err := os.Stat(filepath.Join(repoPrefix, pkg, "quiet.go")); err != nil {
			t.Errorf("%s has no quiet.go — it spawns host tools from a console-less process, so it needs "+
				"the same helper as cmd/orchicon (see that package's quiet.go)", pkg)
		}
		for _, name := range files {
			path := filepath.Join(repoPrefix, pkg, name)
			src, err := os.ReadFile(path)
			if err != nil {
				// A RENAMED FILE MUST FAIL rather than quietly shrink the guard.
				t.Fatalf("cannot read %s: %v — if it was renamed, update quietPackages", path, err)
			}
			body := string(src)
			for _, banned := range []string{"exec.Command(", "exec.CommandContext("} {
				if i := strings.Index(body, banned); i >= 0 {
					line := 1 + strings.Count(body[:i], "\n")
					t.Errorf("%s:%d spawns a helper tool with a bare %s. This package runs inside a "+
						"DETACHED install (no console), so Win32 gives every console child a NEW WINDOW — "+
						"reported as \"it's happening in the desktop app when you install a new server\". "+
						"Use quietCommand/quietCommandContext (see %s/quiet.go)",
						path, line, banned, pkg)
				}
			}
			inspected++
		}
	}
	// VACUITY: a guard that inspected nothing proves nothing.
	if inspected < 6 {
		t.Fatalf("only %d files inspected; the scope guard is not doing its job", inspected)
	}
}

// TestEveryQuietHelperSetsTheAttribute pins the helper's own contract in every copy, so no package can end up
// with a helper that is only a rename.
func TestEveryQuietHelperSetsTheAttribute(t *testing.T) {
	pkgs := []string{"internal/planecontrol", "internal/containerengine", "internal/runtime"}
	for _, pkg := range pkgs {
		src, err := os.ReadFile(filepath.Join(repoPrefix, pkg, "quiet.go"))
		if err != nil {
			t.Fatalf("cannot read %s/quiet.go: %v", pkg, err)
		}
		// COUNTED, not grepped once: a helper that set the attribute on only the context variant would leave
		// every plain call unguarded.
		if got := strings.Count(string(src), "setProcAttrNoWindow(cmd)"); got != 2 {
			t.Errorf("%s/quiet.go applies the no-window attribute %d times, want 2 (one per helper). A "+
				"helper that does not set it is a bare exec.Command wearing a better name", pkg, got)
		}
	}
}

// TestNoWindowFlagsAreRealOnWindows checks the constant, not just the call: an attribute setter that passes
// the wrong flag is worse than none, because it looks fixed.
func TestNoWindowFlagsAreRealOnWindows(t *testing.T) {
	for _, pkg := range []string{"internal/planecontrol", "internal/containerengine", "internal/runtime"} {
		path := filepath.Join(repoPrefix, pkg, "procattr_windows.go")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		if !strings.Contains(string(src), "createNoWindow = 0x08000000") {
			t.Errorf("%s does not define CREATE_NO_WINDOW (0x08000000)", path)
		}
		if !strings.Contains(string(src), "func setProcAttrNoWindow(cmd *exec.Cmd)") {
			t.Errorf("%s has no setProcAttrNoWindow", path)
		}
	}
}
